//go:build integration

package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// The legal path, hospital side (plan Task 26; T29): the named env's admin
// opens a named list through the route; the platform (as the superuser
// acting as mhai_platform, through 00044's functions) exports it and binds
// it to a reference naming hospital admins; a named admin resolves it.
// Names come from the demo fixture behind the counting resolver.

const legalCode = "246813"

type legalEnv struct {
	*namedEnv
	platform uuid.UUID
}

func newLegalEnv(t *testing.T) *legalEnv {
	t.Helper()
	e := &legalEnv{namedEnv: newNamedEnv(t)}
	e.platform = uuid.New()
	run(t, e.super, `INSERT INTO users (id, hospital_id, email, display_name) VALUES ($1, NULL, $2, 'Synthetic Platform Admin')`,
		e.platform, "pa-"+e.platform.String()[:8]+"@platform.example.test")
	run(t, e.super, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'platform_admin')`, e.platform)
	run(t, e.super, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', true)`, e.platform)
	e.hd.d.LegalOn = true
	e.hd.stepUpCheck = func(_ *http.Request, mfa, _ string) error {
		if mfa == legalCode {
			return nil
		}
		return errors.New("wrong code")
	}
	e.mux.Handle("POST "+httpx.BasePath+"/hospital/legal-resolutions", httpx.NoStore(http.HandlerFunc(e.hd.legalResolve)))
	e.mux.Handle("GET "+httpx.BasePath+"/hospital/legal-resolutions", http.HandlerFunc(e.hd.legalList))
	// One named list, through the route.
	if c, b := e.post(t, uuid.Nil, "legal-list-"+uuid.NewString()[:8], e.namedBody(t, "audit", "")); c != 200 {
		t.Fatalf("named list: %d %s", c, b)
	}
	return e
}

// reference exports the hospital's named entries and binds them to a new
// reference naming admins, as the platform would.
func (e *legalEnv) reference(t *testing.T, admins ...uuid.UUID) (uuid.UUID, int) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_platform`); err != nil {
		t.Fatal(err)
	}
	export := uuid.New()
	var n int
	var ref uuid.UUID
	if _, err := tx.Exec(ctx, `INSERT INTO audit_exports (id, kind, owner, hospital_id, reason, filters, max_seq, rows, checksum, file_name)
		VALUES ($1, 'named_access', $2, $3, 'legal_request', jsonb_build_object('hospital', $4::text), (SELECT max(id) FROM audit_log),
			0, repeat('0', 64), 'a.zip')`, export, e.platform, e.h, e.h.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT mhai_audit_export_named($1, 50000)`, export).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_exports SET rows = $2 WHERE id = $1`, export, n); err != nil {
		t.Fatal(err)
	}
	if err := audit.Write(ctx, tx, audit.Event{ActorID: e.platform.String(), Action: "audit.named_access_exported", Entity: "audit_export",
		EntityID: export.String(), Payload: map[string]any{"rows": n, "capped": false, "filters": map[string]any{"hospital": e.h.String()}}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM mhai_legal_reference_create($1, $2, $3, $4)`, export, "LR-2026-"+uuid.NewString()[:6],
		admins, e.platform).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	named := make([]any, len(admins))
	for i, a := range admins {
		named[i] = a.String()
	}
	if err := audit.Write(ctx, tx, audit.Event{ActorID: e.platform.String(), Action: "legal_reference.created", Entity: "legal_reference",
		EntityID: ref.String(), Payload: map[string]any{"export_id": export.String(), "hospital_id": e.h.String(), "admins": named}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ref, n
}

func (e *legalEnv) resolve(t *testing.T, user uuid.UUID, ref uuid.UUID, key, code string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"legal_reference_id": ref.String(), "mfa_code": code})
	return e.do(t, user, http.MethodPost, "/hospital/legal-resolutions", key, string(body))
}

type legalReplyT struct {
	LegalReferenceID string `json:"legal_reference_id"`
	Rows             []map[string]any
}

