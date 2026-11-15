// Package config reads the documents service's settings from the
// environment (ADR 0016; documents design §17). It fails closed: a missing,
// malformed or relaxed setting stops the service. Errors name the
// variable, never its value.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
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

	// KEKProvider is DOCUMENTS_KEK_PROVIDER: "" (none: the keyring is not
	// ready) or file (design §6.2). kms is PR 9's.
	KEKProvider string
	// KeyringPath is DOCUMENTS_KEYRING_PATH, the file keyring, absolute.
	KeyringPath string
	// AllowFileKeyring is DOCUMENTS_ALLOW_FILE_KEYRING: "pilot" lets
	// production use the file keyring (the product owner's written
	// acceptance, D5); anything else is refused.
	AllowFileKeyring string
	// S3 is the object store, nil when none is configured (storage is then
	// not ready).
	S3 *S3Settings
}

// S3Settings are the object store's settings. The secret access key is
// read from SecretKeyFile at start, never from the environment.
type S3Settings struct {
	Endpoint       string // DOCUMENTS_S3_ENDPOINT
	UploadEndpoint string // DOCUMENTS_S3_UPLOAD_ENDPOINT, what browsers post to ("" for Endpoint)
	Bucket         string // DOCUMENTS_S3_BUCKET
	Region         string // DOCUMENTS_S3_REGION
	AccessKeyID    string // DOCUMENTS_S3_ACCESS_KEY_ID
	SecretKeyFile  string // DOCUMENTS_S3_SECRET_KEY_FILE, absolute
}

// FileKeyringAccepted reports whether the file keyring runs under the
// pilot's acceptance outside development and staging.
func (s Settings) FileKeyringAccepted() bool {
	return s.KEKProvider == "file" && s.AllowFileKeyring == "pilot"
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
	errs = append(errs, s.storage(get)...)
	if err := errors.Join(errs...); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// storage reads and checks the keyring and object store settings.
func (s *Settings) storage(get func(string) string) []error {
	var errs []error
	s.KEKProvider, s.KeyringPath, s.AllowFileKeyring = get("DOCUMENTS_KEK_PROVIDER"), get("DOCUMENTS_KEYRING_PATH"),
		get("DOCUMENTS_ALLOW_FILE_KEYRING")
	switch s.KEKProvider {
	case "":
		if s.KeyringPath != "" {
			errs = append(errs, errors.New("DOCUMENTS_KEYRING_PATH is only for DOCUMENTS_KEK_PROVIDER=file"))
		}
	case "file":
		if s.KeyringPath == "" || !filepath.IsAbs(s.KeyringPath) {
			errs = append(errs, errors.New("DOCUMENTS_KEYRING_PATH must be an absolute path with DOCUMENTS_KEK_PROVIDER=file"))
		}
	case "kms":
		errs = append(errs, errors.New("DOCUMENTS_KEK_PROVIDER=kms is not built yet (documents PR 9); use file"))
	default:
		errs = append(errs, errors.New("DOCUMENTS_KEK_PROVIDER must be file or unset"))
	}
	switch {
	case s.AllowFileKeyring != "" && s.AllowFileKeyring != "pilot":
		errs = append(errs, errors.New("DOCUMENTS_ALLOW_FILE_KEYRING must be pilot or unset"))
	case s.KEKProvider == "file" && s.Env == "production" && s.AllowFileKeyring != "pilot":
		errs = append(errs, errors.New("the file keyring is refused in production unless DOCUMENTS_ALLOW_FILE_KEYRING=pilot"))
	}

	st := S3Settings{Endpoint: get("DOCUMENTS_S3_ENDPOINT"), UploadEndpoint: get("DOCUMENTS_S3_UPLOAD_ENDPOINT"),
		Bucket: get("DOCUMENTS_S3_BUCKET"), Region: get("DOCUMENTS_S3_REGION"), AccessKeyID: get("DOCUMENTS_S3_ACCESS_KEY_ID"),
		SecretKeyFile: get("DOCUMENTS_S3_SECRET_KEY_FILE")}
	if st == (S3Settings{}) {
		return errs
	}
	for _, kv := range [][2]string{{"DOCUMENTS_S3_ENDPOINT", st.Endpoint}, {"DOCUMENTS_S3_BUCKET", st.Bucket},
		{"DOCUMENTS_S3_REGION", st.Region}, {"DOCUMENTS_S3_ACCESS_KEY_ID", st.AccessKeyID},
		{"DOCUMENTS_S3_SECRET_KEY_FILE", st.SecretKeyFile}} {
		if kv[1] == "" {
			errs = append(errs, fmt.Errorf("%s is required with the other DOCUMENTS_S3_ settings", kv[0]))
		}
	}
	for _, kv := range [][2]string{{"DOCUMENTS_S3_ENDPOINT", st.Endpoint}, {"DOCUMENTS_S3_UPLOAD_ENDPOINT", st.UploadEndpoint}} {
		if kv[1] == "" {
			continue
		}
		u, err := s3.ParseEndpoint(kv[1])
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s must be http(s)://host[:port], with no credentials, path or query", kv[0]))
		case u.Scheme != "https" && !s.Development():
			errs = append(errs, fmt.Errorf("%s must be https:// outside development", kv[0]))
		}
	}
	for _, e := range s3.CheckNames(st.Bucket, st.Region, st.AccessKeyID) {
		errs = append(errs, fmt.Errorf("DOCUMENTS_S3_%s is invalid", e))
	}
	if st.SecretKeyFile != "" && !filepath.IsAbs(st.SecretKeyFile) {
		errs = append(errs, errors.New("DOCUMENTS_S3_SECRET_KEY_FILE must be an absolute path"))
	}
	s.S3 = &st
	return errs
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

// MaxSecretFileBytes bounds a secret file.
const MaxSecretFileBytes = 4096

// ReadSecretFile reads a secret from a file (the object store's secret
// access key): a regular file of at most 4 KiB, its contents trimmed, at
// least 16 characters and not a placeholder. Errors name variable, never
// the path or the value.
func ReadSecretFile(variable, path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > MaxSecretFileBytes {
		return "", fmt.Errorf("%s: the secret file is missing or not a regular file", variable)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: the secret file can't be read", variable)
	}
	v := strings.TrimSpace(string(b))
	switch {
	case len(v) < 16:
		return "", fmt.Errorf("%s: the secret is shorter than 16 characters", variable)
	case placeholder(v):
		return "", fmt.Errorf("%s: the secret is a placeholder", variable)
	}
	return v, nil
}
