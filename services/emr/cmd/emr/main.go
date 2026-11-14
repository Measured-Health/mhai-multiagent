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

const usage = "usage: emr serve | emr keys seal-init --out <dir> | emr keys proxy-init --out <dir> | emr unmapped --hospital <id> --connection <id> |\n" +
	"       emr rules check <rule set file> |\n" +
	"       emr synth-fhir --name <dns name> --out <dir> [--addr :8443] [--seed 1] [--patients 20] [--late-critical 3m]   (development only)"

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
	case len(args) == 4 && args[0] == "keys" && args[1] == "seal-init" && args[2] == "--out" && args[3] != "":
		return sealInit(args[3], out)
	case len(args) == 4 && args[0] == "keys" && args[1] == "proxy-init" && args[2] == "--out" && args[3] != "":
		return proxyInit(args[3], out)
	case len(args) == 5 && args[0] == "unmapped" && args[1] == "--hospital" && args[3] == "--connection":
		return unmappedCmd(getenv, args[2], args[4], out)
	case len(args) == 3 && args[0] == "rules" && args[1] == "check" && args[2] != "":
		return rulesCheck(args[2], getenv, out)
	case len(args) >= 1 && args[0] == "synth-fhir":
		return synthFHIR(args[1:], getenv, out)
	default:
		return errUsage
	}
}
