package codes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// The code systems the normaliser reads as LOINC or ATC as given (§9.1).
const (
	LOINCSystem = "http://loinc.org"
	ATCSystem   = "http://www.whocc.no/atc"
)

type codeKey struct {
	conn         uuid.UUID
	system, code string
}

type analyte struct {
	canonical string
	from      map[string]float64
}

// Maps are the reviewed per-connection code maps, the unit table and the
// conversions, as loaded at start.
type Maps struct {
	version  string
	loinc    map[codeKey]string
	atc      map[codeKey]string
	units    map[string]string
	analytes map[string]analyte
}

type mapFile struct {
	Version    string `yaml:"version"`
	Hospital   string `yaml:"hospital"`
	Connection string `yaml:"connection"`
	Entries    []struct {
		System  string `yaml:"system"`
		Code    string `yaml:"code"`
		LOINC   string `yaml:"loinc"`
		ATC     string `yaml:"atc"`
		Display string `yaml:"display"`
	} `yaml:"entries"`
}

type unitsFile struct {
	Version string `yaml:"version"`
	Units   []struct {
		UCUM    string   `yaml:"ucum"`
		Aliases []string `yaml:"aliases"`
	} `yaml:"units"`
}

type conversionsFile struct {
	Version    string   `yaml:"version"`
	ReviewedBy []string `yaml:"reviewed_by"`
	Analytes   []struct {
		LOINC       string `yaml:"loinc"`
		Canonical   string `yaml:"canonical"`
		Conversions []struct {
			From   string  `yaml:"from"`
			Factor float64 `yaml:"factor"`
		} `yaml:"conversions"`
	} `yaml:"analytes"`
}

// ErrMap: a map, unit or conversion file is not usable (fixed text plus
// the file's name and the entry's index; never a patient value).
var ErrMap = errors.New("codes: map refused")

func decode(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %s unreadable", ErrMap, filepath.Base(path))
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%w: %s is not a valid map file", ErrMap, filepath.Base(path))
	}
	return nil
}

// LoadMaps reads dir: common/units.yaml, common/conversions.yaml and every
// other *.yaml as a code map. A map naming a code not in t (or deprecated)
// is refused; so are unreviewed conversions and anything under testdata/
// outside development. Version is a hash of every file read.
func LoadMaps(dir string, t *Tables, development bool) (*Maps, error) {
	m := &Maps{loinc: map[codeKey]string{}, atc: map[codeKey]string{}, units: map[string]string{}, analytes: map[string]analyte{}}
	var files []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" && !development {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%w: the maps directory is unreadable", ErrMap)
	}
	slices.Sort(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%w: %s unreadable", ErrMap, rel)
		}
		h.Write([]byte(filepath.ToSlash(rel) + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	m.version = "m-" + hex.EncodeToString(h.Sum(nil))[:16]

	var uf unitsFile
	if err := decode(filepath.Join(dir, "common", "units.yaml"), &uf); err != nil {
		return nil, err
	}
	for i, u := range uf.Units {
		if u.UCUM == "" {
			return nil, fmt.Errorf("%w: units.yaml entry %d has no UCUM code", ErrMap, i)
		}
		for _, a := range append([]string{u.UCUM}, u.Aliases...) {
			if prev, dup := m.units[a]; dup && prev != u.UCUM {
				return nil, fmt.Errorf("%w: units.yaml entry %d: an alias means two units", ErrMap, i)
			}
			m.units[a] = u.UCUM
		}
	}
	ucum := func(s string) bool {
		for _, u := range uf.Units {
			if u.UCUM == s {
				return true
			}
		}
		return false
	}

	var cf conversionsFile
	if err := decode(filepath.Join(dir, "common", "conversions.yaml"), &cf); err != nil {
		return nil, err
	}
	if len(cf.ReviewedBy) == 0 && !development {
		return nil, ErrSynthetic
	}
	for i, a := range cf.Analytes {
		if _, ok := t.LOINC(a.LOINC); !ok || !ucum(a.Canonical) {
			return nil, fmt.Errorf("%w: conversions.yaml analyte %d: code or canonical unit unknown", ErrMap, i)
		}
		if _, dup := m.analytes[a.LOINC]; dup {
			return nil, fmt.Errorf("%w: conversions.yaml analyte %d listed twice", ErrMap, i)
		}
		an := analyte{canonical: a.Canonical, from: map[string]float64{}}
		for j, c := range a.Conversions {
			if !ucum(c.From) || c.From == a.Canonical || !(c.Factor > 0) || math.IsInf(c.Factor, 0) {
				return nil, fmt.Errorf("%w: conversions.yaml analyte %d conversion %d", ErrMap, i, j)
			}
			an.from[c.From] = c.Factor
		}
		m.analytes[a.LOINC] = an
	}

	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		if strings.HasPrefix(filepath.ToSlash(rel), "common/") {
			continue
		}
		var mf mapFile
		if err := decode(f, &mf); err != nil {
			return nil, err
		}
		conn, err := uuid.Parse(mf.Connection)
		if err != nil || mf.Version == "" || mf.Hospital == "" {
			return nil, fmt.Errorf("%w: %s needs version, hospital and a connection id", ErrMap, rel)
		}
		for i, e := range mf.Entries {
			k := codeKey{conn: conn, system: e.System, code: e.Code}
			bad := e.System == "" || e.Code == "" || (e.LOINC == "") == (e.ATC == "")
			if !bad && e.LOINC != "" {
				_, ok := t.LOINC(e.LOINC)
				prev, dup := m.loinc[k]
				bad = !ok || (dup && prev != e.LOINC)
				m.loinc[k] = e.LOINC
			}
			if !bad && e.ATC != "" {
				_, ok := t.ATC(e.ATC)
				prev, dup := m.atc[k]
				bad = !ok || (dup && prev != e.ATC)
				m.atc[k] = e.ATC
			}
			if bad {
				return nil, fmt.Errorf("%w: %s entry %d: needs one known, current code", ErrMap, rel, i)
			}
		}
	}
	return m, nil
}

// Version is the maps' version, recorded on every canonical row as
// mapping_version.
func (m *Maps) Version() string { return m.version }

// LOINC maps a connection's local code to LOINC. By code only: there is no
// lookup by display text.
func (m *Maps) LOINC(conn uuid.UUID, system, code string) (string, bool) {
	l, ok := m.loinc[codeKey{conn, system, code}]
	return l, ok
}

// ATC maps a connection's local drug code to ATC.
func (m *Maps) ATC(conn uuid.UUID, system, code string) (string, bool) {
	a, ok := m.atc[codeKey{conn, system, code}]
	return a, ok
}

// ParseUnit is the UCUM code a source unit string means, from units.yaml
// only: an exact match after trimming spaces, never a guess.
func (m *Maps) ParseUnit(s string) (string, bool) {
	u, ok := m.units[strings.TrimSpace(s)]
	return u, ok
}

// Canonical is the analyte's canonical unit.
func (m *Maps) Canonical(loinc string) (string, bool) {
	a, ok := m.analytes[loinc]
	return a.canonical, ok
}

// Convert puts value, in fromUCUM, into the analyte's canonical unit by the
// reviewed factor. No listed conversion: no value (A9).
func (m *Maps) Convert(loinc string, value float64, fromUCUM string) (float64, bool) {
	a, ok := m.analytes[loinc]
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if fromUCUM == a.canonical {
		return value, true
	}
	f, ok := a.from[fromUCUM]
	if !ok {
		return 0, false
	}
	return value * f, true
}
