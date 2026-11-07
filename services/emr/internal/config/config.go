// Package config reads the emr service's settings from the environment
// (plan Task 4). Errors name the variable, never its value.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Settings are the emr service's settings.
type Settings struct {
	// Env is APP_ENV: development, staging or production. Unset is
	// production: what development relaxes stays off unless it is named.
	Env string
	// NATSURL is EMR_NATS_URL, without credentials: the password or NKey
	// seed comes from its own variable.
	NATSURL string
	// NATSNKeyFile is EMR_NATS_NKEY_FILE, the NKey seed file (required
	// outside development).
	NATSNKeyFile string
	// NATSPassword is EMR_NATS_PASSWORD, development only.
	NATSPassword string
	// DatabaseURL is EMR_DATABASE_URL, the emr_app login (sslmode
	// verify-full outside development).
	DatabaseURL string
	// TrustedKeysPath is EMR_TRUSTED_KEYS_PATH, the request-token
	// trusted-keys file core writes (public keys only).
	TrustedKeysPath string
	// SealKeyPath is EMR_SEAL_KEY_PATH, the sealing private key
	// (emr keys seal-init).
	SealKeyPath string
	// SecretsDir is EMR_SECRETS_DIR, the file secret store (PR 3).
	SecretsDir string
	// ProxyURL and ProxyKeyPath are EMR_PROXY_URL and EMR_PROXY_KEY_PATH,
	// egress-emr and its K_proxy key (PR 4); both or neither.
	ProxyURL, ProxyKeyPath string
	// RuleSetActive is EMR_RULE_SET_ACTIVE: empty means no rule set is
	// active and every finding is shadow (off by default).
	RuleSetActive string
}

// Development reports whether APP_ENV is development.
func (s Settings) Development() bool { return s.Env == "development" }

var ruleSetName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// FromEnv reads the settings with getenv (os.Getenv in main).
func FromEnv(getenv func(string) string) (Settings, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	s := Settings{
		Env:             get("APP_ENV"),
		NATSURL:         get("EMR_NATS_URL"),
		NATSNKeyFile:    get("EMR_NATS_NKEY_FILE"),
		NATSPassword:    getenv("EMR_NATS_PASSWORD"),
		DatabaseURL:     get("EMR_DATABASE_URL"),
		TrustedKeysPath: get("EMR_TRUSTED_KEYS_PATH"),
		SealKeyPath:     get("EMR_SEAL_KEY_PATH"),
		SecretsDir:      get("EMR_SECRETS_DIR"),
		ProxyURL:        get("EMR_PROXY_URL"),
		ProxyKeyPath:    get("EMR_PROXY_KEY_PATH"),
		RuleSetActive:   get("EMR_RULE_SET_ACTIVE"),
	}
	var errs []error
	switch s.Env {
	case "":
		s.Env = "production"
	case "development", "staging", "production":
	default:
		errs = append(errs, errors.New("APP_ENV must be development, staging or production"))
	}
	for name, v := range map[string]string{"EMR_NATS_URL": s.NATSURL, "EMR_DATABASE_URL": s.DatabaseURL,
		"EMR_TRUSTED_KEYS_PATH": s.TrustedKeysPath, "EMR_SEAL_KEY_PATH": s.SealKeyPath} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	if s.NATSURL != "" {
		if err := checkNATSURL(s.NATSURL, s.Development()); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case s.NATSNKeyFile != "" && s.NATSPassword != "":
		errs = append(errs, errors.New("set one of EMR_NATS_NKEY_FILE and EMR_NATS_PASSWORD, not both"))
	case s.NATSNKeyFile == "" && s.NATSPassword == "":
		errs = append(errs, errors.New("EMR_NATS_NKEY_FILE is required (EMR_NATS_PASSWORD in development)"))
	case s.NATSPassword != "" && !s.Development():
		errs = append(errs, errors.New("EMR_NATS_PASSWORD is for development only; use EMR_NATS_NKEY_FILE"))
	case s.NATSPassword != "" && placeholder(s.NATSPassword):
		errs = append(errs, errors.New("EMR_NATS_PASSWORD is a placeholder"))
	}
	if s.DatabaseURL != "" && !s.Development() && !verifyFull(s.DatabaseURL) {
		errs = append(errs, errors.New("EMR_DATABASE_URL must be a postgres:// URL with sslmode=verify-full outside development"))
	}
	if s.ProxyURL != "" || s.ProxyKeyPath != "" {
		if s.ProxyKeyPath == "" {
			errs = append(errs, errors.New("EMR_PROXY_KEY_PATH is required with EMR_PROXY_URL"))
		}
		u, err := url.Parse(s.ProxyURL)
		if s.ProxyURL == "" || err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" ||
			u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("EMR_PROXY_URL must be http://host:port, with no credentials"))
		}
	}
	if s.RuleSetActive != "" && !ruleSetName.MatchString(s.RuleSetActive) {
		errs = append(errs, errors.New("EMR_RULE_SET_ACTIVE must be a rule set name (lower-case letters, digits and -)"))
	}
	if err := errors.Join(errs...); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// checkNATSURL: nats:// or tls:// (tls:// only outside development), a
// host, no credentials. Errors never carry the URL.
func checkNATSURL(raw string, dev bool) error {
	for _, part := range strings.Split(raw, ",") {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls") {
			return errors.New("EMR_NATS_URL is invalid")
		}
		if u.User != nil {
			return errors.New("EMR_NATS_URL must not carry credentials (EMR_NATS_NKEY_FILE or EMR_NATS_PASSWORD)")
		}
		if u.Scheme != "tls" && !dev {
			return errors.New("EMR_NATS_URL must be tls:// outside development")
		}
	}
	return nil
}

// verifyFull reports whether raw is a postgres URL whose one sslmode is
// verify-full.
func verifyFull(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return false
	}
	modes := u.Query()["sslmode"]
	return len(modes) == 1 && modes[0] == "verify-full"
}

func placeholder(v string) bool {
	lower := strings.ToLower(v)
	for _, p := range []string{"change-me", "changeme", "placeholder"} {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
