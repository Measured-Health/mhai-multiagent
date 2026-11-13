package analysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/cells"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The engine's errors. Each maps to one problem code (internal/analysis/routes).
var (
	// ErrNotReady: the body isn't stored and couldn't be computed within
	// the cold budget (503 not_ready); the work goes on, so a retry finds it.
	ErrNotReady = errors.New("analysis: not ready")
	// ErrCap: the period holds its most bodies (503 unavailable, alert).
	ErrCap = errors.New("analysis: body cap reached")
	// ErrNoPeriod: a request names no answerable period (422 invalid_range).
	ErrNoPeriod = errors.New("analysis: not an answerable period")
	// ErrPending: the period has ended but isn't frozen (503 not_ready).
	ErrPending = errors.New("analysis: period not frozen yet")
	// ErrFailed: the database refused or timed out (503 unavailable).
	ErrFailed = errors.New("analysis: body failed")
	// ErrOverBudget: the contexts a request would newly store would take
	// the person past their cold contexts for a period (429).
	ErrOverBudget = errors.New("analysis: over the cold budget")
	// ErrTooMany: a request would newly store more contexts than its
	// ceiling (429).
	ErrTooMany = errors.New("analysis: too many new contexts")
)

// AlertInconsistent is raised when the engine sees inconsistent raw values
// (design §12): the context is stored hidden whole.
const AlertInconsistent = "analysis_inconsistent"

// AlertCap is raised when a period reaches its body cap (design §12).
const AlertCap = "analysis_body_cap"

// Background is how long a computation started by a request may go on
// after the request has answered not_ready.
const Background = 60 * time.Second

// MaxFlights is the most cold computations one core copy runs at once; a
// cold request past it answers not_ready at once (fail closed: nothing is
// computed or served unstored).
const MaxFlights = 8

// Period is a frozen period of the request's hospital.
type Period struct {
	ID     uuid.UUID
	Period calendar.Period
}

// Engine computes, stores and serves guarded bodies (design §11; plan Task
// 9). Every statement runs in the request's transaction (authz.Tx: the
// principal's hospital, as mhai_gateway), through migration 00025's
// functions. A context is computed parents first: every child period and
// every filter parent is stored before it, each in its own transaction, so
// a request cut short by the budget keeps what it finished. Each context's
// guard holds the period's lock (one guard per hospital and period at a
// time) and runs against the period's Ledger, kept between requests.
type Engine struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Budget is the cold budget (0: disclose.ColdBody); Now pins the
	// clock for the period lookups (nil: the database's).
	Budget time.Duration
	Now    func() time.Time
	// OnCompute, when set, is called once per context computed (tests count
	// T21's computations).
	OnCompute func()

	// Test hooks: called after each guard, with the bodies the ledger held
	// and when the guard started (inside the period's lock); a delay held
	// inside the lock.
	onGuard    func(known int, start time.Time)
	guardDelay time.Duration

	// Inconsistent and CapReached count the alerts (metrics).
	Inconsistent, CapReached atomic.Int64

	mu      sync.Mutex
	ledgers map[string]*ledger
	order   []string
	flights map[string]*flight
}

// NewEngine is the production engine on pool.
func NewEngine(pool *pgxpool.Pool, log *slog.Logger) *Engine {
	return &Engine{Pool: pool, Log: log}
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

// ColdBudget is the cold budget a request may spend (design §11).
func (e *Engine) ColdBudget() time.Duration { return e.budget() }

func (e *Engine) budget() time.Duration {
	if e.Budget > 0 {
		return e.Budget
	}
	return disclose.ColdBody
}

func (e *Engine) now() *time.Time {
	if e.Now == nil {
		return nil
	}
	t := e.Now()
	return &t
}

// ClockNow is the pinned clock for the period functions (nil: the
// database's).
func (e *Engine) ClockNow() *time.Time { return e.now() }

// Hash is a context's key: SHA-256 over the canonical filters.
func Hash(f disclose.Filters) []byte {
	var b strings.Builder
	b.WriteString("analysis/context/v1")
	for d, v := range f {
		b.WriteString("\x00" + disclose.DimNames[d] + "=" + v)
	}
	h := sha256.Sum256([]byte(b.String()))
	return h[:]
}

// FiltersHash is a filter set's key, whatever the period (D7's distinct
// filter sets).
func FiltersHash(f disclose.Filters) []byte { return Hash(f) }

// filtersBodyHash is the key of a period's stored filter lists.
var filtersBodyHash = func() []byte { h := sha256.Sum256([]byte("analysis/filters/v1")); return h[:] }()

// Lookup is the period of level starting on start: ErrNoPeriod when it
// isn't one, hasn't ended or is older than 24 months; ErrPending when it
// has ended but isn't frozen.
func (e *Engine) Lookup(ctx context.Context, p calendar.Period) (Period, error) {
	var out Period
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = e.lookup(ctx, tx, p)
		return err
	})
	return out, err
}

