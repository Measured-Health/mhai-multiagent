// Command hl7-bridge is the on-site HL7 bridge a hospital runs next to its
// interface engine (design docs/designs/2026-11-27-hl7v2-file-connector-
// and-bridge.md §4; install guide deploy/hl7-bridge/README.md). It
// receives HL7 v2 over MLLP, acknowledges each message once it is on disk,
// and serves the hl7v2_file batch files over its own SFTP server.
//
//	hl7-bridge serve         run (the default)
//	hl7-bridge fingerprint   print the SSH host key's fingerprint
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7bridge"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	switch cmd {
	case "serve":
		c, err := hl7bridge.ConfigFromEnv(os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := hl7bridge.Run(ctx, c, log); err != nil {
			log.Error("stopped", "error", err.Error())
			os.Exit(1)
		}
	case "fingerprint":
		state := os.Getenv("HL7_BRIDGE_STATE")
		if state == "" {
			state = "/state"
		}
		k, err := hl7bridge.HostKey(state)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hl7-bridge: the host key can't be read or made")
			os.Exit(1)
		}
		fmt.Println(ssh.FingerprintSHA256(k.PublicKey()))
	default:
		fmt.Fprintln(os.Stderr, "usage: hl7-bridge [serve|fingerprint]")
		os.Exit(2)
	}
}
