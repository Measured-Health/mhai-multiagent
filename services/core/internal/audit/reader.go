package audit

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The audit log as the platform app reads it (Plan 4 spec §5.2, §5.3;
// contract §14.9). Every function here takes the caller's transaction,
// which runs as mhai_platform (authz.Tx for a platform principal). Every
// entry leaves through Map or Unmapped only, never as a raw payload, and
// every entry, person and hospital is dropped to its bare form, or left
// out, when a value doesn't fit the shape mhai-web reads or holds anything
// of the pseudonym's shape.

const (
	// TotalCap is the most a list's total counts; past it total_capped.
	TotalCap = 10000
	// MaxLimit and DefaultLimit bound a page.
	MaxLimit     = 100
	DefaultLimit = 50
	// peopleMax is the most people a search returns.
	peopleMax = 20
	// nameMax is mhai-web's longest name; emailMax its longest email.
	nameMax  = 120
	emailMax = 254
	// readDedupe is how long one page load's audit.viewed or
	// audit.searched covers the same read (spec §5.2).
	readDedupe = 5 * time.Minute
	// readScope is the idempotency_keys scope of the dedupe keys; no
	// Idempotency-Key scope takes this form.
	readScope = "audit.read"
)

// ErrCursor is a cursor that doesn't decode, or was made under other
// filters (C33).
var ErrCursor = errors.New("audit: bad cursor")

// ErrNotFound is an event id with no row.
var ErrNotFound = errors.New("audit: no such event")

// Filters are a list's filters, as parsed and checked by the handler.
// Hospital is "" (any), "platform" (no hospital) or a canonical uuid;
// Person a canonical uuid; Types from AuditTypes; Outcome from Outcomes.
// Q, when set, is an exact event or request id, and the other filters are
// ignored.
type Filters struct {
	Hospital string
	Person   string
	Types    []string
	Outcome  string
	Flagged  *bool
	From, To *time.Time
	Q        string
}

// canonical is f as hashed and recorded: types deduplicated in AuditTypes
// order, times in UTC, and only q when q is set.
type canonical struct {
	Hospital string   `json:"hospital,omitempty"`
	Person   string   `json:"person,omitempty"`
	Type     []string `json:"type,omitempty"`
	Outcome  string   `json:"outcome,omitempty"`
	Flagged  *bool    `json:"flagged,omitempty"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Q        string   `json:"q,omitempty"`
}

func instant(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (f Filters) canonical() canonical {
	if f.Q != "" {
		return canonical{Q: f.Q}
	}
	var types []string
	for _, t := range AuditTypes {
		if slices.Contains(f.Types, t) {
			types = append(types, t)
		}
	}
	return canonical{Hospital: f.Hospital, Person: f.Person, Type: types, Outcome: f.Outcome, Flagged: f.Flagged,
		From: instant(f.From), To: instant(f.To)}
}

// hash is the first 16 bytes of sha256 of the canonical filters' JSON.
func (f Filters) hash() []byte {
	raw, _ := json.Marshal(f.canonical()) // strings, a bool and a list: never fails
	sum := sha256.Sum256(raw)
	return sum[:16]
}

// Recorded is f as audit.viewed records it: closed codes, ids and
// instants, never q (spec §5.2).
func (f Filters) Recorded() map[string]any {
	c := f.canonical()
	out := map[string]any{}
	if f.Q != "" {
		return out
	}
	if c.Hospital != "" {
		out["hospital"] = c.Hospital
	}
	if c.Person != "" {
		out["person"] = c.Person
	}
	if len(c.Type) > 0 {
		l := make([]any, len(c.Type))
		for i, t := range c.Type {
			l[i] = t
		}
		out["type"] = l
	}
	if c.Outcome != "" {
		out["outcome"] = c.Outcome
	}
	if c.Flagged != nil {
		out["flagged"] = *c.Flagged
	}
	if c.From != "" {
		out["from"] = c.From
	}
	if c.To != "" {
		out["to"] = c.To
	}
	return out
}

// CursorKeyLabel is the HKDF-SHA256 info label that derives the cursor's
// HMAC key from MFA_ENCRYPTION_KEY (Config.MFAKey), apart from the MFA
// and counter keys.
const CursorKeyLabel = "mhai audit cursor key v1"

// ErrCursorKey means no usable cursor key: the list fails closed.
var ErrCursorKey = errors.New("audit: cursor key not configured")

// CursorKey derives the cursor key from the server's master key (at least
// 32 bytes). The key is never logged.
func CursorKey(master []byte) ([]byte, error) {
	if len(master) < 32 {
		return nil, ErrCursorKey
	}
	k, err := hkdf.Key(sha256.New, master, nil, CursorKeyLabel, 32)
	if err != nil {
		return nil, ErrCursorKey
	}
	return k, nil
}

// Cursors makes and checks one session's cursors.
type Cursors struct {
	Key     []byte // from CursorKey
	Session string // the principal's session id
}

const cursorLen = 8 + 16

// tag is HMAC-SHA256(key, "audit.cursor.v1" | session | canonical filters |
// last id), each part length-prefixed, cut to 16 bytes.
func (c Cursors) tag(last int64, f Filters) []byte {
	raw, _ := json.Marshal(f.canonical())
	var id [8]byte
	binary.BigEndian.PutUint64(id[:], uint64(last))
	m := hmac.New(sha256.New, c.Key)
	for _, part := range [][]byte{[]byte("audit.cursor.v1"), []byte(c.Session), raw, id[:]} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		m.Write(n[:])
		m.Write(part)
	}
	return m.Sum(nil)[:16]
}

// Encode is hex(last id, 8 bytes big-endian || the tag): opaque, bound to
// the session and the filters, and in an alphabet (lowercase hex) that
// can't hold a pseudonym. Without a key it is "" (callers check first).
func (c Cursors) Encode(last int64, f Filters) string {
	if len(c.Key) < 32 || c.Session == "" {
		return ""
	}
	b := make([]byte, 8, cursorLen)
	binary.BigEndian.PutUint64(b, uint64(last))
	return hex.EncodeToString(append(b, c.tag(last, f)...))
}

// Decode returns the cursor's last id, or ErrCursor when it doesn't
// decode, names no row id, or its tag isn't this session's for these
// filters (forged, tampered, another session's, other filters: C33).
func (c Cursors) Decode(s string, f Filters) (int64, error) {
	if len(c.Key) < 32 || c.Session == "" {
		return 0, ErrCursor
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != cursorLen || hex.EncodeToString(b) != s {
		return 0, ErrCursor
	}
	last := int64(binary.BigEndian.Uint64(b[:8]))
	if last <= 0 || !hmac.Equal(b[8:], c.tag(last, f)) {
		return 0, ErrCursor
	}
	return last, nil
}

// --- classification: the filters on derived values, in SQL --------------

// cond is one condition on a row that both SQL and Go can check: its
// action is one of actions, and, when key is set, the payload's string
// under key is one of values; batch adds the catalogue's batch-row gate.
type cond struct {
	actions []string
	key     string
	values  []string
	batch   bool
}

// batchSQL is batchRow in SQL: no job, a job count, no entity.
const batchSQL = `(NOT (payload ? 'job_id') AND payload ? 'job_count' AND COALESCE(entity, '') = '' AND COALESCE(entity_id, '') = '')`

func (c cond) holds(e Event) bool {
	if !slices.Contains(c.actions, e.Action) {
		return false
	}
	if c.key != "" {
		s, ok := e.Payload[c.key].(string)
		if !ok || !slices.Contains(c.values, s) {
			return false
		}
	}
	return !c.batch || batchRow(e)
}

func anyHolds(cs []cond, e Event) bool {
	return slices.ContainsFunc(cs, func(c cond) bool { return c.holds(e) })
}

// The outcome and flag conditions, as the catalogue derives them.
// TestClassifyAgreesWithMap holds them against Map.
var (
	refusedConds = []cond{{actions: []string{"auth.login.failed", "auth.login.locked", "auth.login.ceiling",
		"auth.mfa.failed", "auth.mfa.locked", "correction.decision_refused"}}}
	failedConds = []cond{
		{actions: []string{"correction.apply_failed"}},
		{actions: []string{"audit.verify_finished"}, key: "state", values: []string{"error"}},
	}
	flaggedConds = []cond{
		{actions: []string{"auth.login.locked", "auth.login.ceiling", "auth.mfa.locked"}},
		{actions: []string{"audit.verify_finished"}, key: "state", values: []string{"broken"}},
		{actions: []string{"correction.decision_refused"}, key: "reason", values: []string{"self_approval"}},
	}
)

// gates are the catalogue's When rows as conditions SQL can check: the
// batch gate of the job rows, and audit.searched's web kinds. A When row
// with no gate here panics at first use; TestGatesAgreeWithWhen holds each
// gate against its When.
var gates = map[string]cond{
	"job.retried":    {batch: true},
	"job.cancelled":  {batch: true},
	"audit.searched": {key: "searched_kind", values: []string{"event", "request"}},
}

// typeConds are the conditions under which a row maps to each type: the
// catalogue's static type of each action that isn't withheld, through its
// gate where it has a When.
var typeConds = sync.OnceValue(func() map[string][]cond {
	plain := map[string][]string{}
	out := map[string][]cond{}
	actions := make([]string, 0, len(Catalogue))
	for a := range Catalogue {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		e := Catalogue[a]
		if e.Kind == UnknownKind {
			continue
		}
		if e.When != nil {
			g, ok := gates[a]
			if !ok {
				panic("audit: catalogue row " + a + " has a When the reader has no gate for")
			}
			g.actions = []string{a}
			out[e.Type] = append(out[e.Type], g)
			continue
		}
		plain[e.Type] = append(plain[e.Type], a)
	}
	for t, as := range plain {
		out[t] = append(out[t], cond{actions: as})
	}
	return out
})

func mappedConds() []cond {
	var out []cond
	for _, t := range AuditTypes {
		out = append(out, typeConds()[t]...)
	}
	return out
}

// Class is what the filters see of a row: its type, whether it maps, its
// outcome and whether it is flagged. An unmapped row is the bare entry's
// type with no outcome and no flag.
type Class struct {
	Type    string
	Mapped  bool
	Outcome string
	Flagged bool
}

func classify(e Event) Class {
	for _, t := range AuditTypes {
		if anyHolds(typeConds()[t], e) {
			c := Class{Type: t, Mapped: true, Outcome: "succeeded", Flagged: anyHolds(flaggedConds, e)}
			switch {
			case anyHolds(refusedConds, e):
				c.Outcome = "refused"
			case anyHolds(failedConds, e):
				c.Outcome = "failed"
			}
			return c
		}
	}
	return Class{Type: UnknownType}
}

// query builds a parameterised WHERE clause.
type query struct {
	args  []any
	where []string
}

func (q *query) arg(v any) string {
	q.args = append(q.args, v)
	return "$" + strconv.Itoa(len(q.args))
}

func (q *query) cond(c cond) string {
	s := "action = ANY(" + q.arg(c.actions) + "::text[])"
	if c.key != "" {
		s += " AND payload ->> " + q.arg(c.key) + "::text = ANY(" + q.arg(c.values) + "::text[])"
	}
	if c.batch {
		s += " AND " + batchSQL
	}
	return "(" + s + ")"
}

func (q *query) anyOf(cs []cond) string {
	if len(cs) == 0 {
		return "false"
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = q.cond(c)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// filter adds f's conditions. With q, only q.
func (q *query) filter(f Filters) error {
	if f.Q != "" {
		if seq, ok := eventID(f.Q); ok {
			q.where = append(q.where, "id = "+q.arg(seq))
		} else {
			q.where = append(q.where, "request_id = "+q.arg(f.Q))
		}
		return nil
	}
	switch f.Hospital {
	case "":
	case "platform":
		q.where = append(q.where, "hospital_id IS NULL")
	default:
		id, err := uuid.Parse(f.Hospital)
		if err != nil {
			return errors.New("audit: bad hospital filter")
		}
		q.where = append(q.where, "hospital_id = "+q.arg(id))
	}
	if f.Person != "" {
		id, err := uuid.Parse(f.Person)
		if err != nil {
			return errors.New("audit: bad person filter")
		}
		q.where = append(q.where, "actor_id = "+q.arg(id))
	}
	if len(f.Types) > 0 {
		var parts []string
		for _, t := range f.canonical().Type {
			parts = append(parts, q.anyOf(typeConds()[t]))
			if t == UnknownType {
				// The bare entry is sent as this type: rows with no row
				// in the catalogue, withheld rows, gated-out rows.
				parts = append(parts, "NOT "+q.anyOf(mappedConds()))
			}
		}
		if len(parts) == 0 {
			return errors.New("audit: bad type filter")
		}
		q.where = append(q.where, "("+strings.Join(parts, " OR ")+")")
	}
	if f.Outcome != "" {
		mapped := q.anyOf(mappedConds())
		switch f.Outcome {
		case "refused":
			q.where = append(q.where, mapped+" AND "+q.anyOf(refusedConds))
		case "failed":
			q.where = append(q.where, mapped+" AND NOT "+q.anyOf(refusedConds)+" AND "+q.anyOf(failedConds))
		case "succeeded":
			q.where = append(q.where, mapped+" AND NOT "+q.anyOf(refusedConds)+" AND NOT "+q.anyOf(failedConds))
		default:
			return errors.New("audit: bad outcome filter")
		}
	}
	if f.Flagged != nil {
		flagged := "(" + q.anyOf(mappedConds()) + " AND " + q.anyOf(flaggedConds) + ")"
		if *f.Flagged {
			q.where = append(q.where, flagged)
		} else {
			q.where = append(q.where, "NOT "+flagged)
		}
	}
	if f.From != nil {
		q.where = append(q.where, "ts >= "+q.arg(*f.From))
	}
	if f.To != nil {
		q.where = append(q.where, "ts < "+q.arg(*f.To))
	}
	return nil
}

func (q *query) clause() string {
	if len(q.where) == 0 {
		return "true"
	}
	return strings.Join(q.where, " AND ")
}

// eventID parses an event id: a canonical positive decimal.
func eventID(s string) (int64, bool) {
	if !eventIDPattern.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// EventID parses a path's event id.
func EventID(s string) (int64, bool) { return eventID(s) }

// --- the entries -----------------------------------------------------------

// Actor is an entry's actor.
type Actor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	Kind string `json:"kind"`
}

// HospitalRef is an entry's hospital, and a hospitals list item.
type HospitalRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// Refs are an entry's references. No clearance or export exists yet.
type Refs struct {
	ClearanceID *string `json:"clearance_id"`
	ExportID    *string `json:"export_id"`
	RequestID   *string `json:"request_id"`
}

// EventOut is a mapped entry, exactly the contract's AuditEvent.
type EventOut struct {
	ID         string         `json:"id"`
	Seq        int64          `json:"seq"`
	Hash       string         `json:"hash"`
	At         string         `json:"at"`
	Actor      Actor          `json:"actor"`
	Hospital   *HospitalRef   `json:"hospital"`
	Type       string         `json:"type"`
	Kind       string         `json:"kind"`
	Target     *Target        `json:"target"`
	Outcome    string         `json:"outcome"`
	IP         *string        `json:"ip"`
	Device     *string        `json:"device"`
	Flag       *string        `json:"flag"`
	Reason     *string        `json:"reason"`
	Note       *string        `json:"note"`
	Refs       Refs           `json:"refs"`
	Facts      map[string]any `json:"facts"`
	AfterBreak bool           `json:"after_break"`
}

// Page is the list's reply, exactly the contract's AuditList. Events are
// EventOut or Bare. LastCheckpoint is always null: no retention
// checkpoint exists yet (audit_checkpoints are verify points, not the
// contract's checkpoint).
type Page struct {
	Events         []any     `json:"events"`
	NextCursor     *string   `json:"next_cursor"`
	Total          int       `json:"total"`
	TotalCapped    bool      `json:"total_capped"`
	OldestKeptAt   string    `json:"oldest_kept_at"`
	LastCheckpoint *struct{} `json:"last_checkpoint"`
}

// Detail is an entry plus patients and letter, both null: no named access
// and no clearance letter exist yet (spec §5.2).
type Detail struct {
	Entry    any
	Patients *struct{}
	Letter   *struct{}
}

// MarshalJSON flattens the entry with patients and letter.
func (d Detail) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(d.Entry)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["patients"] = json.RawMessage("null")
	m["letter"] = json.RawMessage("null")
	return json.Marshal(m)
}

const rowColumns = `id, ts, hash, actor_id::text, hospital_id::text, request_id, action, entity, entity_id, payload`

type rawRow struct {
	row        Row
	payload    []byte
	badPayload bool
}

func scanRows(rows pgx.Rows) ([]rawRow, error) {
	defer rows.Close()
	var out []rawRow
	for rows.Next() {
		var r rawRow
		var actor, hospital, rid, entity, entityID *string
		if err := rows.Scan(&r.row.Seq, &r.row.At, &r.row.Hash, &actor, &hospital, &rid, &r.row.Event.Action,
			&entity, &entityID, &r.payload); err != nil {
			return nil, err
		}
		r.row.Event.ActorID, r.row.Event.HospitalID, r.row.Event.RequestID = deref(actor), deref(hospital), deref(rid)
		r.row.Event.Entity, r.row.Event.EntityID = deref(entity), deref(entityID)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEvents is one page of the filters' entries, newest first, after the
// cursor's id (0: the first page). cur signs the next cursor.
func ListEvents(ctx context.Context, tx pgx.Tx, cur Cursors, f Filters, after int64, limit int) (Page, error) {
	if len(cur.Key) < 32 || cur.Session == "" {
		return Page{}, ErrCursorKey
	}
	if limit < 1 || limit > MaxLimit {
		return Page{}, errors.New("audit: bad limit")
	}
	q := &query{}
	if err := q.filter(f); err != nil {
		return Page{}, err
	}
	where := q.clause()
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM audit_log WHERE `+where+
		` LIMIT `+strconv.Itoa(TotalCap+1)+`) s`, q.args...).Scan(&total); err != nil {
		return Page{}, fmt.Errorf("audit: count: %w", err)
	}
	page := *q
	page.args = slices.Clone(q.args)
	if after > 0 {
		page.where = append(slices.Clone(q.where), "id < "+page.arg(after))
	}
	rows, err := tx.Query(ctx, `SELECT `+rowColumns+` FROM audit_log WHERE `+page.clause()+
		` ORDER BY id DESC LIMIT `+strconv.Itoa(limit+1), page.args...)
	if err != nil {
		return Page{}, fmt.Errorf("audit: list: %w", err)
	}
	raws, err := scanRows(rows)
	if err != nil {
		return Page{}, fmt.Errorf("audit: list: %w", err)
	}
	out := Page{Events: []any{}, Total: min(total, TotalCap), TotalCapped: total > TotalCap}
	if len(raws) > limit {
		raws = raws[:limit]
		c := cur.Encode(raws[len(raws)-1].row.Seq, f)
		out.NextCursor = &c
	}
	if out.Events, err = entries(ctx, tx, raws); err != nil {
		return Page{}, err
	}
	if out.OldestKeptAt, err = oldestKept(ctx, tx); err != nil {
		return Page{}, err
	}
	return out, nil
}

