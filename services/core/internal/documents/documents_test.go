package documents

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

var (
	hid = uuid.MustParse("0b1c2d3e-0000-4000-8000-0000000000a1")
	did = uuid.MustParse("0b1c2d3e-0000-4000-8000-0000000000d1")
)

func scanned(t *testing.T, s *docspb.Scanned) []byte {
	t.Helper()
	b, err := proto.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A scanned message is the subject's hospital and a body of exactly
// docspb.Scanned with a known outcome and its reason; anything else is
// refused before the database is touched.
func TestParseScanned(t *testing.T) {
	subj := "documents.v1." + hid.String() + ".scanned"
	ok := []*docspb.Scanned{
		{DocumentId: did.String(), Outcome: "ready", Pages: 3},
		{DocumentId: did.String(), Outcome: "quarantined", Reason: "malware"},
		{DocumentId: did.String(), Outcome: "failed", Reason: "scan_unavailable"},
	}
	for _, s := range ok {
		h, got, fine := Parse(subj, scanned(t, s))
		if !fine || h != hid || got.GetDocumentId() != did.String() {
			t.Errorf("%v: refused", s)
		}
	}
	bad := map[string][]byte{
		"ready without pages":     scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "ready"}),
		"ready with a reason":     scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "ready", Reason: "malware", Pages: 1}),
		"quarantined with pages":  scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "quarantined", Reason: "malware", Pages: 1}),
		"unknown reason":          scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "quarantined", Reason: "nope"}),
		"failed for malware":      scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "failed", Reason: "malware"}),
		"unknown outcome":         scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "done"}),
		"upper-case id":           scanned(t, &docspb.Scanned{DocumentId: strings.ToUpper(did.String()), Outcome: "ready", Pages: 1}),
		"too many pages":          scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "ready", Pages: 51}),
		"not protobuf":            []byte("\xff\xff\xff"),
		"an unknown field":        append(scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "ready", Pages: 1}), 0x48, 0x01),
		"over the stream's limit": bytes.Repeat([]byte{0}, 5000),
	}
	for name, b := range bad {
		if _, _, fine := Parse(subj, b); fine {
			t.Errorf("%s: accepted", name)
		}
	}
	body := scanned(t, ok[0])
	for _, s := range []string{"documents.v1." + hid.String() + ".uploaded", "documents.v1.global.scanned",
		"documents.v1." + uuid.Nil.String() + ".scanned", "documents.v2." + hid.String() + ".scanned",
		"documents.v1." + hid.String() + ".scanned.x", "emr.v1." + hid.String() + ".scanned"} {
		if _, _, fine := Parse(s, body); fine {
			t.Errorf("subject %s: accepted", s)
		}
	}
}

// The entry is the row's: quarantined and failed by their reason, anything
// with pages as ready (a later status or a cancel keeps them).
func TestScanEntry(t *testing.T) {
	r := func(s string) *string { return &s }
	n := func(i int) *int { return &i }
	for _, tc := range []struct {
		status  string
		reason  *string
		pages   *int
		action  string
		outcome string
	}{
		{"ready", nil, n(2), "document.ready", "ready"},
		{"cancelled", nil, n(2), "document.ready", "ready"},
		{"queued", nil, n(1), "document.ready", "ready"},
		{"quarantined", r("malware"), nil, "document.quarantined", "quarantined"},
		{"failed", r("scan_unavailable"), nil, "document.scan_failed", "failed"},
	} {
		action, p := scanEntry(did, tc.status, tc.reason, tc.pages)
		if action != tc.action || p["document_id"] != did.String() || scanOutcome(tc.status, tc.pages) != tc.outcome {
			t.Errorf("%s: %s %v", tc.status, action, p)
		}
		if tc.reason != nil && p["reason"] != *tc.reason {
			t.Errorf("%s: reason %v", tc.status, p["reason"])
		}
		if tc.pages != nil && p["pages"] != *tc.pages {
			t.Errorf("%s: pages %v", tc.status, p["pages"])
		}
	}
}

type msg struct {
	subject string
	data    []byte
	acked   string
}

func (m *msg) Subject() string                  { return m.subject }
func (m *msg) Data() []byte                     { return m.data }
func (m *msg) Ack() error                       { m.acked = "ack"; return nil }
func (m *msg) NakWithDelay(time.Duration) error { m.acked = "nak"; return nil }
func (m *msg) Term() error                      { m.acked = "term"; return nil }

// A malformed message is dropped (Term) and counted; with no database the
// work fails and the message is naked for a retry, never acked.
func TestHandleMsgOutcomes(t *testing.T) {
	c := &Consumer{}
	m := &msg{subject: "documents.v1.nope.scanned", data: []byte("x")}
	c.HandleMsg(context.Background(), m)
	if m.acked != "term" || c.metrics().Refused.Load() != 1 {
		t.Fatalf("malformed: %s", m.acked)
	}
	m = &msg{subject: "documents.v1." + hid.String() + ".scanned",
		data: scanned(t, &docspb.Scanned{DocumentId: did.String(), Outcome: "ready", Pages: 1})}
	c.HandleMsg(context.Background(), m)
	if m.acked != "nak" || c.metrics().Retried.Load() != 1 {
		t.Fatalf("no database: %s", m.acked)
	}
	if err := c.Run(context.Background(), nil); err == nil {
		t.Fatal("Run without JetStream")
	}
	if err := (JSAnnouncer{}).Uploaded(context.Background(), hid, did); err == nil {
		t.Fatal("announce without JetStream")
	}
}

