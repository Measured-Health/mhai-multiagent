//go:build integration

package documents

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// The clinical upload routes, migration 00051's definers and core_documents
// on a migrated database, through the handlers with a verified principal
// (the guard's checks are authz's). The documents service is a fake that
// answers presign_upload. Synthetic hospitals, people and documents only.

// policyMark stands for a policy field; built at run time.
var policyMark = strings.Join([]string{"synthetic", "policy", "mark"}, "-")

type presigner struct {
	mu       sync.Mutex
	outcome  string
	hospital []uuid.UUID
}

func (p *presigner) Request(_ context.Context, h uuid.UUID, _, _ string, _, resp proto.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hospital = append(p.hospital, h)
	if p.outcome != "" {
		proto.Merge(resp, &docspb.PresignUploadReply{Outcome: p.outcome})
		return nil
	}
	proto.Merge(resp, &docspb.PresignUploadReply{Outcome: "ok", Url: "https://store.example.test/b",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Fields:    []*docspb.FormField{{Name: "key", Value: "quarantine/x"}, {Name: "x-amz-meta-synthetic", Value: policyMark}}})
	return nil
}

type announcer struct {
	mu   sync.Mutex
	docs []uuid.UUID
}

func (a *announcer) Uploaded(_ context.Context, _, d uuid.UUID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.docs = append(a.docs, d)
	return nil
}

func (a *announcer) n() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.docs)
}

type world struct {
	super, app   *pgxpool.Pool
	hA, hB       uuid.UUID
	clin, clin2  uuid.UUID
	admin, other uuid.UUID // admin of A; a clinician of B
	patientA     uuid.UUID
	patientB     uuid.UUID
	ctl          *presigner
	ann          *announcer
	d            Deps
	logs         *bytes.Buffer
}

func exec(t *testing.T, p *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	app, err := testutil.AppLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	w := &world{super: super, app: app, hA: uuid.New(), hB: uuid.New(), ctl: &presigner{}, ann: &announcer{}, logs: &bytes.Buffer{}}
	for _, h := range []uuid.UUID{w.hA, w.hB} {
		exec(t, super, `INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, 'Synthetic Hospital', $2, 'active', 'UTC')`,
			h, "D"+h.String()[:8])
	}
	person := func(h uuid.UUID, role string) uuid.UUID {
		id := uuid.New()
		exec(t, super, `INSERT INTO users (id, hospital_id, email, display_name) VALUES ($1, $2, $3, 'Synthetic Person')`,
			id, h, "p-"+id.String()[:8]+"@hospital.example.test")
		exec(t, super, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, role)
		return id
	}
	w.clin, w.clin2, w.admin, w.other = person(w.hA, "clinician"), person(w.hA, "clinician"), person(w.hA, "hospital_admin"), person(w.hB, "clinician")
	for _, p := range []struct {
		h  uuid.UUID
		id *uuid.UUID
	}{{w.hA, &w.patientA}, {w.hB, &w.patientB}} {
		if err := super.QueryRow(context.Background(), `INSERT INTO emr.patients (hospital_id, display_name, mrn, sex)
			VALUES ($1, 'Synthetic Patient', 'SYN-1', 'female') RETURNING id`, p.h).Scan(p.id); err != nil {
			t.Fatal(err)
		}
	}
	w.d = Deps{Pool: app, On: true, Ctl: w.ctl, Announce: w.ann,
		Log: slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return w
}

func (w *world) do(t *testing.T, h, u uuid.UUID, method, path, body string) (int, []byte) {
	t.Helper()
	m := http.NewServeMux()
	for _, r := range []struct {
		p string
		h http.HandlerFunc
	}{{"POST /documents", w.d.start}, {"GET /documents/{id}", w.d.get}, {"POST /documents/{id}/uploaded", w.d.uploaded},
		{"POST /documents/{id}/cancel", w.d.cancel}} {
		meth, p, _ := strings.Cut(r.p, " ")
		m.Handle(meth+" "+httpx.BasePath+p, r.h)
	}
	r := httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader(body))
	r = r.WithContext(authz.WithPrincipal(r.Context(), authz.Principal{UserID: u, HospitalID: h,
		Roles: []string{authz.RoleClinician}, SessionID: uuid.NewString()}))
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, r)
	return rec.Code, rec.Body.Bytes()
}

