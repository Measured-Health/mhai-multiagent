package analysis

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Analysis PR 10 (Task 28): the patient-index changes are sent to the
// database as HMACs and, for a merge's survivor, its sealed reference:
// never a clear reference.
func TestBuildChanges(t *testing.T) {
	h := ids(1)[0]
	k := testKeys(t)
	survivor := "emr-index-vantreek-b"
	b, err := buildChanges(k, h, []ChangeRow{
		{Kind: "merged", Ref: "emr-index-vantreek-a", Survivor: &survivor},
		{Kind: "excluded", Ref: "emr-index-vantreek-c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("vantreek")) {
		t.Fatalf("a clear reference reached the body: %s", b)
	}
	var body struct {
		Changes []struct {
			Kind    string `json:"kind"`
			RefHMAC string `json:"ref_hmac"`
			ToHMAC  string `json:"to_hmac"`
			ToEnc   []byte `json:"to_enc"`
		} `json:"changes"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || len(body.Changes) != 2 {
		t.Fatalf("%s (%v)", b, err)
	}
	m, f := body.Changes[0], body.Changes[1]
	if m.Kind != "merge" || m.RefHMAC != hex.EncodeToString(k.RefHMAC(h, "emr-index-vantreek-a")) ||
		m.ToHMAC != hex.EncodeToString(k.RefHMAC(h, survivor)) {
		t.Errorf("merge %+v", m)
	}
	if ref, err := k.OpenRef(h, m.ToEnc); err != nil || ref != survivor {
		t.Errorf("survivor sealed as %q (%v)", ref, err)
	}
	if f.Kind != "forget" || f.RefHMAC != hex.EncodeToString(k.RefHMAC(h, "emr-index-vantreek-c")) || f.ToHMAC != "" || f.ToEnc != nil {
		t.Errorf("forget %+v", f)
	}
	if !strings.Contains(string(b), `"changes":[`) {
		t.Errorf("body %s", b)
	}

	// Without keys, rows fail closed; no rows is an empty list.
	if _, err := buildChanges(nil, h, []ChangeRow{{Kind: "excluded", Ref: "x"}}); !errors.Is(err, errNoKeys) {
		t.Errorf("no keys: %v", err)
	}
	if b, err := buildChanges(nil, h, nil); err != nil || string(b) != `{"changes":[]}` {
		t.Errorf("no rows: %s %v", b, err)
	}
	for _, bad := range []ChangeRow{{Kind: "renamed", Ref: "x"}, {Kind: "merged", Ref: "x"}, {Kind: "excluded", Ref: ""}} {
		if _, err := buildChanges(k, h, []ChangeRow{bad}); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// Departure 79: without Ingest (ANALYSIS_INGEST) a switched-on hospital
// never reads the source, and the run goes on.
func TestNightlyIngestOffByDefault(t *testing.T) {
	h := ids(1)
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true}},
		rows: []SourceRow{{Ref: "emr-index-0001", Day: testNow, Kind: "consultation", Sex: "female"}}}
	n := &Nightly{Store: s, Keys: testKeys(t), Clock: fixedClock(testNow), Lock: noLock}
	if ran, err := n.Tick(context.Background()); err != nil || len(ran) != 1 {
		t.Fatalf("ran %v (%v)", ran, err)
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c, "ingest ") {
			t.Fatalf("ingest ran with Ingest off: %v", s.calls)
		}
	}
}

// Departure 89: the changes run for every due hospital, whatever its
// switch, after ingest and before the sweep; a failure is the step
// "changes", and the hospital isn't marked ran.
func TestNightlyAppliesChangesWhateverTheSwitch(t *testing.T) {
	h := ids(3)
	s := &fakeStore{due: []Due{{Hospital: h[0], On: true}, {Hospital: h[1], On: true, Demo: true}, {Hospital: h[2]}},
		changes: []ChangeRow{{Kind: "excluded", Ref: "emr-index-0009"}},
		fail:    map[string]error{"changes " + h[2].String(): errors.New("ref emr-index-0009")}}
	n := &Nightly{Store: s, Keys: testKeys(t), Clock: fixedClock(testNow), Lock: noLock, Ingest: true}
	ran, err := n.Tick(context.Background())
	if err == nil || len(ran) != 2 {
		t.Fatalf("ran %v (%v)", ran, err)
	}
	got := strings.Join(s.calls, "\n")
	for _, want := range []string{
		"ingest " + h[0].String() + "\nchanges " + h[0].String() + "\nsweep " + h[0].String(),
		"changes " + h[1].String() + "\nsweep " + h[1].String(),
		"changes " + h[2].String(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("calls\n%s\nlack\n%s", got, want)
		}
	}
	if strings.Contains(got, "ran "+h[2].String()) || strings.Contains(got, "sweep "+h[2].String()) {
		t.Errorf("the failed hospital went on: %s", got)
	}
	if len(s.changed) != 2 || bytes.Contains(s.changed[0], []byte("emr-index")) {
		t.Errorf("changes built %s", s.changed)
	}
}
