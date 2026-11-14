package fileexport

import (
	"context"
	"errors"

	"golang.org/x/crypto/ssh"
)

// dirEntry is one folder entry.
type dirEntry struct {
	name         string
	regular, dir bool
	size         int64
}

var errListing = errors.New("fileexport: listing refused")

// listDir: not yet.
func listDir(context.Context, *ssh.Client, string) ([]dirEntry, error) { return nil, nil }
