// Package codes holds the code tables, the reviewed code and unit maps and
// the per-analyte conversions (EMR design §9.1, §9.2, §9.4; plan Task 18).
// Everything is read from files at start and held in memory: there is no
// map table in the database, so no runtime role can change a map. Nothing
// is ever matched by display text, and nothing is guessed: a code, unit or
// conversion not listed is unmapped.
package codes

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Entry is one row of a code table.
type Entry struct {
	Code, Display string
	Deprecated    bool
}

// Tables are the LOINC and ATC tables.
type Tables struct {
	loinc, atc map[string]Entry
	// Synthetic is true for the repository's synthetic tables (plan
	// departure 13), refused outside development.
	Synthetic bool
}

// ErrSynthetic: synthetic tables or maps outside development (K10).
var ErrSynthetic = errors.New("codes: synthetic tables or unreviewed conversions are for development only")

var (
	loincShape = regexp.MustCompile(`^[0-9]{1,7}-[0-9]$`)
	atcShape   = regexp.MustCompile(`^[A-Z][0-9]{2}([A-Z]([A-Z]([0-9]{2})?)?)?$`)
)

// Load reads dir/loinc.csv (code, display, status, source) and dir/atc.csv
// (code, display, source). A table with any row whose source is synthetic
// is synthetic, and refused unless development.
func Load(dir string, development bool) (*Tables, error) {
	t := &Tables{loinc: map[string]Entry{}, atc: map[string]Entry{}}
	synth := 0
	if err := readCSV(filepath.Join(dir, "loinc.csv"), []string{"code", "display", "status", "source"}, func(r []string) error {
		if !loincShape.MatchString(r[0]) || (r[2] != "ACTIVE" && r[2] != "DEPRECATED" && r[2] != "DISCOURAGED" && r[2] != "TRIAL") {
			return errors.New("bad LOINC row")
		}
		if _, dup := t.loinc[r[0]]; dup {
			return errors.New("duplicate LOINC code")
		}
		t.loinc[r[0]] = Entry{Code: r[0], Display: r[1], Deprecated: r[2] == "DEPRECATED"}
		if r[3] == "synthetic" {
			synth++
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := readCSV(filepath.Join(dir, "atc.csv"), []string{"code", "display", "source"}, func(r []string) error {
		if !atcShape.MatchString(r[0]) {
			return errors.New("bad ATC row")
		}
		if _, dup := t.atc[r[0]]; dup {
			return errors.New("duplicate ATC code")
		}
		t.atc[r[0]] = Entry{Code: r[0], Display: r[1]}
		if r[2] == "synthetic" {
			synth++
		}
		return nil
	}); err != nil {
		return nil, err
	}
	t.Synthetic = synth > 0
	if t.Synthetic && !development {
		return nil, ErrSynthetic
	}
	return t, nil
}

func readCSV(path string, header []string, row func([]string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("codes: open %s", filepath.Base(path))
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(f)
	r.FieldsPerRecord = len(header)
	h, err := r.Read()
	if err != nil || strings.Join(h, ",") != strings.Join(header, ",") {
		return fmt.Errorf("codes: %s: header must be %s", filepath.Base(path), strings.Join(header, ","))
	}
	for line := 2; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("codes: %s line %d unreadable", filepath.Base(path), line)
		}
		if err := row(rec); err != nil {
			return fmt.Errorf("codes: %s line %d: %w", filepath.Base(path), line, err)
		}
	}
}

// LOINC is the code's entry; a deprecated code reads as unknown.
func (t *Tables) LOINC(code string) (Entry, bool) {
	e, ok := t.loinc[code]
	if !ok || e.Deprecated {
		return Entry{}, false
	}
	return e, true
}

// ATC is the code's entry.
func (t *Tables) ATC(code string) (Entry, bool) {
	e, ok := t.atc[code]
	return e, ok
}
