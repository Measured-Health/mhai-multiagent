// Command emr is the EMR integration service (ADR 0014): the only process
// that holds hospital record-system credentials and the only route out to
// them. In this skeleton it answers emr.v1.ctl.health; connectors, the
// secret store, the normaliser and the rules come in later PRs. It runs
// only under the compose profile emr (off by default).
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = "usage: emr serve | emr keys seal-init --out <dir>"

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		if err == errUsage {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "emr:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "serve":
		return serve(getenv)
	default:
		return errUsage
	}
}