// GetEvent is one entry, or ErrNotFound.
func GetEvent(ctx context.Context, tx pgx.Tx, seq int64) (Detail, error) {
	rows, err := tx.Query(ctx, `SELECT `+rowColumns+` FROM audit_log WHERE id = $1`, seq)
	if err != nil {
		return Detail{}, fmt.Errorf("audit: get: %w", err)
	}
	raws, err := scanRows(rows)
	if err != nil {
		return Detail{}, fmt.Errorf("audit: get: %w", err)
	}
	if len(raws) == 0 {
		return Detail{}, ErrNotFound
	}
	es, err := entries(ctx, tx, raws)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Entry: es[0]}, nil
}

// oldestKept is the oldest entry's time (ts never goes backwards in id
// order), or now for an empty log.
func oldestKept(ctx context.Context, tx pgx.Tx) (string, error) {
	var ts time.Time
	err := tx.QueryRow(ctx, `SELECT ts FROM audit_log ORDER BY id LIMIT 1`).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		ts, err = time.Now(), nil
	}
	if err != nil {
		return "", fmt.Errorf("audit: oldest: %w", err)
	}
	return ts.UTC().Format(time.RFC3339Nano), nil
}

// openBreak is the first broken seq of the latest broken run while its
// failure is open (OpenFailure), or 0.
func openBreak(ctx context.Context, tx pgx.Tx) (int64, error) {
	_, seq, err := openFailure(ctx, tx)
	return seq, err
}

