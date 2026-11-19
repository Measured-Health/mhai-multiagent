//go:build integration

package letters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/documents"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

// The live letter (documents PR 5) on a migrated database: the documents
// service is a fake that answers presign_upload and page_image, and the
// scan's outcome is set on the documents row as the documents service's
// definers would, then read by core_documents (documents.Consumer with
// Scanned). Synthetic letters, people and pages only.

const viewerMark = "Synthetic Person"

type fakeDocs struct {
	mu       sync.Mutex
	presign  string // the presign outcome; "" is ok
	page     string // the page outcome; "" is ok
	err      error
	calls    []string
	hospital []uuid.UUID
	viewer   string
}

func (f *fakeDocs) Request(_ context.Context, h uuid.UUID, _, op string, req, resp proto.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
	f.hospital = append(f.hospital, h)
	if f.err != nil {
		return f.err
	}
	switch op {
	case "presign_upload":
		out := &docspb.PresignUploadReply{Outcome: "ok", Url: "https://store.example.test/mhai-documents-test",
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Fields:    []*docspb.FormField{{Name: "key", Value: "quarantine/synthetic"}, {Name: "policy", Value: "synthetic-policy"}}}
		if f.presign != "" {
			out = &docspb.PresignUploadReply{Outcome: f.presign}
		}
		proto.Merge(resp, out)
	case "page_image":
		f.viewer = req.(*docspb.PageImageRequest).GetViewerName()
		out := &docspb.PageImageReply{Outcome: "ok", Jpeg: []byte("\xff\xd8\xffsynthetic jpeg"), Width: 10, Height: 10}
		if f.page != "" {
			out = &docspb.PageImageReply{Outcome: f.page}
		}
		proto.Merge(resp, out)
	}
	return nil
}

type fakeAnnounce struct {
	mu   sync.Mutex
	docs []uuid.UUID
}

func (a *fakeAnnounce) Uploaded(_ context.Context, _, d uuid.UUID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.docs = append(a.docs, d)
	return nil
}

func (a *fakeAnnounce) n() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.docs)
}

// liveEnv is newEnv's hospital with the live routes on.
func liveEnv(t *testing.T, demo bool) (*env, *fakeDocs, *fakeAnnounce) {
	t.Helper()
	super, app := pools(t)
	return liveEnvIn(t, super, app, demo)
}

// liveEnvIn is liveEnv's hospital in an existing database.
func liveEnvIn(t *testing.T, super, app *pgxpool.Pool, demo bool) (*env, *fakeDocs, *fakeAnnounce) {
	t.Helper()
	e := newEnv(t, super, app, demo, "Africa/Lagos")
	docs, ann := &fakeDocs{}, &fakeAnnounce{}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := Deps{Pool: app, Log: log, Live: true, Ctl: docs, Announce: ann, stepUpCheck: func(_ *http.Request, mfa, _ string) error {
		if mfa == stepCode {
			return nil
		}
		return errors.New("wrong code")
	}}
	e.mux = http.NewServeMux()
	for _, r := range []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"POST /hospital/clearances", d.upload},
		{"POST /hospital/clearances/live", d.live},
		{"GET /hospital/clearances/{id}", d.get},
		{"POST /hospital/clearances/{id}/uploaded", d.uploaded},
		{"GET /hospital/clearances/{id}/pages/{n}", d.page},
		{"POST /hospital/clearances/{id}/revoke", d.revoke},
	} {
		method, path, _ := strings.Cut(r.pattern, " ")
		e.mux.Handle(method+" "+httpx.BasePath+path, r.h)
	}
	return e, docs, ann
}

func (e *env) liveBody(staff ...uuid.UUID) map[string]any {
	ids := []string{}
	for _, s := range staff {
		ids = append(ids, s.String())
	}
	return map[string]any{"signed_by": signerMark, "signer_title": "Synthetic Director",
		"letter_date": e.today.Format(time.DateOnly), "end_date": e.today.AddDate(0, 6, 0).Format(time.DateOnly),
		"covered": ids, "content_type": "application/pdf", "size_bytes": 4096, "mfa_code": stepCode}
}

func (e *env) startLive(t *testing.T, body map[string]any) (int, LiveOut, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances/live", "application/json", raw, true)
	var out LiveOut
	_ = json.Unmarshal(b, &out)
	return c, out, b
}

