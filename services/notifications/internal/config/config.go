// Package config reads the notifications service's settings from the
// environment (ADR 0018; notifications design §15). It fails closed: a
// missing, malformed or relaxed setting stops the service. Errors name the
// variable, never its value. Everything is off by default: with
// NOTIFY_CHANNELS empty the service sends nothing.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

// ChannelLog is the development channel: it logs that a message would be
// sent, by kind and ids, and sends nothing. It is refused outside
// development (as core's CORE_DELIVERY=log).
const ChannelLog = "log"

// ChannelEmail is the email channel. It is not built yet (PR 4), so it is
// refused everywhere until it is.
const ChannelEmail = "email"

// Settings are the notifications service's settings.
type Settings struct {
	// Env is APP_ENV: development, staging or production. Unset is
	// production: what development relaxes stays off unless it is named.
	Env string
	// NATSURL is NOTIFY_NATS_URL, without credentials: the password or
	// NKey seed comes from its own variable.
	NATSURL string
	// NATSNKeyFile is NOTIFY_NATS_NKEY_FILE, the NKey seed file
	// (required outside development).
	NATSNKeyFile string
	// NATSPassword is NOTIFY_NATS_PASSWORD, development only.
	NATSPassword string
	// DatabaseURL is NOTIFY_DATABASE_URL, the service's own login (a
	// member of mhai_notify_app only); sslmode verify-full outside
	// development.
	DatabaseURL string
	// TrustedKeysPath is NOTIFY_TRUSTED_KEYS_PATH, the request-token
	// trusted-keys file core writes (public keys only), an absolute path.
	TrustedKeysPath string
	// Channels is NOTIFY_CHANNELS, a comma-separated list of the channels
	// the service may send on. Empty (the default): it sends nothing.
	Channels []string
}

// Development reports whether APP_ENV is development.
func (s Settings) Development() bool { return s.Env == "development" }

// FromEnv reads the settings with getenv (os.Getenv in main).
func FromEnv(getenv func(string) string) (Settings, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	s := Settings{
		Env:             get("APP_ENV"),
		NATSURL:         get("NOTIFY_NATS_URL"),
		NATSNKeyFile:    get("NOTIFY_NATS_NKEY_FILE"),
		NATSPassword:    getenv("NOTIFY_NATS_PASSWORD"),
		DatabaseURL:     get("NOTIFY_DATABASE_URL"),
		TrustedKeysPath: get("NOTIFY_TRUSTED_KEYS_PATH"),
	}
	channels, chErr := parseChannels(get("NOTIFY_CHANNELS"), s.Env == "development")
	s.Channels = channels
	var errs []error
	switch s.Env {
	case "":
		s.Env = "production"
	case "development", "staging", "production":
	default:
		errs = append(errs, errors.New("APP_ENV must be development, staging or production"))
	}
	// A fixed order, so the first error is always the same one.
	for _, kv := range [][2]string{{"NOTIFY_NATS_URL", s.NATSURL}, {"NOTIFY_DATABASE_URL", s.DatabaseURL},
		{"NOTIFY_TRUSTED_KEYS_PATH", s.TrustedKeysPath}} {
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
		errs = append(errs, errors.New("set one of NOTIFY_NATS_NKEY_FILE and NOTIFY_NATS_PASSWORD, not both"))
	case s.NATSNKeyFile == "" && s.NATSPassword == "":
		errs = append(errs, errors.New("NOTIFY_NATS_NKEY_FILE is required (NOTIFY_NATS_PASSWORD in development)"))
	case s.NATSPassword != "" && !s.Development():
		errs = append(errs, errors.New("NOTIFY_NATS_PASSWORD is for development only; use NOTIFY_NATS_NKEY_FILE"))
	case s.NATSPassword != "" && placeholder(s.NATSPassword):
		errs = append(errs, errors.New("NOTIFY_NATS_PASSWORD is a placeholder"))
	}
	if s.DatabaseURL != "" {
		switch {
		case !postgresURL(s.DatabaseURL):
			errs = append(errs, errors.New("NOTIFY_DATABASE_URL must be a postgres:// URL"))
		case !s.Development() && !verifyFull(s.DatabaseURL):
			errs = append(errs, errors.New("NOTIFY_DATABASE_URL must have sslmode=verify-full outside development"))
		}
	}
	if s.TrustedKeysPath != "" && !filepath.IsAbs(s.TrustedKeysPath) {
		errs = append(errs, errors.New("NOTIFY_TRUSTED_KEYS_PATH must be an absolute path"))
	}
	if chErr != nil {
		errs = append(errs, chErr)
	}
	if err := errors.Join(errs...); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// parseChannels reads NOTIFY_CHANNELS: names from a closed set, each once.
// log is development only; email is not built yet (PR 4).
func parseChannels(raw string, dev bool) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		c := strings.TrimSpace(part)
		switch {
		case slices.Contains(out, c):
			return nil, errors.New("NOTIFY_CHANNELS names a channel twice")
		case c == ChannelLog && !dev:
			return nil, errors.New("NOTIFY_CHANNELS: log is for development only")
		case c == ChannelLog:
		case c == ChannelEmail:
			return nil, errors.New("NOTIFY_CHANNELS: email is not built yet (notifications PR 4)")
		default:
			return nil, errors.New("NOTIFY_CHANNELS: unknown channel (log in development; email from PR 4)")
		}
		out = append(out, c)
	}
	return out, nil
}

// Sends reports whether any channel is on.
func (s Settings) Sends() bool { return len(s.Channels) > 0 }

// checkNATSURL: nats:// or tls:// (tls:// only outside development), a
// host, no credentials, for each comma-separated server. Errors never
// carry the URL.
func checkNATSURL(raw string, dev bool) error {
	for _, part := range strings.Split(raw, ",") {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls") {
			return errors.New("NOTIFY_NATS_URL is invalid")
		}
		if u.User != nil {
			return errors.New("NOTIFY_NATS_URL must not carry credentials (NOTIFY_NATS_NKEY_FILE or NOTIFY_NATS_PASSWORD)")
		}
		if u.Scheme != "tls" && !dev {
			return errors.New("NOTIFY_NATS_URL must be tls:// outside development")
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
