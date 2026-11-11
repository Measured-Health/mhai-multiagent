package analysis

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixedClock is a pinned clock: no test depends on today.
type fixedClock time.Time

func (c fixedClock) Now(context.Context) (time.Time, error) { return time.Time(c), nil }

var testNow = time.Date(2026, 2, 10, 1, 0, 0, 0, time.UTC)

// fakeStore records the calls in order; fail makes one step fail.
type fakeStore struct {
	mu     sync.Mutex
	due    []Due
	rows   []SourceRow
	calls  []string
	built  [][]byte
	fail   map[string]error // "<step> <hospital>"
	inDue  func()
	frozen int
}

func (s *fakeStore) record(step string, h uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, step+" "+h.String())
	return s.fail[step+" "+h.String()]
}

func (s *fakeStore) Due(_ context.Context, now time.Time) ([]Due, error) {
	if !now.Equal(testNow) {
		return nil, errors.New("not the injected clock")
	}
	if s.inDue != nil {
		s.inDue()
	}
	return s.due, nil
}

func (s *fakeStore) Ingest(_ context.Context, h uuid.UUID, _ *time.Time, until time.Time, build func([]SourceRow) ([]byte, error)) (int, error) {
	if err := s.record("ingest", h); err != nil {
		return 0, err
	}
	b, err := build(s.rows)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.built = append(s.built, b)
	s.mu.Unlock()
	return 2, nil
}

func (s *fakeStore) Sweep(_ context.Context, h uuid.UUID, _ time.Time) error {
	return s.record("sweep", h)
}

func (s *fakeStore) Freeze(_ context.Context, h uuid.UUID, _ time.Time) (int, error) {
	return s.frozen, s.record("freeze", h)
}

func (s *fakeStore) Ran(_ context.Context, h uuid.UUID, _ time.Time) error { return s.record("ran", h) }

func noLock(context.Context) (func(), bool, error) { return func() {}, true, nil }

func ids(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
	}
	return out
}

// Ingest runs before sweep and freeze; a demo hospital never ingests; a
// switched-off hospital with facts is only swept.
func TestNightlyOrder(t *testing.T) {
	h := ids(3)
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true}, {Hospital: h[1], On: true, Demo: true}, {Hospital: h[2]}}}
	n := &Nightly{Store: s, Keys: testKeys(t), Clock: fixedClock(testNow), Lock: noLock}
	ran, err := n.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ingest " + h[0].String(), "sweep " + h[0].String(), "freeze " + h[0].String(), "ran " + h[0].String(),
		"sweep " + h[1].String(), "freeze " + h[1].String(), "ran " + h[1].String(),
		"sweep " + h[2].String(), "ran " + h[2].String()}
	if strings.Join(s.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls\n%s\nwant\n%s", strings.Join(s.calls, "\n"), strings.Join(want, "\n"))
	}
	if len(ran) != 3 || n.LateFacts.Load() != 2 {
		t.Fatalf("ran %v, late %d", ran, n.LateFacts.Load())
	}
}

// Two copies wake together: one runs, the other skips.
func TestNightlyOneCopy(t *testing.T) {
	h := ids(1)
	var mu sync.Mutex
	held := false
	lock := func(context.Context) (func(), bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if held {
			return nil, false, nil
		}
		held = true
		return func() { mu.Lock(); held = false; mu.Unlock() }, true, nil
	}
	release := make(chan struct{})
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true, Demo: true}}, inDue: func() { <-release }}
	a := &Nightly{Store: s, Clock: fixedClock(testNow), Lock: lock}
	b := &Nightly{Store: s, Clock: fixedClock(testNow), Lock: lock}
	var wg sync.WaitGroup
	results := make([][]uuid.UUID, 2)
	for i, n := range []*Nightly{a, b} {
		wg.Add(1)
		go func(i int, n *Nightly) {
			defer wg.Done()
			results[i], _ = n.Tick(context.Background())
		}(i, n)
	}
	// Let the loser find the lock held, then free the winner.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if len(results[0])+len(results[1]) != 1 {
		t.Fatalf("ran %v and %v: want one copy", results[0], results[1])
	}
	if strings.Count(strings.Join(s.calls, " "), "freeze") != 1 {
		t.Fatalf("calls %v", s.calls)
	}
}

