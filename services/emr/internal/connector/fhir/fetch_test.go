package fhir

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

var fetchNow = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func obsRes(id, lu, patient string) map[string]any {
	return map[string]any{
		"resourceType": "Observation", "id": id, "meta": map[string]any{"versionId": "1", "lastUpdated": lu},
		"status": "final",
		"category": []any{map[string]any{"coding": []any{map[string]any{
			"system": "http://terminology.hl7.org/CodeSystem/observation-category", "code": "laboratory"}}}},
		"code":              map[string]any{"coding": []any{map[string]any{"system": "urn:synthetic:lab", "code": "K", "display": "Potassium"}}, "text": "Potassium"},
		"subject":           map[string]any{"reference": "Patient/" + patient},
		"effectiveDateTime": "2026-11-10T07:30:00+01:00",
		"issued":            "2026-11-10T08:00:00Z",
		"valueQuantity":     map[string]any{"value": json.Number("5.40"), "unit": "mmol/L", "system": "http://unitsofmeasure.org", "code": "mmol/L"},
		"interpretation":    []any{map[string]any{"coding": []any{map[string]any{"code": "H"}}}},
		"referenceRange":    []any{map[string]any{"low": map[string]any{"value": 3.5}, "high": map[string]any{"value": 5.1}, "text": "3.5-5.1"}},
	}
}

func patientRes(id, lu string) map[string]any {
	return map[string]any{
		"resourceType": "Patient", "id": id, "meta": map[string]any{"versionId": "3", "lastUpdated": lu},
		"identifier": []any{map[string]any{"system": "urn:synthetic:mrn", "value": "SYN-000001"}},
		"name":       []any{map[string]any{"given": []any{"Synthia"}, "family": "Testperson"}},
		"gender":     "female", "birthDate": "1970-01-02", "active": true,
	}
}

func newFetcher(f *fakeFHIR) *FHIR {
	c := New(func(connector.Config) *http.Client { return f.srv.Client() })
	c.Now = func() time.Time { return fetchNow }
	return c
}

func cursorOf(t *testing.T, cur connector.Cursor) cursor {
	t.Helper()
	var c cursor
	if err := json.Unmarshal(cur, &c); err != nil {
		t.Fatalf("cursor %s: %v", cur, err)
	}
	return c
}

func searches(reqs []string, typ string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, "GET /r4/"+typ+"?") && !strings.Contains(r, "_id=") {
			n++
		}
	}
	return n
}

// Review Focus 2: many resources share one lastUpdated and a page ends
// among them. The cursor is the last completed page's greatest lastUpdated
// minus the 2-minute overlap, so the next run reads the rest; it never
// moves backward.
func TestCursorEqualTimestampsAcrossPages(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	f.pageSize = 2
	const at = "2026-11-12T08:00:00Z"
	for _, id := range []string{"o-1", "o-2", "o-3", "o-4", "o-5"} {
		f.add("Observation", obsRes(id, at, "p-1"))
	}
	f.add("Patient", patientRes("p-1", "2026-11-01T00:00:00Z"))
	c := newFetcher(f)
	c.MaxPages = 1
	sink := &memSink{}
	cur, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), nil, sink)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	want := time.Date(2026, 11, 12, 7, 58, 0, 0, time.UTC)
	if got := cursorOf(t, cur).at("Observation"); !got.Equal(want) {
		t.Fatalf("run 1 cursor %v, want %v", got, want)
	}
	if c.CaughtUp(cur) {
		t.Fatal("a run stopped by its limit is not caught up")
	}
	c.MaxPages = 10
	cur2, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), cur, sink)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	got := sink.ids("observation")
	slices.Sort(got)
	got = slices.Compact(got)
	if !slices.Equal(got, []string{"o-1", "o-2", "o-3", "o-4", "o-5"}) {
		t.Fatalf("stored %v", got)
	}
	if at2 := cursorOf(t, cur2).at("Observation"); !at2.Equal(want) {
		t.Fatalf("run 2 cursor %v, want %v (never backward, never past)", at2, want)
	}
	if !c.CaughtUp(cur2) {
		t.Fatal("a run that read everything is caught up")
	}
	// An older record seen again (its lastUpdated minus the overlap is
	// before the cursor) never moves the cursor back.
	f.mu.Lock()
	f.res["Observation"] = []map[string]any{obsRes("o-6", "2026-11-12T07:59:00Z", "p-1")}
	f.mu.Unlock()
	cur3, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), cur2, sink)
	if err != nil || !cursorOf(t, cur3).at("Observation").Equal(want) {
		t.Fatalf("run 3: %v %s", err, cur3)
	}
}

