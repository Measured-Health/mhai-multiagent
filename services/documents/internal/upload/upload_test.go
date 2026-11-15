package upload

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Synthetic ids only.
const (
	hospA = "22222222-2222-4222-8222-222222222222"
	hospB = "33333333-3333-4333-8333-333333333333"
	docID = "55555555-5555-4555-8555-555555555555"
)

// fakeRows holds rows by hospital, as RLS would show them.
type fakeRows struct {
	rows map[string]Row // hospital/document
	err  error
	seen []string
}

func (f *fakeRows) UploadRow(_ context.Context, h, d uuid.UUID) (Row, error) {
	f.seen = append(f.seen, h.String())
	if f.err != nil {
		return Row{}, f.err
	}
	r, ok := f.rows[h.String()+"/"+d.String()]
	if !ok {
		return Row{}, ErrNotFound
	}
	return r, nil
}

type fakeSigner struct {
	got []s3.PolicyRequest
	err error
}

func (f *fakeSigner) PostPolicy(r s3.PolicyRequest, _ string) (s3.Policy, error) {
	f.got = append(f.got, r)
	if f.err != nil {
		return s3.Policy{}, f.err
	}
	return s3.Policy{URL: "https://store.example.test/mhai-documents-test", Fields: []s3.Field{{Name: "key", Value: r.Key}},
		ExpiresAt: time.Unix(1700000600, 0)}, nil
}

func claims(hid string) reqtoken.Claims { return reqtoken.Claims{Sub: reqtoken.SvcCore, Hid: hid} }

func ready(context.Context) error { return nil }

func newPresign(rows map[string]Row) (*Presign, *fakeRows, *fakeSigner) {
	fr, fs := &fakeRows{rows: rows}, &fakeSigner{}
	return &Presign{Rows: fr, Signer: fs, Ready: ready}, fr, fs
}

func run(t *testing.T, p *Presign, hid, doc string) *docspb.PresignUploadReply {
	t.Helper()
	out, err := p.Handle(context.Background(), claims(hid), &docspb.PresignUploadRequest{DocumentId: doc})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	return out.(*docspb.PresignUploadReply)
}

func TestPresignSignsTheRowsQuarantineKey(t *testing.T) {
	for purpose, max := range map[string]int64{"clinical": 26214400, "clearance": 10485760} {
		p, fr, fs := newPresign(map[string]Row{hospA + "/" + docID: {purpose, "uploading", "image/png"}})
		r := run(t, p, hospA, docID)
		if r.GetOutcome() != OutcomeOK || r.GetUrl() == "" || r.GetExpiresAt() != 1700000600 || len(r.GetFields()) != 1 {
			t.Fatalf("%s: %v", purpose, r)
		}
		want := s3.PolicyRequest{Key: "quarantine/" + hospA + "/" + docID, ContentType: "image/png", MaxBytes: max}
		if len(fs.got) != 1 || fs.got[0] != want {
			t.Fatalf("%s: signed %+v", purpose, fs.got)
		}
		if strings.Join(fr.seen, ",") != hospA {
			t.Fatalf("%s: read under %v", purpose, fr.seen)
		}
	}
}

// DS1, DS5 (the signer's side), S4: another hospital's document, a row that
// isn't uploading, an archive row, a bad id, or a service that isn't ready:
// no policy is signed.
func TestPresignRefusals(t *testing.T) {
	rows := map[string]Row{
		hospA + "/" + docID: {"clinical", "uploading", "application/pdf"},
	}
	cases := map[string]struct {
		hid, doc string
		rows     map[string]Row
		mut      func(*Presign, *fakeRows, *fakeSigner)
		want     string
	}{
		"another hospital": {hospB, docID, rows, nil, OutcomeNotFound},
		"no such document": {hospA, uuid.NewString(), rows, nil, OutcomeNotFound},
		"not a uuid":       {hospA, "../" + docID, rows, nil, OutcomeNotFound},
		"upper-case id":    {hospA, "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", map[string]Row{hospA + "/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee": {"clinical", "uploading", "application/pdf"}}, nil, OutcomeNotFound},
		"nil id":           {hospA, uuid.Nil.String(), rows, nil, OutcomeNotFound},
		"empty id":         {hospA, "", rows, nil, OutcomeNotFound},
		"archive row":      {hospA, docID, map[string]Row{hospA + "/" + docID: {"archive", "uploading", "application/pdf"}}, nil, OutcomeNotFound},
		"already uploaded": {hospA, docID, map[string]Row{hospA + "/" + docID: {"clinical", "uploaded", "application/pdf"}}, nil, OutcomeConflict},
		"cancelled":        {hospA, docID, map[string]Row{hospA + "/" + docID: {"clearance", "cancelled", "application/pdf"}}, nil, OutcomeConflict},
		"keyring not ready": {hospA, docID, rows, func(p *Presign, _ *fakeRows, _ *fakeSigner) {
			p.Ready = func(context.Context) error { return errors.New("x") }
		}, OutcomeUnavailable},
		"no readiness":   {hospA, docID, rows, func(p *Presign, _ *fakeRows, _ *fakeSigner) { p.Ready = nil }, OutcomeUnavailable},
		"no store":       {hospA, docID, rows, func(p *Presign, _ *fakeRows, _ *fakeSigner) { p.Signer = nil }, OutcomeUnavailable},
		"database down":  {hospA, docID, rows, func(_ *Presign, r *fakeRows, _ *fakeSigner) { r.err = errors.New("x") }, OutcomeUnavailable},
		"signer refuses": {hospA, docID, rows, func(_ *Presign, _ *fakeRows, s *fakeSigner) { s.err = s3.ErrPolicy }, OutcomeUnavailable},
	}
	for name, tc := range cases {
		p, fr, fs := newPresign(tc.rows)
		if tc.mut != nil {
			tc.mut(p, fr, fs)
		}
		r := run(t, p, tc.hid, tc.doc)
		if r.GetOutcome() != tc.want || r.GetUrl() != "" || len(r.GetFields()) != 0 || r.GetExpiresAt() != 0 {
			t.Errorf("%s: %v, want %s and nothing else", name, r, tc.want)
		}
		if tc.want != OutcomeUnavailable || name == "signer refuses" {
			continue
		}
		if len(fs.got) != 0 {
			t.Errorf("%s: a policy was signed", name)
		}
	}
}

// The handler never trusts a token without a hospital, or a request of
// another type: no reply at all.
func TestPresignHandlerErrors(t *testing.T) {
	p, _, _ := newPresign(nil)
	if _, err := p.Handle(context.Background(), claims(""), &docspb.PresignUploadRequest{DocumentId: docID}); err == nil {
		t.Fatal("no hospital")
	}
	if _, err := p.Handle(context.Background(), claims(hospA), &docspb.HealthRequest{}); err == nil {
		t.Fatal("another request type")
	}
}
