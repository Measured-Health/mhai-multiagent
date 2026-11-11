package main

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
)

// Loader holds the allow-list in use. Each Reload reads the file again: a
// missing, malformed or half-written file leaves no list at all (every
// CONNECT is refused), never the previous one. Freshness is checked at
// each CONNECT (proxyconf.List.Fresh), so a file the emr service stops
// rewriting ages out on its own.
type Loader struct {
	Path string
	Log  *slog.Logger
	cur  atomic.Pointer[proxyconf.List]
	// overlap is the last set of overlapping connections logged (Reload
	// runs on one goroutine).
	overlap string
}

// Reload reads the file and reports whether a list is now in use. When
// connections are left out for overlapping ranges, it logs an alert, once
// per change of that set.
func (l *Loader) Reload() bool {
	list, err := proxyconf.ReadFile(l.Path)
	if err != nil {
		l.cur.Store(nil)
		return false
	}
	ids := make([]string, len(list.Overlapping))
	for i, id := range list.Overlapping {
		ids[i] = id.String()
	}
	if key := strings.Join(ids, ","); key != l.overlap {
		l.overlap = key
		if len(ids) > 0 && l.Log != nil {
			l.Log.Error("allow_list_ranges_overlap", "code", "ranges_overlap", "alert", true, "connection_ids", ids)
		}
	}
	l.cur.Store(list)
	return true
}

// Current is the list in use, or nil.
func (l *Loader) Current() *proxyconf.List { return l.cur.Load() }

// Run reloads every interval until ctx ends, calling changed when the list
// appears or goes away.
func (l *Loader) Run(ctx context.Context, every time.Duration, changed func(ok bool)) {
	t := time.NewTicker(every)
	defer t.Stop()
	prev := l.Current() != nil
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ok := l.Reload(); ok != prev {
				prev = ok
				if changed != nil {
					changed(ok)
				}
			}
		}
	}
}