func (e *Engine) lookup(ctx context.Context, tx pgx.Tx, p calendar.Period) (Period, error) {
	var id *uuid.UUID
	var state string
	if err := tx.QueryRow(ctx, `SELECT period_id, state FROM mhai_analysis_period($1, $2::date, $3)`,
		string(p.Kind), p.From.Format(time.DateOnly), e.now()).Scan(&id, &state); err != nil {
		return Period{}, failed(err)
	}
	switch state {
	case "frozen":
		return Period{ID: *id, Period: p}, nil
	case "pending":
		return Period{}, ErrPending
	}
	return Period{}, ErrNoPeriod
}

// Periods are the answerable frozen periods, oldest first per level.
func (e *Engine) Periods(ctx context.Context) ([]Period, error) {
	var out []Period
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT period_id, level, start::text FROM mhai_analysis_periods($1)`, e.now())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var level, start string
			if err := rows.Scan(&id, &level, &start); err != nil {
				return err
			}
			p, err := disclose.PeriodOf(level, start)
			if err != nil {
				return err
			}
			out = append(out, Period{ID: id, Period: p})
		}
		return rows.Err()
	})
	return out, failed(err)
}

// Stored is a served body: the five views as stored, and when.
type Stored struct {
	Views    map[string]json.RawMessage
	Record   disclose.Body
	StoredAt time.Time
}

// stored is the JSON kept in analysis_snapshots.body.
type stored struct {
	Record json.RawMessage            `json:"record"`
	Views  map[string]json.RawMessage `json:"views"`
}

// Body is the stored body of (p, f), computed parents first if it isn't
// stored yet, within the cold budget: past it, ErrNotReady, and the
// computation goes on in the background so that a retry finds it.
func (e *Engine) Body(ctx context.Context, p Period, f disclose.Filters) (Stored, error) {
	if err := disclose.CheckFilters(f); err != nil {
		return Stored{}, err
	}
	if s, ok, err := e.get(ctx, p, f); err != nil || ok {
		return s, err
	}
	key := flightKey(ctx, p, f)
	fl, leader, full := e.join(key)
	if full {
		return Stored{}, ErrNotReady
	}
	if leader {
		// The computation outlives the request (bounded by Background): its
		// principal stays, its cancellation doesn't.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), Background)
		go func() {
			defer cancel()
			fl.err = e.ensure(bg, p, f, 0)
			e.leave(key)
			close(fl.done)
		}()
	}
	timer := time.NewTimer(e.budget())
	defer timer.Stop()
	select {
	case <-fl.done:
		if fl.err != nil {
			return Stored{}, fl.err
		}
	case <-timer.C:
		return Stored{}, ErrNotReady
	case <-ctx.Done():
		return Stored{}, ErrNotReady
	}
	s, ok, err := e.get(ctx, p, f)
	if err == nil && !ok {
		err = ErrFailed
	}
	return s, err
}

// Filters is the period's stored filter lists, stored on first use.
func (e *Engine) Filters(ctx context.Context, p Period) (json.RawMessage, time.Time, error) {
	var body json.RawMessage
	var at time.Time
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		if err := setTimeout(ctx, tx); err != nil {
			return err
		}
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT body::text, stored_at FROM mhai_analysis_snapshot_get($1, $2)`, p.ID, filtersBodyHash).Scan(&raw, &at)
		if err == nil {
			body = raw
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_lock($1)`, p.ID); err != nil {
			return err
		}
		var lists []byte
		if err := tx.QueryRow(ctx, `SELECT mhai_analysis_filters($1)::text`, p.ID).Scan(&lists); err != nil {
			return err
		}
		doc, _ := json.Marshal(map[string]json.RawMessage{"filters": lists})
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_snapshot_put($1, $2, $3::jsonb)`, p.ID, filtersBodyHash, string(doc)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT body::text, stored_at FROM mhai_analysis_snapshot_get($1, $2)`, p.ID, filtersBodyHash).Scan(&body, &at)
	})
	if err != nil {
		return nil, time.Time{}, failed(err)
	}
	var doc struct {
		Filters json.RawMessage `json:"filters"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Filters == nil {
		return nil, time.Time{}, ErrFailed
	}
	return doc.Filters, at, nil
}

