package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	_ "time/tzdata" // IANA zones for --time-zone checks, even in a minimal image
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

const seedUsage = `usage:
  core hospitals create --name N --short-code CODE --time-zone TZ [--require-mfa-for-clinicians] [--allow-trust-device]
  core hospitals activate <hospital id or short code>
  core hospitals model-policy set <hospital id or short code> --provider openai|anthropic|xai|none [--agreement-ref REF] [--budget TOKENS]
  core users create --email E --name N --roles clinician,hospital_admin [--hospital CODE]

The password is never an argument: users create prompts for it on a terminal
and reads one line from stdin otherwise.

Both commands connect with DATABASE_URL (the variable "core migrate" uses,
so run them in the one-shot container, not with the core service's own env) as a
provisioning login: a login that is a plain member of mhai_provision, for
example
  CREATE ROLE provisioner LOGIN PASSWORD '...' IN ROLE mhai_provision;
Each command runs SET LOCAL ROLE mhai_provision, which can insert hospitals,
settings, users and roles, read only hospitals(id, short_code, status), and
update only hospitals.status from setup_incomplete to active; it has no
BYPASSRLS or DELETE. The core service's runtime login must not be a member
of it (the core service refuses to start if it is).

hospitals activate moves a setup_incomplete hospital to active (POST /queries
needs an active hospital) and audits hospital.activated. An active hospital
is left as it is. It refuses any other status: a deactivated hospital is
reactivated through the platform app.

hospitals model-policy set sets the hospital's cloud model policy and audits
hospital.model_policy_changed (via cli, no actor). A provider must be enabled
in CORE_CLOUD_PROVIDERS and needs --agreement-ref (the recorded agreement
check's reference, e.g. DEMO-SYNTHETIC for the synthetic demo); none switches
the hospital back to its local models. --budget is the monthly token budget
(1 to 1000000000; unchanged when left out, 2000000 for a new policy). It
prints the hospital id and the provider only.`

var errArgs = errors.New("invalid arguments (run core hospitals|users create -h)")

var errActivateArgs = errors.New("usage: core hospitals activate <hospital id or short code>")

// seed runs "hospitals create", "hospitals activate" or "users create". url
// is the provisioning login. Errors and output never contain the email, name
// or password.
func seed(ctx context.Context, args []string, url string, stdin io.Reader, out io.Writer) error {
	if len(args) >= 2 && args[0] == "hospitals" && args[1] == "activate" {
		return hospitalsActivate(ctx, args[2:], url, out)
	}
	if len(args) >= 2 && args[0] == "hospitals" && args[1] == "model-policy" {
		return hospitalsModelPolicy(ctx, args[2:], url, os.Getenv, out)
	}
	if len(args) < 2 || args[1] != "create" {
		return fmt.Errorf("%s", seedUsage)
	}
	switch args[0] {
	case "hospitals":
		return hospitalsCreate(ctx, args[2:], url, out)
	case "users":
		return usersCreate(ctx, args[2:], url, stdin, out)
	}
	return fmt.Errorf("%s", seedUsage)
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors echo input; report a fixed message instead
	return fs
}

func hospitalsCreate(ctx context.Context, args []string, url string, out io.Writer) error {
	fs := newFlags("hospitals create")
	h := account.NewHospital{}
	fs.StringVar(&h.Name, "name", "", "")
	fs.StringVar(&h.ShortCode, "short-code", "", "")
	fs.StringVar(&h.TimeZone, "time-zone", "", "")
	fs.BoolVar(&h.RequireMFAForClinicians, "require-mfa-for-clinicians", false, "")
	fs.BoolVar(&h.AllowTrustDevice, "allow-trust-device", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errArgs
	}
	if _, ok := account.NormaliseShortCode(h.ShortCode); !ok {
		return account.ErrBadShortCode
	}
	if strings.TrimSpace(h.Name) == "" {
		return errors.New("--name is required")
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	id, err := account.CreateHospital(ctx, pool, h)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "created hospital %s\n", id)
	return nil
}

// hospitalsActivate is "hospitals activate <id or short code>"
// (account.ActivateHospital). The reference is checked before connecting.
func hospitalsActivate(ctx context.Context, args []string, url string, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errActivateArgs
	}
	ref := args[0]
	if err := account.ValidateHospitalRef(ref); err != nil {
		return err
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	id, already, err := account.ActivateHospital(ctx, pool, ref)
	if err != nil {
		return err
	}
	if already {
		fmt.Fprintf(out, "hospital %s is already active; nothing changed\n", id)
		return nil
	}
	fmt.Fprintf(out, "activated hospital %s\n", id)
	return nil
}

var (
	errModelPolicyArgs = errors.New("usage: core hospitals model-policy set <hospital id or short code> " +
		"--provider openai|anthropic|xai|none [--agreement-ref REF] [--budget TOKENS]")
	errUnknownProvider   = errors.New("--provider must be openai, anthropic, xai or none")
	errProviderDisabled  = errors.New("that provider is not enabled in CORE_CLOUD_PROVIDERS")
	errNeedsAgreementRef = errors.New("a provider needs --agreement-ref")
	errBadAgreementRef   = errors.New("--agreement-ref must be 3 to 64 of A-Z, 0-9, _, . and -, starting with a letter or digit")
	errBadBudget         = errors.New("--budget must be a whole number from 1 to 1000000000")
	errModelPolicyWrite  = errors.New("could not set the model policy; nothing was changed")
)

