// Command notifications is the notifications service (ADR 0018;
// notifications design §4.2): the only process that will hold the email
// provider's credential. Core decides who is told and about what; this
// service delivers. PR 2 answers notify.v1.ctl.health only. It runs only
// under the compose profile notifications (off by default), and with
// NOTIFY_CHANNELS empty (the default) it sends nothing.
//
//	notifications serve
package main

import (
	"errors"
	"fmt"
	"os"
)

const usage = "usage: notifications serve"

var errUsage = errors.New("usage")

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		if err == errUsage {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "notifications:", err)
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