// OpenFailure is the latest broken run while its failure is open, or
// uuid.Nil: open until a later run is intact and the broken run has its
// outcome note (spec §5.4; contract §14.9). The note itself is never read.
func OpenFailure(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	id, _, err := openFailure(ctx, tx)
	return id, err
}

func openFailure(ctx context.Context, tx pgx.Tx) (uuid.UUID, int64, error) {
	var id uuid.UUID
	var seq int64
	var noted, intactAfter bool
	err := tx.QueryRow(ctx, `
		SELECT b.id, b.first_broken_seq, b.outcome_note IS NOT NULL,
		       EXISTS (SELECT 1 FROM audit_verify_runs i WHERE i.state = 'intact' AND i.started_at > b.started_at)
		FROM audit_verify_runs b WHERE b.state = 'broken'
		ORDER BY b.started_at DESC, b.id DESC LIMIT 1`).Scan(&id, &seq, &noted, &intactAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, nil
	}
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("audit: open failure: %w", err)
	}
	if noted && intactAfter {
		return uuid.Nil, 0, nil
	}
	return id, seq, nil
}

type person struct {
	name     string
	hospital *string
	roles    []string
}

// people loads the users named by ids.
func people(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[string]person, error) {
	out := map[string]person{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT u.id::text, u.display_name, u.hospital_id::text,
		       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
		FROM users u WHERE u.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("audit: actors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p person
		if err := rows.Scan(&id, &p.name, &p.hospital, &p.roles); err != nil {
			return nil, fmt.Errorf("audit: actors: %w", err)
		}
		out[id] = p
	}
	return out, rows.Err()
}

func hospitalsByID(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[string]HospitalRef, error) {
	out := map[string]HospitalRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text, name, status = 'removed' FROM hospitals WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("audit: hospitals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h HospitalRef
		if err := rows.Scan(&h.ID, &h.Name, &h.Removed); err != nil {
			return nil, fmt.Errorf("audit: hospitals: %w", err)
		}
		out[h.ID] = h
	}
	return out, rows.Err()
}

// actorRoles are the roles an actor is shown as (mhai-web ActorRole).
var actorRoles = []string{authz.RoleClinician, authz.RoleHospitalAdmin, authz.RolePlatformAdmin, authz.RoleAuditor}

// reviewerActions are a clinician's decisions on someone else's
// correction: the actor is shown as a reviewer.
var reviewerActions = []string{"correction.approved", "correction.rejected", "correction.decision_refused"}

// actorOf is the entry's actor, or false when it can't be shown: an
// unknown user, a name mhai-web wouldn't read, or no role it knows.
func actorOf(actorID, action string, ps map[string]person) (Actor, bool) {
	if actorID == "" {
		return Actor{ID: "system", Name: "System", Role: "system", Kind: "system"}, true
	}
	p, ok := ps[actorID]
	if !ok || !displayText(p.name, nameMax) {
		return Actor{}, false
	}
	a := Actor{ID: actorID, Name: p.name, Kind: "staff"}
	if p.hospital == nil {
		a.Kind = "platform"
	}
	for _, r := range authz.SortRoles(p.roles) {
		if slices.Contains(actorRoles, r) && authz.IsPlatformRole(r) == (p.hospital == nil) {
			a.Role = r
			break
		}
	}
	if a.Role == "" {
		return Actor{}, false
	}
	if a.Role == authz.RoleClinician && slices.Contains(reviewerActions, action) {
		a.Role = "reviewer"
	}
	return a, true
}

// entries builds each row's entry: the mapped event, or the bare entry
// when the catalogue, the actor, the hospital or the pseudonym check says
// it can't be shown.
func entries(ctx context.Context, tx pgx.Tx, raws []rawRow) ([]any, error) {
	var actorIDs, hospitalIDs []uuid.UUID
	for i := range raws {
		var payload map[string]any
		if err := json.Unmarshal(raws[i].payload, &payload); err != nil || payload == nil {
			// Not an object: sent bare.
			raws[i].badPayload = true
		}
		raws[i].row.Event.Payload = payload
		if id, err := uuid.Parse(raws[i].row.Event.ActorID); err == nil {
			actorIDs = append(actorIDs, id)
		}
		if id, err := uuid.Parse(raws[i].row.Event.HospitalID); err == nil {
			hospitalIDs = append(hospitalIDs, id)
		}
	}
	ps, err := people(ctx, tx, actorIDs)
	if err != nil {
		return nil, err
	}
	hs, err := hospitalsByID(ctx, tx, hospitalIDs)
	if err != nil {
		return nil, err
	}
	brokenAt, err := openBreak(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(raws))
	for _, r := range raws {
		if r.badPayload {
			out = append(out, safeBare(r.row))
			continue
		}
		out = append(out, entry(r.row, ps, hs, brokenAt))
	}
	return out, nil
}

// safeBare is the bare entry, without a stored hash that isn't one core
// wrote (64 lowercase hex): mhai-web reads either as undisplayable.
func safeBare(r Row) Bare {
	b := Unmapped(r)
	if !hexHash(r.Hash) {
		b.Hash = ""
	}
	return b
}

func entry(r Row, ps map[string]person, hs map[string]HospitalRef, brokenAt int64) any {
	bare := safeBare(r)
	if bare.Hash == "" {
		return bare
	}
	m, ok := Map(r.Event)
	if !ok {
		return bare
	}
	actor, ok := actorOf(r.Event.ActorID, r.Event.Action, ps)
	if !ok {
		return bare
	}
	ev := EventOut{
		ID: bare.ID, Seq: r.Seq, Hash: r.Hash, At: bare.At, Actor: actor,
		Type: m.Type, Kind: m.Kind, Target: m.Target, Outcome: m.Outcome, Flag: m.Flag, Facts: m.Facts,
		AfterBreak: brokenAt > 0 && r.Seq > brokenAt,
	}
	if ev.Facts == nil {
		ev.Facts = map[string]any{}
	}
	if r.Event.HospitalID != "" {
		h, found := hs[r.Event.HospitalID]
		if !found || !displayText(h.Name, nameMax) {
			return bare
		}
		ev.Hospital = &h
	}
	if rid := r.Event.RequestID; rid != "" && opaqueID.MatchString(rid) {
		ev.Refs.RequestID = &rid
	}
	// Last: nothing in the whole entry, at any depth, of the pseudonym's
	// shape (C22). Names and request ids come from outside the catalogue.
	raw, err := json.Marshal(ev)
	if err != nil {
		return bare
	}
	var back any
	if err := json.Unmarshal(raw, &back); err != nil || holdsPseudonym(back) {
		return bare
	}
	return ev
}

func hexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// displayText is mhai-web's text check: 1 to max UTF-16 units, no control
// or format character.
func displayText(s string, max int) bool {
	n := len(utf16.Encode([]rune(s)))
	if n < 1 || n > max {
		return false
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

// --- hospitals and people --------------------------------------------------

// Hospitals is every hospital the log names, removed ones included, by
// name. A hospital whose name mhai-web wouldn't read is left out.
func Hospitals(ctx context.Context, tx pgx.Tx) ([]HospitalRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT h.id::text, h.name, h.status = 'removed' FROM hospitals h
		WHERE EXISTS (SELECT 1 FROM audit_log a WHERE a.hospital_id = h.id)
		ORDER BY h.name, h.id`)
	if err != nil {
		return nil, fmt.Errorf("audit: hospitals: %w", err)
	}
	defer rows.Close()
	out := []HospitalRef{}
	for rows.Next() {
		var h HospitalRef
		if err := rows.Scan(&h.ID, &h.Name, &h.Removed); err != nil {
			return nil, fmt.Errorf("audit: hospitals: %w", err)
		}
		if displayText(h.Name, nameMax) && !holdsPseudonym(h.Name) {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// PersonRef is a person's hospital.
type PersonRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PersonOut is one person the picker can show (contract AuditPerson).
type PersonOut struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Email    string     `json:"email"`
	Role     string     `json:"role"`
	Kind     string     `json:"kind"`
	Hospital *PersonRef `json:"hospital"`
}

// likeEscape makes s literal inside an ILIKE pattern (escape \).
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SearchPeople is at most 20 people whose name or email holds q, ignoring
// case, platform-wide (spec §5.2). The handler checks q. A person mhai-web
// couldn't show (a name, email or hospital name it wouldn't read, no role
// it knows, anything of the pseudonym's shape) is left out.
func SearchPeople(ctx context.Context, tx pgx.Tx, q string) ([]PersonOut, error) {
	out := []PersonOut{}
	pattern := "%" + likeEscape(q) + "%"
	// Keyset pages over (display_name, id) until 20 people can be shown or
	// the matches run out: a match mhai-web couldn't show never shortens
	// the list while showable matches remain.
	var afterName, afterID *string
	for {
		n, last, lastID, err := peoplePage(ctx, tx, pattern, afterName, afterID, &out)
		if err != nil {
			return nil, err
		}
		if len(out) == peopleMax || n < peopleBatch {
			return out, nil
		}
		afterName, afterID = &last, &lastID
	}
}

// peopleBatch is how many matches one round reads.
const peopleBatch = 100

// peoplePage reads one batch of matches after (afterName, afterID), adds
// the showable ones to out until it holds 20, and returns how many rows it
// read and the last row's key.
func peoplePage(ctx context.Context, tx pgx.Tx, pattern string, afterName, afterID *string, out *[]PersonOut) (n int, lastName, lastID string, err error) {
	rows, err := tx.Query(ctx, `
		SELECT u.id::text, u.display_name, u.email::text, u.hospital_id::text, h.name,
		       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
		FROM users u LEFT JOIN hospitals h ON h.id = u.hospital_id
		WHERE (u.display_name ILIKE $1 ESCAPE '\' OR u.email::text ILIKE $1 ESCAPE '\')
		  AND ($2::text IS NULL OR (u.display_name, u.id::text) > ($2::text, $3::text))
		ORDER BY u.display_name, u.id::text LIMIT $4`, pattern, afterName, afterID, peopleBatch)
	if err != nil {
		return 0, "", "", fmt.Errorf("audit: people: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p PersonOut
		var hid, hname *string
		var roles []string
		if err := rows.Scan(&p.ID, &p.Name, &p.Email, &hid, &hname, &roles); err != nil {
			return 0, "", "", fmt.Errorf("audit: people: %w", err)
		}
		n++
		lastName, lastID = p.Name, p.ID
		if len(*out) == peopleMax {
			break
		}
		if showable(&p, hid, hname, roles) {
			*out = append(*out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", "", fmt.Errorf("audit: people: %w", err)
	}
	return n, lastName, lastID, nil
}

// showable fills p's role, kind and hospital, and reports whether mhai-web
// could show p: a role it knows, names and an email it would read, nothing
// of the pseudonym's shape.
func showable(p *PersonOut, hid, hname *string, roles []string) bool {
	a, ok := actorOf(p.ID, "", map[string]person{p.ID: {name: p.Name, hospital: hid, roles: roles}})
	if !ok || !displayText(p.Email, emailMax) {
		return false
	}
	p.Role, p.Kind = a.Role, a.Kind
	if hid != nil {
		if hname == nil || !displayText(*hname, nameMax) {
			return false
		}
		p.Hospital = &PersonRef{ID: *hid, Name: *hname}
	}
	raw, _ := json.Marshal(p)
	var back any
	return json.Unmarshal(raw, &back) == nil && !holdsPseudonym(back)
}

// --- reads are recorded ----------------------------------------------------

// Reader is who reads, from the verified principal.
type Reader struct {
	UserID, SessionID, RequestID string
}

// RecordRead records a list page or a detail read inside tx: audit.viewed
// for the list, audit.searched for a q search (an event or request id).
// The detail of event n is recorded as the search q=n, so a walk over ids
// leaves one row per entry read. Every page is recorded, cursor or not.
// Rows collapse per session, list and filters (q included, so each
// distinct search is its own row) for 5 minutes (spec §5.2). Call it last
// in tx: audit.Write holds the chain lock until commit.
func RecordRead(ctx context.Context, tx pgx.Tx, r Reader, f Filters) (recorded bool, err error) {
	var e Event
	switch _, isEvent := eventID(f.Q); {
	case f.Q != "" && isEvent:
		e = Event{Action: "audit.searched", Entity: "audit_event", EntityID: f.Q,
			Payload: map[string]any{"searched_kind": "event"}}
	case f.Q != "":
		e = Event{Action: "audit.searched", Entity: "request", EntityID: f.Q,
			Payload: map[string]any{"searched_kind": "request"}}
	default:
		e = Event{Action: "audit.viewed", Payload: map[string]any{"list": "events", "filters": f.Recorded()}}
	}
	return recordRead(ctx, tx, r, "events", f.hash(), e)
}

// RecordPeopleSearch records a person-picker search: audit.searched
// {searched_kind: person}, never the text searched; rows collapse per
// session and a hash of q for 5 minutes.
func RecordPeopleSearch(ctx context.Context, tx pgx.Tx, r Reader, q string) (bool, error) {
	sum := sha256.Sum256([]byte(q))
	return recordRead(ctx, tx, r, "people", sum[:], Event{Action: "audit.searched",
		Payload: map[string]any{"searched_kind": "person"}})
}

// RecordHospitalsRead records a read of the hospitals list: audit.searched
// {searched_kind: hospital}, once per session per 5 minutes.
func RecordHospitalsRead(ctx context.Context, tx pgx.Tx, r Reader) (bool, error) {
	return recordRead(ctx, tx, r, "hospitals", nil, Event{Action: "audit.searched",
		Payload: map[string]any{"searched_kind": "hospital"}})
}

// recordRead writes e unless this session already recorded the same list and
// dedupe material in the last 5 minutes. The dedupe key is a row in
// idempotency_keys inserted in the same transaction as the entry, so both
// commit or neither, and only the first of parallel reads writes. recorded
// is false when the key was already held.
func recordRead(ctx context.Context, tx pgx.Tx, r Reader, list string, material []byte, e Event) (recorded bool, err error) {
	if _, err := uuid.Parse(r.UserID); err != nil || r.SessionID == "" {
		return false, errors.New("audit: read without a reader")
	}
	sum := sha256.New()
	for _, part := range [][]byte{[]byte(r.SessionID), []byte(list), material} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		sum.Write(n[:])
		sum.Write(part)
	}
	key := hex.EncodeToString(sum.Sum(nil))
	if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE scope = $1 AND key = $2 AND expires_at <= now()`,
		readScope, key); err != nil {
		return false, fmt.Errorf("audit: read key: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (scope, key, hospital_id, request_hash, expires_at)
		VALUES ($1, $2, NULL, $2, now() + make_interval(secs => $3))
		ON CONFLICT (scope, key) DO NOTHING`, readScope, key, readDedupe.Seconds())
	if err != nil {
		return false, fmt.Errorf("audit: read key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	e.ActorID, e.RequestID = r.UserID, r.RequestID
	if err := Write(ctx, tx, e); err != nil {
		return false, err
	}
	return true, nil
}
