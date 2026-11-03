//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
)

// Plan 4 Task 10: the chain check routes and the auditors' writes (spec
// §5.2, §5.4; contract §14.9; C16, C25, C26). The handler's walk is held
// (walks), so a started run stays running until a test finishes it.

const noteMarker = "SYNTHETIC-OUTCOME-NOTE-7c1d"

// send is do with a JSON body.
func send(t *testing.T, h http.Handler, method, path, tok, body string) resp {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, httpx.BasePath+path, nil)
	} else {
		r = httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Audience", "platform")
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	if method != http.MethodGet {
		r.Header.Set("Idempotency-Key", "k-"+uniq())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	out := resp{code: w.Code, raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.body)
	return out
}

func post(t *testing.T, path, tok, body string) resp {
	return send(t, handler, http.MethodPost, path, tok, body)
}

func platformAdmin(t *testing.T) user { return seedUser(t, uuid.Nil, "", authz.RolePlatformAdmin) }

// clearRuns empties the runs (and the held walks) now and after the test.
func clearRuns(t *testing.T) {
	t.Helper()
	reset := func() {
		for len(walks) > 0 {
			<-walks
		}
		if _, err := super.Exec(context.Background(), `DELETE FROM audit_verify_runs`); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)
}

// seedRun inserts a finished run as the superuser: broken or intact,
// started ago before now.
func seedRun(t *testing.T, state string, ago time.Duration) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	var err error
	switch state {
	case "broken":
		err = super.QueryRow(ctx, `INSERT INTO audit_verify_runs (started_at, state, finished_at, entries_checked, from_first,
			first_broken_seq, first_broken_id, first_broken_at)
			VALUES (now() - $1::interval, 'broken', now() - $1::interval + interval '1 second', 4, true, 5, '5', now())
			RETURNING id`, fmt.Sprintf("%d seconds", int(ago.Seconds()))).Scan(&id)
	case "intact":
		err = super.QueryRow(ctx, `INSERT INTO audit_verify_runs (started_at, state, finished_at, entries_checked, from_first, to_seq, to_at)
			VALUES (now() - $1::interval, 'intact', now() - $1::interval + interval '1 second', 9, true, 9, now())
			RETURNING id`, fmt.Sprintf("%d seconds", int(ago.Seconds()))).Scan(&id)
	default:
		t.Fatalf("seedRun %s", state)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var runKeys = []string{"id", "started_at", "finished_at", "by", "state", "entries_checked", "from_checkpoint", "from_first",
	"to_seq", "to_at", "first_broken_seq", "first_broken_id", "first_broken_at", "error_code", "outcome", "version"}

func runVersion(r map[string]any) int { return int(r["version"].(float64)) }

// C25: parallel POST /verify from platform admins and auditors return one
// run_id, and one verify_started is written; the walk is started once.
func TestTwoStartsOneRunOverHTTP(t *testing.T) {
	clearRuns(t)
	toks := []string{mint(t, auditor(t)), mint(t, platformAdmin(t))}
	var wg sync.WaitGroup
	out := make([]resp, 10)
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = post(t, "/platform/audit/verify", toks[i%2], "")
		}(i)
	}
	wg.Wait()
	first := out[0].body["run_id"]
	for i, r := range out {
		if r.code != 200 || r.body["run_id"] != first || !canonical(r.body["run_id"]) {
			t.Fatalf("start %d: %d %s (first %v)", i, r.code, r.raw, first)
		}
		sameKeys(t, "start", r.body, "run_id")
	}
	if len(walks) != 1 {
		t.Fatalf("walks started: %d", len(walks))
	}
	var started int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'audit.verify_started'
		AND entity_id = $1 AND payload->>'by' = 'person'`, first).Scan(&started); err != nil || started != 1 {
		t.Fatalf("verify_started rows: %d %v", started, err)
	}
	// The run is the caller's: a person, never null.
	var by *uuid.UUID
	if err := super.QueryRow(context.Background(), `SELECT "by" FROM audit_verify_runs WHERE id = $1`, first).Scan(&by); err != nil || by == nil {
		t.Fatalf("run by: %v %v", by, err)
	}
	// A body other than nothing or {} is refused.
	problem(t, "start with a body", post(t, "/platform/audit/verify", toks[0], `{"by":"`+uuid.NewString()+`"}`), 422, "validation_failed")
	if r := post(t, "/platform/audit/verify", toks[0], `{}`); r.code != 200 || r.body["run_id"] != first {
		t.Errorf("start with {}: %d %s", r.code, r.raw)
	}
}

func canonical(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// pendingWrites are writes the plan adds later (Task 12) that an auditor
// must be refused; until they are registered they are carried here, and
// the test checks they aren't served yet.
var pendingWrites = []string{"POST /api/v1/platform/jobs/retry", "POST /api/v1/platform/jobs/cancel"}

// C26: an auditor gets 403 on every unsafe route but the self-service ones
// and the three audit writes.
func TestAuditorRefusedOtherWrites(t *testing.T) {
	tok := mint(t, auditor(t))
	ids := regexp.MustCompile(`\{[^}]+\}`)
	table := server.Table()
	registered := map[string]bool{}
	checked := 0
	for _, rr := range table {
		key := rr.Method + " " + rr.Pattern
		registered[key] = true
		if rr.Method == http.MethodGet || rr.Method == http.MethodHead || rr.Rule.Public || rr.Rule.SelfService || rr.Rule.AuditorWrite {
			continue
		}
		path := ids.ReplaceAllString(strings.TrimPrefix(rr.Pattern, httpx.BasePath), uuid.NewString())
		r := send(t, handler, rr.Method, path, tok, `{}`)
		if r.code != 403 || (r.body["code"] != "forbidden" && r.body["code"] != "not_platform") {
			t.Errorf("auditor %s: %d %s", key, r.code, r.raw)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no write routes checked")
	}
	for _, key := range pendingWrites {
		method, pattern, _ := strings.Cut(key, " ")
		r := send(t, handler, method, strings.TrimPrefix(pattern, httpx.BasePath), tok, `{}`)
		if registered[key] {
			// Registered now: it must be refused like any other write.
			if r.code != 403 || r.body["code"] != "forbidden" {
				t.Errorf("auditor %s: %d %s", key, r.code, r.raw)
			}
			continue
		}
		t.Logf("pending (Task 12): %s", key)
		problem(t, "pending "+key, r, 404, "not_found")
	}
}

// The runs reads: shape, paging, 404s, and each read recorded once per
// session and read (audit.searched {searched_kind: run}).
func TestVerifyRunsReads(t *testing.T) {
	clearRuns(t)
	u := auditor(t)
	tok := mint(t, u)
	var made []uuid.UUID
	for i := 0; i < audit.RunsLimit+3; i++ {
		made = append(made, seedRun(t, "intact", time.Duration(100+i)*time.Second))
	}
	r := get(t, "/platform/audit/verify/runs", tok)
	ok(t, "runs", r)
	sameKeys(t, "runs page", r.body, "runs", "next_cursor")
	page := r.body["runs"].([]any)
	if len(page) != audit.RunsLimit {
		t.Fatalf("first page %d runs", len(page))
	}
	for i, x := range page {
		run := x.(map[string]any)
		sameKeys(t, "run", run, runKeys...)
		if run["id"] != made[i].String() || run["by"] != nil || run["from_checkpoint"] != nil || run["outcome"] != nil {
			t.Errorf("run %d: %v", i, run)
		}
	}
	next, _ := r.body["next_cursor"].(string)
	if len(next) != 48 {
		t.Fatalf("next_cursor %q", next)
	}
	firstPage := len(actionsBy(t, u, "audit.searched"))
	r = get(t, "/platform/audit/verify/runs?cursor="+next, tok)
	ok(t, "runs page 2", r)
	// Paging writes nothing (spec §5.2): only the first page is a read.
	if n := len(actionsBy(t, u, "audit.searched")); n != firstPage || firstPage != 1 {
		t.Fatalf("recorded reads: %d after the first page, %d after the second", firstPage, n)
	}
	if p2 := r.body["runs"].([]any); len(p2) != 3 || r.body["next_cursor"] != nil || p2[0].(map[string]any)["id"] != made[audit.RunsLimit].String() {
		t.Fatalf("page 2: %s", r.raw)
	}
	for _, bad := range []string{"?cursor=zz", "?cursor=" + strings.ToUpper(next), "?cursor=", "?limit=5", "?cursor=" + next + "&cursor=" + next} {
		problem(t, "runs "+bad, get(t, "/platform/audit/verify/runs"+bad, tok), 422, "validation_failed")
	}
	r = get(t, "/platform/audit/verify/runs/"+made[0].String(), tok)
	ok(t, "run", r)
	sameKeys(t, "run detail", r.body, runKeys...)
	for _, bad := range []string{uuid.NewString(), "1", strings.ToUpper(made[0].String())} {
		problem(t, "run "+bad, get(t, "/platform/audit/verify/runs/"+bad, tok), 404, "not_found")
	}
	// Recorded: the list's first page, the run (its id in entity_id); a
	// repeat collapses.
	ok(t, "run again", get(t, "/platform/audit/verify/runs/"+made[0].String(), tok))
	ok(t, "runs again", get(t, "/platform/audit/verify/runs", tok))
	rows := actionsBy(t, u, "audit.searched")
	if len(rows) != 2 {
		t.Fatalf("recorded reads: %+v", rows)
	}
	for _, row := range rows {
		if row.payload["searched_kind"] != "run" || len(row.payload) != 1 {
			t.Errorf("read row: %+v", row)
		}
	}
	if rows[1].entity != "audit_run" || rows[1].entityID != made[0].String() || rows[0].entity != "" {
		t.Errorf("read rows' entities: %+v", rows)
	}
	// Clinical accounts are refused.
	h := seedHospital(t, "active")
	clin := mint(t, seedUser(t, h, "", authz.RoleClinician))
	problem(t, "clinician runs", get(t, "/platform/audit/verify/runs", clin), 403, "not_platform")
}

// TestOutcomeAuditorOnlyBrokenOnly: the outcome note is auditors' alone, on
// a broken run only; the note is stored on the run and never on the chain
// (C16).
func TestOutcomeAuditorOnlyBrokenOnly(t *testing.T) {
	clearRuns(t)
	aud := auditor(t)
	tok := mint(t, aud)
	admin := mint(t, platformAdmin(t))
	broken := seedRun(t, "broken", time.Hour)
	intact := seedRun(t, "intact", time.Minute)
	note := "Investigated: " + noteMarker + "\nRe-anchored."
	body := func(n string, v int) string {
		b, _ := json.Marshal(map[string]any{"note": n, "version": v})
		return string(b)
	}
	path := func(id string) string { return "/platform/audit/verify/runs/" + id + "/outcome" }

	problem(t, "platform admin", post(t, path(broken.String()), admin, body(note, 1)), 403, "forbidden")
	problem(t, "intact run", post(t, path(intact.String()), tok, body(note, 1)), 409, "conflict")
	problem(t, "stale version", post(t, path(broken.String()), tok, body(note, 2)), 409, "conflict")
	problem(t, "unknown run", post(t, path(uuid.NewString()), tok, body(note, 1)), 404, "not_found")
	problem(t, "not a uuid", post(t, path("5"), tok, body(note, 1)), 404, "not_found")
	for name, b := range map[string]string{
		"empty note":     body("", 1),
		"blank note":     body("  \n", 1),
		"1001 chars":     body(strings.Repeat("n", 1001), 1),
		"pseudonym":      body("patient P-ABCD12", 1),
		"format char":    body("bidi‮", 1),
		"no version":     `{"note":"x"}`,
		"version 0":      body("x", 0),
		"null note":      `{"note":null,"version":1}`,
		"unknown key":    `{"note":"x","version":1,"by":"` + uuid.NewString() + `"}`,
		"version string": `{"note":"x","version":"1"}`,
		"no body":        "",
	} {
		problem(t, name, post(t, path(broken.String()), tok, b), 422, "validation_failed")
	}
	// Nothing was written by the refusals.
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'audit.check_outcome' AND entity_id = $1`, broken.String()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("check_outcome rows after refusals: %d %v", n, err)
	}

	r := post(t, path(broken.String()), tok, body(note, 1))
	ok(t, "outcome", r)
	sameKeys(t, "outcome run", r.body, runKeys...)
	oc, _ := r.body["outcome"].(map[string]any)
	if oc == nil || oc["note"] != note || runVersion(r.body) != 2 || r.body["state"] != "broken" {
		t.Fatalf("outcome reply: %s", r.raw)
	}
	by := oc["by"].(map[string]any)
	if by["id"] != aud.id.String() || by["name"] != aud.name {
		t.Errorf("outcome by: %v", by)
	}
	// Once: a second note, at the new version, is refused.
	problem(t, "second note", post(t, path(broken.String()), tok, body("again", 2)), 409, "conflict")

	ctx := context.Background()
	var payload []byte
	var actor, entityID string
	if err := super.QueryRow(ctx, `SELECT payload, actor_id::text, entity_id FROM audit_log WHERE action = 'audit.check_outcome' AND actor_id = $1`, aud.id).
		Scan(&payload, &actor, &entityID); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal(payload, &p)
	if len(p) != 1 || p["run_id"] != broken.String() || actor != aud.id.String() || entityID != broken.String() {
		t.Errorf("check_outcome row: %s by %s on %s", payload, actor, entityID)
	}
	// C16: the note is in no audit payload.
	if err := super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE payload::text LIKE '%' || $1 || '%'
		OR coalesce(entity_id, '') LIKE '%' || $1 || '%' OR coalesce(request_id, '') LIKE '%' || $1 || '%'`, noteMarker).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the note on the chain: %d %v", n, err)
	}
	// The run itself shows it.
	got := get(t, "/platform/audit/verify/runs/"+broken.String(), tok)
	ok(t, "run", got)
	if got.body["outcome"].(map[string]any)["note"] != note {
		t.Errorf("run's outcome: %s", got.raw)
	}
}

// TestOpenFailureClears: open_failure is the latest broken run until a
// later run is intact and the broken run has its note, both.
func TestOpenFailureClears(t *testing.T) {
	clearRuns(t)
	u := auditor(t)
	tok := mint(t, u)
	status := func() map[string]any {
		t.Helper()
		r := get(t, "/platform/audit/verify/status", tok)
		ok(t, "status", r)
		sameKeys(t, "status", r.body, "latest", "running", "open_failure", "security_contact", "oldest_kept_at")
		return r.body
	}
	s := status()
	if s["latest"] != nil || s["running"] != nil || s["open_failure"] != nil {
		t.Fatalf("no runs: %v", s)
	}
	c := s["security_contact"].(map[string]any)
	if c["name"] != testContact.Name || c["email"] != testContact.Email || len(c) != 2 {
		t.Errorf("security_contact: %v", c)
	}
	if _, err := time.Parse(time.RFC3339Nano, s["oldest_kept_at"].(string)); err != nil {
		t.Errorf("oldest_kept_at: %v", s["oldest_kept_at"])
	}
	broken := seedRun(t, "broken", 2*time.Hour)
	openID := func() any {
		of, _ := status()["open_failure"].(map[string]any)
		if of == nil {
			return nil
		}
		return of["id"]
	}
	if got := openID(); got != broken.String() {
		t.Fatalf("broken: open_failure %v", got)
	}
	// The note alone doesn't close it.
	ok(t, "note", post(t, "/platform/audit/verify/runs/"+broken.String()+"/outcome", tok, `{"note":"Synthetic finding.","version":1}`))
	if got := openID(); got != broken.String() {
		t.Fatalf("note without a later intact run: open_failure %v", got)
	}
	// An intact run that started before the broken one doesn't either.
	seedRun(t, "intact", 3*time.Hour)
	if got := openID(); got != broken.String() {
		t.Fatalf("earlier intact run: open_failure %v", got)
	}
	// A later intact run with the note: closed.
	later := seedRun(t, "intact", time.Hour)
	s = status()
	if s["open_failure"] != nil || s["latest"].(map[string]any)["id"] != later.String() {
		t.Fatalf("closed: %v", s)
	}
	// A new broken run opens a new failure; an intact run after it
	// without its note leaves it open.
	again := seedRun(t, "broken", 30*time.Minute)
	seedRun(t, "intact", 10*time.Minute)
	if got := openID(); got != again.String() {
		t.Fatalf("second failure: open_failure %v", got)
	}
	// running is the running run.
	r := post(t, "/platform/audit/verify", tok, "")
	ok(t, "start", r)
	if s = status(); s["running"].(map[string]any)["id"] != r.body["run_id"] || s["running"].(map[string]any)["state"] != "running" {
		t.Errorf("running: %v", s["running"])
	}
	// The status read is recorded, once per session per 5 minutes.
	n := 0
	for _, row := range actionsBy(t, u, "audit.searched") {
		if row.payload["searched_kind"] == "run" && row.entity == "" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("status reads recorded: %d", n)
	}
	problem(t, "status with a query", get(t, "/platform/audit/verify/status?x=1", tok), 422, "validation_failed")
}

// Without a configured security contact the status fails closed.
func TestStatusWithoutContactNotReady(t *testing.T) {
	mux := authz.NewMux(guard)
	server.Register(mux, server.Deps{Auth: authSvc, Pool: app})
	h := mux.Handler([]string{serviceKey})
	r := send(t, h, http.MethodGet, "/platform/audit/verify/status", mint(t, auditor(t)), "")
	problem(t, "no contact", r, 503, "not_ready")
	for _, c := range []struct{ name, email string }{
		{"", "a@b.example"}, {"Desk", ""}, {"Desk", "not an email"}, {"Desk", "Desk <a@b.example>"}, {" Desk", "a@b.example"},
	} {
		mux := authz.NewMux(guard)
		server.Register(mux, server.Deps{Auth: authSvc, Pool: app,
			SecurityContact: platform.SecurityContact{Name: c.name, Email: c.email}})
		r := send(t, mux.Handler([]string{serviceKey}), http.MethodGet, "/platform/audit/verify/status", mint(t, auditor(t)), "")
		problem(t, "contact "+c.name+"/"+c.email, r, 503, "not_ready")
	}
}

// TestUndisplayableReportOneRow: one report is one entry, with the count
// and the first and last ids, never the list.
func TestUndisplayableReportOneRow(t *testing.T) {
	u := auditor(t)
	tok := mint(t, u)
	ctx := context.Background()
	rows := func(who user) []map[string]any {
		t.Helper()
		rs, err := super.Query(ctx, `SELECT payload FROM audit_log WHERE action = 'audit.undisplayable_reported' AND actor_id = $1 ORDER BY id`, who.id)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out []map[string]any
		for rs.Next() {
			var raw []byte
			if err := rs.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var p map[string]any
			_ = json.Unmarshal(raw, &p)
			out = append(out, p)
		}
		return out
	}
	r := post(t, "/platform/audit/undisplayable", tok, `{"ids":["50","3","9","3"]}`)
	if r.code != 204 || r.raw != "" {
		t.Fatalf("report: %d %s", r.code, r.raw)
	}
	got := rows(u)
	if len(got) != 1 || len(got[0]) != 3 || got[0]["count"] != float64(3) || got[0]["first_id"] != "3" || got[0]["last_id"] != "50" {
		t.Fatalf("rows: %v", got)
	}
	var many []string
	for i := 1; i <= 51; i++ {
		many = append(many, strconv.Itoa(i))
	}
	fifty, _ := json.Marshal(map[string]any{"ids": many[:50]})
	fiftyOne, _ := json.Marshal(map[string]any{"ids": many})
	for name, b := range map[string]string{
		"no ids":     `{"ids":[]}`,
		"51 ids":     string(fiftyOne),
		"pseudonym":  `{"ids":["P-ABCD12"]}`,
		"leading 0":  `{"ids":["01"]}`,
		"zero":       `{"ids":["0"]}`,
		"a number":   `{"ids":[3]}`,
		"extra key":  `{"ids":["3"],"note":"x"}`,
		"no body":    "",
		"null":       `{"ids":null}`,
		"not a list": `{"ids":"3"}`,
	} {
		problem(t, name, post(t, "/platform/audit/undisplayable", tok, b), 422, "validation_failed")
	}
	if got := rows(u); len(got) != 1 {
		t.Fatalf("refusals wrote rows: %v", got)
	}
	// 50 is allowed, and platform admins report too.
	a := platformAdmin(t)
	if r := post(t, "/platform/audit/undisplayable", mint(t, a), string(fifty)); r.code != 204 {
		t.Fatalf("50 ids: %d %s", r.code, r.raw)
	}
	if got := rows(a); len(got) != 1 || got[0]["count"] != float64(50) || got[0]["first_id"] != "1" || got[0]["last_id"] != "50" {
		t.Fatalf("admin rows: %v", got)
	}
	// The entry maps to mhai-web's kind.
	l := get(t, "/platform/audit?type=audit", tok)
	ok(t, "list", l)
	found := slices.ContainsFunc(events(t, l), func(e map[string]any) bool { return e["kind"] == "audit_undisplayable_reported" })
	if !found {
		t.Error("the report is not in the list as audit_undisplayable_reported")
	}
}
