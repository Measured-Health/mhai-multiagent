package scheduler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/scheduler/schedtest"
)

// Plan 7c Task 4: the snapshot builder and the scheduler, over the fake
// source. Every id, name and count is synthetic.

var rawSmall = regexp.MustCompile(`"value":\s*[1-4]\s*[,}]`)

func at(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// fixture is a fake with three hospitals created before the epoch and
// counts in every week from the epoch to the clock, small ones included.
func fixture(clock, epoch time.Time) (*schedtest.Fake, []uuid.UUID) {
	f := schedtest.New(clock, epoch)
	var ids []uuid.UUID
	for i, name := range []string{"Synthetic Alpha", "Synthetic Beta", "Synthetic Gamma"} {
		id := uuid.New()
		ids = append(ids, id)
		f.Hosp = append(f.Hosp, scheduler.Hospital{ID: id, Name: name, Status: "active", CreatedAt: epoch.AddDate(0, -2, 0)})
		f.StaffN[id] = int64(7 + i)
	}
	n := int64(0)
	for w := calendar.Of(calendar.Week, epoch); w.From.Before(clock); w = calendar.Next(w) {
		for i, id := range ids {
			n++
			r := scheduler.FrozenRow{Clinicians: 3 + int64(i)}
			r.QG[0] = 7 + n%9        // pass
			r.QG[2] = int64(i) + n%3 // regenerate, small
			r.QG[4] = 2 * int64(i)   // escalate
			r.QG[5] = n % 2          // escalate degraded
			r.QG[6] = int64(i % 2)   // unanswered
			r.R[0], r.R[1] = 6, n%4  // reviews
			f.Set(w, id, r)
		}
	}
	return f, ids
}

func sched(f *schedtest.Fake, log *slog.Logger) scheduler.Scheduler {
	return scheduler.Scheduler{Builder: scheduler.Builder{Src: f}, Lock: schedtest.NoLock, Log: log}
}

func TestBuildStoresGuardedOnly(t *testing.T) {
	f, _ := fixture(at(2026, 12, 1).Add(time.Hour), at(2026, 10, 1))
	done, err := sched(f, nil).Tick(context.Background())
	if err != nil || len(done) == 0 {
		t.Fatalf("tick: %v (%d stored)", err, len(done))
	}
	for _, s := range f.All() {
		if s.State != scheduler.StateOK {
			t.Errorf("%v stored %s", s.Period, s.State)
			continue
		}
		if m := rawSmall.Find(s.Body); m != nil {
			t.Errorf("%v: a small count stored: %s", s.Period, m)
		}
		for _, raw := range []string{"q_pass", "unanswered", "r_agreed", "agreed", "corrected", "reviews_decided", "degraded_"} {
			if bytes.Contains(s.Body, []byte(raw)) {
				t.Errorf("%v: a raw-count field %q stored", s.Period, raw)
			}
		}
		if _, err := analytics.DecodeBody(s.Body, s.Period); err != nil {
			t.Errorf("%v: the stored body doesn't decode: %v", s.Period, err)
		}
	}
}

// On 1 Jan 00:15 the week 22–31 Dec, December and Q4 are built in that
// order, and the quarter sees December frozen.
func TestSchedulerOrderWeekMonthQuarter(t *testing.T) {
	f, _ := fixture(at(2027, 1, 1).Add(15*time.Minute), at(2026, 10, 1))
	done, err := sched(f, nil).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := len(done)
	if n < 3 || done[n-3].Kind != calendar.Week || !done[n-3].From.Equal(at(2026, 12, 22)) ||
		done[n-2].Kind != calendar.Month || !done[n-2].From.Equal(at(2026, 12, 1)) ||
		done[n-1].Kind != calendar.Quarter || !done[n-1].From.Equal(at(2026, 10, 1)) {
		t.Fatalf("built %v", done)
	}
	for i := 1; i < n; i++ {
		if done[i].To.Before(done[i-1].To) {
			t.Fatalf("not oldest first: %v then %v", done[i-1], done[i])
		}
	}
	q, ok := f.Snapshot(done[n-1])
	if !ok || q.State != scheduler.StateOK {
		t.Fatalf("Q4: %+v", q)
	}
	idx := map[string]int{}
	for i, k := range f.Freezes {
		idx[k] = i
	}
	if idx["month 2026-12-01"] > idx["quarter 2026-10-01"] || idx["week 2026-12-22"] > idx["month 2026-12-01"] {
		t.Errorf("freeze order %v", f.Freezes)
	}
	// At 00:14 nothing of the last week is due yet.
	f2, _ := fixture(at(2027, 1, 1).Add(14*time.Minute), at(2026, 10, 1))
	done, _ = sched(f2, nil).Tick(context.Background())
	for _, p := range done {
		if p.To.Equal(at(2027, 1, 1)) {
			t.Errorf("%v built inside the grace", p)
		}
	}
}

// A missed week is built on the next wake; a week never frozen and out of
// reach is stored unavailable with its parents; with the epoch mid-month,
// that month and its quarter are absent (no row), and the next full month
// and quarter are built.
func TestSchedulerCatchUpAndUnavailable(t *testing.T) {
	ctx := context.Background()
	epoch := at(2026, 4, 10) // mid-April: April and Q2 are never published
	f, _ := fixture(at(2026, 10, 1).Add(time.Hour), epoch)
	// The scheduler was down until 1 Oct: everything before 5 Jun (118 days)
	// can't be frozen any more.
	f.FailOnce["week 2026-09-08"] = schedtest.ErrInsert
	s := sched(f, nil)
	if _, err := s.Tick(ctx); err == nil {
		t.Fatal("a failed insert wasn't reported")
	}
	if _, ok := f.Snapshot(calendar.Of(calendar.Week, at(2026, 9, 8))); ok {
		t.Fatal("the failed week is stored")
	}
	if _, ok := f.Snapshot(calendar.Of(calendar.Month, at(2026, 9, 1))); ok {
		t.Fatal("a month was built after its failed week")
	}
	if _, err := s.Tick(ctx); err != nil {
		t.Fatalf("second wake: %v", err)
	}
	for _, c := range []struct {
		p     calendar.Period
		state string // "" absent
	}{
		{calendar.Of(calendar.Week, at(2026, 4, 1)), ""},             // started before the epoch
		{calendar.Of(calendar.Week, at(2026, 4, 8)), ""},             // started before the epoch
		{calendar.Of(calendar.Week, at(2026, 4, 15)), "unavailable"}, // eligible, out of reach
		{calendar.Of(calendar.Month, at(2026, 4, 1)), ""},
		{calendar.Of(calendar.Quarter, at(2026, 4, 1)), ""},
		{calendar.Of(calendar.Month, at(2026, 5, 1)), "unavailable"},
		{calendar.Of(calendar.Month, at(2026, 6, 1)), "unavailable"}, // its first week is out of reach
		{calendar.Of(calendar.Week, at(2026, 6, 8)), "ok"},
		{calendar.Of(calendar.Month, at(2026, 7, 1)), "ok"},
		{calendar.Of(calendar.Week, at(2026, 9, 8)), "ok"}, // caught up
		{calendar.Of(calendar.Month, at(2026, 9, 1)), "ok"},
		{calendar.Of(calendar.Quarter, at(2026, 7, 1)), "ok"},
	} {
		s, ok := f.Snapshot(c.p)
		switch {
		case c.state == "" && ok:
			t.Errorf("%v stored (%s), want absent", c.p, s.State)
		case c.state != "" && (!ok || s.State != c.state):
			t.Errorf("%v: %v %s, want %s", c.p, ok, s.State, c.state)
		}
	}
	// A third wake has nothing to do.
	if done, err := s.Tick(ctx); err != nil || len(done) != 0 {
		t.Errorf("third wake built %v (%v)", done, err)
	}
}

// A forced failure logs the period and a fixed reason; no number from the
// data.
func TestBuilderLogsNoCounts(t *testing.T) {
	f, ids := fixture(at(2026, 11, 2), at(2026, 10, 1))
	w := calendar.Of(calendar.Week, at(2026, 10, 8))
	f.Set(w, ids[0], scheduler.FrozenRow{QG: [7]int64{7777, 0, 3, 0, 0, 0, 0}, R: [2]int64{4321, 0}, Clinicians: 9999})
	f.FailOnce[schedtest.Key(w)] = schedtest.ErrInsert
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := sched(f, log).Tick(context.Background()); err == nil {
		t.Fatal("no failure")
	}
	out := buf.String()
	for _, n := range []string{"7777", "4321", "9999", ids[0].String()} {
		if strings.Contains(out, n) {
			t.Errorf("the log holds %s: %s", n, out)
		}
	}
	if !strings.Contains(out, "reason=internal_error") || !strings.Contains(out, "from=2026-10-08") {
		t.Errorf("the log lacks the period or the reason: %s", out)
	}
}

// A29: no route package reaches the scheduler: internal/platform (and
// everything inside the module it imports) never imports
// internal/analytics/scheduler, calls db.WithAnalyticsJob or names
// mhai_analytics_job (the database lets the core login SET ROLE to it).
func TestRoutesCantReachSchedulerPool(t *testing.T) {
	const mod = "github.com/Measured-Health/mhai-multiagent/services/core/"
	root := filepath.Join("..", "..", "..")
	seen := map[string]bool{}
	var walk func(pkg, via string)
	walk = func(pkg, via string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		if pkg == "internal/analytics/scheduler" || strings.HasPrefix(pkg, "internal/analytics/scheduler/") {
			t.Errorf("%s reaches the scheduler (%s)", via, pkg)
			return
		}
		files, _ := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if pkg != "internal/db" && bytes.Contains(src, []byte("WithAnalyticsJob")) {
				t.Errorf("%s calls WithAnalyticsJob (from %s)", file, via)
			}
			// Review fix 2: the core login may SET ROLE to the scheduler's
			// role, so no route-reachable file may name it at all.
			if pkg != "internal/db" && bytes.Contains(src, []byte("mhai_analytics_job")) {
				t.Errorf("%s names mhai_analytics_job (from %s)", file, via)
			}
			fs := token.NewFileSet()
			af, err := parser.ParseFile(fs, file, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range af.Imports {
				p, _ := strconv.Unquote(im.Path.Value)
				if strings.HasPrefix(p, mod) {
					walk(strings.TrimPrefix(p, mod), via+" → "+strings.TrimPrefix(p, mod))
				}
			}
		}
	}
	for _, pkg := range []string{"internal/platform", "internal/analytics"} {
		walk(pkg, pkg)
	}
	if !seen["internal/smallgroup"] && !seen["internal/db"] {
		t.Fatal("the walk read nothing")
	}
}

// Review fix 3: a median review time not shown reads the same whether no
// correction or 1 to 4 were decided (no {null,false} beside {null,true}).
// Review fix 4: the staff count is under the small-group rule (Q4 open):
// 3 is hidden, 9 is shown.
func TestMedianAndStaffHideAlike(t *testing.T) {
	clock, epoch := at(2026, 11, 2), at(2026, 10, 1)
	f := schedtest.New(clock, epoch)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		f.Hosp = append(f.Hosp, scheduler.Hospital{ID: id, Name: "Synthetic " + string(rune('A'+i)), Status: "active", CreatedAt: epoch.AddDate(0, -1, 0)})
	}
	f.StaffN[ids[0]], f.StaffN[ids[1]], f.StaffN[ids[2]] = 3, 9, 0
	median := 12.5
	for w := calendar.Of(calendar.Week, epoch); w.From.Before(clock); w = calendar.Next(w) {
		f.Set(w, ids[0], scheduler.FrozenRow{QG: [7]int64{20}, Clinicians: 5})                              // decided 0
		f.Set(w, ids[1], scheduler.FrozenRow{QG: [7]int64{20}, Clinicians: 5, Decided: 3, Median: &median}) // decided 3
		f.Set(w, ids[2], scheduler.FrozenRow{QG: [7]int64{20}, Clinicians: 5, Decided: 6, Median: &median}) // decided 6
	}
	if _, err := sched(f, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, ok := f.Snapshot(calendar.Of(calendar.Week, at(2026, 10, 8)))
	if !ok {
		t.Fatal("no week stored")
	}
	var body struct {
		Hospitals []map[string]json.RawMessage `json:"hospitals"`
	}
	if err := json.Unmarshal(st.Body, &body); err != nil {
		t.Fatal(err)
	}
	row := map[string]map[string]json.RawMessage{}
	for _, h := range body.Hospitals {
		var ref struct{ ID string }
		_ = json.Unmarshal(h["hospital"], &ref)
		row[ref.ID] = h
	}
	a, b, c := row[ids[0].String()], row[ids[1].String()], row[ids[2].String()]
	if string(a["median_review_hours"]) != string(b["median_review_hours"]) {
		t.Errorf("median: 0 decided %s, 3 decided %s: a reader can tell them apart", a["median_review_hours"], b["median_review_hours"])
	}
	if string(c["median_review_hours"]) != `{"value":12.5,"suppressed":false}` {
		t.Errorf("median of 6 decided: %s", c["median_review_hours"])
	}
	if string(a["staff"]) != `{"value":null,"suppressed":true}` || string(b["staff"]) != `{"value":9,"suppressed":false}` ||
		string(c["staff"]) != `{"value":0,"suppressed":false}` {
		t.Errorf("staff 3, 9, 0: %s %s %s", a["staff"], b["staff"], c["staff"])
	}
}