// get reads the stored body of (p, f).
func (e *Engine) get(ctx context.Context, p Period, f disclose.Filters) (Stored, bool, error) {
	var s Stored
	var ok bool
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		var err error
		s, ok, err = getTx(ctx, tx, p, f)
		return err
	})
	return s, ok, failed(err)
}

func getTx(ctx context.Context, tx pgx.Tx, p Period, f disclose.Filters) (Stored, bool, error) {
	var s Stored
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT body::text, stored_at FROM mhai_analysis_snapshot_get($1, $2)`, p.ID, Hash(f)).Scan(&raw, &s.StoredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	var doc stored
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Record == nil || len(doc.Views) != len(disclose.Views) {
		return s, false, ErrFailed
	}
	rec, err := DecodeRecord(doc.Record)
	if err != nil {
		return s, false, ErrFailed
	}
	s.Views, s.Record = doc.Views, rec
	return s, true, nil
}

// Reserve charges the person every context Body(p, f) would newly store
// (the context, its child periods' and its filter parents', recursively:
// what ensure computes) against their cold budget, before anything is
// computed (security review): all or none, in one transaction, each
// period's counter row locked then checked (mhai_analysis_reserve_cold,
// in period order so two requests can't deadlock). A context already
// stored costs nothing. More than ceiling new contexts is ErrTooMany; past
// cap in any period, ErrOverBudget. It returns how many it reserved.
func (e *Engine) Reserve(ctx context.Context, p Period, f disclose.Filters, cap, ceiling int) (int, error) {
	if err := disclose.CheckFilters(f); err != nil {
		return 0, err
	}
	refs, err := e.missing(ctx, p, f, ceiling)
	if err != nil || len(refs) == 0 {
		return 0, failed(err)
	}
	sort.Slice(refs, func(i, j int) bool {
		if a, b := refs[i].p.ID.String(), refs[j].p.ID.String(); a != b {
			return a < b
		}
		return string(Hash(refs[i].f)) < string(Hash(refs[j].f))
	})
	pr, _ := authz.FromContext(ctx)
	err = authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		for _, r := range refs {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT mhai_analysis_reserve_cold($1, $2, $3, $4)`, pr.UserID, r.p.ID, Hash(r.f), cap).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrOverBudget // the transaction rolls back: nothing reserved
			}
		}
		return nil
	})
	if err != nil {
		return 0, failed(err)
	}
	return len(refs), nil
}

type ctxRef struct {
	p Period
	f disclose.Filters
}

