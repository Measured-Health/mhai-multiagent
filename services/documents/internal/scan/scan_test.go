package scan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
)

// fakes for every dependency, recording what happened.

type fakeRows struct {
	mu       sync.Mutex
	claim    Claim
	claimErr error
	renew    bool
	ready    bool
	readyErr error
	released int
	status   string
	reason   string
	pages    int
	objects  []Object
	sum      *[32]byte
	lease    uuid.UUID
}

func (f *fakeRows) Claim(_ context.Context, _, _, lease uuid.UUID) (Claim, error) {
	f.lease = lease
	return f.claim, f.claimErr
}
func (f *fakeRows) Renew(_ context.Context, _, _, lease uuid.UUID) (bool, error) {
	return f.renew && lease == f.lease, nil
}
func (f *fakeRows) Release(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	return true, nil
}
func (f *fakeRows) Ready(_ context.Context, _, _, _ uuid.UUID, sum [32]byte, pages int, objs []Object) (bool, error) {
	if f.readyErr != nil || !f.ready {
		return false, f.readyErr
	}
	f.status, f.pages, f.objects, f.sum = "ready", pages, objs, &sum
	return true, nil
}
func (f *fakeRows) Quarantine(_ context.Context, _, _, _ uuid.UUID, reason string, sum *[32]byte) (bool, error) {
	f.status, f.reason, f.sum = "quarantined", reason, sum
	return true, nil
}

type fakeQuarantine struct {
	data    []byte
	headN   int64
	headErr error
	getErr  error
	reads   int
	deleted []string
}

func (q *fakeQuarantine) Head(context.Context, string) (int64, error) {
	if q.headErr != nil {
		return 0, q.headErr
	}
	if q.headN != 0 {
		return q.headN, nil
	}
	return int64(len(q.data)), nil
}
func (q *fakeQuarantine) Get(context.Context, string) (io.ReadCloser, error) {
	q.reads++
	if q.getErr != nil {
		return nil, q.getErr
	}
	return io.NopCloser(bytes.NewReader(q.data)), nil
}
func (q *fakeQuarantine) Delete(_ context.Context, key string) error {
	q.deleted = append(q.deleted, key)
	return nil
}

type fakeStore struct {
	put     []envelope.Ref
	deleted []envelope.Ref
	failAt  int
}

func (s *fakeStore) Put(_ context.Context, ref envelope.Ref, src io.Reader) (envelope.Sealed, error) {
	if s.failAt > 0 && len(s.put)+1 == s.failAt {
		return envelope.Sealed{}, errors.New("store down")
	}
	b, _ := io.ReadAll(src)
	s.put = append(s.put, ref)
	return envelope.Sealed{WrappedDEK: make([]byte, 60), KEKVersion: 1, SHA256: sha256.Sum256(b), Size: int64(len(b))}, nil
}
func (s *fakeStore) Delete(_ context.Context, ref envelope.Ref) error {
	s.deleted = append(s.deleted, ref)
	return nil
}

type fakeVirus struct {
	readyErr error
	scanErr  error
	hit      bool
	scanned  int
}

func (v *fakeVirus) Ready(context.Context) error { return v.readyErr }
func (v *fakeVirus) Scan(context.Context, []byte) (bool, error) {
	v.scanned++
	return v.hit, v.scanErr
}

type fakeRenderer struct {
	readyErr error
	err      error
	reason   string
	pages    int
	rendered int
}

func (r *fakeRenderer) Ready(context.Context) error { return r.readyErr }
func (r *fakeRenderer) Render(context.Context, []byte, string, int) ([][]byte, string, error) {
	r.rendered++
	if r.err != nil || r.reason != "" {
		return nil, r.reason, r.err
	}
	out := make([][]byte, r.pages)
	for i := range out {
		out[i] = []byte("synthetic page png")
	}
	return out, "", nil
}

type fakeAnnounce struct{ got []Result }

func (a *fakeAnnounce) Scanned(_ context.Context, _, _ uuid.UUID, outcome, reason string, pages int) error {
	a.got = append(a.got, Result{Outcome: outcome, Reason: reason, Pages: pages})
	return nil
}

type rig struct {
	s     *Scanner
	rows  *fakeRows
	q     *fakeQuarantine
	store *fakeStore
	virus *fakeVirus
	rend  *fakeRenderer
	ann   *fakeAnnounce
	logs  *bytes.Buffer
	keyOK bool
}

var (
	hospital = uuid.MustParse("0a1b2c3d-0000-4000-8000-000000000001")
	document = uuid.MustParse("0a1b2c3d-0000-4000-8000-000000000002")
	pdfBytes = []byte("%PDF-1.7\nSYNTHETIC-FILE-CONTENT\n")
)