// A failure logs the hospital id, the step, a fixed reason and the alert;
// never the error's text (a count, a code, a day, a reference). The other
// hospitals still run, and the failed one isn't marked ran.
func TestNightlyLogsNoValues(t *testing.T) {
	h := ids(2)
	var logs bytes.Buffer
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true}, {Hospital: h[1], On: true}},
		fail: map[string]error{"freeze " + h[0].String(): errors.New("E11 on 2026-02-10 for ref-secret-77, count 9876")}}
	n := &Nightly{Store: s, Keys: testKeys(t), Clock: fixedClock(testNow), Lock: noLock,
		Log: slog.New(slog.NewJSONHandler(&logs, nil))}
	ran, err := n.Tick(context.Background())
	if err == nil || len(ran) != 1 || ran[0] != h[1] {
		t.Fatalf("ran %v (%v)", ran, err)
	}
	out := logs.String()
	for _, want := range []string{h[0].String(), `"step":"freeze"`, `"reason":"internal_error"`, `"alert":"analysis_freeze_failed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s: %s", want, out)
		}
	}
	for _, leak := range []string{"E11", "2026-02-10", "ref-secret-77", "9876"} {
		if strings.Contains(out, leak) {
			t.Errorf("log holds %q: %s", leak, out)
		}
	}
	for _, c := range s.calls {
		if c == "ran "+h[0].String() {
			t.Error("the failed hospital was marked ran")
		}
	}
	if n.Failures.Load() != 1 {
		t.Errorf("failures %d", n.Failures.Load())
	}
}

// Ingest turns each reference into its HMAC and ciphertext: the clear
// reference never reaches the database, and facts group by subject.
func TestIngestBuildsReferencesWithKeys(t *testing.T) {
	h := ids(1)
	k := testKeys(t)
	day := time.Date(2026, 2, 9, 0, 0, 0, 0, time.UTC)
	urg := "routine"
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true}}, rows: []SourceRow{
		{Ref: "emr-index-0001", Day: day, Kind: "consultation", Sex: "female", Urgency: &urg},
		{Ref: "emr-index-0001", Day: day, Kind: "consultation", Sex: "female"},
		{Ref: "emr-index-0002", Day: day, Kind: "consultation", Sex: "male"},
	}}
	n := &Nightly{Store: s, Keys: k, Clock: fixedClock(testNow), Lock: noLock}
	if _, err := n.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.built) != 1 || bytes.Contains(s.built[0], []byte("emr-index")) {
		t.Fatalf("built %s", s.built)
	}
	var body struct {
		Subjects []struct {
			RefHMAC string           `json:"ref_hmac"`
			RefEnc  []byte           `json:"ref_enc"`
			Facts   []map[string]any `json:"facts"`
		} `json:"subjects"`
	}
	dec := json.NewDecoder(bytes.NewReader(s.built[0]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Subjects) != 2 {
		t.Fatalf("%d subjects", len(body.Subjects))
	}
	for _, sub := range body.Subjects {
		ref, err := k.OpenRef(h[0], sub.RefEnc)
		if err != nil {
			t.Fatal(err)
		}
		if sub.RefHMAC != hex.EncodeToString(k.RefHMAC(h[0], ref)) {
			t.Errorf("%s: HMAC mismatch", ref)
		}
		if want := map[string]int{"emr-index-0001": 2, "emr-index-0002": 1}[ref]; len(sub.Facts) != want {
			t.Errorf("%s: %d facts", ref, len(sub.Facts))
		}
		for _, f := range sub.Facts {
			if f["day"] != "2026-02-09" || f["kind"] != "consultation" {
				t.Errorf("fact %v", f)
			}
			if _, ok := f["ref"]; ok {
				t.Errorf("fact carries the reference: %v", f)
			}
		}
	}
}

// Without keys, a hospital with rows to ingest fails closed; with none,
// the run goes on.
func TestIngestWithoutKeysFailsClosed(t *testing.T) {
	h := ids(1)
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true}},
		rows: []SourceRow{{Ref: "emr-index-0001", Day: testNow, Kind: "consultation", Sex: "female"}}}
	n := &Nightly{Store: s, Clock: fixedClock(testNow), Lock: noLock}
	if ran, err := n.Tick(context.Background()); err == nil || len(ran) != 0 {
		t.Fatalf("ran %v without keys (%v)", ran, err)
	}
	s.rows, s.calls = nil, nil
	if ran, err := n.Tick(context.Background()); err != nil || len(ran) != 1 {
		t.Fatalf("no rows, no keys: ran %v (%v)", ran, err)
	}
}

// N3: only internal/analysis (not its routes) and internal/db name
// WithAnalysisJob: no route handler, platform code or other package can
// reach the job's role.
func TestRoutesCantReachAnalysisJobPool(t *testing.T) {
	root := filepath.Join("..", "..")
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != "WithAnalysisJob" {
				return true
			}
			found++
			if !strings.HasSuffix(dir, "internal/analysis") && !strings.HasSuffix(dir, "internal/db") {
				t.Errorf("%s names WithAnalysisJob", path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("WithAnalysisJob found nowhere: the test proves nothing")
	}
}