func (w *world) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := w.super.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const pdf = `{"purpose":"clinical","content_type":"application/pdf","size_bytes":2048}`

func (w *world) start(t *testing.T, h, u uuid.UUID, body string) (int, StartOut, []byte) {
	t.Helper()
	c, b := w.do(t, h, u, http.MethodPost, "/documents", body)
	var out StartOut
	_ = json.Unmarshal(b, &out)
	return c, out, b
}

func problem(b []byte) string {
	var p struct{ Code string }
	_ = json.Unmarshal(b, &p)
	return p.Code
}

// The upload from start to the scan's audit entry: the row is the caller's
// in the caller's hospital, the policy goes to the caller only (never a
// log), uploaded is announced once (a replay changes nothing), and
// core_documents writes document.ready once from the row.
func TestUploadFlow(t *testing.T) {
	w := newWorld(t)
	c, out, b := w.start(t, w.hA, w.clin, pdf)
	if c != http.StatusCreated || out.Document.Status != "uploading" || out.Document.Purpose != "clinical" ||
		out.Upload.URL == "" || len(out.Upload.Fields) != 2 || out.Document.Version != 1 {
		t.Fatalf("start: %d %s", c, b)
	}
	if w.ctl.hospital[0] != w.hA || strings.Contains(w.logs.String(), policyMark) {
		t.Fatal("presigned for another hospital, or the policy was logged")
	}
	id := out.Document.ID
	if w.count(t, `SELECT count(*) FROM documents WHERE id = $1 AND hospital_id = $2 AND uploaded_by = $3 AND via = 'user'`,
		id, w.hA, w.clin) != 1 ||
		w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.upload_started' AND entity_id = $1 AND actor_id = $2
			AND payload ->> 'purpose' = 'clinical'`, id, w.clin) != 1 {
		t.Fatal("the row or its entry")
	}
	if c, b := w.do(t, w.hA, w.clin, http.MethodGet, "/documents/"+id, ""); c != http.StatusOK || !strings.Contains(string(b), `"uploading"`) {
		t.Fatalf("get: %d %s", c, b)
	}
	// Replay of uploaded: one change, one announcement, one entry.
	for i := 0; i < 3; i++ {
		if c, b := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id+"/uploaded", ""); c != http.StatusAccepted ||
			!strings.Contains(string(b), `"uploaded"`) {
			t.Fatalf("uploaded %d: %d %s", i, c, b)
		}
	}
	if w.ann.n() != 1 || w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.uploaded' AND entity_id = $1`, id) != 1 {
		t.Fatalf("announced %d", w.ann.n())
	}
	// The scan's outcome (as the documents service's definers leave it).
	exec(t, w.super, `UPDATE documents SET status = 'ready', pages = 3, sha256 = $2, scanned_at = now(), scan_started_at = now()
		WHERE id = $1`, id, bytes.Repeat([]byte{1}, 32))
	cons := &Consumer{Pool: w.app}
	doc := uuid.MustParse(id)
	for i := 0; i < 2; i++ {
		if err := cons.Handle(context.Background(), w.hA, doc, "ready"); err != nil {
			t.Fatal(err)
		}
	}
	if w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.ready' AND entity_id = $1 AND actor_id IS NULL
		AND (payload ->> 'pages')::int = 3`, id) != 1 || cons.metrics().Duplicates.Load() != 1 {
		t.Fatal("document.ready not written exactly once")
	}
	// A message disagreeing with the row: the row is recorded.
	q := w.startUploaded(t, w.hA, w.clin)
	exec(t, w.super, `UPDATE documents SET status = 'quarantined', reason = 'malware', scanned_at = now(), scan_started_at = now()
		WHERE id = $1`, q)
	if err := cons.Handle(context.Background(), w.hA, q, "ready"); err != nil {
		t.Fatal(err)
	}
	if w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.quarantined' AND entity_id = $1
		AND payload ->> 'reason' = 'malware'`, q.String()) != 1 || cons.metrics().Mismatches.Load() != 1 {
		t.Fatal("quarantined not recorded from the row")
	}
}

