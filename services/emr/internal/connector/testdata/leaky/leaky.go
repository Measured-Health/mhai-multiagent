// Package leaky is a fixture for TestNoConnectorLogsCreds: each call below
// hands a credential to a logger or a print function, and the test must
// find every one. It is never built (testdata).
package leaky

import (
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Leak logs credentials six ways.
func Leak(c connector.Creds, s seal.Secret, l *slog.Logger) error {
	log.Println("creds", c)                                // 1
	slog.Info("x", "c", &c)                                // 2
	fmt.Printf("%v\n", s)                                  // 3
	l.Info("x", "k", c.Get("api_key").Bytes())             // 4
	fmt.Fprintln(os.Stderr, string(s.Bytes()))             // 5
	return fmt.Errorf("refused with %s", c.Get("api_key")) // 6
}

// Fine names no credential.
func Fine(c connector.Creds, l *slog.Logger) {
	l.Info("x", "set", c.Has("api_key"))
}
