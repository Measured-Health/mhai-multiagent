package disclose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// Every constant of the plan's Global Constraints that lives here.
func TestConstantsPinned(t *testing.T) {
	for name, c := range map[string][2]int64{
		"Min":               {Min, 5},
		"smallgroup.Min":    {Min, smallgroup.Min},
		"QuerySetMin":       {QuerySetMin, 10},
		"FilterCap":         {FilterCap, 2},
		"HistoryMonths":     {HistoryMonths, 24},
		"HistoryPoints":     {HistoryPoints, 7},
		"DrillMax":          {DrillMax, 200},
		"PatientCap":        {PatientCap, 300},
		"PairsMax":          {PairsMax, 20},
		"RowItemsMax":       {RowItemsMax, 10},
		"UnusualFilterSets": {UnusualFilterSets, 100},
		"BodyCap":           {BodyCap, 20000},
		"BodyMaxBytes":      {BodyMaxBytes, 4 << 20},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, want %d", name, c[0], c[1])
		}
	}
	for name, c := range map[string][2]time.Duration{
		"Grace":            {Grace, 72 * time.Hour},
		"ColdBody":         {ColdBody, 2 * time.Second},
		"StatementTimeout": {StatementTimeout, 5 * time.Second},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
}

// N13: what Analysis shares with 7c is used from where it lives, never
// copied. One bridge finder, one modular whole-set rule, one exact one, and
// 7c's checker uses the moved exact code.
func TestSharedExportsAreMovesNotCopies(t *testing.T) {
	root := filepath.Join("..", "..") // internal/
	counts := map[string]int{}
	needles := []string{"func bridge(", "func SetViolates(", "func setViolates(", "func Violates(", "func violates(", "func acrossRows(", "type rbasis "}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || filepath.Base(p) == "constants_test.go" {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 -- the repository's own sources
		if err != nil {
			return err
		}
		for _, n := range needles {
			counts[n] += strings.Count(string(b), n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]int{
		"func bridge(": 1, "func SetViolates(": 1, "func setViolates(": 0,
		"func Violates(": 1, "func violates(": 0, "func acrossRows(": 1, "type rbasis ": 0,
	} {
		if counts[n] != want {
			t.Errorf("%q appears %d times in internal/, want %d", n, counts[n], want)
		}
	}
}

// Analysis takes 7c's calendar on civil dates: Of agrees with Parse (the
// period's first day parses back to it) for every day from 2024 to 2030,
// and PeriodOf accepts exactly the first days. The "now" given to Parse is
// fixed, far after every day, so no test reads the real date.
func TestOfAgreesWithParse(t *testing.T) {
	now := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); d.Year() < 2031; d = d.AddDate(0, 0, 1) {
		for _, k := range []calendar.Kind{calendar.Week, calendar.Month, calendar.Quarter} {
			p := calendar.Of(k, d)
			if d.Before(p.From) || !d.Before(p.To) {
				t.Fatalf("%v: %s is outside %v", k, d.Format(time.DateOnly), p)
			}
			q, err := calendar.Parse(string(k), p.From.Format(time.DateOnly), now)
			if err != nil || !q.Equal(p) {
				t.Fatalf("Parse(%v, %v) = %v, %v; Of gave %v", k, p.From, q, err, p)
			}
			got, err := PeriodOf(string(k), d.Format(time.DateOnly))
			if first := d.Equal(p.From); first != (err == nil) || (first && !got.Equal(p)) {
				t.Fatalf("PeriodOf(%v, %s) = %v, %v", k, d.Format(time.DateOnly), got, err)
			}
		}
	}
	for _, bad := range [][2]string{{"day", "2026-10-01"}, {"month", "2026-10-1"}, {"week", "2026-10-05"}, {"quarter", "2026-11-01"}} {
		if _, err := PeriodOf(bad[0], bad[1]); err == nil {
			t.Errorf("PeriodOf%v accepted", bad)
		}
	}
}

// A Body holds guarded values only: no int, int64 or float anywhere in it,
// except inside the guarded types themselves. Row keys are strings.
func TestBodyHasNoRawField(t *testing.T) {
	leaf := map[reflect.Type]bool{
		reflect.TypeOf(smallgroup.Guarded{}):      true,
		reflect.TypeOf(smallgroup.GuardedShare{}): true,
		reflect.TypeOf(GuardedMinutes{}):          true,
		reflect.TypeOf(calendar.Period{}):         true,
	}
	seen := map[reflect.Type]bool{}
	var walk func(path string, t0 reflect.Type)
	walk = func(path string, t0 reflect.Type) {
		if leaf[t0] || seen[t0] {
			return
		}
		seen[t0] = true
		switch t0.Kind() {
		case reflect.Struct:
			for i := 0; i < t0.NumField(); i++ {
				f := t0.Field(i)
				walk(path+"."+f.Name, f.Type)
			}
		case reflect.Slice, reflect.Array, reflect.Pointer:
			walk(path+"[]", t0.Elem())
		case reflect.Map:
			walk(path+"{key}", t0.Key())
			walk(path+"{}", t0.Elem())
		case reflect.String, reflect.Bool:
		default:
			t.Errorf("%s is a %v: a Body may hold no raw number", path, t0)
		}
	}
	walk("Body", reflect.TypeOf(Body{}))
	if !seen[reflect.TypeOf(Body{})] {
		t.Fatal("nothing walked")
	}
}

func reflectNumField(v any) int { return reflect.TypeOf(v).NumField() }