// missing lists the contexts ensure(p, f) would newly store: a stored
// context's own family is stored (parents first), so the walk stops there.
// Past ceiling it is ErrTooMany.
func (e *Engine) missing(ctx context.Context, p Period, f disclose.Filters, ceiling int) ([]ctxRef, error) {
	seen := map[string]bool{}
	var out []ctxRef
	var walk func(p Period, f disclose.Filters) error
	walk = func(p Period, f disclose.Filters) error {
		key := p.ID.String() + string(Hash(f))
		if seen[key] {
			return nil
		}
		seen[key] = true
		ok, err := e.stored(ctx, p, f)
		if err != nil || ok {
			return err
		}
		out = append(out, ctxRef{p, f})
		if len(out) > ceiling {
			return ErrTooMany
		}
		kids, err := e.children(ctx, p)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := walk(k, f); err != nil {
				return err
			}
		}
		for d, v := range f {
			if v != "" {
				if err := walk(p, f.Without(d)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(p, f); err != nil {
		return nil, err
	}
	return out, nil
}

// stored reports whether (p, f) has a stored body, without reading it.
func (e *Engine) stored(ctx context.Context, p Period, f disclose.Filters) (bool, error) {
	var ok bool
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mhai_analysis_snapshot_get($1, $2))`, p.ID, Hash(f)).Scan(&ok)
	})
	return ok, failed(err)
}

// ensure stores (p, f), its child periods' bodies and its filter parents
// first. depth guards against a runaway recursion (a period has at most
// two levels below it, a filter set two filters).
func (e *Engine) ensure(ctx context.Context, p Period, f disclose.Filters, depth int) error {
	if depth > 8 {
		return ErrFailed
	}
	if _, ok, err := e.get(ctx, p, f); err != nil || ok {
		return err
	}
	kids, err := e.children(ctx, p)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := e.ensure(ctx, k, f, depth+1); err != nil {
			return err
		}
	}
	for d, v := range f {
		if v != "" {
			if err := e.ensure(ctx, p, f.Without(d), depth+1); err != nil {
				return err
			}
		}
	}
	return e.compute(ctx, p, f, kids)
}

// children are p's child periods, frozen with it (D2).
func (e *Engine) children(ctx context.Context, p Period) ([]Period, error) {
	var out []Period
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		for _, k := range calendar.Children(p.Period) {
			var id *uuid.UUID
			var state string
			// A child of a frozen period is frozen, and within its window.
			if err := tx.QueryRow(ctx, `SELECT period_id, state FROM mhai_analysis_period($1, $2::date, $3)`,
				string(k.Kind), k.From.Format(time.DateOnly), e.now()).Scan(&id, &state); err != nil {
				return err
			}
			if state != "frozen" || id == nil {
				return ErrFailed // a parent frozen without its children: never
			}
			out = append(out, Period{ID: *id, Period: k})
		}
		return nil
	})
	return out, failed(err)
}

// compute guards (p, f) under the period's lock, against the period's
// ledger, and stores it; the ledger records it once committed.
func (e *Engine) compute(ctx context.Context, p Period, f disclose.Filters, kids []Period) error {
	hospital := hospitalOf(ctx)
	l := e.ledger(hospital, p.ID)
	var body disclose.Body
	var seq int64
	err := authz.Tx(ctx, e.Pool, func(tx pgx.Tx) error {
		if err := setTimeout(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_lock($1)`, p.ID); err != nil {
			return err
		}
		if _, ok, err := getTx(ctx, tx, p, f); err != nil || ok {
			return err
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if err := l.sync(ctx, tx, p.ID); err != nil {
			return err
		}
		raw, err := cells.Read(ctx, tx, p.ID, p.Period, f)
		if err != nil {
			return err
		}
		var children []disclose.ChildBodies
		for _, k := range kids {
			kr, err := cells.Read(ctx, tx, k.ID, k.Period, f)
			if err != nil {
				return err
			}
			ks, ok, err := getTx(ctx, tx, k, f)
			if err != nil {
				return err
			}
			if !ok {
				return ErrFailed // stored first, by ensure
			}
			children = append(children, disclose.ChildBodies{Raw: kr, Body: ks.Record})
		}
		if e.OnCompute != nil {
			e.OnCompute()
		}
		start, known := time.Now(), l.l.Len()
		if e.guardDelay > 0 {
			time.Sleep(e.guardDelay)
		}
		body, err = l.l.Guard(raw, children)
		if errors.Is(err, disclose.ErrInconsistent) {
			e.Inconsistent.Add(1)
			e.log().Error("analysis: inconsistent raw values", "hospital_id", hospital.String(),
				"period_id", p.ID.String(), "reason", "inconsistent", "alert", AlertInconsistent)
		} else if err != nil {
			return err
		}
		doc, err := e.document(ctx, tx, body)
		if err != nil {
			return err
		}
		var got *int64
		if err := tx.QueryRow(ctx, `SELECT mhai_analysis_snapshot_put($1, $2, $3::jsonb)`, p.ID, Hash(f), string(doc)).Scan(&got); err != nil {
			return err
		}
		if got == nil {
			return nil // stored by another copy since: never under this lock, but harmless
		}
		seq = *got
		if e.onGuard != nil {
			e.onGuard(known, start)
		}
		return nil
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "54000" {
			e.CapReached.Add(1)
			e.log().Error("analysis: body cap reached", "hospital_id", hospital.String(),
				"period_id", p.ID.String(), "reason", "cap", "alert", AlertCap)
			return ErrCap
		}
		return failed(err)
	}
	if seq != 0 {
		l.mu.Lock()
		l.add(seq, body)
		l.mu.Unlock()
	}
	return nil
}

// document is the stored JSON: the engine's record and the five views as
// sent (rendered with the hospital's names now, so a stored body never
// changes).
func (e *Engine) document(ctx context.Context, tx pgx.Tx, b disclose.Body) ([]byte, error) {
	names, err := loadNames(ctx, tx)
	if err != nil {
		return nil, err
	}
	rec, err := EncodeRecord(b)
	if err != nil {
		return nil, err
	}
	doc := stored{Record: rec, Views: map[string]json.RawMessage{}}
	for _, v := range disclose.Views {
		j, err := renderView(b, v, names)
		if err != nil {
			return nil, err
		}
		doc.Views[string(v)] = j
	}
	return json.Marshal(doc)
}

func setTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '`+disclose.StatementTimeout.String()+`'`)
	return err
}

// failed maps a database or decoding failure to ErrFailed, keeping the
// engine's own errors (never the driver's text, which may hold a value).
func failed(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotReady), errors.Is(err, ErrCap), errors.Is(err, ErrNoPeriod), errors.Is(err, ErrPending),
		errors.Is(err, ErrFailed), errors.Is(err, disclose.ErrTooManyFilters), errors.Is(err, disclose.ErrInvalid),
		errors.Is(err, ErrOverBudget), errors.Is(err, ErrTooMany):
		return err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return ErrNotReady
	}
	return ErrFailed
}

func hospitalOf(ctx context.Context) uuid.UUID {
	p, _ := authz.FromContext(ctx)
	return p.HospitalID
}

// ---- one computation per context in this copy (T21) -----------------------

type flight struct {
	done chan struct{}
	err  error
}

func flightKey(ctx context.Context, p Period, f disclose.Filters) string {
	return hospitalOf(ctx).String() + "/" + p.ID.String() + "/" + string(Hash(f))
}

func (e *Engine) join(key string) (fl *flight, leader, full bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flights == nil {
		e.flights = map[string]*flight{}
	}
	if fl, ok := e.flights[key]; ok {
		return fl, false, false
	}
	if len(e.flights) >= MaxFlights {
		return nil, false, true
	}
	fl = &flight{done: make(chan struct{})}
	e.flights[key] = fl
	return fl, true, false
}

func (e *Engine) leave(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.flights, key)
}

// ---- the period's record, kept between requests (PR 3 review, condition 3)

// LedgersKept is how many periods' records one copy keeps; the oldest is
// dropped (and rebuilt from the stored bodies when next needed).
const LedgersKept = 64

type ledger struct {
	mu   sync.Mutex
	l    *disclose.Ledger
	have map[int64]bool
}

func (e *Engine) ledger(hospital, period uuid.UUID) *ledger {
	key := hospital.String() + "/" + period.String()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ledgers == nil {
		e.ledgers = map[string]*ledger{}
	}
	if l, ok := e.ledgers[key]; ok {
		return l
	}
	l := &ledger{l: disclose.NewLedger(), have: map[int64]bool{}}
	e.ledgers[key] = l
	e.order = append(e.order, key)
	if len(e.order) > LedgersKept {
		delete(e.ledgers, e.order[0])
		e.order = e.order[1:]
	}
	return l
}

// sync adds every body stored for the period (and inside it) since the
// ledger last looked, by any core copy. Run under the period's lock.
func (l *ledger) sync(ctx context.Context, tx pgx.Tx, period uuid.UUID) error {
	have := make([]int64, 0, len(l.have))
	for s := range l.have {
		have = append(have, s)
	}
	rows, err := tx.Query(ctx, `SELECT seq, record::text FROM mhai_analysis_records($1, $2)`, period, have)
	if err != nil {
		return err
	}
	defer rows.Close()
	type rec struct {
		seq int64
		b   disclose.Body
	}
	var got []rec
	for rows.Next() {
		var seq int64
		var raw []byte
		if err := rows.Scan(&seq, &raw); err != nil {
			return err
		}
		b, err := DecodeRecord(raw)
		if err != nil {
			return ErrFailed
		}
		got = append(got, rec{seq, b})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range got {
		l.add(r.seq, r.b)
	}
	return nil
}

func (l *ledger) add(seq int64, b disclose.Body) {
	if l.have[seq] {
		return
	}
	l.have[seq] = true
	l.l.Add(b)
}

// ---- the stored record ------------------------------------------------------

// EncodeRecord is a body as the engine keeps it in storage: guarded values
// only (a Body holds no raw value; a test pins it).
func EncodeRecord(b disclose.Body) ([]byte, error) { return json.Marshal(b) }

// DecodeRecord reads a stored record strictly: an unknown field, or a
// shown count of 1 to 4 (smallgroup.Guarded's own check), is refused.
func DecodeRecord(raw []byte) (disclose.Body, error) {
	var b disclose.Body
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return disclose.Body{}, err
	}
	return b, nil
}
