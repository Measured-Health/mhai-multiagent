// Command documents is the documents service (ADR 0016): the only process
// that holds the object store credential and the hospitals' key
// encryption keys. It answers documents.v1.ctl.health (PR 1) and
// presign_upload (PR 3), and scans uploads with clamd and docsandbox (PR 4,
// the documents_scan durable on DOCS and the sweeper); the rest come
// later. It runs only under the compose profile documents (off by
// default).
//
//	documents serve
//	documents keyring init <path>                       an empty file keyring, mode 0400; never overwrites
//	documents keyring add-hospital <path> <hospital id>  one hospital's KEK, on the admin machine
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
)

const usage = "usage: documents serve | documents keyring init <absolute path> | " +
	"documents keyring add-hospital <absolute path> <hospital id>"

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
	case len(args) == 4 && args[0] == "keyring" && args[1] == "add-hospital" && filepath.IsAbs(args[2]):
		return keyringAddHospital(args[2], args[3], os.Stdout)
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

// keyringAddHospital adds one hospital's KEK to a provisioned keyring, on
// the admin machine (README: provision, then copy to every documents server
// and restart each). It refuses a hospital that already has a key (it is
// never replaced: that would make the hospital's documents unreadable) and
// a malformed id, and writes a new file and renames it, never in place. It
// prints the new fingerprint (the file's SHA-256), never key material.
func keyringAddHospital(path, id string, out io.Writer) error {
	hospital, err := kek.ParseHospital(id)
	if err != nil {
		return errors.New("keyring add-hospital: the hospital id must be a lower-case uuid, not nil")
	}
	fp, n, err := kek.AddHospital(path, hospital)
	switch {
	case errors.Is(err, kek.ErrHospitalExists):
		return errors.New("keyring add-hospital: the hospital already has a key; it is never replaced")
	case errors.Is(err, kek.ErrKeyringMissing):
		return errors.New("keyring add-hospital: no keyring there; make one with documents keyring init")
	case errors.Is(err, kek.ErrKeyringMode):
		return errors.New("keyring add-hospital: the keyring must be a regular file readable by its owner only")
	case err != nil:
		return fmt.Errorf("keyring add-hospital: the keyring is not usable (%s); nothing was written", keyringReason(err))
	}
	noun := "hospitals"
	if n == 1 {
		noun = "hospital"
	}
	fmt.Fprintf(out, "hospital added; the keyring holds %d %s; fingerprint %s\n", n, noun, fp)
	fmt.Fprintln(out, "copy it to every documents server (mode 0600 or 0400, owned by the service user) and restart each;")
	fmt.Fprintln(out, "each logs this fingerprint at start")
	return nil
}