// Review Focus 2: a failed page leaves the cursor where the last completed
// page put it, and what the failed page held is not stored; a put that
// fails half-way through a page counts as a failed page.
func TestCursorNotAdvancedOnFailedPage(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	f.pageSize = 2
	for i, lu := range []string{"2026-11-12T07:00:00Z", "2026-11-12T07:10:00Z", "2026-11-12T07:20:00Z", "2026-11-12T07:30:00Z"} {
		f.add("Observation", obsRes("o-"+string(rune('1'+i)), lu, "p-1"))
	}
	f.add("Patient", patientRes("p-1", "2026-11-01T00:00:00Z"))
	f.fail["Observation"] = 1
	c := newFetcher(f)
	sink := &memSink{}
	cur, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), nil, sink)
	if connector.CodeOf(err) != "unreachable" {
		t.Fatalf("a 500 page: %v", err)
	}
	if got := cursorOf(t, cur).at("Observation"); !got.Equal(time.Date(2026, 11, 12, 7, 8, 0, 0, time.UTC)) {
		t.Fatalf("cursor %v, want the first page's 07:10 minus 2 minutes", got)
	}
	if got := sink.ids("observation"); !slices.Equal(got, []string{"o-1", "o-2"}) {
		t.Fatalf("stored %v", got)
	}
	if c.CaughtUp(cur) {
		t.Fatal("a failed run is not caught up")
	}
	// The first page fails: the cursor is the one given, unchanged.
	f.fail["Observation"] = 0
	cur2, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), cur, &memSink{})
	if err == nil || !cursorOf(t, cur2).at("Observation").Equal(cursorOf(t, cur).at("Observation")) {
		t.Fatalf("first page failed: %v %s", err, cur2)
	}
	// The sink refuses the second record of the page: the page is not
	// complete, so the cursor stays.
	delete(f.fail, "Observation")
	half := &memSink{failAfter: 2} // the patient, then o-3
	cur3, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), cur, half)
	if err == nil || !cursorOf(t, cur3).at("Observation").Equal(cursorOf(t, cur).at("Observation")) {
		t.Fatalf("half-stored page: %v %s", err, cur3)
	}
}

// A7: a next link to another host, another base path or another scheme is
// never followed.
func TestNextLinkElsewhereRefused(t *testing.T) {
	key, pub := rsaKey(t)
	for name, link := range map[string]func(f *fakeFHIR) string{
		"other host":      func(*fakeFHIR) string { return "https://evil.example.org/r4/Observation?_offset=1" },
		"other base path": func(f *fakeFHIR) string { return f.srv.URL + "/other/Observation?_offset=1" },
		"prefix trick":    func(f *fakeFHIR) string { return f.srv.URL + "/r4x/Observation?_offset=1" },
		"dot segments":    func(f *fakeFHIR) string { return f.srv.URL + "/r4/../other/Observation?_offset=1" },
		"http": func(f *fakeFHIR) string {
			return strings.Replace(f.base(), "https://", "http://", 1) + "/Observation?_offset=1"
		},
		"userinfo": func(f *fakeFHIR) string {
			return strings.Replace(f.base(), "https://", "https://u:p@", 1) + "/Observation?_offset=1"
		},
		"relative": func(*fakeFHIR) string { return "/other/Observation?_offset=1" },
	} {
		f := newFake(t, pub)
		f.pageSize = 1
		f.add("Observation", obsRes("o-1", "2026-11-12T07:00:00Z", "p-1"))
		f.add("Observation", obsRes("o-2", "2026-11-12T07:10:00Z", "p-1"))
		f.add("Patient", patientRes("p-1", "2026-11-01T00:00:00Z"))
		f.nextLink["Observation"] = link(f)
		sink := &memSink{}
		_, err := newFetcher(f).Fetch(context.Background(), f.cfg(), credsWith(key), nil, sink)
		if connector.CodeOf(err) != "unreadable_data" {
			t.Errorf("%s: %v", name, err)
		}
		for _, r := range f.reqs() {
			if strings.Contains(r, "/other/") || strings.Contains(r, "/r4x/") {
				t.Errorf("%s: followed %s", name, r)
			}
		}
		if got := sink.ids("observation"); !slices.Equal(got, []string{"o-1"}) {
			t.Errorf("%s: stored %v", name, got)
		}
	}
}