func serve(t *testing.T, d Deps, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	m := http.NewServeMux()
	for _, r := range []struct {
		p string
		h http.HandlerFunc
	}{{"POST /documents", d.start}, {"GET /documents/{id}", d.get}, {"POST /documents/{id}/uploaded", d.uploaded},
		{"POST /documents/{id}/cancel", d.cancel}} {
		meth, p, _ := strings.Cut(r.p, " ")
		m.Handle(meth+" "+httpx.BasePath+p, r.h)
	}
	r := httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader(body))
	r = r.WithContext(authz.WithPrincipal(r.Context(), authz.Principal{UserID: uuid.New(), HospitalID: hid,
		Roles: []string{authz.RoleClinician}}))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

// Off (CORE_DOCUMENTS_ENABLED false): every route is 404, whatever is
// asked, and nothing is read.
func TestRoutesOffAre404(t *testing.T) {
	d := Deps{}
	for _, tc := range [][2]string{{"POST", "/documents"}, {"GET", "/documents/" + did.String()},
		{"POST", "/documents/" + did.String() + "/uploaded"}, {"POST", "/documents/" + did.String() + "/cancel"}} {
		if w := serve(t, d, tc[0], tc[1], `{"purpose":"clinical","content_type":"application/pdf","size_bytes":1}`); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", tc[0], tc[1], w.Code)
		}
	}
}

// The body is checked before the database (422), and a malformed id is 404.
func TestRouteValidation(t *testing.T) {
	d := Deps{On: true}
	for name, body := range map[string]string{
		"archive":       `{"purpose":"archive","content_type":"application/pdf","size_bytes":1}`,
		"clearance":     `{"purpose":"clearance","content_type":"application/pdf","size_bytes":1}`,
		"docx":          `{"purpose":"clinical","content_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","size_bytes":1}`,
		"html":          `{"purpose":"clinical","content_type":"text/html","size_bytes":1}`,
		"empty":         `{"purpose":"clinical","content_type":"application/pdf","size_bytes":0}`,
		"oversize":      `{"purpose":"clinical","content_type":"application/pdf","size_bytes":26214401}`,
		"a hospital":    `{"purpose":"clinical","content_type":"application/pdf","size_bytes":1,"hospital_id":"` + hid.String() + `"}`,
		"an uploader":   `{"purpose":"clinical","content_type":"application/pdf","size_bytes":1,"uploaded_by":"` + did.String() + `"}`,
		"a bad patient": `{"purpose":"clinical","content_type":"application/pdf","size_bytes":1,"patient_id":"nope"}`,
		"not json":      `purpose=clinical`,
		"two objects":   `{} {}`,
	} {
		if w := serve(t, d, "POST", "/documents", body); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := serve(t, d, "POST", "/documents?x=1", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("query: %d", w.Code)
	}
	for _, p := range []string{"/documents/nope", "/documents/" + strings.ToUpper(did.String()), "/documents/" + uuid.Nil.String()} {
		if w := serve(t, d, "GET", p, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, w.Code)
		}
		if w := serve(t, d, "POST", p+"/uploaded", ""); w.Code != http.StatusNotFound {
			t.Errorf("uploaded %s: %d", p, w.Code)
		}
	}
	for _, body := range []string{`{}`, `{"version":0}`, `{"version":"1"}`, `{"version":1,"status":"cancelled"}`} {
		if w := serve(t, d, "POST", "/documents/"+did.String()+"/cancel", body); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("cancel %s: %d", body, w.Code)
		}
	}
}

type fakeCtl struct {
	rep *docspb.PresignUploadReply
	err error
}

func (f fakeCtl) Request(_ context.Context, _ uuid.UUID, _, _ string, _, resp proto.Message) error {
	if f.err != nil {
		return f.err
	}
	proto.Merge(resp, f.rep)
	return nil
}

// Presign passes a whole policy through and refuses anything less as
// unavailable.
func TestPresign(t *testing.T) {
	good := &docspb.PresignUploadReply{Outcome: "ok", Url: "https://store.example.test/bucket", ExpiresAt: 1_900_000_000,
		Fields: []*docspb.FormField{{Name: "key", Value: "quarantine/x"}, {Name: "policy", Value: "synthetic"}}}
	out, err := Presign(context.Background(), fakeCtl{rep: good}, hid, did)
	if err != nil || out.URL != good.Url || len(out.Fields) != 2 || out.Fields[0].Name != "key" || out.ExpiresAt == "" {
		t.Fatalf("ok: %+v %v", out, err)
	}
	for name, f := range map[string]fakeCtl{
		"no answer":   {err: context.DeadlineExceeded},
		"not found":   {rep: &docspb.PresignUploadReply{Outcome: "not_found"}},
		"unavailable": {rep: &docspb.PresignUploadReply{Outcome: "unavailable"}},
		"no url":      {rep: &docspb.PresignUploadReply{Outcome: "ok", ExpiresAt: 1, Fields: good.Fields}},
		"no fields":   {rep: &docspb.PresignUploadReply{Outcome: "ok", Url: "u", ExpiresAt: 1}},
		"no expiry":   {rep: &docspb.PresignUploadReply{Outcome: "ok", Url: "u", Fields: good.Fields}},
	} {
		if _, err := Presign(context.Background(), f, hid, did); err == nil {
			t.Errorf("%s: passed", name)
		} else if code, refused := codeOf(err); code != "unavailable" || !refused {
			t.Errorf("%s: %s", name, code)
		}
	}
	if _, err := Presign(context.Background(), nil, hid, did); err == nil {
		t.Fatal("no client: passed")
	}
}
