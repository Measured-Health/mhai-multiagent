package fileexport

// The mhai_csv_v1 parser (plan Task 43). RFC 4180, UTF-8 without a byte
// order mark, the kind's header exactly, every row the header's width;
// file, row and row-count limits; each row mapped to raw records through
// the allow-list's field names. Text cells go through sink.CleanText and
// sink.CSVCell (A7, A8), as the sink does again.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"iter"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
)

// FileError refuses a file whole. Reason is closed (the constants below);
// Line is the 1-based line of the row at fault, 0 for the file itself.
// Neither names a cell's content.
type FileError struct {
	Reason string
	Line   int
}

func (e *FileError) Error() string { return "fileexport: file refused: " + e.Reason }

// The reasons a file is refused whole.
const (
	ReasonTooLarge   = "too_large"
	ReasonRowTooLong = "row_too_long"
	ReasonTooMany    = "too_many_rows"
	ReasonEncoding   = "encoding"
	ReasonHeader     = "header"
	ReasonShape      = "shape"
	ReasonSourceID   = "source_id"
	ReasonDuplicate  = "duplicate"
	ReasonKind       = "kind"
)

// The reasons a row is kept only as a dead letter.
const (
	RowTime     = "time"
	RowCode     = "code"
	RowValue    = "value"
	RowRequired = "required"
	RowTooLong  = "too_long"
)

// Reject is a row kept only as a dead letter: its record's identity, never
// its content.
type Reject struct {
	Kind, SourceID, SourceVersion string
	SourceUpdatedAt               time.Time
	Reason                        string
}

// Row is one data row: the raw records it maps to (an encounter row may
// also give its location), or why it is a dead letter.
type Row struct {
	Line    int
	Records []connector.RawRecord
	Reject  *Reject
}

// rawKind is the raw record kind of each file kind.
var rawKind = map[string]string{"patients": "patient", "encounters": "encounter", "results": "observation",
	"vitals": "observation", "medications": "medication"}

// Parse reads one file of kind from r (at most MaxFile bytes are read).
// It yields each data row in order; a *FileError ends the sequence, and the
// caller must then store nothing of the file (Fetch reads the whole
// sequence once before storing). mrnSystem is the connection's MRN
// namespace: the mrn column is kept as an identifier in it, or not at all.
func Parse(kind string, r io.Reader, mrnSystem string) iter.Seq2[Row, error] {
	data, err := io.ReadAll(io.LimitReader(r, MaxFile+1))
	if err != nil {
		return func(yield func(Row, error) bool) { yield(Row{}, &FileError{Reason: ReasonShape}) }
	}
	return ParseBytes(kind, data, mrnSystem)
}

// ParseBytes is Parse over data already read, without a copy: the
// connector reads a file once and parses it twice (security review).
func ParseBytes(kind string, data []byte, mrnSystem string) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		fail := func(reason string, line int) { yield(Row{}, &FileError{Reason: reason, Line: line}) }
		cols, ok := Columns[kind]
		if !ok {
			fail(ReasonKind, 0)
			return
		}
		if len(data) > MaxFile {
			fail(ReasonTooLarge, 0)
			return
		}
		if !utf8.Valid(data) || bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) || bytes.IndexByte(data, 0) >= 0 {
			fail(ReasonEncoding, 0)
			return
		}
		cr := csv.NewReader(bytes.NewReader(data))
		cr.FieldsPerRecord = len(cols)
		cr.ReuseRecord = true
		header, err := cr.Read()
		if err != nil || !slices.Equal(header, cols) {
			fail(ReasonHeader, 1)
			return
		}
		last := cr.InputOffset()
		seen := map[string]string{} // source id -> version
		rows := 0
		for {
			rec, err := cr.Read()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				line := 0
				var pe *csv.ParseError
				if errors.As(err, &pe) {
					line = pe.Line
				}
				fail(ReasonShape, line)
				return
			}
			line, _ := cr.FieldPos(0)
			off := cr.InputOffset()
			if off-last > MaxRow {
				fail(ReasonRowTooLong, line)
				return
			}
			last = off
			if rows++; rows > MaxRows {
				fail(ReasonTooMany, line)
				return
			}
			row := map[string]string{}
			for i, c := range cols {
				row[c] = rec[i]
			}
			id := rec[0]
			if !idShape.MatchString(id) {
				fail(ReasonSourceID, line)
				return
			}
			version := versionOf(kind, rec)
			if v, dup := seen[id]; dup {
				if v != version {
					fail(ReasonDuplicate, line)
					return
				}
				continue // the same row twice: once is enough
			}
			seen[id] = version
			out := mapRow(kind, row, id, version, mrnSystem)
			out.Line = line
			if !yield(out, nil) {
				return
			}
		}
	}
}

