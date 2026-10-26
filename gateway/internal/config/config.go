// Package config loads the gateway's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const minSecretLen = 32

// Config holds the gateway's settings.
type Config struct {
	Env              string
	HTTPAddr         string
	DatabaseURL      string
	NATSURL          string
	NATSCreds        string
	AccessKeyPath    string
	MFAKey           []byte
	BFFServiceKeys   []string
	BreachedListPath string
}

// Load reads the environment. Secrets are required, at least 32 bytes and
// must not be placeholders. Errors name the variable, never its value.
func Load() (Config, error) {
	c := Config{
		Env:              getenv("GATEWAY_ENV", "development"),
		HTTPAddr:         getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		NATSURL:          os.Getenv("NATS_URL"),
		NATSCreds:        os.Getenv("NATS_CREDS"),
		AccessKeyPath:    os.Getenv("ACCESS_KEY_PATH"),
		BreachedListPath: os.Getenv("BREACHED_LIST_PATH"),
	}
	var errs []error

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

func checkSecret(name, v string) error {
	lower := strings.ToLower(v)
	for _, p := range []string{"change-me", "changeme", "placeholder"} {
		if strings.Contains(lower, p) {
			return fmt.Errorf("%s is a placeholder", name)
		}
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