func newRig() *rig {
	r := &rig{
		rows:  &fakeRows{claim: Claim{Outcome: ClaimClaimed, Purpose: "clinical", MimeType: PDF}, renew: true, ready: true},
		q:     &fakeQuarantine{data: pdfBytes},
		store: &fakeStore{},
		virus: &fakeVirus{},
		rend:  &fakeRenderer{pages: 2},
		ann:   &fakeAnnounce{},
		logs:  &bytes.Buffer{},
		keyOK: true,
	}
	r.s = &Scanner{Rows: r.rows, Quarantine: r.q, Store: r.store, Virus: r.virus, Renderer: r.rend, Announce: r.ann,
		HospitalKey: func(context.Context, uuid.UUID) error {
			if r.keyOK {
				return nil
			}
			return errors.New("no key")
		},
		StorageReady: func(context.Context) error { return nil },
		Log:          slog.New(slog.NewTextHandler(r.logs, nil))}
	return r
}

func (r *rig) scan(t *testing.T) (Result, error) {
	t.Helper()
	res, err := r.s.Scan(context.Background(), hospital, document)
	if strings.Contains(r.logs.String(), "SYNTHETIC-FILE-CONTENT") {
		t.Fatal("a byte of the file reached the log")
	}
	return res, err
}

func TestCleanScanStoresSealedObjectsAndAnnounces(t *testing.T) {
	r := newRig()
	res, err := r.scan(t)
	if err != nil || res != (Result{Outcome: OutcomeReady, Pages: 2}) {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.store.put) != 3 || r.store.put[0].Kind != envelope.Original || r.store.put[2] != (envelope.Ref{Hospital: hospital,
		Document: document, Kind: envelope.Page, N: 2}) {
		t.Fatalf("stored %+v", r.store.put)
	}
	if r.rows.status != "ready" || r.rows.pages != 2 || len(r.rows.objects) != 3 || *r.rows.sum != sha256.Sum256(pdfBytes) {
		t.Fatalf("row %+v", r.rows)
	}
	if len(r.q.deleted) != 1 || r.q.deleted[0] != envelope.QuarantineKey(hospital, document) {
		t.Fatalf("quarantine deleted %v", r.q.deleted)
	}
	if len(r.ann.got) != 1 || r.ann.got[0].Outcome != OutcomeReady {
		t.Fatalf("announced %+v", r.ann.got)
	}
	if r.virus.scanned != 1 || r.rend.rendered != 1 {
		t.Fatal("not scanned and rendered once")
	}
}

// Each refusal is quarantined with its reason; nothing is stored, the
// quarantine object goes, and later stages never see the file.
func TestRefusals(t *testing.T) {
	for _, c := range []struct {
		name, reason   string
		setup          func(*rig)
		read, scan, rd bool
	}{
		{"B1 too large by size", "too_large", func(r *rig) { r.q.headN = 26 << 20 }, false, false, false},
		{"too large when read", "too_large", func(r *rig) { r.q.headN = 10; r.q.data = make([]byte, 25<<20+1) }, true, false, false},
		{"clearance over 10 MB", "too_large", func(r *rig) { r.rows.claim.Purpose = "clearance"; r.q.headN = 10<<20 + 1 }, false, false, false},
		{"nothing posted", "unreadable", func(r *rig) { r.q.headErr = s3.ErrNotFound }, false, false, false},
		{"B4 html as pdf", "unsupported_type", func(r *rig) { r.q.data = []byte("<html>synthetic</html>") }, true, false, false},
		{"png declared pdf", "unsupported_type", func(r *rig) { r.q.data = []byte("\x89PNG\r\n\x1a\nsynthetic") }, true, false, false},
		{"DS19 checksum", "checksum_mismatch", func(r *rig) {
			r.rows.claim.Purpose = "archive"
			r.rows.claim.SHA256 = make([]byte, 32)
		}, true, false, false},
		{"DS6 malware", "malware", func(r *rig) { r.virus.hit = true }, true, true, false},
		{"DS8 password", "password_protected", func(r *rig) { r.rend.reason = "password_protected" }, true, true, true},
		{"B1 pages", "too_many_pages", func(r *rig) { r.rend.reason = "too_many_pages" }, true, true, true},
		{"B3 bomb", "unreadable", func(r *rig) { r.rend.reason = "unreadable" }, true, true, true},
		{"no pages", "unreadable", func(r *rig) { r.rend.pages = 0 }, true, true, true},
		{"unknown purpose", "unsupported_type", func(r *rig) { r.rows.claim.Purpose = "letter" }, false, false, false},
	} {
		r := newRig()
		c.setup(r)
		res, err := r.scan(t)
		if err != nil || res.Outcome != OutcomeQuarantined || res.Reason != c.reason || r.rows.reason != c.reason {
			t.Errorf("%s: %+v %v (row %q)", c.name, res, err, r.rows.reason)
			continue
		}
		if len(r.store.put) != 0 {
			t.Errorf("%s: stored %d objects", c.name, len(r.store.put))
		}
		if (r.q.reads > 0) != c.read || (r.virus.scanned > 0) != c.scan || (r.rend.rendered > 0) != c.rd {
			t.Errorf("%s: read %d scanned %d rendered %d", c.name, r.q.reads, r.virus.scanned, r.rend.rendered)
		}
		if len(r.q.deleted) != 1 || len(r.ann.got) != 1 || r.ann.got[0].Reason != c.reason {
			t.Errorf("%s: deleted %v announced %+v", c.name, r.q.deleted, r.ann.got)
		}
	}
}

