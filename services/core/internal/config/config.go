// Package config loads the core service's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const minSecretLen = 32

// Config holds the core service's settings.
type Config struct {
	Env              string
	HTTPAddr         string
	DatabaseURL      string
	NATSURL          string
	NATSNkeySeedPath string // production: the service's NKey seed file
	AccessKeyPath    string
	RequestKeyPath   string // the request-token signing key (spec §7.2), never the access key
	// RequestTrustedKeysPath is the public trusted-keys file the workers
	// read (spec §7.5). Optional: with it, the dead-letter consumer can tell
	// a job refused for a removed kid (an emergency rotation) and re-mint
	// it; without it, such a job fails like any refusal.
	RequestTrustedKeysPath string
	MFAKey                 []byte
	BFFServiceKeys         []string
	BreachedListPath       string
	NATSTLS                NATSTLS // required outside development (ValidateServe)
	// NATSJSReplicas is the replica count for JOBS, DEAD and the KV
	// buckets: 1 in dev compose, 3 on the production cluster.
	NATSJSReplicas int
	// AllowSingleNode (CORE_ALLOW_SINGLE_NODE=true) lets a non-development
	// core run with fewer than 3 JetStream replicas, a single-node install
	// on purpose; serve logs a warning (SingleNode). There is no such
	// escape for the database's TLS.
	AllowSingleNode bool
	// JobsPipelineMaxAckPending bounds the pipeline consumer's jobs in
	// flight: the worker count times its concurrency.
	JobsPipelineMaxAckPending int
	// IntelligenceQueryCap is how many clinical questions one hospital may
	// have queued or running at once (INTELLIGENCE_QUERY_CAP, decision 8);
	// past it a submit is 429 rate_limited (intel.ReserveInFlight).
	IntelligenceQueryCap int
	// APIDocs serves the Redoc page and openapi.yaml (CORE_API_DOCS).
	APIDocs            bool
	HTTPAPI            bool
	NATSAPIConcurrency int
	// OTLPEndpoint is where traces and metrics go (OTEL_EXPORTER_OTLP_ENDPOINT,
	// OTLP/gRPC: a URL or host:port). Empty: telemetry is off.
	OTLPEndpoint string
	// OTLPInsecure (OTEL_EXPORTER_OTLP_INSECURE=true) allows a plaintext
	// non-loopback endpoint; development allows it too.
	OTLPInsecure bool
	// SecurityContactName and SecurityContactEmail are who the audit
	// integrity page names (SECURITY_CONTACT_NAME, SECURITY_CONTACT_EMAIL;
	// Plan 4 spec §5.2). Required outside development (ValidateServe); in
	// development, unset, GET /platform/audit/verify/status answers 503
	// not_ready and serve warns at start-up.
	SecurityContactName  string
	SecurityContactEmail string
	// CloudProviders are the cloud model providers this deployment enables
	// (CORE_CLOUD_PROVIDERS: a comma list of CloudProviderNames, each at
	// most once; empty by default). A hospital's policy may name only one of
	// these (cloud model providers plan, Task 2). Sorted.
	CloudProviders []string
	// EMREnabled (CORE_EMR_ENABLED=true) switches the EMR integration on:
	// the EMR stream and its durables, and later the connection routes and
	// the finding consumer. Off by default (EMR plan, Global Constraints).
	EMREnabled bool
	// EMRSealPublicKeyPath is the emr service's sealing public key
	// (CORE_EMR_SEAL_PUBLIC_KEY_PATH; EMR design §6.2), required when
	// EMREnabled. A public key: core never holds the private one.
	EMRSealPublicKeyPath string
}

// CloudProviderNames are the cloud model providers core knows (the closed
// vocabulary of the design; Python holds the same list).
var CloudProviderNames = []string{"anthropic", "openai", "xai"}

// CloudOutcomes are the cloud call's outcome codes (design §11), the closed
// set of query.completed's cloud_outcome. The worker holds the same list
// (boundary/cloud/vocab.py OUTCOMES; TestOutcomeVocabularyMatchesWorker).
var CloudOutcomes = []string{"ok", "egress_blocked", "budget_exhausted", "unconfigured", "unavailable", "timeout",
	"auth_failed", "refused", "truncated", "malformed", "insufficient", "retention_unconfirmed"}

// EgressValues are query.completed's egress codes: where an answer's text
// went, "none" or one of CloudProviderNames. A fresh slice each call.
func EgressValues() []string { return append([]string{"none"}, CloudProviderNames...) }

// errCloudProviders names the variable only, never its value.
var errCloudProviders = errors.New("CORE_CLOUD_PROVIDERS must list each of anthropic openai xai at most once, comma separated")

