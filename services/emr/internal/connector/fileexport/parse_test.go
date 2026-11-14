package fileexport

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

// Synthetic MRN namespace.
const testNS = "urn:synthetic:csv-mrn"

type parsed struct {
	rows []Row
	err  error
}

func parse(t *testing.T, kind, data string) parsed {
	t.Helper()
	var p parsed
	for row, err := range Parse(kind, strings.NewReader(data), testNS) {
		if err != nil {
			p.err = err
			break
		}
		p.rows = append(p.rows, row)
	}
	return p
}

func fileReason(err error) string {
	var fe *FileError
	if errors.As(err, &fe) {
		return fe.Reason
	}
	return ""
}

func header(kind string) string { return strings.Join(Columns[kind], ",") + "\n" }

// A good results row, with cells replaced by name.
func resultRow(over map[string]string) string {
	cells := map[string]string{"source_result_id": "SYNR-T-1", "source_patient_id": "SYNP-T-1",
		"collected_at": "2026-11-14T05:10:00+01:00", "updated_at": "2026-11-14T05:35:00+01:00", "status": "final",
		"local_code": "K", "local_name": "Potassium (synthetic)", "value": "6.83", "unit": "mmol/L"}
	for k, v := range over {
		cells[k] = v
	}
	var out []string
	for _, c := range Columns["results"] {
		v := cells[c]
		if strings.ContainsAny(v, ",\"\n") {
			v = `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
		}
		out = append(out, v)
	}
	return strings.Join(out, ",") + "\n"
}

// The header must be the kind's, exactly: no column missing, extra,
// renamed, reordered or in another case; a file of an unknown kind is
// refused.
func TestHeaderExact(t *testing.T) {
	cols := Columns["results"]
	bad := map[string]string{
		"missing":   strings.Join(cols[:len(cols)-1], ","),
		"extra":     strings.Join(append(append([]string{}, cols...), "note"), ","),
		"reordered": strings.Join(append([]string{cols[1], cols[0]}, cols[2:]...), ","),
		"case":      strings.ToUpper(strings.Join(cols, ",")),
		"spaced":    strings.Join(cols, ", "),
		"empty":     "",
	}
	for name, h := range bad {
		if p := parse(t, "results", h+"\n"+resultRow(nil)); fileReason(p.err) != ReasonHeader || len(p.rows) != 0 {
			t.Errorf("%s header: %v, %d rows", name, p.err, len(p.rows))
		}
	}
	if p := parse(t, "labs", header("results")); fileReason(p.err) != ReasonKind {
		t.Errorf("unknown kind: %v", p.err)
	}
	if p := parse(t, "results", header("results")+resultRow(nil)); p.err != nil || len(p.rows) != 1 {
		t.Fatalf("good file: %v %d", p.err, len(p.rows))
	}
}

// A10: a time without an offset is never read in a guessed zone; a date
// alone isn't a time; seconds are required. The row is a dead letter, not
// a record.
func TestCSVNoOffsetRefused(t *testing.T) {
	for _, v := range []string{"2026-11-14T05:10:00", "2026-11-14 05:10:00+01:00", "2026-11-14", "2026-11-14T05:10+01:00",
		"2026-11-14T05:10:00+0100", "14/11/2026 05:10", "2026-13-14T05:10:00Z", "2026-11-14T25:10:00Z"} {
		for _, col := range []string{"collected_at", "updated_at", "resulted_at"} {
			p := parse(t, "results", header("results")+resultRow(map[string]string{col: v}))
			if p.err != nil || len(p.rows) != 1 || p.rows[0].Reject == nil || p.rows[0].Reject.Reason != RowTime || len(p.rows[0].Records) != 0 {
				t.Errorf("%s %q: %+v %v", col, v, p.rows, p.err)
			}
		}
	}
	for _, v := range []string{"2026-11-14T05:10:00Z", "2026-11-14T05:10:00.123+01:00", "2026-11-14T05:10:00-05:00"} {
		p := parse(t, "results", header("results")+resultRow(map[string]string{"collected_at": v}))
		if p.err != nil || p.rows[0].Reject != nil || p.rows[0].Records[0].Fields["effective"] != v {
			t.Errorf("%q: %+v %v", v, p.rows, p.err)
		}
	}
}

// A7: limits refuse the whole file, never part of it: a file over 50 MB, a
// row over 8 KB, more than 200,000 rows.
func TestCSVLimitsRefuseWhole(t *testing.T) {
	long := resultRow(map[string]string{"ref_text": strings.Repeat("x", MaxRow)})
	if p := parse(t, "results", header("results")+resultRow(nil)+long); fileReason(p.err) != ReasonRowTooLong {
		t.Errorf("long row: %v", p.err)
	}
	var b strings.Builder
	b.WriteString(header("results"))
	for i := 0; i <= MaxRows; i++ {
		b.WriteString(resultRow(map[string]string{"source_result_id": "SYNR-T-" + itoa(i)}))
	}
	if p := parse(t, "results", b.String()); fileReason(p.err) != ReasonTooMany {
		t.Errorf("too many rows: %v", p.err)
	}
	big := header("results") + strings.Repeat(resultRow(nil), MaxFile/len(resultRow(nil))+2)
	if p := parse(t, "results", big); fileReason(p.err) != ReasonTooLarge || len(p.rows) != 0 {
		t.Errorf("too large: %v, %d rows", p.err, len(p.rows))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for ; i > 0; i /= 10 {
		d = append([]byte{byte('0' + i%10)}, d...)
	}
	return string(d)
}

// The file's shape: UTF-8 without a byte order mark or NUL, RFC 4180
// quoting, every row the header's width. Each refuses the whole file.
func TestCSVShapeRefusesWhole(t *testing.T) {
	good := header("results") + resultRow(nil)
	cases := map[string]struct{ data, reason string }{
		"latin-1":     {header("results") + resultRow(map[string]string{"local_name": "caf\xe9"}), ReasonEncoding},
		"bom":         {"\xef\xbb\xbf" + good, ReasonEncoding},
		"nul":         {good + "\x00", ReasonEncoding},
		"short row":   {good + "SYNR-T-2,SYNP-T-1\n", ReasonShape},
		"bare quote":  {header("results") + strings.Replace(resultRow(nil), "Potassium", `Pot"assium`, 1), ReasonShape},
		"bad id":      {header("results") + resultRow(map[string]string{"source_result_id": "=1+2"}), ReasonSourceID},
		"empty id":    {header("results") + resultRow(map[string]string{"source_result_id": ""}), ReasonSourceID},
		"spaced id":   {header("results") + resultRow(map[string]string{"source_result_id": "SYNR T"}), ReasonSourceID},
		"conflicting": {good + resultRow(map[string]string{"value": "7.1"}), ReasonDuplicate},
	}
	for name, c := range cases {
		if p := parse(t, "results", c.data); fileReason(p.err) != c.reason {
			t.Errorf("%s: %v, want %s", name, p.err, c.reason)
		}
	}
	// The same row twice is one record.
	if p := parse(t, "results", good+resultRow(nil)); p.err != nil || len(p.rows) != 1 {
		t.Errorf("repeated row: %v %d", p.err, len(p.rows))
	}
}

// A7, A8: a text cell a spreadsheet would read as a formula is neutralised
// with a leading apostrophe, and markup and control characters are taken
// out; codes and ids can't start like a formula at all.
func TestCSVFormulaCells(t *testing.T) {
	p := parse(t, "results", header("results")+resultRow(map[string]string{"local_name": "=HYPERLINK(\"http://x\")",
		"ref_text": "@SUM(A1)", "value": "", "value_text": "+Positive"}))
	if p.err != nil || p.rows[0].Reject != nil {
		t.Fatalf("%+v %v", p.rows, p.err)
	}
	f := p.rows[0].Records[0].Fields
	cs := f["codings"].([]connector.Coding)
	if cs[0].Display != `'=HYPERLINK("http://x")` || f["ref_text"] != "'@SUM(A1)" || f["value_text"] != "'+Positive" {
		t.Errorf("not neutralised: %+v", f)
	}
	p = parse(t, "results", header("results")+resultRow(map[string]string{"local_name": "Potassium<script>x</script>‮"}))
	if d := p.rows[0].Records[0].Fields["codings"].([]connector.Coding)[0].Display; strings.ContainsAny(d, "<>‮") {
		t.Errorf("markup kept: %q", d)
	}
	for col, v := range map[string]string{"local_code": "=K", "unit": "=cmd|' /C calc'!A0", "source_patient_id": "-SYNP"} {
		p := parse(t, "results", header("results")+resultRow(map[string]string{col: v}))
		if p.err != nil || p.rows[0].Reject == nil {
			t.Errorf("%s %q kept: %+v %v", col, v, p.rows, p.err)
		}
	}
}

// A row whose values aren't the format's is a dead letter with its
// identity only; the other rows of the file are records.
func TestRowDeadLetters(t *testing.T) {
	bad := map[string]map[string]string{
		"status":         {"status": "done"},
		"no status":      {"status": ""},
		"no patient":     {"source_patient_id": ""},
		"no code":        {"local_code": ""},
		"comma decimal":  {"value": "6,83"},
		"exponent":       {"value": "6.8e1"},
		"unit in value":  {"value": "6.83 mmol/L"},
		"spaced value":   {"value": " 6.83"},
		"unicode minus":  {"value": "−3"},
		"nan":            {"value": "NaN"},
		"both values":    {"value": "6.83", "value_text": "high"},
		"flag":           {"flag": "CRIT"},
		"ref comparator": {"ref_low": "<3.5"},
		"long name":      {"local_name": strings.Repeat("n", 201)},
	}
	for name, over := range bad {
		over["source_result_id"] = "SYNR-BAD"
		data := header("results") + resultRow(nil) + resultRow(over)
		p := parse(t, "results", data)
		if p.err != nil || len(p.rows) != 2 || p.rows[0].Reject != nil || p.rows[1].Reject == nil {
			t.Errorf("%s: %+v %v", name, p.rows, p.err)
			continue
		}
		rj := p.rows[1].Reject
		if rj.Kind != "observation" || rj.SourceID != "SYNR-BAD" || len(rj.SourceVersion) != 64 || rj.SourceUpdatedAt.IsZero() {
			t.Errorf("%s: reject %+v", name, rj)
		}
	}
	for _, v := range []string{"6.83", "-3", "<0.5", ">=100", "0"} {
		p := parse(t, "results", header("results")+resultRow(map[string]string{"value": v}))
		if p.rows[0].Reject != nil || p.rows[0].Records[0].Fields["value_quantity"] != v {
			t.Errorf("value %q: %+v", v, p.rows[0])
		}
	}
}

// A row's version is its content: the same row again is the same version,
// a change is a new one.
func TestVersionIsContent(t *testing.T) {
	a := parse(t, "results", header("results")+resultRow(nil)).rows[0].Records[0]
	b := parse(t, "results", header("results")+resultRow(nil)).rows[0].Records[0]
	c := parse(t, "results", header("results")+resultRow(map[string]string{"status": "amended", "value": "5.02"})).rows[0].Records[0]
	if a.SourceVersion != b.SourceVersion || a.SourceVersion == c.SourceVersion || a.SourceID != c.SourceID {
		t.Fatalf("versions %s %s %s", a.SourceVersion, b.SourceVersion, c.SourceVersion)
	}
}

// The published sample files parse whole, into the allow-list's fields.
func TestSampleFiles(t *testing.T) {
	want := map[string]int{"patients": 3, "encounters": 3, "results": 4, "vitals": 2, "medications": 1}
	files, err := filepath.Glob("testdata/sample/*.csv")
	if err != nil || len(files) != len(want) {
		t.Fatalf("samples %v %v", files, err)
	}
	for _, f := range files {
		k, ok := parseName(filepath.Base(f))
		if !ok {
			t.Fatalf("%s: not a file name the connector reads", f)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var recs []connector.RawRecord
		for row, err := range Parse(k.kind, bytes.NewReader(data), testNS) {
			if err != nil || row.Reject != nil {
				t.Fatalf("%s line %d: %v %+v", f, row.Line, err, row.Reject)
			}
			recs = append(recs, row.Records...)
		}
		if len(recs) != want[k.kind] {
			t.Errorf("%s: %d records, want %d", k.kind, len(recs), want[k.kind])
		}
		switch k.kind {
		case "patients":
			ids := recs[0].Fields["identifier"].([]connector.Coding)
			if recs[0].Kind != "patient" || ids[0].System != testNS || ids[0].Code != "SYN-CSV-770001" ||
				recs[0].Fields["given"] != "Synthia" || recs[0].Fields["active"] != true || recs[2].Fields["identifier"] != nil {
				t.Errorf("patients: %+v", recs)
			}
		case "encounters":
			if recs[0].Kind != "location" || recs[0].Fields["name"] != "Synthetic Ward Seven" || recs[1].Fields["location"] != "SYNL-WARD-7" {
				t.Errorf("encounters: %+v", recs)
			}
		case "results":
			r := recs[0]
			if r.Fields["category"] != "laboratory" || r.Fields["interpretation"] != "HH" || r.Fields["value_quantity"] != "6.83" ||
				r.Fields["codings"].([]connector.Coding)[0].System != ResultSystem || r.Fields["patient"] != "SYNP-CSV-0001" {
				t.Errorf("results: %+v", r)
			}
			if recs[2].Fields["value_text"] != "Positive (synthetic)" || recs[2].Fields["value_quantity"] != nil {
				t.Errorf("text result: %+v", recs[2])
			}
		case "vitals":
			if recs[0].Fields["category"] != "vital-signs" || recs[0].Fields["effective"] != "2026-11-14T05:30:00+01:00" {
				t.Errorf("vitals: %+v", recs[0])
			}
		case "medications":
			if recs[0].Kind != "medication" || recs[0].Fields["kind"] != "request" || recs[0].Fields["dose_value"] != "40" {
				t.Errorf("medications: %+v", recs[0])
			}
		}
	}
}

// Without an MRN namespace no MRN is kept, as for FHIR (design §10.2).
func TestNoNamespaceNoMRN(t *testing.T) {
	data, err := os.ReadFile("testdata/sample/patients-20261114T060000Z.csv")
	if err != nil {
		t.Fatal(err)
	}
	for row := range Parse("patients", bytes.NewReader(data), "") {
		if _, ok := row.Records[0].Fields["identifier"]; ok {
			t.Fatalf("identifier kept without a namespace: %+v", row.Records[0])
		}
	}
}

// Only <kind>-<UTC time>[-<seq>].csv is read, in export order then kind
// order; anything else (a patient's name, another extension, a path) is not.
func TestFileNames(t *testing.T) {
	for _, n := range []string{"results-20261114T060000Z.csv", "patients-20261114T060000Z-12.csv"} {
		if _, ok := parseName(n); !ok {
			t.Errorf("%s refused", n)
		}
	}
	for _, n := range []string{"Synthia Testperson.csv", "results.csv", "results-20261114T060000Z.CSV", "results-20261114T060000.csv",
		"../results-20261114T060000Z.csv", "results-20261314T060000Z.csv", "labs-20261114T060000Z.csv", "results-20261114T060000Z.csv.part"} {
		if _, ok := parseName(n); ok {
			t.Errorf("%s accepted", n)
		}
	}
	a, _ := parseName("results-20261114T060000Z.csv")
	b, _ := parseName("patients-20261114T060000Z.csv")
	c, _ := parseName("patients-20261114T070000Z.csv")
	if a.less(b) <= 0 || b.less(c) >= 0 || a.less(c) >= 0 {
		t.Fatal("order: export time, then kind")
	}
}
