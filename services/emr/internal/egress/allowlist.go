package egress

// The allow-list writer (EMR design §6.4, plan departure 15). Every
// Interval it lists the connections that may go out: enabled and not
// paused (read as mhai_emr_sched, across hospitals), plus any held for a
// test; reads each one's addresses under its own hospital (mhai_emr, RLS);
// and writes allow.json atomically for egress-emr. A connection is listed
// only when its saved addresses still give the origin its credentials were
// entered for (A28), on the proxy's ports (https 443, sftp 22), with
// declarable ranges; anything else is left out. When the database can't be
// read nothing is written, so the file ages out and the proxy refuses
// everything (fail closed).

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// Interval is how often the file is rewritten: inside the 30 s bound of
// design §6.4, and well inside the proxy's proxyconf.MaxAge.
const Interval = 15 * time.Second

// errRead is the writer's one database error: a fixed value.
var errRead = errors.New("egress: cannot read the connections")

// WriteAllowList writes dir/allow.json every Interval until ctx ends.
func WriteAllowList(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	return (&Writer{Pool: pool, Dir: dir}).Run(ctx)
}

type heldKey struct{ hospital, conn uuid.UUID }

// Writer writes the allow-list.
type Writer struct {
	Pool *pgxpool.Pool
	Dir  string
	// Development allows what emrurl allows there (a loopback literal,
	// which the proxy still refuses).
	Development bool
	Log         *slog.Logger
	Now         func() time.Time

	once    sync.Once
	kick    chan struct{}
	mu      sync.Mutex
	held    map[heldKey]int
	writeMu sync.Mutex
	last    atomic.Int64
}

func (w *Writer) init() {
	w.once.Do(func() {
		w.kick = make(chan struct{}, 1)
		w.held = map[heldKey]int{}
	})
}

func (w *Writer) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

// Run writes at once, then every Interval and after each release, until
// ctx ends.
func (w *Writer) Run(ctx context.Context) error {
	w.init()
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		if err := w.WriteOnce(ctx); err != nil && ctx.Err() == nil && w.Log != nil {
			code := "write_failed" // a refused list (overlapping ranges) or the directory
			if errors.Is(err, errRead) {
				code = "database_unavailable"
			}
			w.Log.Warn("allow_list_not_written", "code", code)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-w.kick:
		}
	}
}

// Age is how long ago the file was last written by this process.
func (w *Writer) Age(now time.Time) (time.Duration, bool) {
	n := w.last.Load()
	if n == 0 {
		return 0, false
	}
	return now.Sub(time.Unix(0, n)), true
}

// Hold lists connection id of hospital for a test (design §6.4: enabled
// or being tested): the file is written with it before Hold returns, and
// Hold then waits for the proxy's next reload. The connection is read
// under hospital, so an id of another hospital lists nothing. release ends
// the hold; the next write drops it.
func (w *Writer) Hold(ctx context.Context, hospital, id uuid.UUID) (release func(), err error) {
	w.init()
	k := heldKey{hospital, id}
	w.mu.Lock()
	w.held[k]++
	w.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			w.mu.Lock()
			if w.held[k]--; w.held[k] <= 0 {
				delete(w.held, k)
			}
			w.mu.Unlock()
			select {
			case w.kick <- struct{}{}:
			default:
			}
		})
	}
	if err := w.WriteOnce(ctx); err != nil {
		release()
		return nil, err
	}
	select {
	case <-time.After(proxyconf.ReloadEvery + 500*time.Millisecond):
	case <-ctx.Done():
	}
	return release, nil
}

// WriteOnce reads the connections and writes the file.
func (w *Writer) WriteOnce(ctx context.Context) error {
	w.init()
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	conns, err := w.read(ctx)
	if err != nil {
		return err
	}
	now := w.now()
	b, err := proxyconf.Marshal(proxyconf.File{GeneratedAt: now, Connections: conns})
	if err != nil {
		return err
	}
	// Ranges that overlap across hospitals (A37): the proxy leaves those
	// connections out and serves the rest. A person must act: alert.
	if l, err := proxyconf.Parse(b); err == nil && len(l.Overlapping) > 0 && w.Log != nil {
		ids := make([]string, len(l.Overlapping))
		for i, id := range l.Overlapping {
			ids[i] = id.String()
		}
		w.Log.Error("allow_list_ranges_overlap", "code", "ranges_overlap", "alert", true, "connection_ids", ids)
	}
	if err := writeAtomic(w.Dir, b); err != nil {
		return err
	}
	w.last.Store(now.UnixNano())
	return nil
}