// A20: a 429 names how long to wait (capped at 15 minutes); a 5xx is
// unreachable; nothing more is requested in that run.
func TestRetryAfterHonoured(t *testing.T) {
	key, pub := rsaKey(t)
	for name, c := range map[string]struct {
		status int
		ra     string
		code   string
		wait   time.Duration
	}{
		"seconds":   {429, "120", "too_many_requests", 120 * time.Second},
		"capped":    {429, "86400", "too_many_requests", 15 * time.Minute},
		"http date": {429, fetchNow.Add(5 * time.Minute).Format(http.TimeFormat), "too_many_requests", 5 * time.Minute},
		"junk":      {429, "soon", "too_many_requests", 0},
		"negative":  {429, "-5", "too_many_requests", 0},
		"5xx":       {503, "", "unreachable", 0},
		"401":       {401, "", "sign_in_refused", 0},
		"404":       {404, "", "unreadable_data", 0},
	} {
		f := newFake(t, pub)
		f.status["Observation"] = c.status
		f.retryAfter = c.ra
		_, err := newFetcher(f).Fetch(context.Background(), f.cfg(), credsWith(key), nil, &memSink{})
		var fe *connector.FetchError
		if !errors.As(err, &fe) || fe.Code != c.code || fe.RetryAfter != c.wait {
			t.Errorf("%s: %v (%+v)", name, err, fe)
		}
		if n := searches(f.reqs(), "Observation") + searches(f.reqs(), "MedicationRequest") +
			searches(f.reqs(), "MedicationStatement"); n != 1 {
			t.Errorf("%s: %d searches after the refusal", name, n)
		}
	}
}

// A20: at most 20 pages a run; a page with more entries than asked for is
// refused whole.
func TestRunLimits(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	f.pageSize = 1
	for i := 0; i < 30; i++ {
		f.add("Observation", obsRes("o-"+time.Duration(i).String(), fetchNow.Add(-time.Duration(60-i)*time.Minute).Format(time.RFC3339), "p-1"))
	}
	f.add("MedicationRequest", medRes("m-1", "2026-11-12T06:00:00Z", "p-1"))
	f.add("Patient", patientRes("p-1", "2026-11-01T00:00:00Z"))
	c := newFetcher(f)
	cur, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), nil, &memSink{})
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.reqs()
	if n := searches(reqs, "Observation") + searches(reqs, "MedicationRequest") + searches(reqs, "MedicationStatement"); n != 20 {
		t.Fatalf("%d search pages in one run, want 20", n)
	}
	if c.CaughtUp(cur) {
		t.Fatal("caught up after the page limit")
	}
	// The record limit.
	c.MaxRecords = 3
	f2 := newFake(t, pub)
	f2.pageSize = 2
	for i := 0; i < 10; i++ {
		f2.add("Observation", obsRes("o-"+time.Duration(i).String(), fetchNow.Add(-time.Duration(60-i)*time.Minute).Format(time.RFC3339), "p-1"))
	}
	c2 := newFetcher(f2)
	c2.MaxRecords = 3
	sink := &memSink{}
	if _, err := c2.Fetch(context.Background(), f2.cfg(), credsWith(key), nil, sink); err != nil {
		t.Fatal(err)
	}
	if n := len(sink.ids("observation")); n != 4 {
		t.Fatalf("%d records with a limit of 3 and pages of 2, want 4 (the page that crosses it, then stop)", n)
	}
	// More entries than _count: the page is refused, nothing stored.
	f3 := newFake(t, pub)
	f3.pageSize = PageCount
	f3.extra = 1
	for i := 0; i < PageCount+1; i++ {
		f3.add("Observation", obsRes("o-"+time.Duration(i).String(), "2026-11-12T07:00:00Z", "p-1"))
	}
	sink3 := &memSink{}
	if _, err := newFetcher(f3).Fetch(context.Background(), f3.cfg(), credsWith(key), nil, sink3); connector.CodeOf(err) != "unreadable_data" ||
		len(sink3.ids("observation")) != 0 {
		t.Fatalf("oversized page: %v, %d stored", err, len(sink3.ids("observation")))
	}
	// Results out of _lastUpdated order: the server isn't sorting, so the
	// cursor can't be trusted: refused.
	f4 := newFake(t, pub)
	f4.pageSize = 5
	f4.add("Observation", obsRes("o-1", "2026-11-12T07:00:00Z", "p-1"))
	f4.add("Observation", obsRes("o-2", "2026-11-12T06:00:00Z", "p-1"))
	f4.noSort = true
	sink4 := &memSink{}
	if _, err := newFetcher(f4).Fetch(context.Background(), f4.cfg(), credsWith(key), nil, sink4); connector.CodeOf(err) != "unreadable_data" ||
		len(sink4.ids("observation")) != 0 {
		t.Fatalf("unsorted page: %v, %d stored", err, len(sink4.ids("observation")))
	}
}

