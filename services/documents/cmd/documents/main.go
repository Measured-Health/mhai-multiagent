// Command documents is the documents service (ADR 0016): the only process
// that will hold the object store credential and the hospitals' key
// encryption keys. In PR 1 it answers documents.v1.ctl.health; storage and
// keys (PR 3), scanning and the sandbox (PR 4) and the rest come later. It
// runs only under the compose profile documents (off by default).
package main

import (
	"errors"
	"fmt"
	"os"
)

const usage = "usage: documents serve"

var errUsage = errors.New("usage")

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		if err == errUsage {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "documents:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string) error {
	switch {
	case len(args) == 1 && args[0] == "serve":
		return serve(getenv)
	default:
		return errUsage
	}
}
