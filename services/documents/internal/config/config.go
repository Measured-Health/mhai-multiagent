// Package config reads the documents service's settings from the
// environment (ADR 0016; documents design §17). It fails closed: a missing,
// malformed or relaxed setting stops the service. Errors name the
// variable, never its value.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Settings are the documents service's settings.
type Settings struct {
	// Env is APP_ENV: development, staging or production. Unset is
	// production: what development relaxes stays off unless it is named.
	Env string
	// NATSURL is DOCUMENTS_NATS_URL, without credentials: the password or
	// NKey seed comes from its own variable.
	NATSURL string
	// NATSNKeyFile is DOCUMENTS_NATS_NKEY_FILE, the NKey seed file
	// (required outside development).
	NATSNKeyFile string
	// NATSPassword is DOCUMENTS_NATS_PASSWORD, development only.
	NATSPassword string
	// DatabaseURL is DOCUMENTS_DATABASE_URL, the service's own login
	// (role mhai_documents, PR 3); sslmode verify-full outside development.
	DatabaseURL string
	// TrustedKeysPath is DOCUMENTS_TRUSTED_KEYS_PATH, the request-token
	// trusted-keys file core writes (public keys only), an absolute path.
	TrustedKeysPath string
}

// Development reports whether APP_ENV is development.
func (s Settings) Development() bool { return s.Env == "development" }

// FromEnv reads the settings with getenv (os.Getenv in main).
func FromEnv(getenv func(string) string) (Settings, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	s := Settings{
		Env:             get("APP_ENV"),
		NATSURL:         get("DOCUMENTS_NATS_URL"),
		NATSNKeyFile:    get("DOCUMENTS_NATS_NKEY_FILE"),
		NATSPassword:    getenv("DOCUMENTS_NATS_PASSWORD"),
		DatabaseURL:     get("DOCUMENTS_DATABASE_URL"),
		TrustedKeysPath: get("DOCUMENTS_TRUSTED_KEYS_PATH"),
	}
	var errs []error
	switch s.Env {
	case "":
		s.Env = "production"
	case "development", "staging", "production":
	default:
		errs = append(errs, errors.New("APP_ENV must be development, staging or production"))
	}
	// A fixed order, so the first error is always the same one.
	for _, kv := range [][2]string{{"DOCUMENTS_NATS_URL", s.NATSURL}, {"DOCUMENTS_DATABASE_URL", s.DatabaseURL},
		{"DOCUMENTS_TRUSTED_KEYS_PATH", s.TrustedKeysPath}} {
		if kv[1] == "" {
			errs = append(errs, fmt.Errorf("%s is required", kv[0]))
		}
	}
	if s.NATSURL != "" {
		if err := checkNATSURL(s.NATSURL, s.Development()); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case s.NATSNKeyFile != "" && s.NATSPassword != "":
		errs = append(errs, errors.New("set one of DOCUMENTS_NATS_NKEY_FILE and DOCUMENTS_NATS_PASSWORD, not both"))
	case s.NATSNKeyFile == "" && s.NATSPassword == "":
		errs = append(errs, errors.New("DOCUMENTS_NATS_NKEY_FILE is required (DOCUMENTS_NATS_PASSWORD in development)"))
	case s.NATSPassword != "" && !s.Development():
		errs = append(errs, errors.New("DOCUMENTS_NATS_PASSWORD is for development only; use DOCUMENTS_NATS_NKEY_FILE"))
	case s.NATSPassword != "" && placeholder(s.NATSPassword):
		errs = append(errs, errors.New("DOCUMENTS_NATS_PASSWORD is a placeholder"))
	}
	if s.DatabaseURL != "" {
		switch {
		case !postgresURL(s.DatabaseURL):
			errs = append(errs, errors.New("DOCUMENTS_DATABASE_URL must be a postgres:// URL"))
		case !s.Development() && !verifyFull(s.DatabaseURL):
			errs = append(errs, errors.New("DOCUMENTS_DATABASE_URL must have sslmode=verify-full outside development"))
		}
	}
	if s.TrustedKeysPath != "" && !filepath.IsAbs(s.TrustedKeysPath) {
		errs = append(errs, errors.New("DOCUMENTS_TRUSTED_KEYS_PATH must be an absolute path"))
	}
	if err := errors.Join(errs...); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// checkNATSURL: nats:// or tls:// (tls:// only outside development), a
// host, no credentials, for each comma-separated server. Errors never
// carry the URL.
func checkNATSURL(raw string, dev bool) error {
	for _, part := range strings.Split(raw, ",") {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls") {
			return errors.New("DOCUMENTS_NATS_URL is invalid")
		}
		if u.User != nil {
			return errors.New("DOCUMENTS_NATS_URL must not carry credentials (DOCUMENTS_NATS_NKEY_FILE or DOCUMENTS_NATS_PASSWORD)")
		}
		if u.Scheme != "tls" && !dev {
			return errors.New("DOCUMENTS_NATS_URL must be tls:// outside development")
		}
	}
	return nil
}

func postgresURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") && u.Host != ""
}

// verifyFull reports whether raw's one sslmode is verify-full.
func verifyFull(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
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