// hospitalsModelPolicy is "hospitals model-policy set <id or short code>
// --provider P [--agreement-ref R] [--budget N]" (platform.ApplyModelPolicy
// as mhai_provision, via cli). Everything is checked before connecting; the
// provider must be enabled in CORE_CLOUD_PROVIDERS (read with getenv).
// Errors and output never hold the reference.
func hospitalsModelPolicy(ctx context.Context, args []string, url string, getenv func(string) string, out io.Writer) error {
	if len(args) < 2 || args[0] != "set" || strings.HasPrefix(args[1], "-") {
		return errModelPolicyArgs
	}
	ref := args[1]
	fs := newFlags("hospitals model-policy set")
	var provider, agreement string
	var budget int64
	fs.StringVar(&provider, "provider", "", "")
	fs.StringVar(&agreement, "agreement-ref", "", "")
	fs.Int64Var(&budget, "budget", 0, "")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || provider == "" {
		return errModelPolicyArgs
	}
	budgetSet := false
	fs.Visit(func(f *flag.Flag) { budgetSet = budgetSet || f.Name == "budget" })
	if err := account.ValidateHospitalRef(ref); err != nil {
		return err
	}
	c := platform.ModelPolicyChange{Via: platform.ViaCLI, AgreementRef: agreement}
	switch {
	case provider == platform.ProviderNone:
	case slices.Contains(config.CloudProviderNames, provider):
		c.Provider = provider
	default:
		return errUnknownProvider
	}
	enabled, err := config.ParseCloudProviders(getenv("CORE_CLOUD_PROVIDERS"))
	if err != nil {
		return err
	}
	if c.Provider != "" && !slices.Contains(enabled, c.Provider) {
		return errProviderDisabled
	}
	if agreement != "" && !platform.AgreementRefPattern.MatchString(agreement) {
		return errBadAgreementRef
	}
	if c.Provider != "" && agreement == "" {
		return errNeedsAgreementRef
	}
	if budgetSet {
		if budget < 1 || budget > platform.MaxMonthlyTokenBudget {
			return errBadBudget
		}
		c.Budget = &budget
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	c.Hospital, err = account.WithHospital(ctx, pool, ref, func(tx pgx.Tx, hospital uuid.UUID) error {
		c.Hospital = hospital
		if _, err := platform.ApplyModelPolicy(ctx, tx, c); err != nil {
			if errors.Is(err, platform.ErrNoSuchHospital) {
				return account.ErrNoSuchHospital
			}
			return errModelPolicyWrite
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "model policy %s %s\n", c.Hospital, provider)
	return nil
}

func usersCreate(ctx context.Context, args []string, url string, stdin io.Reader, out io.Writer) error {
	if err := password.Preload(); err != nil {
		return err
	}
	fs := newFlags("users create")
	var email, name, roles, hospital string
	fs.StringVar(&email, "email", "", "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&roles, "roles", "", "")
	fs.StringVar(&hospital, "hospital", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errArgs
	}
	var list []string
	for _, r := range strings.Split(roles, ",") {
		if r = strings.TrimSpace(r); r != "" {
			list = append(list, r)
		}
	}
	code := ""
	if strings.TrimSpace(hospital) != "" {
		var ok bool
		if code, ok = account.NormaliseShortCode(hospital); !ok {
			return account.ErrBadShortCode
		}
	}
	// Check everything we can before asking for a password or connecting.
	if err := account.ValidateNewUser(account.NewUser{Email: email, Name: name, Roles: list, HospitalShortCode: code}); err != nil {
		return err
	}
	pw, err := obtainPassword(stdin)
	if err != nil {
		return err
	}
	if err := password.Check(pw, account.NormaliseEmail(email)); err != nil {
		return err
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	id, err := account.CreateUser(ctx, pool, account.NewUser{Email: email, Name: name, Roles: list,
		HospitalShortCode: code, Password: pw})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "created user %s (must change password at first sign-in)\n", id)
	return nil
}

// obtainPassword prompts without echo on a terminal (twice, to catch typos),
// and reads a line from stdin when there is no terminal (Ruling 4).
func obtainPassword(stdin io.Reader) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		fmt.Fprint(os.Stderr, "Password: ")
		a, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", errors.New("could not read password")
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", errors.New("could not read password")
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		return string(a), nil
	}
	return readPassword(stdin)
}

// readPassword takes the first line of r as the password, minus the line
// ending only (spaces are part of a password). A line over 1024 bytes, or
// invalid UTF-8, is refused rather than truncated.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxPasswordBytes+1)).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errors.New("no password on stdin")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) > maxPasswordBytes {
		return "", errors.New("password too long")
	}
	if line == "" {
		return "", errors.New("no password on stdin")
	}
	if !utf8.ValidString(line) {
		return "", errors.New("password is not valid UTF-8")
	}
	return line, nil
}

const maxPasswordBytes = 1024

// openOwner connects with the provisioning login in DATABASE_URL. It does not
// run db.CheckLoginRole: that check exists to keep the gateway off such
// logins. It does load the breached list now, so a broken build fails before
// a password is accepted.
func openOwner(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	return db.Open(ctx, url)
}