// writeAtomic writes dir/allow.json through a temporary file in dir and a
// rename, so the proxy never reads half a file. 0644: the proxy runs as
// the same user; the mount it reads through is read-only.
func writeAtomic(dir string, b []byte) error {
	f, err := os.CreateTemp(dir, ".allow-*.tmp")
	if err != nil {
		return errors.New("egress: cannot write the allow-list")
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil || os.Chmod(tmp, 0o644) != nil ||
		os.Rename(tmp, filepath.Join(dir, proxyconf.FileName)) != nil {
		_ = os.Remove(tmp)
		return errors.New("egress: cannot write the allow-list")
	}
	return nil
}

// read lists the connections to allow.
func (w *Writer) read(ctx context.Context) ([]proxyconf.Conn, error) {
	if w.Pool == nil {
		return nil, errRead
	}
	byHospital := map[uuid.UUID][]uuid.UUID{}
	add := func(h, c uuid.UUID) {
		if !slices.Contains(byHospital[h], c) {
			byHospital[h] = append(byHospital[h], c)
		}
	}
	if err := w.enabled(ctx, add); err != nil {
		return nil, err
	}
	w.mu.Lock()
	for k := range w.held {
		add(k.hospital, k.conn)
	}
	w.mu.Unlock()

	out := []proxyconf.Conn{}
	for hid, ids := range byHospital {
		err := dbx.WithHospital(ctx, w.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id, type, base_url, settings, credentials_origin, private_cidrs
				FROM emr_connections WHERE id = ANY($1) AND hospital_id = $2`, ids, hid)
			if err != nil {
				return errRead
			}
			defer rows.Close()
			for rows.Next() {
				var (
					id        uuid.UUID
					typ, base string
					settings  map[string]any
					stored    []string
					cidrs     []netip.Prefix
				)
				if err := rows.Scan(&id, &typ, &base, &settings, &stored, &cidrs); err != nil {
					return errRead
				}
				if c, ok := w.entry(id, hid, typ, base, settings, stored, cidrs); ok {
					out = append(out, c)
				} else if w.Log != nil {
					w.Log.Info("allow_list_left_out", "connection_id", id.String())
				}
			}
			if rows.Err() != nil {
				return errRead
			}
			return nil
		})
		if err != nil {
			return nil, errRead
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// enabled lists the enabled, unpaused connections across hospitals, as the
// scheduler's role (the only one that sees them all, and only those).
func (w *Writer) enabled(ctx context.Context, add func(h, c uuid.UUID)) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return errRead
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE mhai_emr_sched"); err != nil {
		return errRead
	}
	rows, err := tx.Query(ctx, `SELECT id, hospital_id FROM emr_connections WHERE enabled AND NOT paused`)
	if err != nil {
		return errRead
	}
	defer rows.Close()
	for rows.Next() {
		var id, hid uuid.UUID
		if err := rows.Scan(&id, &hid); err != nil {
			return errRead
		}
		add(hid, id)
	}
	if rows.Err() != nil {
		return errRead
	}
	rows.Close()
	if tx.Commit(ctx) != nil {
		return errRead
	}
	return nil
}

// entry is one connection's rules, or false when it must be left out.
func (w *Writer) entry(id, hid uuid.UUID, typ, base string, raw map[string]any, stored []string, cidrs []netip.Prefix) (proxyconf.Conn, bool) {
	settings := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			settings[k] = s
		}
	}
	origins, err := emrurl.OriginsOf(typ, base, settings, w.Development)
	stored = slices.Clone(stored)
	slices.Sort(stored)
	if err != nil || !slices.Equal(emrurl.Strings(origins), stored) {
		return proxyconf.Conn{}, false
	}
	c := proxyconf.Conn{ID: id.String(), HospitalID: hid.String(), Hosts: []string{}, CIDRs: []string{}}
	for _, o := range origins {
		u, err := url.Parse(string(o))
		if err != nil {
			return proxyconf.Conn{}, false
		}
		want := map[string]string{"https": "443", "sftp": "22"}[u.Scheme]
		hp := strings.TrimPrefix(string(o), u.Scheme+"://")
		if _, port, err := net.SplitHostPort(hp); err != nil || port != want || !proxyconf.ValidHost(hp) {
			return proxyconf.Conn{}, false
		}
		if !slices.Contains(c.Hosts, hp) {
			c.Hosts = append(c.Hosts, hp)
		}
	}
	for _, p := range cidrs {
		if !proxyconf.Declarable(p) {
			return proxyconf.Conn{}, false
		}
		c.CIDRs = append(c.CIDRs, p.String())
	}
	return c, len(c.Hosts) > 0
}