// The first sync reads back the backfill window; later runs read from the
// cursor.
func TestBackfillWindow(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	c := newFetcher(f)
	cur, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), nil, &memSink{})
	if err != nil {
		t.Fatal(err)
	}
	want := "_lastUpdated=gt" + strings.ReplaceAll(fetchNow.Add(-30*24*time.Hour).Format(time.RFC3339), ":", "%3A")
	for _, typ := range []string{"Observation", "MedicationRequest", "MedicationStatement"} {
		found := false
		for _, r := range f.reqs() {
			if strings.HasPrefix(r, "GET /r4/"+typ+"?") && strings.Contains(r, want) && strings.Contains(r, "_sort=_lastUpdated") &&
				strings.Contains(r, "_count=200") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no backfill search in %v", typ, f.reqs())
		}
	}
	if !strings.Contains(strings.Join(f.reqs(), "\n"), "category=laboratory%2Cvital-signs") {
		t.Errorf("observations not limited to laboratory and vital signs: %v", f.reqs())
	}
	if !c.CaughtUp(cur) {
		t.Fatal("an empty server is caught up")
	}
	// A cursor for another base address is not this server's: backfill.
	held := cursor{V: cursorVersion, Base: f.base(), LU: map[string]string{"Observation": "2026-11-12T08:00:00Z"}}.encode()
	if c.cursorFor(held, f.cfg()).at("Observation").IsZero() {
		t.Fatal("this server's cursor not taken")
	}
	other := f.cfg()
	other.BaseURL = f.srv.URL + "/fhir"
	if !c.cursorFor(held, other).at("Observation").IsZero() {
		t.Fatal("cursor taken for another base address")
	}
}

func medRes(id, lu, patient string) map[string]any {
	return map[string]any{
		"resourceType": "MedicationRequest", "id": id, "meta": map[string]any{"versionId": "1", "lastUpdated": lu},
		"status": "active", "intent": "order",
		"medicationCodeableConcept": map[string]any{"coding": []any{map[string]any{"system": "urn:synthetic:drug", "code": "FUROS"}}, "text": "Loop diuretic one"},
		"subject":                   map[string]any{"reference": "Patient/" + patient},
		"authoredOn":                "2026-11-11T10:00:00Z",
		"dosageInstruction": []any{map[string]any{"route": map[string]any{"text": "oral"},
			"timing":      map[string]any{"code": map[string]any{"text": "twice daily"}},
			"doseAndRate": []any{map[string]any{"doseQuantity": map[string]any{"value": 40, "unit": "mg"}}}}},
	}
}
