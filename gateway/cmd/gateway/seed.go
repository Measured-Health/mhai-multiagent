package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	_ "time/tzdata" // IANA zones for --time-zone checks, even in a minimal image

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/account"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/db"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/password"
)

const seedUsage = `usage:
  gateway hospitals create --name N --short-code CODE --time-zone TZ [--require-mfa-for-clinicians] [--allow-trust-device]
  gateway users create --email E --name N --roles clinician,hospital_admin [--hospital CODE]

The password is never an argument: users create prompts for it on a terminal
and reads one line from stdin otherwise. Both commands write through the
owner/migrate connection in DATABASE_URL (the same variable "gateway migrate"
uses), not the gateway's runtime login: that login cannot insert users, roles
or hospitals. Run them in the one-shot migrate container, whose login is the
provisioning role (a superuser or a role with BYPASSRLS, because the tables
have forced row-level security and the owner has no insert policies).`

var errArgs = errors.New("invalid arguments (run gateway hospitals|users create -h)")

// seed runs "hospitals create" or "users create". url is the owner/migrate
// connection. Errors and output never contain the email, name or password.
func seed(ctx context.Context, args []string, url string, stdin io.Reader, out io.Writer) error {
	if len(args) < 2 || args[1] != "create" {
		return errors.New(seedUsage)
	}
	switch args[0] {
	case "hospitals":
		return hospitalsCreate(ctx, args[2:], url, out)
	case "users":
		return usersCreate(ctx, args[2:], url, stdin, out)
	}
	return errors.New(seedUsage)
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

func usersCreate(ctx context.Context, args []string, url string, stdin io.Reader, out io.Writer) error {
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
	if err := account.ValidateRoles(list, code); err != nil {
		return err
	}
	pw, err := obtainPassword(stdin)
	if err != nil {
		return err
	}
	if err := password.Check(pw); err != nil {
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
// ending only (spaces are part of a password). At most 1 KiB is read.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 1024)).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errors.New("no password on stdin")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return "", errors.New("no password on stdin")
	}
	return line, nil
}

// openOwner connects as the owner/migrate login. It does not run
// db.CheckLoginRole: that check exists to keep the gateway off such logins.
func openOwner(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	return db.Open(ctx, url)
}