// versionOf is the row's content hash: an unchanged row exported again is
// the same version (a no-op in the sink); a changed row is a new one. Each
// cell is length-prefixed, so no two different rows hash alike by moving a
// separator between cells (security review).
func versionOf(kind string, rec []string) string {
	h := sha256.New()
	h.Write([]byte(Format + "\x00" + kind))
	for _, c := range rec {
		h.Write(binary.AppendUvarint(nil, uint64(len(c))))
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rowReader checks one row's cells; the first problem found is its reject
// reason.
type rowReader struct {
	row    map[string]string
	reason string
	fields map[string]any
}

func (r *rowReader) bad(reason string) {
	if r.reason == "" {
		r.reason = reason
	}
}

func (r *rowReader) need(col string) string {
	v := r.row[col]
	if v == "" {
		r.bad(RowRequired)
	}
	return v
}

// id puts an optional reference under key.
func (r *rowReader) id(col, key string, required bool) string {
	v := r.row[col]
	switch {
	case v == "" && required:
		r.bad(RowRequired)
	case v != "" && !idShape.MatchString(v):
		r.bad(RowCode)
	case v != "":
		r.fields[key] = v
	}
	return v
}

func (r *rowReader) time(col, key string, required bool) time.Time {
	v := r.row[col]
	if v == "" {
		if required {
			r.bad(RowRequired)
		}
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if !timeShape.MatchString(v) || err != nil {
		r.bad(RowTime)
		return time.Time{}
	}
	if key != "" {
		r.fields[key] = v
	}
	return t.UTC()
}

func (r *rowReader) date(col, key string) {
	v := r.row[col]
	if v == "" {
		return
	}
	if _, err := time.Parse("2006-01-02", v); !dateShape.MatchString(v) || err != nil {
		r.bad(RowTime)
		return
	}
	r.fields[key] = v
}

func (r *rowReader) closed(col, key string, set []string, required bool) string {
	v := r.row[col]
	switch {
	case v == "" && required:
		r.bad(RowRequired)
	case v != "" && !slices.Contains(set, v):
		r.bad(RowCode)
	case v != "" && key != "":
		r.fields[key] = v
	}
	return v
}

// text puts a display cell, cleaned and neutralised, under key; one longer
// than max (after cleaning) is a dead letter, never cut.
func (r *rowReader) text(col, key string, max int) string {
	v := r.row[col]
	if v == "" {
		return ""
	}
	c := sink.CSVCell(sink.CleanText(v, -1))
	if utf8.RuneCountInString(c) > max {
		r.bad(RowTooLong)
		return ""
	}
	if c != "" && key != "" {
		r.fields[key] = c
	}
	return c
}

func (r *rowReader) shaped(col, key string, re *regexp.Regexp) {
	v := r.row[col]
	if v == "" {
		return
	}
	if !re.MatchString(v) {
		r.bad(RowValue)
		return
	}
	r.fields[key] = v
}

// unit puts a unit under key when the sink will keep it whole: at most 40
// bytes of printable UTF-8 (no control, format or space character), never
// starting like a formula. Units such as µmol/L stay.
func (r *rowReader) unit(col, key string) {
	v := r.row[col]
	if v == "" {
		return
	}
	if !unitOK(v) {
		r.bad(RowValue)
		return
	}
	r.fields[key] = v
}

func unitOK(v string) bool {
	if len(v) > 40 || !utf8.ValidString(v) || strings.ContainsAny(v[:1], "=+-@") {
		return false
	}
	for _, c := range v {
		if unicode.IsSpace(c) || unicode.Is(unicode.Cc, c) || unicode.Is(unicode.Cf, c) || c == utf8.RuneError {
			return false
		}
	}
	return true
}

func (r *rowReader) code(col string, required bool) string {
	v := r.row[col]
	switch {
	case v == "" && required:
		r.bad(RowRequired)
	case v != "" && !codeShape.MatchString(v):
		r.bad(RowCode)
	}
	return v
}

// mapRow maps a row whose id is good to its raw records, or to a dead
// letter.
func mapRow(kind string, row map[string]string, id, version, mrnSystem string) Row {
	r := &rowReader{row: row, fields: map[string]any{}}
	updated := r.time("updated_at", "", true)
	var extra []connector.RawRecord
	switch kind {
	case "patients":
		if mrn := r.row["mrn"]; mrn != "" {
			if !mrnShape.MatchString(mrn) {
				r.bad(RowCode)
			} else if mrnSystem != "" {
				r.fields["identifier"] = []connector.Coding{{System: mrnSystem, Code: mrn}}
			}
		}
		r.text("given_name", "given", 60)
		r.text("family_name", "family", 60)
		r.date("birth_date", "birth_date")
		r.closed("sex", "sex", sexes, false)
		if a := r.closed("active", "", actives, false); a != "" {
			r.fields["active"] = a == "true"
		}
		if rb := r.id("replaced_by", "link_replaced_by", false); rb == id {
			r.bad(RowCode)
		}
	case "encounters":
		r.id("source_patient_id", "patient", true)
		r.closed("class", "class", encounterClass, true)
		r.closed("status", "status", encounterStatus, true)
		r.time("start_at", "period_start", false)
		r.time("end_at", "period_end", false)
		r.text("bed_label", "bed_label", 20)
		if loc := r.id("source_location_id", "location", false); loc != "" {
			lr := &rowReader{row: row, fields: map[string]any{}}
			if lr.text("location_name", "name", 60) == "" {
				r.bad(RowRequired)
			}
			lr.closed("location_type", "type", locationTypes, false)
			r.bad(lr.reason)
			extra = append(extra, connector.RawRecord{Kind: "location", SourceID: loc,
				SourceVersion:   versionOf("location", []string{loc, row["location_name"], row["location_type"]}),
				SourceUpdatedAt: updated, Fields: lr.fields})
		} else if row["location_name"] != "" || row["location_type"] != "" {
			r.bad(RowRequired)
		}
	case "results", "vitals":
		category, system, at, issued := "laboratory", ResultSystem, "collected_at", "resulted_at"
		if kind == "vitals" {
			category, system, at, issued = "vital-signs", VitalSystem, "measured_at", ""
		}
		r.fields["category"] = category
		r.id("source_patient_id", "patient", true)
		r.id("source_encounter_id", "encounter", false)
		r.time(at, "effective", true)
		if issued != "" {
			r.time(issued, "issued", false)
		}
		r.closed("status", "status", resultStatus, true)
		code := r.code("local_code", true)
		name := r.text("local_name", "", 200)
		if code != "" && r.reason == "" {
			r.fields["codings"] = []connector.Coding{{System: system, Code: code, Display: name}}
		}
		r.shaped("value", "value_quantity", valueShape)
		if kind == "results" {
			r.text("value_text", "value_text", 120)
			if row["value_text"] != "" && row["value"] != "" {
				r.bad(RowValue)
			}
			r.shaped("ref_low", "ref_low", decimalShape)
			r.shaped("ref_high", "ref_high", decimalShape)
			r.text("ref_text", "ref_text", 120)
		}
		r.unit("unit", "unit")
		r.closed("flag", "interpretation", flags, false)
	case "medications":
		r.id("source_patient_id", "patient", true)
		r.closed("kind", "kind", medKinds, true)
		r.closed("status", "status", medStatus, true)
		code := r.code("local_code", false)
		name := r.text("local_name", "med_text", 200)
		if code == "" && name == "" {
			r.bad(RowRequired)
		}
		if code != "" && r.reason == "" {
			r.fields["codings"] = []connector.Coding{{System: MedicationSystem, Code: code, Display: name}}
		}
		r.shaped("dose_value", "dose_value", decimalShape)
		r.unit("dose_unit", "dose_unit")
		r.text("route", "route", 40)
		r.text("timing_text", "timing_text", 60)
		r.time("authored_at", "authored", false)
		r.time("start_at", "period_start", false)
		r.time("end_at", "period_end", false)
	}
	rk := rawKind[kind]
	if r.reason != "" {
		return Row{Reject: &Reject{Kind: rk, SourceID: id, SourceVersion: version, SourceUpdatedAt: updated, Reason: r.reason}}
	}
	rec := connector.RawRecord{Kind: rk, SourceID: id, SourceVersion: version, SourceUpdatedAt: updated, Fields: r.fields}
	return Row{Records: append(extra, rec)}
}