func (w *world) startUploaded(t *testing.T, h, u uuid.UUID) uuid.UUID {
	t.Helper()
	c, out, b := w.start(t, h, u, pdf)
	if c != http.StatusCreated {
		t.Fatalf("start: %d %s", c, b)
	}
	if c, b := w.do(t, h, u, http.MethodPost, "/documents/"+out.Document.ID+"/uploaded", ""); c != http.StatusAccepted {
		t.Fatalf("uploaded: %d %s", c, b)
	}
	return uuid.MustParse(out.Document.ID)
}

// DS1 and the uploader rule: another hospital's document, and another
// person's, are 404 on every route, and nothing changes; core_documents
// under another hospital's subject finds nothing and writes nothing.
func TestCrossHospitalAndCrossPerson(t *testing.T) {
	w := newWorld(t)
	_, out, _ := w.start(t, w.hA, w.clin, pdf)
	id := out.Document.ID
	for _, who := range []struct{ h, u uuid.UUID }{{w.hB, w.other}, {w.hA, w.clin2}, {w.hA, w.admin}} {
		for _, r := range [][3]string{{"GET", "/documents/" + id, ""}, {"POST", "/documents/" + id + "/uploaded", ""},
			{"POST", "/documents/" + id + "/cancel", `{"version":1}`}} {
			if c, b := w.do(t, who.h, who.u, r[0], r[1], r[2]); c != http.StatusNotFound || problem(b) != "not_found" {
				t.Errorf("%s %s by %v: %d %s", r[0], r[1], who.u, c, b)
			}
		}
	}
	if w.count(t, `SELECT count(*) FROM documents WHERE id = $1 AND status = 'uploading' AND version = 1`, id) != 1 || w.ann.n() != 0 {
		t.Fatal("another's call changed the row")
	}
	// A patient of another hospital is 404, the same as none; one of the
	// caller's hospital is the hint.
	if c, _, b := w.start(t, w.hA, w.clin, `{"purpose":"clinical","content_type":"image/png","size_bytes":10,"patient_id":"`+
		w.patientB.String()+`"}`); c != http.StatusNotFound {
		t.Fatalf("another hospital's patient: %d %s", c, b)
	}
	if c, _, b := w.start(t, w.hA, w.clin, `{"purpose":"clinical","content_type":"image/tiff","size_bytes":10,"patient_id":"`+
		w.patientA.String()+`"}`); c != http.StatusCreated {
		t.Fatalf("own patient: %d %s", c, b)
	}
	exec(t, w.super, `UPDATE documents SET status = 'ready', pages = 1, scanned_at = now(), scan_started_at = now() WHERE id = $1`, id)
	cons := &Consumer{Pool: w.app}
	if err := cons.Handle(context.Background(), w.hB, uuid.MustParse(id), "ready"); err != nil {
		t.Fatal(err)
	}
	if w.count(t, `SELECT count(*) FROM audit_log WHERE action LIKE 'document.ready'`) != 0 || cons.metrics().Mismatches.Load() != 1 {
		t.Fatal("core_documents wrote under another hospital's subject")
	}
	// A message for a row whose scan hasn't finished writes nothing.
	_, out2, _ := w.start(t, w.hA, w.clin, pdf)
	if err := cons.Handle(context.Background(), w.hA, uuid.MustParse(out2.Document.ID), "ready"); err != nil {
		t.Fatal(err)
	}
	if w.count(t, `SELECT count(*) FROM audit_log WHERE action LIKE 'document.ready'`) != 0 {
		t.Fatal("an entry for an unscanned row")
	}
}