func (e *env) letterDocument(t *testing.T, id string) uuid.UUID {
	t.Helper()
	var d uuid.UUID
	if err := e.super.QueryRow(context.Background(), `SELECT document_id FROM clearances WHERE id = $1`, id).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// scanned sets a document's scan outcome as the documents service's
// definers leave it, then core_documents reads it.
func (e *env) scanned(t *testing.T, doc uuid.UUID, status, reason string, pages int) {
	t.Helper()
	var r, p any
	if reason != "" {
		r = reason
	}
	if pages > 0 {
		p = pages
	}
	sum := bytes.Repeat([]byte{7}, 32)
	run(t, e.super, `UPDATE documents SET status = $2, reason = $3, pages = $4, sha256 = $5, scanned_at = now(),
		scan_started_at = now() WHERE id = $1`, doc, status, r, p, sum)
	c := &documents.Consumer{Pool: e.app, Letter: Scanned}
	outcome := status
	if status == "ready" {
		outcome = "ready"
	}
	if err := c.Handle(context.Background(), e.h, doc, outcome); err != nil {
		t.Fatalf("core_documents: %v", err)
	}
}

// The live letter end to end: made checking with a POST policy for its own
// document, uploaded once (announced once, a replay changes nothing), then
// the scan's outcome makes it waiting with the row's pages; its page is the
// documents service's, stamped for the viewer, and the view is audited.
func TestLiveLetterEndToEnd(t *testing.T) {
	e, docs, ann := liveEnv(t, false)
	c, out, b := e.startLive(t, e.liveBody(e.analyst))
	if c != http.StatusCreated || out.Clearance.Status != "checking" || out.Upload.URL == "" || len(out.Upload.Fields) != 2 {
		t.Fatalf("live: %d %s", c, b)
	}
	if strings.Contains(e.logs.String(), "synthetic-policy") || strings.Contains(e.logs.String(), signerMark) {
		t.Fatal("the policy or the signer reached a log")
	}
	doc := e.letterDocument(t, out.Clearance.ID)
	if n := e.count(t, `SELECT count(*) FROM documents WHERE id = $1 AND purpose = 'clearance' AND status = 'uploading'
		AND uploaded_by = $2 AND hospital_id = $3`, doc, e.admin, e.h); n != 1 {
		t.Fatal("no uploading clearance document for the letter")
	}
	if docs.hospital[0] != e.h {
		t.Fatalf("presigned for %v", docs.hospital)
	}
	for i := 0; i < 2; i++ {
		c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances/"+out.Clearance.ID+"/uploaded", "", nil, true)
		if c != http.StatusAccepted {
			t.Fatalf("uploaded %d: %d %s", i, c, b)
		}
	}
	if ann.n() != 1 || e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.uploaded' AND entity_id = $1`, doc.String()) != 1 {
		t.Fatalf("announced %d", ann.n())
	}
	// Pages before the scan: not ready.
	if c, b := e.get(t, "/hospital/clearances/"+out.Clearance.ID+"/pages/1"); c != http.StatusServiceUnavailable || code(b) != "not_ready" {
		t.Fatalf("page before the scan: %d %s", c, b)
	}
	e.scanned(t, doc, "ready", "", 2)
	_, l := e.getLetter(t, out.Clearance.ID)
	if l.Status != "waiting" || l.Pages != 2 {
		t.Fatalf("after the scan: %+v", l)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE (action = 'clearance.ready' AND entity_id = $1)
		OR (action = 'document.ready' AND entity_id = $2 AND actor_id IS NULL)`, out.Clearance.ID, doc.String()); n != 2 {
		t.Fatalf("entries after the scan: %d", n)
	}
	// A redelivered outcome writes nothing more.
	if err := (&documents.Consumer{Pool: e.app, Letter: Scanned}).Handle(context.Background(), e.h, doc, "ready"); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action IN ('clearance.ready', 'document.ready')
		AND entity_id IN ($1, $2)`, out.Clearance.ID, doc.String()); n != 2 {
		t.Fatalf("a redelivery wrote again: %d", n)
	}
	// The stamped page: JPEG, the viewer's name sent for the stamp, the view
	// audited; the name never logged.
	c, h, body := e.do(t, e.admin, http.MethodGet, "/hospital/clearances/"+out.Clearance.ID+"/pages/2", "", nil, false)
	if c != http.StatusOK || h.Get("Content-Type") != "image/jpeg" || h.Get("Cache-Control") != "no-store" ||
		h.Get("X-Content-Type-Options") != "nosniff" || !bytes.HasPrefix(body, []byte("\xff\xd8\xff")) {
		t.Fatalf("page: %d %v", c, h)
	}
	if docs.viewer != viewerMark || strings.Contains(e.logs.String(), viewerMark) {
		t.Fatalf("viewer %q (or logged)", docs.viewer)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.viewed' AND entity_id = $1 AND actor_id = $2`,
		out.Clearance.ID, e.admin); n != 1 {
		t.Fatalf("views audited: %d", n)
	}
	// The documents service down: 503 unavailable, and no view recorded.
	docs.page = "unavailable"
	if c, b := e.get(t, "/hospital/clearances/"+out.Clearance.ID+"/pages/1"); c != http.StatusServiceUnavailable || code(b) != "unavailable" {
		t.Fatalf("page unavailable: %d %s", c, b)
	}
	docs.page = "not_found"
	if c, _ := e.get(t, "/hospital/clearances/"+out.Clearance.ID+"/pages/1"); c != http.StatusNotFound {
		t.Fatalf("page not found: %d", c)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.viewed' AND entity_id = $1`, out.Clearance.ID); n != 1 {
		t.Fatalf("a failed view was recorded: %d", n)
	}
	if c, _ := e.get(t, "/hospital/clearances/"+out.Clearance.ID+"/pages/3"); c != http.StatusNotFound {
		t.Fatalf("a page past the last: %d", c)
	}
}