// ParseCloudProviders reads CORE_CLOUD_PROVIDERS: blank is none; otherwise
// each comma-separated name, trimmed, must be one of CloudProviderNames and
// appear once. The result is sorted. The error never holds the value.
func ParseCloudProviders(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if !slices.Contains(CloudProviderNames, p) || slices.Contains(out, p) {
			return nil, errCloudProviders
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

// NATSTLS names the PEM files for TLS to NATS with a client certificate.
type NATSTLS struct {
	CAPath   string // NATS_TLS_CA_PATH
	CertPath string // NATS_TLS_CERT_PATH
	KeyPath  string // NATS_TLS_KEY_PATH
}

// Load reads the environment. Secrets are required, at least 32 bytes and
// must not be placeholders. Errors name the variable, never its value.
func Load() (Config, error) {
	c := Config{
		// Unset is production: everything that development relaxes (the
		// API docs, NATS without TLS, test-only intelligence operations)
		// stays off unless development is named.
		Env:                    getenv("CORE_ENV", "production"),
		HTTPAddr:               os.Getenv("HTTP_ADDR"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		NATSURL:                os.Getenv("NATS_URL"),
		NATSNkeySeedPath:       os.Getenv("NATS_NKEY_SEED_PATH"),
		AccessKeyPath:          os.Getenv("ACCESS_KEY_PATH"),
		RequestKeyPath:         os.Getenv("REQUEST_KEY_PATH"),
		RequestTrustedKeysPath: os.Getenv("REQUEST_TRUSTED_KEYS_PATH"),
		BreachedListPath:       os.Getenv("BREACHED_LIST_PATH"),
		OTLPEndpoint:           strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		OTLPInsecure:           strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")), "true"),
		SecurityContactName:    strings.TrimSpace(os.Getenv("SECURITY_CONTACT_NAME")),
		SecurityContactEmail:   strings.TrimSpace(os.Getenv("SECURITY_CONTACT_EMAIL")),
		EMRSealPublicKeyPath:   strings.TrimSpace(os.Getenv("CORE_EMR_SEAL_PUBLIC_KEY_PATH")),
		NATSTLS: NATSTLS{
			CAPath:   os.Getenv("NATS_TLS_CA_PATH"),
			CertPath: os.Getenv("NATS_TLS_CERT_PATH"),
			KeyPath:  os.Getenv("NATS_TLS_KEY_PATH"),
		},
	}
	var errs []error

	var err error
	if c.NATSJSReplicas, err = intEnv("NATS_JS_REPLICAS", 1, func(n int) bool { return n == 1 || n == 3 || n == 5 }); err != nil {
		errs = append(errs, errors.New("NATS_JS_REPLICAS must be 1, 3 or 5"))
	}
	if c.JobsPipelineMaxAckPending, err = intEnv("JOBS_PIPELINE_MAX_ACK_PENDING", 8, func(n int) bool { return n >= 1 && n <= 100000 }); err != nil {
		errs = append(errs, errors.New("JOBS_PIPELINE_MAX_ACK_PENDING must be 1 to 100000"))
	}
	if c.IntelligenceQueryCap, err = intEnv("INTELLIGENCE_QUERY_CAP", 10, func(n int) bool { return n >= 1 && n <= 1000 }); err != nil {
		errs = append(errs, errors.New("INTELLIGENCE_QUERY_CAP must be 1 to 1000"))
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("CORE_ALLOW_SINGLE_NODE"))) {
	case "", "false":
	case "true":
		c.AllowSingleNode = true
	default:
		errs = append(errs, errors.New("CORE_ALLOW_SINGLE_NODE must be true or false"))
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("CORE_EMR_ENABLED"))) {
	case "", "false":
	case "true":
		c.EMREnabled = true
	default:
		errs = append(errs, errors.New("CORE_EMR_ENABLED must be true or false"))
	}

	if c.CloudProviders, err = ParseCloudProviders(os.Getenv("CORE_CLOUD_PROVIDERS")); err != nil {
		errs = append(errs, err)
	}

	if c.NATSAPIConcurrency, err = intEnv("CORE_NATS_API_CONCURRENCY", 256, func(n int) bool { return n >= 1 && n <= 100000 }); err != nil {
		errs = append(errs, errors.New("CORE_NATS_API_CONCURRENCY must be 1 to 100000"))
	}

	// CORE_HTTP_API: on until mhai-web's BFF uses NATS. Off, HTTP serves
	// the probes only, and only on a loopback address.
	c.HTTPAPI = true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CORE_HTTP_API"))) {
	case "", "on", "true":
	case "off", "false":
		c.HTTPAPI = false
	default:
		errs = append(errs, errors.New("CORE_HTTP_API must be on or off"))
	}
	switch {
	case c.HTTPAddr == "" && c.HTTPAPI:
		c.HTTPAddr = ":8080"
	case c.HTTPAddr == "":
		c.HTTPAddr = "127.0.0.1:8080"
	case !c.HTTPAPI && !loopback(c.HTTPAddr):
		errs = append(errs, errors.New("HTTP_ADDR must be a loopback address (127.0.0.1 or [::1]) when CORE_HTTP_API is off"))
	}

	// CORE_API_DOCS: on by default only in development, so production
	// (also the value when CORE_ENV is unset) or an unrecognised
	// environment fails closed.
	c.APIDocs = c.Development()
	if raw := strings.TrimSpace(os.Getenv("CORE_API_DOCS")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, errors.New("CORE_API_DOCS must be true or false"))
		}
		c.APIDocs = b
	}

	// Only this service's own credentials are read: the NATS password is in
	// NATS_URL (or the NKey seed), the database password in DATABASE_URL.
	// Other services' secrets (the worker's NATS password) are not in the
	// core service's environment at all.
	if err := checkNATSURL(c.NATSURL); err != nil {
		errs = append(errs, err)
	}
	if err := checkURLPassword("DATABASE_URL", c.DatabaseURL); err != nil {
		errs = append(errs, err)
	}

	if v, err := secret("MFA_ENCRYPTION_KEY"); err != nil {
		errs = append(errs, err)
	} else {
		c.MFAKey = []byte(v)
	}

	// The BFF key guards the HTTP API only; over NATS the BFF's NATS user
	// is the credential (spec §17.1).
	if raw := os.Getenv("BFF_SERVICE_KEYS"); strings.TrimSpace(raw) == "" {
		if c.HTTPAPI {
			errs = append(errs, errors.New("BFF_SERVICE_KEYS is required while CORE_HTTP_API is on"))
		}
	} else {
		for _, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(k)
			if err := checkSecret("BFF_SERVICE_KEYS", k); err != nil {
				errs = append(errs, err)
				break
			}
			c.BFFServiceKeys = append(c.BFFServiceKeys, k)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// pseudonymIn is mhai-web's PSEUDONYM_IN: a pseudonym anywhere in a string.
var pseudonymIn = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)

