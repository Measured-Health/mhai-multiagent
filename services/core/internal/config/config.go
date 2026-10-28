// Package config loads the core service's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
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
	MFAKey           []byte
	BFFServiceKeys   []string
	BreachedListPath string
}

// Load reads the environment. Secrets are required, at least 32 bytes and
// must not be placeholders. Errors name the variable, never its value.
func Load() (Config, error) {
	c := Config{
		Env:              getenv("CORE_ENV", "development"),
		HTTPAddr:         getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		NATSURL:          os.Getenv("NATS_URL"),
		NATSNkeySeedPath: os.Getenv("NATS_NKEY_SEED_PATH"),
		AccessKeyPath:    os.Getenv("ACCESS_KEY_PATH"),
		BreachedListPath: os.Getenv("BREACHED_LIST_PATH"),
	}
	var errs []error

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

	if raw := os.Getenv("BFF_SERVICE_KEYS"); strings.TrimSpace(raw) == "" {
		errs = append(errs, errors.New("BFF_SERVICE_KEYS is required"))
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
// NATS and the access-token signing key.
func (c Config) ValidateServe() error {
	var errs []error
	for name, v := range map[string]string{"DATABASE_URL": c.DatabaseURL, "NATS_URL": c.NATSURL, "ACCESS_KEY_PATH": c.AccessKeyPath} {
		if strings.TrimSpace(v) == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	return errors.Join(errs...)
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

func getenv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