// T29: only an admin named on the reference resolves it; the rows carry
// the export's refs, periods, names and MRNs, and no pid.
func TestT29OnlyNamedAdmins(t *testing.T) {
	e := newLegalEnv(t)
	ref, n := e.reference(t, e.user)
	// Listed for the named admin only.
	c, b := e.do(t, uuid.Nil, http.MethodGet, "/hospital/legal-resolutions", "", "")
	if c != 200 || !strings.Contains(string(b), ref.String()) {
		t.Errorf("list: %d %s", c, b)
	}
	if c, b := e.do(t, e.other, http.MethodGet, "/hospital/legal-resolutions", "", ""); c != 200 || strings.Contains(string(b), ref.String()) {
		t.Errorf("another admin's list: %d %s", c, b)
	}
	if c, b := e.resolve(t, e.other, ref, "k-other", legalCode); c != 404 {
		t.Errorf("an admin not named: %d %s", c, b)
	}
	if c, _ := e.resolve(t, uuid.Nil, ref, "k-1", ""); c != 403 {
		t.Errorf("no code: %d", c)
	}
	if c, _ := e.resolve(t, uuid.Nil, ref, "k-1", "000000"); c != 403 {
		t.Errorf("a wrong code: %d", c)
	}
	calls := e.res.n()
	c, b = e.resolve(t, uuid.Nil, ref, "k-1", legalCode)
	if c != 200 {
		t.Fatalf("resolve: %d %s", c, b)
	}
	var out legalReplyT
	if err := json.Unmarshal(b, &out); err != nil || out.LegalReferenceID != ref.String() || len(out.Rows) != n {
		t.Fatalf("reply %s (%v)", b, err)
	}
	for _, r := range out.Rows {
		name, _ := r["name"].(string)
		p, _ := r["period"].(map[string]any)
		if !strings.HasSuffix(name, "(synthetic)") || r["mrn"] == nil || len(r["export_row_ref"].(string)) != 24 ||
			p["level"] != "week" || len(r) != 4 {
			t.Errorf("row %v", r)
		}
	}
	if strings.Contains(string(b), `"P-`) {
		t.Error("the reply holds a pid")
	}
	if e.res.n() != calls+1 {
		t.Errorf("identity called %d times", e.res.n()-calls)
	}
	var payload string
	if err := e.super.QueryRow(context.Background(), `SELECT payload::text FROM audit_log WHERE action = 'analysis.legal_resolved'
		AND entity_id = $1 AND actor_id = $2 AND hospital_id = $3`, ref.String(), e.user, e.h).Scan(&payload); err != nil ||
		strings.Contains(payload, "synthetic") || strings.Contains(payload, "SYN-") {
		t.Errorf("entry %s (%v)", payload, err)
	}
	if strings.Contains(e.logs.String(), "(synthetic)") || strings.Contains(e.logs.String(), "SYN-") {
		t.Error("a name or MRN in the logs")
	}
}

// T29: once only; the same key replays (each audited), at most 3.
func TestT29OnceOnly(t *testing.T) {
	e := newLegalEnv(t)
	ref, _ := e.reference(t, e.user, e.other)
	if c, b := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 200 {
		t.Fatalf("resolve: %d %s", c, b)
	}
	if c, b := e.resolve(t, uuid.Nil, ref, "k-2", legalCode); c != 409 || !strings.Contains(string(b), "already_used") {
		t.Errorf("a new key: %d %s", c, b)
	}
	var refused int
	if err := e.super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'analysis.legal_resolve_refused'
		AND entity_id = $1 AND payload ->> 'reason' = 'already_used'`, ref.String()).Scan(&refused); err != nil || refused != 1 {
		t.Errorf("%d refusal entries (%v)", refused, err)
	}
	if c, b := e.resolve(t, e.other, ref, "k-1", legalCode); c != 409 {
		t.Errorf("the other named admin: %d %s", c, b)
	}
	for i := 0; i < 3; i++ {
		if c, b := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 200 {
			t.Fatalf("replay %d: %d %s", i+1, c, b)
		}
	}
	// The minute's limit (5) is reached here; a fourth replay is
	// already_used (00044's test).
	if c, _ := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 429 {
		t.Errorf("past the minute's limit: %d", c)
	}
	var entries int
	if err := e.super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'analysis.legal_resolved'
		AND entity_id = $1`, ref.String()).Scan(&entries); err != nil || entries != 4 {
		t.Errorf("%d resolution entries, want 4 (%v)", entries, err)
	}
}