// SecurityContactOK reports whether name and email are a contact mhai-web
// shows: a trimmed name of 1 to 120 characters with no control or format
// character and nothing of the pseudonym's shape, and a plain address (no
// display name) of at most 254 characters.
func SecurityContactOK(name, email string) bool {
	if name == "" || strings.TrimSpace(name) != name || utf8.RuneCountInString(name) > 120 || !utf8.ValidString(name) ||
		strings.ContainsFunc(name, func(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }) ||
		pseudonymIn.MatchString(name) {
		return false
	}
	if email == "" || len(email) > 254 {
		return false
	}
	a, err := mail.ParseAddress(email)
	return err == nil && a.Address == email && a.Name == ""
}

func secret(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if err := checkSecret(name, v); err != nil {
		return "", err
	}
	return v, nil
}

// checkNATSURL rejects an unparsable NATS_URL and a placeholder password in
// it. Errors never carry the URL.
func checkNATSURL(raw string) error {
	if raw == "" {
		return nil
	}
	for _, part := range strings.Split(raw, ",") {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Host == "" {
			return errors.New("NATS_URL is invalid")
		}
		if pw, ok := u.User.Password(); ok {
			if err := checkPlaceholder("NATS_URL", pw); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkURLPassword rejects an unparsable URL and a placeholder password in
// it. Errors never carry the URL.
func checkURLPassword(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is invalid", name)
	}
	if pw, ok := u.User.Password(); ok {
		return checkPlaceholder(name, pw)
	}
	return nil
}

// ValidateServe checks what "core serve" needs beyond Load: the database,
// NATS, the access-token signing key and the request-token signing key, in
// separate files (spec §7.2: a leak or bug in one can't mint the other
// kind).
func (c Config) ValidateServe() error {
	var errs []error
	for name, v := range map[string]string{"DATABASE_URL": c.DatabaseURL, "NATS_URL": c.NATSURL,
		"ACCESS_KEY_PATH": c.AccessKeyPath, "REQUEST_KEY_PATH": c.RequestKeyPath} {
		if strings.TrimSpace(v) == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	if c.RequestKeyPath != "" && filepath.Clean(c.RequestKeyPath) == filepath.Clean(c.AccessKeyPath) {
		errs = append(errs, errors.New("REQUEST_KEY_PATH must be a different file from ACCESS_KEY_PATH"))
	}
	// NATS carries the frontend's API traffic and access tokens since
	// spec §17, so TLS with a client certificate is required everywhere
	// but development; an empty or unknown environment fails closed.
	t := c.NATSTLS
	anyTLS := t.CAPath != "" || t.CertPath != "" || t.KeyPath != ""
	allTLS := t.CAPath != "" && t.CertPath != "" && t.KeyPath != ""
	switch {
	case anyTLS && !allTLS:
		errs = append(errs, errors.New("NATS_TLS_CA_PATH, NATS_TLS_CERT_PATH and NATS_TLS_KEY_PATH must be set together"))
	case !allTLS && !c.Development():
		errs = append(errs, errors.New("NATS_TLS_CA_PATH, NATS_TLS_CERT_PATH and NATS_TLS_KEY_PATH are required outside development"))
	}
	if c.EMREnabled && c.EMRSealPublicKeyPath == "" {
		errs = append(errs, errors.New("CORE_EMR_SEAL_PUBLIC_KEY_PATH is required when CORE_EMR_ENABLED is true"))
	}
	// The audit integrity page names a security contact (Plan 4 §5.2):
	// required outside development, and always well formed when set.
	switch {
	case c.SecurityContactName == "" && c.SecurityContactEmail == "":
		if !c.Development() {
			errs = append(errs, errors.New("SECURITY_CONTACT_NAME and SECURITY_CONTACT_EMAIL are required outside development"))
		}
	case !SecurityContactOK(c.SecurityContactName, c.SecurityContactEmail):
		errs = append(errs, errors.New("SECURITY_CONTACT_NAME must be 1 to 120 characters with no control character, and SECURITY_CONTACT_EMAIL a plain address; set both"))
	}
	if !c.Development() {
		// Final review I-2: the database carries every hospital's clinical
		// data, so outside development the connection is TLS with the
		// server's certificate and name checked. There is no escape hatch.
		if c.DatabaseURL != "" && !verifyFull(c.DatabaseURL) {
			errs = append(errs, errors.New("DATABASE_URL must be a postgres:// URL with sslmode=verify-full outside development"))
		}
		// One replica loses every queued job and the denylist with its
		// node; a single-node install must say so.
		if c.NATSJSReplicas < 3 && !c.AllowSingleNode {
			errs = append(errs, errors.New("NATS_JS_REPLICAS must be 3 or more outside development (CORE_ALLOW_SINGLE_NODE=true for a single-node install)"))
		}
	}
	return errors.Join(errs...)
}

// verifyFull reports whether raw is a postgres:// (or postgresql://) URL
// whose one sslmode is verify-full. A key=value DSN is refused: a URL is
// the one form checked here. Never logs or returns raw.
func verifyFull(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return false
	}
	modes := u.Query()["sslmode"]
	return len(modes) == 1 && modes[0] == "verify-full"
}

// MinPasswordLen is the shortest database login password accepted.
const MinPasswordLen = 16

// CheckPassword checks a database login password read from the
// environment variable name: present, at least MinPasswordLen bytes and not
// a placeholder. Errors name the variable, never the value.
func CheckPassword(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	if err := checkPlaceholder(name, v); err != nil {
		return err
	}
	if len(v) < MinPasswordLen {
		return fmt.Errorf("%s must be at least %d bytes", name, MinPasswordLen)
	}
	return nil
}

func checkPlaceholder(name, v string) error {
	lower := strings.ToLower(v)
	for _, p := range []string{"change-me", "changeme", "placeholder"} {
		if strings.Contains(lower, p) {
			return fmt.Errorf("%s is a placeholder", name)
		}
	}
	return nil
}

func checkSecret(name, v string) error {
	if err := checkPlaceholder(name, v); err != nil {
		return err
	}
	if len(v) < minSecretLen {
		return fmt.Errorf("%s must be at least %d bytes", name, minSecretLen)
	}
	return nil
}

// intEnv reads an integer variable, def when unset, refused when it isn't
// a number or ok says no.
func intEnv(name string, def int, ok func(int) bool) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || !ok(n) {
		return def, fmt.Errorf("%s is invalid", name)
	}
	return n, nil
}

// loopback reports whether addr is host:port with a loopback IP literal
// for host. A name (localhost) is refused: it resolves at listen time.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Development reports whether CORE_ENV names development. Only then are
// the development relaxations on: API docs by default, NATS without TLS,
// test-only intelligence operations granted. Anything else, an unset
// CORE_ENV (production) included, fails closed.
// SingleNode reports whether this is a non-development core running on
// fewer than 3 JetStream replicas because CORE_ALLOW_SINGLE_NODE said so:
// serve logs a warning at start.
func (c Config) SingleNode() bool {
	return !c.Development() && c.AllowSingleNode && c.NATSJSReplicas < 3
}

func (c Config) Development() bool { return strings.EqualFold(strings.TrimSpace(c.Env), "development") }

func getenv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
