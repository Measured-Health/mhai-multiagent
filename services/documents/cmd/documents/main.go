// Command documents is the documents service (ADR 0016): the only process
// that holds the object store credential and the hospitals' key
// encryption keys. It answers documents.v1.ctl.health (PR 1) and
// presign_upload (PR 3); scanning and the sandbox (PR 4) and the rest come
// later. It runs only under the compose profile documents (off by
// default).
//
//	documents serve
//	documents keyring init <path>   an empty file keyring, mode 0400; never overwrites
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
)

const usage = "usage: documents serve | documents keyring init <absolute path>"

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
	case len(args) == 3 && args[0] == "keyring" && args[1] == "init" && filepath.IsAbs(args[2]):
		return keyringInit(args[2], os.Stdout)
	default:
		return errUsage
	}
}

// keyringInit makes an empty file keyring (design §6.2): each hospital's
// KEK is added when its first document is sealed. It never overwrites a
// keyring, which would destroy every KEK in it.
func keyringInit(path string, out io.Writer) error {
	if err := kek.InitFile(path); err != nil {
		if errors.Is(err, kek.ErrExists) {
			return errors.New("keyring init: a keyring is already there; it is never overwritten")
		}
		return errors.New("keyring init: cannot write the keyring")
	}
	fmt.Fprintln(out, "keyring ready: empty, mode 0400")
	return nil
}
