package audit_test

import (
	"encoding/base64"
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// The list's type, outcome and flagged filters run in SQL, over the same
// conditions Classify evaluates in Go. TestClassifyAgreesWithMap holds
// Classify against Map, so a catalogue row that changes its type, outcome
// or flag without the reader's conditions fails here, not in the list.
func TestClassifyAgreesWithMap(t *testing.T) {
	rng := rand.New(rand.NewSource(20261104))
	pool := candidates()
	actions := make([]string, 0, len(audit.Catalogue))
	for a := range audit.Catalogue {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		fields := audit.SchemaFields(a)
		sort.Strings(fields)
		required := audit.SchemaRequired(a)
		accepted := map[string][]any{}
		for _, f := range fields {
			for _, c := range pool {
				if audit.FieldAccepts(a, f, c) {
					accepted[f] = append(accepted[f], c)
				}
			}
		}
		for i := 0; i < 500; i++ {
			payload := map[string]any{}
			for _, f := range fields {
				vals := accepted[f]
				if len(vals) == 0 || (!in(required, f) && rng.Intn(2) == 0) {
					continue
				}
				payload[f] = vals[rng.Intn(len(vals))]
			}
			ev := audit.Event{Action: a, Payload: payload}
			m, ok := audit.Map(ev)
			c := audit.Classify(ev)
			if c.Mapped != ok {
				t.Fatalf("%s %v: Classify mapped=%v, Map ok=%v", a, payload, c.Mapped, ok)
			}
			if !ok {
				if c.Type != audit.UnknownType {
					t.Fatalf("%s %v: an unmapped row classifies as %s, want %s", a, payload, c.Type, audit.UnknownType)
				}
				continue
			}
			if c.Type != m.Type || c.Outcome != m.Outcome || c.Flagged != (m.Flag != nil) {
				t.Fatalf("%s %v: Classify %+v, Map %s/%s flag %v", a, payload, c, m.Type, m.Outcome, m.Flag)
			}
		}
	}
	// An action with no row is the bare entry's type, unmapped.
	if c := audit.Classify(audit.Event{Action: "test.made_up"}); c.Mapped || c.Type != audit.UnknownType {
		t.Errorf("an action with no row classifies as %+v", c)
	}
}

// The batch gate in SQL is the catalogue's When for the job rows: held
// against When directly, since no schema takes job_count until Task 12.
func TestBatchGateAgreesWithWhen(t *testing.T) {
	job := "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"
	gated := 0
	for a, e := range audit.Catalogue {
		if e.When == nil {
			continue
		}
		gated++
		for _, ev := range []audit.Event{
			{Action: a, Payload: map[string]any{"job_count": 3}},
			{Action: a, Payload: map[string]any{"job_count": 3, "job_id": job}},
			{Action: a, Payload: map[string]any{"job_id": job}},
			{Action: a, Payload: map[string]any{}},
			{Action: a, Entity: "job", EntityID: job, Payload: map[string]any{"job_count": 3}},
			{Action: a, EntityID: job, Payload: map[string]any{"job_count": 3}},
		} {
			if got, want := audit.GateHolds(ev), e.When(ev); got != want {
				t.Errorf("%s %+v: the reader's gate says %v, the catalogue's When %v", a, ev, got, want)
			}
		}
	}
	if gated == 0 {
		t.Fatal("no gated row: the test proves nothing")
	}
}

func TestCursorBoundToFilters(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	yes := true
	f := audit.Filters{Hospital: "platform", Types: []string{"sign_in", "audit"}, Outcome: "refused", Flagged: &yes, From: &from}
	c := audit.EncodeCursor(42, f)
	if _, err := base64.RawURLEncoding.DecodeString(c); err != nil {
		t.Fatalf("cursor %q is not base64url: %v", c, err)
	}
	got, err := audit.DecodeCursor(c, f)
	if err != nil || got != 42 {
		t.Fatalf("round trip: %d, %v", got, err)
	}
	// The same filters in another order are the same filters.
	same := audit.Filters{Hospital: "platform", Types: []string{"audit", "sign_in", "audit"}, Outcome: "refused", Flagged: &yes,
		From: func() *time.Time { x := from.In(time.FixedZone("x", 3600)); return &x }()}
	if got, err := audit.DecodeCursor(c, same); err != nil || got != 42 {
		t.Errorf("equivalent filters: %d, %v", got, err)
	}
	no := false
	for name, other := range map[string]audit.Filters{
		"no filters":      {},
		"other hospital":  {Hospital: "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", Types: f.Types, Outcome: "refused", Flagged: &yes, From: &from},
		"other type":      {Hospital: "platform", Types: []string{"sign_in"}, Outcome: "refused", Flagged: &yes, From: &from},
		"other outcome":   {Hospital: "platform", Types: f.Types, Outcome: "failed", Flagged: &yes, From: &from},
		"flagged false":   {Hospital: "platform", Types: f.Types, Outcome: "refused", Flagged: &no, From: &from},
		"flagged unset":   {Hospital: "platform", Types: f.Types, Outcome: "refused", From: &from},
		"no from":         {Hospital: "platform", Types: f.Types, Outcome: "refused", Flagged: &yes},
		"from as to":      {Hospital: "platform", Types: f.Types, Outcome: "refused", Flagged: &yes, To: &from},
		"with a search q": {Hospital: "platform", Types: f.Types, Outcome: "refused", Flagged: &yes, From: &from, Q: "42"},
	} {
		if _, err := audit.DecodeCursor(c, other); !errors.Is(err, audit.ErrCursor) {
			t.Errorf("%s: want ErrCursor, got %v", name, err)
		}
	}
	for _, bad := range []string{"", "x", "!!!!", c + "A", c[:len(c)-1], strings.Repeat("A", 32),
		audit.EncodeCursor(0, f), audit.EncodeCursor(-5, f), base64.StdEncoding.EncodeToString([]byte(c))} {
		if _, err := audit.DecodeCursor(bad, f); !errors.Is(err, audit.ErrCursor) {
			t.Errorf("cursor %q: want ErrCursor, got %v", bad, err)
		}
	}
}

// The filters as recorded on audit.viewed fit the action's schema, and
// never carry q.
func TestRecordedFiltersFitSchema(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.FixedZone("x", 3600))
	to := from.Add(time.Hour)
	yes := true
	for _, f := range []audit.Filters{
		{},
		{Hospital: "platform"},
		{Hospital: "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", Person: "0b6c6d4e-8a9f-4f6e-b2d3-1c2a3b4c5d6e",
			Types: audit.AuditTypes, Outcome: "failed", Flagged: &yes, From: &from, To: &to},
	} {
		payload := map[string]any{"list": "events", "filters": f.Recorded()}
		if err := audit.CheckActionPayload("audit.viewed", payload); err != nil {
			t.Errorf("%+v: recorded filters don't fit audit.viewed: %v", f, err)
		}
		if _, ok := f.Recorded()["q"]; ok {
			t.Errorf("%+v: q recorded", f)
		}
	}
}