// T29: parallel resolutions with different keys, exactly one succeeds.
func TestT29ParallelExactlyOne(t *testing.T) {
	e := newLegalEnv(t)
	ref, _ := e.reference(t, e.user)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < LegalPerMinute; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _ := e.resolve(t, uuid.Nil, ref, "k-par-"+string(rune('a'+i)), legalCode)
			mu.Lock()
			codes[c]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[200] != 1 || codes[409] != LegalPerMinute-1 {
		t.Errorf("codes %v", codes)
	}
}

// T18: the entry can't be written, so nothing is used and no name is read.
func TestLegalAuditFailNoIdentityCall(t *testing.T) {
	e := newLegalEnv(t)
	ref, _ := e.reference(t, e.user)
	e.hd.afterOpen = func() error { return errors.New("synthetic failure before the entry") }
	calls := e.res.n()
	if c, b := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 503 || strings.Contains(string(b), "synthetic") {
		t.Errorf("a failed entry: %d %s", c, b)
	}
	if e.res.n() != calls {
		t.Error("identity was called without an entry")
	}
	e.hd.afterOpen = nil
	if c, b := e.resolve(t, uuid.Nil, ref, "k-2", legalCode); c != 200 {
		t.Errorf("the reference was used by the failed call: %d %s", c, b)
	}
}

// The cover is read again after the identity call: a revocation while
// names are read sends none, records the failure, and the reference stays
// used.
func TestLegalCoverReadAgain(t *testing.T) {
	e := newLegalEnv(t)
	ref, _ := e.reference(t, e.user)
	e.res.during = func() {
		run(t, e.super, `UPDATE clearances SET status = 'revoked', revoked_at = now(), revoked_side = 'hospital',
			revoke_reason = 'no_longer_needed', revoked_by = verified_by WHERE id = $1`, e.letter)
	}
	c, b := e.resolve(t, uuid.Nil, ref, "k-1", legalCode)
	if c != 503 || strings.Contains(string(b), "synthetic") {
		t.Errorf("revoked while reading: %d %s", c, b)
	}
	var failed int
	if err := e.super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'analysis.legal_resolve_failed'
		AND entity_id = $1`, ref.String()).Scan(&failed); err != nil || failed != 1 {
		t.Errorf("%d failure entries (%v)", failed, err)
	}
}

// Off: analysis off is 404 before anything; no cover is 403.
func TestLegalGates(t *testing.T) {
	e := newLegalEnv(t)
	ref, _ := e.reference(t, e.user)
	run(t, e.super, `UPDATE clearances SET status = 'revoked', revoked_at = now(), revoked_side = 'hospital',
		revoke_reason = 'no_longer_needed', revoked_by = verified_by WHERE id = $1`, e.letter)
	if c, b := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 403 || !strings.Contains(string(b), "not_cleared") {
		t.Errorf("no cover: %d %s", c, b)
	}
	run(t, e.super, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, e.h)
	if c, _ := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 404 {
		t.Errorf("analysis off: %d", c)
	}
	if c, _ := e.do(t, uuid.Nil, http.MethodGet, "/hospital/legal-resolutions", "", ""); c != 404 {
		t.Errorf("list with analysis off: %d", c)
	}
	// Exports off (CORE_AUDIT_EXPORTS): 404 too, even with analysis on
	// (security review).
	run(t, e.super, `UPDATE analysis_switch SET position = 'on' WHERE hospital_id = $1`, e.h)
	e.hd.d.LegalOn = false
	if c, _ := e.resolve(t, uuid.Nil, ref, "k-1", legalCode); c != 404 {
		t.Errorf("exports off: %d", c)
	}
	if c, _ := e.do(t, uuid.Nil, http.MethodGet, "/hospital/legal-resolutions", "", ""); c != 404 {
		t.Errorf("list with exports off: %d", c)
	}
}