// The limits, reserved then checked: 30 starts an hour per person, even
// when they all arrive at once; the queue cap is 503 busy; the documents
// service not answering is 503 unavailable and the row is cancelled.
func TestUploadLimits(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	codes := make(chan int, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, _ := w.start(t, w.hA, w.clin, pdf)
			codes <- c
		}()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusCreated] != 30 || got[http.StatusTooManyRequests] != 10 {
		t.Fatalf("parallel starts: %v", got)
	}
	// Another person isn't limited by the first.
	if c, _, b := w.start(t, w.hA, w.clin2, pdf); c != http.StatusCreated {
		t.Fatalf("another person: %d %s", c, b)
	}
	// The queue cap: 20 of the hospital's documents waiting for extraction.
	for i := 0; i < 20; i++ {
		exec(t, w.super, `INSERT INTO documents (hospital_id, purpose, via, uploaded_by, mime_type, size_bytes, status, pages, scanned_at)
			VALUES ($1, 'clinical', 'user', $2, 'application/pdf', 10, 'queued', 1, now())`, w.hA, w.admin)
	}
	if c, _, b := w.start(t, w.hA, w.clin2, pdf); c != http.StatusServiceUnavailable || problem(b) != "busy" {
		t.Fatalf("queue cap: %d %s", c, b)
	}
	if c, _, b := w.start(t, w.hB, w.other, pdf); c != http.StatusCreated {
		t.Fatalf("another hospital isn't held by the cap: %d %s", c, b)
	}
	// The documents service not answering.
	w.ctl.outcome = "unavailable"
	before := w.count(t, `SELECT count(*) FROM documents WHERE hospital_id = $1 AND status = 'cancelled'`, w.hB)
	if c, _, b := w.start(t, w.hB, w.other, pdf); c != http.StatusServiceUnavailable || problem(b) != "unavailable" {
		t.Fatalf("no policy: %d %s", c, b)
	}
	if w.count(t, `SELECT count(*) FROM documents WHERE hospital_id = $1 AND status = 'cancelled'`, w.hB) != before+1 ||
		w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.cancelled' AND payload ->> 'reason' = 'unavailable'`) != 1 {
		t.Fatal("the row without a policy wasn't cancelled")
	}
}