func (e *env) getLetter(t *testing.T, id string) (int, ClearanceOut) {
	t.Helper()
	c, b := e.get(t, "/hospital/clearances/"+id)
	var l ClearanceOut
	_ = json.Unmarshal(b, &l)
	return c, l
}

// The scan's refusals refuse the letter, with the letter's reason (§17
// departure 38): malware is failed_safety_check, a scan that never ran is
// unreadable.
func TestLiveLetterRefused(t *testing.T) {
	e, _, _ := liveEnv(t, false)
	for _, tc := range []struct{ status, reason, want string }{
		{"quarantined", "malware", "failed_safety_check"},
		{"quarantined", "unsupported_type", "file_type"},
		{"quarantined", "too_large", "file_too_large"},
		{"quarantined", "too_many_pages", "too_many_pages"},
		{"quarantined", "password_protected", "password_protected"},
		{"quarantined", "too_many_pixels", "unreadable"},
		{"failed", "scan_unavailable", "unreadable"},
	} {
		c, out, b := e.startLive(t, e.liveBody(e.analyst))
		if c != http.StatusCreated {
			t.Fatalf("live: %d %s", c, b)
		}
		doc := e.letterDocument(t, out.Clearance.ID)
		if c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances/"+out.Clearance.ID+"/uploaded", "", nil, true); c != http.StatusAccepted {
			t.Fatalf("uploaded: %d %s", c, b)
		}
		e.scanned(t, doc, tc.status, tc.reason, 0)
		_, l := e.getLetter(t, out.Clearance.ID)
		if l.Status != "refused" || l.RefusedReason == nil || *l.RefusedReason != tc.want || l.Pages != 0 {
			t.Errorf("%s %s: %+v", tc.status, tc.reason, l)
		}
		if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'clearance.refused' AND entity_id = $1
			AND payload ->> 'reason' = $2`, out.Clearance.ID, tc.want); n != 1 {
			t.Errorf("%s: clearance.refused %d", tc.reason, n)
		}
	}
}

// Abuse cases: off is 404; another admin can't say another's file is
// uploaded; a demo letter has no document; another hospital's letter is
// 404; a file never posted refuses the letter after 30 minutes; the
// documents service not answering makes no letter and cancels the
// document; a wrong type, an oversize file or a field the server owns is
// 422; the eligibility checks still apply.
func TestLiveLetterAbuse(t *testing.T) {
	e, docs, ann := liveEnv(t, false)

	// Off: the live routes are 404 and nothing is made.
	off := Deps{Pool: e.app}
	raw, _ := json.Marshal(e.liveBody(e.analyst))
	for _, h := range []http.HandlerFunc{off.live, off.uploaded} {
		e.mux = http.NewServeMux()
		e.mux.Handle("POST "+httpx.BasePath+"/hospital/clearances/live", h)
		e.mux.Handle("POST "+httpx.BasePath+"/hospital/clearances/{id}/uploaded", h)
		for _, p := range []string{"/hospital/clearances/live", "/hospital/clearances/" + uuid.NewString() + "/uploaded"} {
			if c, _, _ := e.do(t, e.admin, http.MethodPost, p, "application/json", raw, true); c != http.StatusNotFound {
				t.Errorf("off %s: %d", p, c)
			}
		}
	}
	e, docs, ann = liveEnv(t, false)
	if e.letters(t) != 0 {
		t.Fatal("a letter while off")
	}

	for name, mut := range map[string]func(map[string]any){
		"docx":           func(m map[string]any) { m["content_type"] = "application/msword" },
		"tiff":           func(m map[string]any) { m["content_type"] = "image/tiff" },
		"oversize":       func(m map[string]any) { m["size_bytes"] = 10485761 },
		"empty":          func(m map[string]any) { m["size_bytes"] = 0 },
		"a hospital":     func(m map[string]any) { m["hospital_id"] = uuid.NewString() },
		"a document":     func(m map[string]any) { m["document_id"] = uuid.NewString() },
		"a status":       func(m map[string]any) { m["status"] = "waiting" },
		"nobody covered": func(m map[string]any) { m["covered"] = []string{} },
	} {
		body := e.liveBody(e.analyst)
		mut(body)
		if c, _, b := e.startLive(t, body); c != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, c, b)
		}
	}
	body := e.liveBody(e.clin)
	if c, _, b := e.startLive(t, body); c != http.StatusUnprocessableEntity || code(b) != "not_eligible" {
		t.Errorf("a clinician covered: %d %s", c, b)
	}
	body = e.liveBody(e.analyst)
	body["mfa_code"] = "000000"
	if c, _, _ := e.startLive(t, body); c != http.StatusForbidden {
		t.Errorf("wrong code: %d", c)
	}
	if n := e.count(t, `SELECT count(*) FROM documents WHERE hospital_id = $1`, e.h); n != 0 {
		t.Fatalf("documents made by refused uploads: %d", n)
	}

	// The documents service down: no letter, the document cancelled.
	docs.presign = "unavailable"
	if c, _, b := e.startLive(t, e.liveBody(e.analyst)); c != http.StatusServiceUnavailable || code(b) != "unavailable" {
		t.Fatalf("presign down: %d %s", c, b)
	}
	if e.letters(t) != 0 || e.count(t, `SELECT count(*) FROM documents WHERE hospital_id = $1 AND status = 'cancelled'`, e.h) != 1 ||
		e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'document.cancelled' AND payload ->> 'reason' = 'unavailable'`) != 1 {
		t.Fatal("presign down: a letter, or the document not cancelled")
	}
	docs.presign = ""

	c, out, b := e.startLive(t, e.liveBody(e.analyst))
	if c != http.StatusCreated {
		t.Fatalf("live: %d %s", c, b)
	}
	// Another hospital admin of the same hospital didn't upload it.
	other := user(t, e.super, &e.h, "admin2-"+e.h.String()[:8]+"@hospital.example.test", true, "hospital_admin")
	if c, _, _ := e.do(t, other, http.MethodPost, "/hospital/clearances/"+out.Clearance.ID+"/uploaded", "", nil, true); c != http.StatusNotFound {
		t.Errorf("another admin: %d", c)
	}
	// Another hospital's letter, in the same database.
	e2, _, _ := liveEnvIn(t, e.super, e.app, false)
	if c, _, _ := e2.do(t, e2.admin, http.MethodPost, "/hospital/clearances/"+out.Clearance.ID+"/uploaded", "", nil, true); c != http.StatusNotFound {
		t.Errorf("another hospital: %d", c)
	}
	if ann.n() != 0 {
		t.Fatal("announced for a refused uploaded")
	}
	// Never posted: after 30 minutes the document ends and the letter is
	// refused.
	doc := e.letterDocument(t, out.Clearance.ID)
	run(t, e.super, `UPDATE documents SET received_at = now() - interval '31 minutes' WHERE id = $1`, doc)
	if c, _, b := e.do(t, e.admin, http.MethodPost, "/hospital/clearances/"+out.Clearance.ID+"/uploaded", "", nil, true); c != http.StatusConflict {
		t.Fatalf("expired: %d %s", c, b)
	}
	if _, l := e.getLetter(t, out.Clearance.ID); l.Status != "refused" || *l.RefusedReason != "unreadable" {
		t.Fatalf("expired letter: %+v", l)
	}
	if ann.n() != 0 {
		t.Fatal("an expired upload was announced")
	}
	// A demo letter has no document: 404.
	d, _, _ := liveEnv(t, true)
	l := d.demoLetter(t, d.analyst)
	if c, _, _ := d.do(t, d.admin, http.MethodPost, "/hospital/clearances/"+l.ID+"/uploaded", "", nil, true); c != http.StatusNotFound {
		t.Errorf("demo letter: %d", c)
	}
	// The demo's page stays the fixture's PNG.
	if c, h, _ := d.do(t, d.admin, http.MethodGet, "/hospital/clearances/"+l.ID+"/pages/1", "", nil, false); c != http.StatusOK || h.Get("Content-Type") != "image/png" {
		t.Errorf("demo page: %d %s", c, h.Get("Content-Type"))
	}
	// The multipart route still refuses a hospital that isn't the demo.
	if c, b := e.upload(t, e.letterForm(e.analyst)); c != http.StatusServiceUnavailable || code(b) != "unavailable" {
		t.Errorf("multipart outside the demo: %d %s", c, b)
	}
}
