package password

import (
	"bufio"
	"compress/gzip"
	"embed"
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
)

// loadBreached reads the list once, lowercased, so a case variant of a
// listed password is also refused. The embedded file is fixed at build time,
// so a read failure is a build defect and panics at first use.
func loadBreached() map[string]struct{} {
	breachedOnce.Do(func() {
		f, err := listFS.Open("breached.txt.gz")
		if err != nil {
			panic("password: breached list missing: " + err.Error())
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			panic("password: breached list corrupt: " + err.Error())
		}
		set := make(map[string]struct{}, 100000)
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			if w := strings.TrimRight(sc.Text(), "\r"); w != "" {
				set[strings.ToLower(w)] = struct{}{}
			}
		}
		if err := sc.Err(); err != nil {
			panic("password: breached list unreadable: " + err.Error())
		}
		breachedSet = set
	})
	return breachedSet
}

func isBreached(pw string) bool {
	_, ok := loadBreached()[strings.ToLower(pw)]
	return ok
}