// §5.3, D8, DS6: whatever is missing, the document is never ready: the
// lease is released, nothing is stored or announced, the quarantine object
// stays for the retry, and documents_scan_blocked is logged.
func TestBlockedScansFailClosed(t *testing.T) {
	for name, setup := range map[string]func(*rig){
		"clamd stopped":       func(r *rig) { r.virus.readyErr = errors.New("down") },
		"signatures stale":    func(r *rig) { r.virus.readyErr = errors.New("stale") },
		"clamd errors":        func(r *rig) { r.virus.scanErr = errors.New("error") },
		"sandbox down":        func(r *rig) { r.rend.readyErr = errors.New("down") },
		"sandbox errors":      func(r *rig) { r.rend.err = errors.New("unavailable") },
		"no hospital key":     func(r *rig) { r.keyOK = false },
		"store not ready":     func(r *rig) { r.s.StorageReady = func(context.Context) error { return errors.New("x") } },
		"store errors":        func(r *rig) { r.q.getErr = errors.New("500") },
		"store write fails":   func(r *rig) { r.store.failAt = 2 },
		"no scanner":          func(r *rig) { r.s.Virus = nil },
		"no renderer":         func(r *rig) { r.s.Renderer = nil },
		"no store configured": func(r *rig) { r.s.Store = nil },
	} {
		r := newRig()
		setup(r)
		res, err := r.scan(t)
		if !errors.Is(err, ErrBlocked) || res.Outcome != "" {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		if r.rows.status != "" || r.rows.released != 1 || len(r.ann.got) != 0 || len(r.q.deleted) != 0 {
			t.Errorf("%s: status %q released %d announced %d deleted %v", name, r.rows.status, r.rows.released,
				len(r.ann.got), r.q.deleted)
		}
		if len(r.store.put) != len(r.store.deleted) {
			t.Errorf("%s: %d objects stored, %d removed", name, len(r.store.put), len(r.store.deleted))
		}
		if !strings.Contains(r.logs.String(), "documents_scan_blocked") {
			t.Errorf("%s: no alert", name)
		}
	}
}

func TestClaimOutcomes(t *testing.T) {
	r := newRig()
	r.rows.claim = Claim{Outcome: ClaimNotClaimable}
	if res, err := r.scan(t); err != nil || res.Outcome != OutcomeSkipped || r.q.reads != 0 {
		t.Fatalf("not claimable: %+v %v", res, err)
	}
	// Six hours scanning: failed scan_unavailable, announced; the
	// quarantine object goes.
	r = newRig()
	r.rows.claim = Claim{Outcome: ClaimFailed}
	if res, err := r.scan(t); err != nil || res != (Result{Outcome: OutcomeFailed, Reason: "scan_unavailable"}) ||
		len(r.ann.got) != 1 || len(r.q.deleted) != 1 {
		t.Fatalf("failed: %+v %v", res, err)
	}
	r = newRig()
	r.rows.claimErr = errors.New("database down")
	if _, err := r.scan(t); err == nil {
		t.Fatal("a database error passed")
	}
	if res, _ := r.s.Scan(context.Background(), uuid.Nil, document); res.Outcome != OutcomeSkipped {
		t.Fatal("nil hospital")
	}
}

// The lease is lost before storing: nothing is stored. Ready refused
// (the row changed): the stored objects are deleted.
func TestLostLease(t *testing.T) {
	r := newRig()
	r.rows.renew = false
	if res, err := r.scan(t); err != nil || res.Outcome != OutcomeSkipped || len(r.store.put) != 0 {
		t.Fatalf("renew refused: %+v %v %d", res, err, len(r.store.put))
	}
	r = newRig()
	r.rows.ready = false
	if res, err := r.scan(t); err != nil || res.Outcome != OutcomeSkipped || len(r.store.deleted) != 3 || len(r.ann.got) != 0 {
		t.Fatalf("ready refused: %+v %v deleted %d", res, err, len(r.store.deleted))
	}
	r = newRig()
	r.rows.readyErr = errors.New("database down")
	if _, err := r.scan(t); err == nil || len(r.store.deleted) != 3 || r.rows.released != 1 {
		t.Fatalf("ready error: %v deleted %d released %d", err, len(r.store.deleted), r.rows.released)
	}
}

func TestBlockedAlertIsThrottled(t *testing.T) {
	r := newRig()
	r.virus.readyErr = errors.New("down")
	for i := 0; i < 5; i++ {
		_, _ = r.scan(t)
	}
	if n := strings.Count(r.logs.String(), "documents_scan_blocked"); n != 1 {
		t.Fatalf("alert logged %d times", n)
	}
	if r.s.Metrics.Blocked.Load() != 5 {
		t.Fatalf("blocked counted %d", r.s.Metrics.Blocked.Load())
	}
	_ = time.Second
}
