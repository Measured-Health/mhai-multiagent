package password

import (
	"bufio"
	"compress/gzip"
	"embed"
	"errors"
	"strings"
	"sync"
)

// listFS holds the bundled breached-password list (see README.md next to it).
//
//go:embed breached.txt.gz
var listFS embed.FS

var (
	breachedOnce sync.Once
	breachedSet  map[string]struct{}
	breachedErr  error
)

func load() {
	breachedOnce.Do(func() {
		breachedSet, breachedErr = readList()
	})
}

// Preload loads the breached list now. Call it at startup so a broken list
// stops the process before it takes traffic, and so the first sign-in
// doesn't pay for decompressing it.
func Preload() error {
	load()
	return breachedErr
}

// readList reads the list, lowercased, so a case variant of a listed
// password is also refused.
func readList() (map[string]struct{}, error) {
	f, err := listFS.Open("breached.txt.gz")
	if err != nil {
		return nil, errors.New("password: breached list missing")
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, errors.New("password: breached list corrupt")
	}
	set := make(map[string]struct{}, 520000)
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		if w := strings.TrimRight(sc.Text(), "\r"); w != "" {
			set[strings.ToLower(w)] = struct{}{}
		}
	}
	if sc.Err() != nil {
		return nil, errors.New("password: breached list unreadable")
	}
	if len(set) < 100000 {
		return nil, errors.New("password: breached list too short")
	}
	return set, nil
}

// isBreached fails closed: if the list could not be loaded, every password
// counts as breached.
func isBreached(pw string) bool {
	load()
	if breachedErr != nil || breachedSet == nil {
		return true
	}
	_, ok := breachedSet[strings.ToLower(pw)]
	return ok
}
