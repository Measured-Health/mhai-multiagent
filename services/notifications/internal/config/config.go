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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ChannelLog is the development channel: it logs that a message would be
// sent, by kind and ids, and sends nothing. It is refused outside
// development (as core's CORE_DELIVERY=log).
const ChannelLog = "log"

// ChannelEmail is the email channel: SMTP submission over TLS (PR 4). It
// needs the NOTIFY_SMTP_ settings and NOTIFY_APP_URL.
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
	// RatePerSecond is NOTIFY_RATE_PER_SECOND, the global pace of sends
	// (design §9.4; default 5, 1 to 50).
	RatePerSecond int
	// HospitalPerHour is NOTIFY_HOSPITAL_PER_HOUR, one hospital's outbound
	// messages in an hour, mandatory kinds aside (default 200, 1 to
	// 10000).
	HospitalPerHour int
	// RecipientPerHour is NOTIFY_RECIPIENT_PER_HOUR, one person's messages
	// in an hour (default 10, 1 to 100).
	RecipientPerHour int
	// AppURL is NOTIFY_APP_URL, the app's address every link in an email
	// must be on (core's CORE_APP_URL): https, or http in development.
	// Required with the email channel.
	AppURL string
	// SMTP is the email channel's settings (nil unless email is on).
	SMTP *SMTP
}

// SMTP is NOTIFY_SMTP_*: submission over TLS to the sending mailbox.
type SMTP struct {
	Host string // NOTIFY_SMTP_HOST, a host name
	// Port is NOTIFY_SMTP_PORT: 465 (implicit TLS) or 587 (STARTTLS); no
	// other, so there is no plaintext submission.
	Port     int
	Username string // NOTIFY_SMTP_USERNAME
	// PasswordFile is NOTIFY_SMTP_PASSWORD_FILE, an absolute path. The
	// password is never taken from the environment.
	PasswordFile string
	From         string // NOTIFY_SMTP_FROM, a bare address
	FromName     string // NOTIFY_SMTP_FROM_NAME (default MHAI)
	ReplyTo      string // NOTIFY_SMTP_REPLY_TO, the support address
	// CAFile is NOTIFY_SMTP_CA_FILE: extra trusted roots (a private CA),
	// an absolute path; "" is the system's. Verification is never off.
	CAFile string
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
	smtpErrs := s.readSMTP(get, getenv)
	var rateErrs []error
	for _, r := range []struct {
		name          string
		dst           *int
		def, min, max int
	}{
		{"NOTIFY_RATE_PER_SECOND", &s.RatePerSecond, 5, 1, 50},
		{"NOTIFY_HOSPITAL_PER_HOUR", &s.HospitalPerHour, 200, 1, 10000},
		{"NOTIFY_RECIPIENT_PER_HOUR", &s.RecipientPerHour, 10, 1, 100},
	} {
		v, err := intSetting(get(r.name), r.def, r.min, r.max)
		if err != nil {
			rateErrs = append(rateErrs, fmt.Errorf("%s must be a whole number from %d to %d", r.name, r.min, r.max))
		}
		*r.dst = v
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
	errs = append(errs, rateErrs...)
	errs = append(errs, smtpErrs...)
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
		default:
			return nil, errors.New("NOTIFY_CHANNELS: unknown channel (log in development, or email)")
		}
		out = append(out, c)
	}
	return out, nil
}

// readSMTP reads NOTIFY_APP_URL and NOTIFY_SMTP_* when email is on: each
// is required but the From name and the CA file; the port is 465 or 587;
// the paths absolute. A password in the environment (NOTIFY_SMTP_PASSWORD)
// is refused whether email is on or not.
func (s *Settings) readSMTP(get, raw func(string) string) []error {
	var errs []error
	if raw("NOTIFY_SMTP_PASSWORD") != "" {
		errs = append(errs, errors.New("NOTIFY_SMTP_PASSWORD is refused: give the password as a file (NOTIFY_SMTP_PASSWORD_FILE)"))
	}
	if !slices.Contains(s.Channels, ChannelEmail) {
		return errs
	}
	s.AppURL = get("NOTIFY_APP_URL")
	if u, err := url.Parse(s.AppURL); s.AppURL == "" || err != nil || u.Host == "" || u.User != nil ||
		!(u.Scheme == "https" || (u.Scheme == "http" && s.Env == "development")) {
		errs = append(errs, errors.New("NOTIFY_APP_URL must be the app's https address (http in development) with email on"))
	}
	m := &SMTP{Host: get("NOTIFY_SMTP_HOST"), Username: get("NOTIFY_SMTP_USERNAME"),
		PasswordFile: get("NOTIFY_SMTP_PASSWORD_FILE"), From: get("NOTIFY_SMTP_FROM"), FromName: get("NOTIFY_SMTP_FROM_NAME"),
		ReplyTo: get("NOTIFY_SMTP_REPLY_TO"), CAFile: get("NOTIFY_SMTP_CA_FILE")}
	if m.FromName == "" {
		m.FromName = "MHAI"
	}
	for _, kv := range [][2]string{{"NOTIFY_SMTP_HOST", m.Host}, {"NOTIFY_SMTP_USERNAME", m.Username},
		{"NOTIFY_SMTP_PASSWORD_FILE", m.PasswordFile}, {"NOTIFY_SMTP_FROM", m.From}, {"NOTIFY_SMTP_REPLY_TO", m.ReplyTo}} {
		if kv[1] == "" {
			errs = append(errs, fmt.Errorf("%s is required with email on", kv[0]))
		}
	}
	switch get("NOTIFY_SMTP_PORT") {
	case "465":
		m.Port = 465
	case "587":
		m.Port = 587
	default:
		errs = append(errs, errors.New("NOTIFY_SMTP_PORT must be 465 (implicit TLS) or 587 (STARTTLS)"))
	}
	if strings.ContainsAny(m.Host, " /:@") {
		errs = append(errs, errors.New("NOTIFY_SMTP_HOST must be a host name only"))
	}
	for _, kv := range [][2]string{{"NOTIFY_SMTP_PASSWORD_FILE", m.PasswordFile}, {"NOTIFY_SMTP_CA_FILE", m.CAFile}} {
		if kv[1] != "" && !filepath.IsAbs(kv[1]) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path", kv[0]))
		}
	}
	s.SMTP = m
	return errs
}

// ReadSMTPPassword reads the password file: a regular file of at most
// 4 KB, one line of at least 8 characters, not a placeholder. Errors name
// the variable, never the path or the value.
func ReadSMTPPassword(path string) (string, error) {
	const variable = "NOTIFY_SMTP_PASSWORD_FILE"
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return "", errors.New(variable + ": the secret file is missing or not a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New(variable + ": the secret file can't be read")
	}
	v := strings.TrimRight(string(b), "\r\n")
	switch {
	case strings.ContainsAny(v, "\r\n"):
		return "", errors.New(variable + ": the file holds more than one line")
	case len(v) < 8:
		return "", errors.New(variable + ": the password is shorter than 8 characters")
	case placeholder(v):
		return "", errors.New(variable + ": the password is a placeholder")
	}
	return v, nil
}

// intSetting is raw as a whole number in [lo, hi], or def when raw is "".
func intSetting(raw string, def, lo, hi int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return def, errors.New("out of range")
	}
	return n, nil
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