// Cancel: at the version seen, before review; a scan that holds the row
// when it is cancelled stores nothing (its ready is refused); a cancelled
// document can't be marked uploaded; a file never posted ends the
// document after 30 minutes.
func TestCancelAndRaces(t *testing.T) {
	w := newWorld(t)
	id := w.startUploaded(t, w.hA, w.clin)
	ctx := context.Background()
	// The scan claims the row (as the documents service does).
	lease := uuid.New()
	asDocs := func(fn func(pgx.Tx) error) {
		t.Helper()
		tx, err := w.super.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_documents`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, w.hA.String()); err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	asDocs(func(tx pgx.Tx) error {
		var outcome string
		if err := tx.QueryRow(ctx, `SELECT outcome FROM documents_scan_claim($1, $2)`, id, lease).Scan(&outcome); err != nil {
			return err
		}
		if outcome != "claimed" {
			t.Fatalf("claim: %s", outcome)
		}
		return nil
	})
	var version int
	if err := w.super.QueryRow(ctx, `SELECT version FROM documents WHERE id = $1`, id).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if c, b := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id.String()+"/cancel", `{"version":1}`); c != http.StatusConflict {
		t.Fatalf("a stale version: %d %s", c, b)
	}
	c, b := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id.String()+"/cancel", `{"version":`+itoa(version)+`}`)
	if c != http.StatusOK || !strings.Contains(string(b), `"cancelled"`) {
		t.Fatalf("cancel while scanning: %d %s", c, b)
	}
	asDocs(func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT documents_scan_ready($1, $2, $3, 1, ARRAY['original', 'page'], ARRAY[0, 1],
			ARRAY[$4::bytea, $4::bytea], ARRAY[1, 1], ARRAY[$3::bytea, $3::bytea], ARRAY[10::bigint, 10::bigint])`,
			id, lease, bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 60)).Scan(&ok); err != nil {
			return err
		}
		if ok {
			t.Fatal("a cancelled document was made ready")
		}
		return nil
	})
	if w.count(t, `SELECT count(*) FROM document_objects WHERE document_id = $1`, id) != 0 ||
		w.count(t, `SELECT count(*) FROM documents WHERE id = $1 AND status = 'cancelled' AND scan_lease_id IS NULL`, id) != 1 {
		t.Fatal("the cancelled row holds objects or a lease")
	}
	if c, _ := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id.String()+"/cancel", `{"version":`+itoa(version+1)+`}`); c != http.StatusConflict {
		t.Fatalf("cancel twice: %d", c)
	}
	if c, _ := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id.String()+"/uploaded", ""); c != http.StatusConflict {
		t.Fatalf("uploaded after cancel: %d", c)
	}
	// Parallel cancels at one version: exactly one wins.
	id2 := w.startUploaded(t, w.hA, w.clin)
	var wg sync.WaitGroup
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id2.String()+"/cancel", `{"version":2}`)
			results <- c
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for c := range results {
		if c == http.StatusOK {
			won++
		} else if c != http.StatusConflict {
			t.Errorf("parallel cancel: %d", c)
		}
	}
	if won != 1 || w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.cancelled' AND entity_id = $1`, id2.String()) != 1 {
		t.Fatalf("parallel cancels won %d", won)
	}
	// In review it can't be cancelled.
	id3 := w.startUploaded(t, w.hA, w.clin)
	exec(t, w.super, `UPDATE documents SET status = 'in_review', pages = 1, scanned_at = now(), scan_started_at = now() WHERE id = $1`, id3)
	if c, _ := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+id3.String()+"/cancel", `{"version":2}`); c != http.StatusConflict {
		t.Fatalf("cancel in review: %d", c)
	}
	// Never posted: 30 minutes after the start the document ends.
	_, out, _ := w.start(t, w.hA, w.clin, pdf)
	exec(t, w.super, `UPDATE documents SET received_at = now() - interval '31 minutes' WHERE id = $1`, out.Document.ID)
	before := w.ann.n()
	if c, b := w.do(t, w.hA, w.clin, http.MethodPost, "/documents/"+out.Document.ID+"/uploaded", ""); c != http.StatusConflict {
		t.Fatalf("expired: %d %s", c, b)
	}
	if w.ann.n() != before || w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.cancelled' AND entity_id = $1
		AND payload ->> 'reason' = 'expired'`, out.Document.ID) != 1 {
		t.Fatal("an expired upload was announced, or its end not recorded")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The gateway's definers refuse what the routes never send: another
// purpose, a type or size outside §4.1, a letter naming a patient, no user.
func TestStartDefinerRefuses(t *testing.T) {
	w := newWorld(t)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.clin, HospitalID: w.hA, Roles: []string{"clinician"}})
	for name, args := range map[string][]any{
		"archive":        {"archive", "application/pdf", int64(10), nil},
		"html":           {"clinical", "text/html", int64(10), nil},
		"too large":      {"clinical", "application/pdf", int64(26214401), nil},
		"letter tiff":    {"clearance", "image/tiff", int64(10), nil},
		"letter too big": {"clearance", "application/pdf", int64(10485761), nil},
		"letter patient": {"clearance", "application/pdf", int64(10), w.patientA},
		"empty":          {"clinical", "application/pdf", int64(0), nil},
	} {
		err := authz.TxUser(ctx, w.app, func(tx pgx.Tx) error {
			var outcome string
			var id *uuid.UUID
			return tx.QueryRow(ctx, `SELECT outcome, document_id FROM documents_upload_start($1, $2, $3, $4)`, args...).
				Scan(&outcome, &id)
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if w.count(t, `SELECT count(*) FROM documents`) != 0 {
		t.Fatal("a refused start made a row")
	}
	// Without a user the definers refuse outright.
	err := authz.Tx(ctx, w.app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT documents_upload_start('clinical', 'application/pdf', 10, NULL)`)
		return err
	})
	if err == nil {
		t.Fatal("a start without a user")
	}
}
