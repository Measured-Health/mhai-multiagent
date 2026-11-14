//go:build integration

package platform_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// The named-access list, the letter on an entry, exports and legal
// references (analysis plan Tasks 25-26). A named list is opened as a
// synthetic hospital admin of a demo hospital would (00042's functions,
// its entry appended in the same transaction), under an active demo
// letter seeded by the superuser. Synthetic data only.

var pidAnywhere = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)

// namedWorld is a demo hospital with named access on, an admin covered
// by an active demo letter, and one named list of n subjects.
type namedWorld struct {
	h, letter, view uuid.UUID
	admin           user
	seq             int64
	pids            []string
}

func namedList(t *testing.T, n int) namedWorld {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	w := namedWorld{h: uuid.New(), letter: uuid.New(), view: uuid.New()}
	exec(`INSERT INTO hospitals (id, name, short_code, status, time_zone, is_demo) VALUES ($1, $2, $3, 'active', 'Africa/Lagos', true)`,
		w.h, "Synthetic Demo "+uniq(), "D"+uniq())
	exec(`INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, w.h)
	for _, scope := range []any{nil, w.h} {
		exec(`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
			VALUES ($1, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC') ON CONFLICT DO NOTHING`, scope)
		exec(`INSERT INTO named_access_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
			VALUES ($1, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC') ON CONFLICT ON CONSTRAINT named_access_switch_one
			DO UPDATE SET position = 'on'`, scope)
	}
	period, cond := uuid.New(), uuid.New()
	exec(`INSERT INTO analysis_periods (id, hospital_id, level, start, "end", engine_version) VALUES ($1, $2, 'week', '2026-04-01', '2026-04-07', '1')`, period, w.h)
	exec(`INSERT INTO analysis_conditions (hospital_id, id, icd10, name) VALUES ($1, $2, 'I10', 'Synthetic hypertension')`, w.h, cond)
	for i := 0; i < n; i++ {
		s := uuid.New()
		exec(`INSERT INTO analysis_subjects (hospital_id, subject_id, ref_hmac, ref_enc, created_day)
			VALUES ($1, $2, decode(md5($3) || md5($3), 'hex'), decode(md5($3) || md5($3) || md5($3), 'hex'), '2026-03-01')`, w.h, s, s.String())
		exec(`INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting, period_late)
			VALUES ($1, $2, '2026-04-02', 'diagnosis', $3, '40-64', 'female', 'outpatient', false)`, w.h, s, cond)
	}
	w.admin = seedUser(t, w.h, "", authz.RoleHospitalAdmin)
	verifier := platformAdmin(t)
	exec(`INSERT INTO clearances (id, hospital_id, status, signed_by, signer_title, letter_date, end_date, uploaded_by,
			verified_by, verified_at, pages, sha256, demo)
		VALUES ($1, $2, 'active', 'Synthetic Signer', 'Synthetic Title', (now() AT TIME ZONE 'Africa/Lagos')::date - 40,
			(now() AT TIME ZONE 'Africa/Lagos')::date + 30, $3, $4, now(), 1, sha256('synthetic'), true)`, w.letter, w.h, w.admin.id, verifier.id)
	exec(`INSERT INTO clearance_covered (hospital_id, clearance_id, user_id, side) VALUES ($1, $2, $3, 'hospital')`, w.h, w.letter, w.admin.id)
	key, body := sha256.Sum256([]byte("synthetic-key-"+w.view.String())), sha256.Sum256([]byte("synthetic-body"))
	if err := db.WithHospitalUser(ctx, app, w.h, w.admin.id, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT mhai_analysis_named_admit(5)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT pid FROM mhai_analysis_open_named($1, $2, '{}'::jsonb,
			'{"kind": "part", "dim": "sex", "key": "female"}'::jsonb, 'affected', $3, $4, 300, 'audit', NULL, $5, $6)`,
			w.view, period, make([]byte, 32), n, key[:], body[:])
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			w.pids = append(w.pids, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: w.admin.id.String(), HospitalID: w.h.String(), Action: "analysis.named_viewed",
			Entity: "analysis_view", EntityID: w.view.String(), Payload: map[string]any{"view": "affected", "period": "week:2026-04-01",
				"filters": map[string]any{}, "patient_count": n, "rows": len(w.pids), "purpose": "audit", "replays": 0,
				"clearance_id": w.letter.String()}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := super.QueryRow(ctx, `SELECT id FROM audit_log WHERE entity = 'analysis_view' AND entity_id = $1`, w.view.String()).Scan(&w.seq); err != nil {
		t.Fatal(err)
	}
	return w
}

// The named-access list: the named entries, filtered, each with its
// letter; recorded; its cursor bound to the session and filters.
func TestNamedAccessList(t *testing.T) {
	w := namedList(t, 6)
	a := auditor(t)
	tok := mint(t, a)
	r := get(t, "/platform/audit/named-access?hospital="+w.h.String()+"&clearance="+w.letter.String(), tok)
	ok(t, "named access", r)
	sameKeys(t, "named access", r.body, "events", "next_cursor")
	evs, _ := r.body["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("%d events, want 1: %s", len(evs), r.raw)
	}
	ev := evs[0].(map[string]any)
	l, _ := ev["letter"].(map[string]any)
	if ev["seq"] != float64(w.seq) || l["id"] != w.letter.String() || l["status"] != "active" || l["valid_to"] == nil {
		t.Errorf("event %v", ev)
	}
	if vb, _ := l["verified_by"].(map[string]any); vb["id"] == nil || vb["name"] == nil {
		t.Errorf("verified_by %v", l["verified_by"])
	}
	if pidAnywhere.MatchString(r.raw) {
		t.Error("the named-access list sends a pid")
	}
	// Another letter, another hospital: nothing.
	for _, q := range []string{"clearance=" + uuid.NewString(), "hospital=" + uuid.NewString()} {
		r := get(t, "/platform/audit/named-access?"+q, tok)
		ok(t, q, r)
		if evs, _ := r.body["events"].([]any); len(evs) != 0 {
			t.Errorf("%s: %d events", q, len(evs))
		}
	}
	for _, q := range []string{"type=audit", "q=" + strconv.FormatInt(w.seq, 10), "hospital=platform", "clearance=x", "cursor=00"} {
		problem(t, q, get(t, "/platform/audit/named-access?"+q, tok), 422, "validation_failed")
	}
	var recorded bool
	for _, row := range actionsBy(t, a, "audit.viewed") {
		if row.payload["list"] == "named_access" {
			f, _ := row.payload["filters"].(map[string]any)
			recorded = recorded || f["clearance"] == w.letter.String()
		}
	}
	if !recorded {
		t.Error("the named-access read wasn't recorded with its filters")
	}
	// The detail carries the letter too.
	d := get(t, "/platform/audit/"+strconv.FormatInt(w.seq, 10), tok)
	ok(t, "detail", d)
	if l, _ := d.body["letter"].(map[string]any); l["id"] != w.letter.String() {
		t.Errorf("detail letter %v", d.body["letter"])
	}
	other := insertRow(t, row{action: "auth.logout"})
	if d := get(t, "/platform/audit/"+strconv.FormatInt(other, 10), tok); d.body["letter"] != nil {
		t.Errorf("a plain entry has letter %v", d.body["letter"])
	}
}

func exportBody(t *testing.T, kind string, filters map[string]any, backup string, extra map[string]any) string {
	m := map[string]any{"kind": kind, "filters": filters, "reason": "legal_request", "reference": "LR-2026-" + uniq()[:6]}
	if backup != "" {
		m["backup_code"] = backup
	}
	for k, v := range extra {
		m[k] = v
	}
	return jsonBody(t, m)
}

// Every route of this file, the named-access list included (security
// review), answers 404 while exports are off.
func TestExportsOffIs404(t *testing.T) {
	tok := mint(t, platformAdmin(t))
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/platform/audit/exports", `{}`},
		{http.MethodGet, "/platform/audit/exports", ""},
		{http.MethodGet, "/platform/audit/exports/" + uuid.NewString() + "/file", ""},
		{http.MethodPost, "/platform/audit/legal-references", `{}`},
		{http.MethodGet, "/platform/audit/named-access", ""},
	} {
		problem(t, c.path, send(t, emrOff, c.method, c.path, tok, c.body), 404, "not_found")
	}
}

func TestExportRefusals(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	h := uuid.NewString()
	var before int
	count := func() int {
		var n int
		if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_exports`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before = count()
	for what, body := range map[string]string{
		"unknown kind":            exportBody(t, "people", map[string]any{}, "x", nil),
		"named without hospital":  exportBody(t, "named_access", map[string]any{}, "x", nil),
		"named with type":         exportBody(t, "named_access", map[string]any{"hospital": h, "type": []any{"audit"}}, "x", nil),
		"events with q":           exportBody(t, "events", map[string]any{"q": "12"}, "x", nil),
		"unknown filter":          exportBody(t, "events", map[string]any{"color": "red"}, "x", nil),
		"unknown reason":          exportBody(t, "events", map[string]any{}, "x", map[string]any{"reason": "curiosity"}),
		"other without a note":    exportBody(t, "events", map[string]any{}, "x", map[string]any{"reason": "other"}),
		"a long note":             exportBody(t, "events", map[string]any{}, "x", map[string]any{"note": strings.Repeat("n", 201)}),
		"a pseudonym in the note": exportBody(t, "events", map[string]any{}, "x", map[string]any{"note": "see P-ABCDEFGH"}),
		"a formula reference":     exportBody(t, "events", map[string]any{}, "x", map[string]any{"reference": "=HYPERLINK()"}),
		"an unknown field":        exportBody(t, "events", map[string]any{}, "x", map[string]any{"owner": admin.id.String()}),
		"from after to": exportBody(t, "events", map[string]any{"from": "2026-11-02T00:00:00Z", "to": "2026-11-01T00:00:00Z"},
			"x", nil),
	} {
		problem(t, what, post(t, "/platform/audit/exports", tok, body), 422, "validation_failed")
	}
	problem(t, "no code", post(t, "/platform/audit/exports", tok, exportBody(t, "events", map[string]any{}, "", nil)), 403, "mfa_required")
	problem(t, "a clinical account", send(t, handler, http.MethodPost, "/platform/audit/exports",
		mint(t, seedUser(t, seedHospital(t, "active"), "", authz.RoleHospitalAdmin)), exportBody(t, "events", map[string]any{}, "x", nil)),
		403, "not_platform")
	if after := count(); after != before {
		t.Errorf("%d records after refusals", after-before)
	}
}

// An events export: the record and its entry before the reply, the
// checksum the file's, the file once to its owner.
func TestExportEventsAndDownloadOnce(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	h := seedHospital(t, "active")
	for i := 0; i < 3; i++ {
		insertRow(t, row{hospital: h, action: "auth.logout"})
	}
	r := post(t, "/platform/audit/exports", tok, exportBody(t, "events", map[string]any{"hospital": h.String()}, backupCode(t, admin), nil))
	ok(t, "export", r)
	sameKeys(t, "export", r.body, "export_id", "rows", "capped", "checksum", "expires_at", "file_name")
	id, _ := r.body["export_id"].(string)
	if r.body["rows"] != float64(3) || r.body["capped"] != false || !hexHash.MatchString(r.body["checksum"].(string)) ||
		!strings.HasPrefix(r.body["file_name"].(string), "audit-events-"+id+"-") || pidAnywhere.MatchString(r.raw) {
		t.Errorf("export %v", r.body)
	}
	if rows := actionsBy(t, admin, "audit.exported"); len(rows) != 0 {
		t.Log("audit.exported is not a read") // actionsBy reads viewed/searched only
	}
	var entries int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'audit.exported' AND entity_id = $1
		AND actor_id = $2 AND hospital_id IS NULL AND NOT (payload ? 'note') AND NOT (payload ? 'reference')`, id, admin.id).Scan(&entries); err != nil || entries != 1 {
		t.Errorf("%d export entries (%v)", entries, err)
	}
	// Someone else: 403, recorded; the owner: once; again: 410, recorded.
	other := auditor(t)
	problem(t, "not the owner", get(t, "/platform/audit/exports/"+id+"/file", mint(t, other)), 403, "forbidden")
	f := get(t, "/platform/audit/exports/"+id+"/file", tok)
	if f.code != 200 || !strings.HasPrefix(f.hdr.Get("Content-Type"), "text/csv") || f.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", f.code, f.hdr)
	}
	sum := sha256.Sum256([]byte(f.raw))
	if hex.EncodeToString(sum[:]) != r.body["checksum"] {
		t.Error("the file's checksum isn't the record's")
	}
	for _, want := range []string{"export_id," + id, "made_by," + admin.id.String(), "reason,legal_request", "filter_hospital," + h.String(),
		"rows,3", "seq,hash,at,actor_id"} {
		if !strings.Contains(f.raw, want) {
			t.Errorf("the file lacks %q", want)
		}
	}
	if strings.Contains(f.raw, admin.name) || strings.Contains(f.raw, admin.email) {
		t.Error("the file names a person")
	}
	problem(t, "again", get(t, "/platform/audit/exports/"+id+"/file", tok), 410, "gone")
	var refused []string
	rows, err := super.Query(context.Background(), `SELECT payload ->> 'reason' FROM audit_log WHERE action = 'audit.export_download_refused'
		AND entity_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		refused = append(refused, s)
	}
	rows.Close()
	if strings.Join(refused, ",") != "not_owner,gone" {
		t.Errorf("refusals recorded %v", refused)
	}
	// The list shows it downloaded, and records the read.
	l := get(t, "/platform/audit/exports", tok)
	ok(t, "list", l)
	found := false
	for _, x := range l.body["exports"].([]any) {
		e := x.(map[string]any)
		if e["id"] == id {
			found = e["downloaded_at"] != nil && e["kind"] == "events"
			if fn, _ := e["filter_names"].(map[string]any); fn["hospital"] == nil {
				t.Errorf("filter_names %v", e["filter_names"])
			}
		}
	}
	if !found {
		t.Errorf("the export isn't listed as downloaded: %s", l.raw)
	}
}

// Parallel downloads: one file.
func TestExportDownloadUnderRace(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	insertRow(t, row{action: "auth.logout"})
	r := post(t, "/platform/audit/exports", tok, exportBody(t, "events", map[string]any{"hospital": "platform"}, backupCode(t, admin), nil))
	ok(t, "export", r)
	id := r.body["export_id"].(string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := get(t, "/platform/audit/exports/"+id+"/file", tok).code
			mu.Lock()
			codes[c]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[200] != 1 || codes[410] != 9 {
		t.Errorf("codes %v, want one 200 and nine 410", codes)
	}
}

// A file past expires_at is gone.
func TestExportExpired(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	r := post(t, "/platform/audit/exports", tok, exportBody(t, "events", map[string]any{"hospital": "platform"}, backupCode(t, admin), nil))
	ok(t, "export", r)
	id := r.body["export_id"].(string)
	if _, err := super.Exec(context.Background(), `UPDATE audit_exports SET created_at = now() - interval '16 minutes',
		expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	problem(t, "expired", get(t, "/platform/audit/exports/"+id+"/file", tok), 410, "gone")
}

// A named export is a ZIP of the CSV (refs, periods, pids; no name) and
// the letter's pages; its replies hold no pid (T23).
func TestExportNamedAccessZip(t *testing.T) {
	w := namedList(t, 6)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	r := post(t, "/platform/audit/exports", tok, exportBody(t, "named_access",
		map[string]any{"hospital": w.h.String(), "clearance": w.letter.String()}, backupCode(t, admin), nil))
	ok(t, "named export", r)
	if r.body["rows"] != float64(6) || pidAnywhere.MatchString(r.raw) {
		t.Errorf("named export %s", r.raw)
	}
	id := r.body["export_id"].(string)
	l := get(t, "/platform/audit/exports", tok)
	ok(t, "list", l)
	if pidAnywhere.MatchString(l.raw) {
		t.Error("the exports list holds a pid")
	}
	f := get(t, "/platform/audit/exports/"+id+"/file", tok)
	if f.code != 200 || f.hdr.Get("Content-Type") != "application/zip" {
		t.Fatalf("download %d %v", f.code, f.hdr)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(f.raw)), int64(len(f.raw)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var table string
	for _, z := range zr.File {
		names = append(names, z.Name)
		if z.Name == "named-access.csv" {
			rc, _ := z.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			table = string(b)
		}
	}
	if strings.Join(names, ",") != "named-access.csv,letters/"+w.letter.String()+"/page-1.png" {
		t.Errorf("entries %v", names)
	}
	for _, p := range w.pids {
		if !strings.Contains(table, ",week:2026-04-01,"+p) {
			t.Errorf("the table lacks pid row %s", p)
		}
	}
	if strings.Contains(table, w.admin.name) || strings.Contains(table, "Synthetic Signer") {
		t.Error("the table names a person")
	}
	// The export's entries and the detail send no pid either.
	for _, p := range []string{"/platform/audit?type=export", "/platform/audit/named-access?hospital=" + w.h.String()} {
		if x := get(t, p, tok); x.code != 200 || pidAnywhere.MatchString(x.raw) {
			t.Errorf("%s: %d or a pid", p, x.code)
		}
	}
}

// Over the cap: 422 too_many_rows {rows}, nothing recorded.
func TestExportTooManyRows(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	h := seedHospital(t, "active")
	if _, err := super.Exec(context.Background(), `INSERT INTO audit_log (ts, actor_id, hospital_id, action, payload, prev_hash, hash)
		SELECT now(), NULL, $1, 'auth.logout', '{}', repeat('0', 64), md5(g::text || $2) || md5($2 || g::text)
		FROM generate_series(1, 50001) g`, h, uniq()); err != nil {
		t.Fatal(err)
	}
	r := post(t, "/platform/audit/exports", tok, exportBody(t, "events", map[string]any{"hospital": h.String()}, backupCode(t, admin), nil))
	if r.code != 422 || r.body["code"] != "too_many_rows" || r.body["rows"] != float64(50001) || len(r.body) != 2 {
		t.Fatalf("over the cap: %d %s", r.code, r.raw)
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_exports WHERE owner = $1`, admin.id).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d records after too_many_rows (%v)", n, err)
	}
}

// The person's hour holds under parallel requests (reserve, then check).
func TestExportRateLimitUnderRace(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	codes := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		codes = append(codes, backupCode(t, admin))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int]int{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := post(t, "/platform/audit/exports", tok, exportBody(t, "events", map[string]any{"hospital": "platform"}, codes[i], nil)).code
			mu.Lock()
			got[c]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if got[429] != 10 || got[200]+got[403]+got[500] != 10 {
		t.Errorf("codes %v: want ten past the limit", got)
	}
}

// Step 1 of the legal path through the route (T29's step-1 half).
func TestLegalReferenceRoute(t *testing.T) {
	w := namedList(t, 6)
	admin := platformAdmin(t)
	tok := mint(t, admin)
	r := post(t, "/platform/audit/exports", tok, exportBody(t, "named_access", map[string]any{"hospital": w.h.String()},
		backupCode(t, admin), nil))
	ok(t, "export", r)
	exportID := r.body["export_id"].(string)
	ref := "LR-2026-" + uniq()[:6]
	body := func(backup string, admins ...string) string {
		m := map[string]any{"export_id": exportID, "reference": ref, "admins": admins}
		if backup != "" {
			m["backup_code"] = backup
		}
		return jsonBody(t, m)
	}
	path := "/platform/audit/legal-references"
	problem(t, "an auditor", post(t, path, mint(t, auditor(t)), body("x", w.admin.id.String())), 403, "forbidden")
	problem(t, "no code", post(t, path, tok, body("", w.admin.id.String())), 403, "mfa_required")
	other := platformAdmin(t)
	problem(t, "not the export's owner", post(t, path, mint(t, other), body(backupCode(t, other), w.admin.id.String())), 403, "forbidden")
	stranger := seedUser(t, seedHospital(t, "active"), "", authz.RoleHospitalAdmin)
	problem(t, "another hospital's admin", post(t, path, tok, body(backupCode(t, admin), stranger.id.String())), 422, "not_eligible")
	problem(t, "four admins", post(t, path, tok, body("x", w.admin.id.String(), uuid.NewString(), uuid.NewString(), uuid.NewString())),
		422, "validation_failed")
	r = post(t, path, tok, body(backupCode(t, admin), w.admin.id.String()))
	ok(t, "reference", r)
	sameKeys(t, "reference", r.body, "id", "export_id", "reference", "admins", "created_at")
	var payload string
	if err := super.QueryRow(context.Background(), `SELECT payload::text FROM audit_log WHERE action = 'legal_reference.created'
		AND entity_id = $1 AND actor_id = $2`, r.body["id"], admin.id).Scan(&payload); err != nil || strings.Contains(payload, ref) ||
		!strings.Contains(payload, w.admin.id.String()) {
		t.Errorf("entry %s (%v)", payload, err)
	}
	problem(t, "bound twice", post(t, path, tok, body(backupCode(t, admin), w.admin.id.String())), 409, "conflict")
	// 201 rows: refused at step 1 with the count.
	r = post(t, "/platform/audit/exports", tok, exportBody(t, "named_access", map[string]any{"hospital": w.h.String()},
		backupCode(t, admin), nil))
	ok(t, "second export", r)
	exportID = r.body["export_id"].(string)
	if _, err := super.Exec(context.Background(), `UPDATE audit_exports SET rows = 201 WHERE id = $1`, exportID); err != nil {
		t.Fatal(err)
	}
	ref = "LR-2026-" + uniq()[:6]
	r = post(t, path, tok, body(backupCode(t, admin), w.admin.id.String()))
	if r.code != 422 || r.body["code"] != "too_many_rows" || r.body["rows"] != float64(201) {
		t.Errorf("201 rows: %d %s", r.code, r.raw)
	}
}
