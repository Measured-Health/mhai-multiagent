package platform

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// Plan Task 25: the export file's cells are never formulas, the file is
// the same bytes when rebuilt, and its name has the contract's shape.

func TestCSVCellsNeutralised(t *testing.T) {
	for in, want := range map[string]string{
		"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")",
		"+1":                "'+1",
		"-2":                "'-2",
		"@SUM(A1)":          "'@SUM(A1)",
		"\tx":               "'\tx",
		"LR-2026-0042":      "LR-2026-0042",
		"":                  "",
	} {
		if got := cell(in); got != want {
			t.Errorf("cell(%q) = %q, want %q", in, got, want)
		}
	}
	ref := "=cmd"
	b, err := eventsCSV(exportHeader{ID: uuid.New(), Kind: "events", By: uuid.New(), At: time.Now(), Reason: "other",
		Reference: &ref, Filters: map[string]any{"type": []any{"export", "audit"}}},
		[]audit.ExportEvent{{Seq: 1, Kind: "@x", Type: "export"}})
	if err != nil {
		t.Fatal(err)
	}
	// The header's rows and the table's differ in width.
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		for _, c := range rec {
			if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
				t.Errorf("a cell starts with a formula character: %q", c)
			}
		}
	}
}

func namedFixture() (exportHeader, uuid.UUID, []namedExportRow, []letterCopy) {
	h := exportHeader{ID: uuid.MustParse("6f1d7c2e-4b3a-4c5d-8e9f-0a1b2c3d4e5f"), Kind: "named_access",
		By: uuid.MustParse("1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"), At: time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC),
		Reason: "legal_request", Filters: map[string]any{"hospital": "9e8d7c6b-5a49-4382-9716-a5b4c3d2e1f0"}, Rows: 2}
	hosp := uuid.MustParse("9e8d7c6b-5a49-4382-9716-a5b4c3d2e1f0")
	letter := uuid.MustParse("3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f")
	rows := []namedExportRow{
		{Ref: "0123456789abcdef01234567", Seq: 41, Hash: strings.Repeat("a", 64), At: h.At, Actor: h.By, Clearance: letter,
			Purpose: "audit", Level: "week", Start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Pid: "P-QZXWVUTS"},
		{Ref: "fedcba9876543210fedcba98", Seq: 41, Hash: strings.Repeat("a", 64), At: h.At, Actor: h.By, Clearance: letter,
			Purpose: "audit", Level: "week", Start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Pid: "P-MNBVCXZL"},
	}
	return h, hosp, rows, []letterCopy{{Letter: letter, Page: 2, PNG: []byte("synthetic-page-2")}, {Letter: letter, Page: 1, PNG: []byte("synthetic-page-1")}}
}

func TestZipDeterministic(t *testing.T) {
	build := func() []byte {
		h, hosp, rows, letters := namedFixture()
		table, err := namedCSV(h, hosp, rows)
		if err != nil {
			t.Fatal(err)
		}
		z, err := namedZip(table, letters)
		if err != nil {
			t.Fatal(err)
		}
		return z
	}
	a := build()
	time.Sleep(1100 * time.Millisecond)
	b := build()
	if !bytes.Equal(a, b) || checksum(a) != checksum(b) {
		t.Fatal("the same export built twice differs")
	}
	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Method != zip.Store {
			t.Errorf("%s is compressed", f.Name)
		}
	}
	want := "named-access.csv,letters/3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f/page-1.png,letters/3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f/page-2.png"
	if strings.Join(names, ",") != want {
		t.Errorf("entries %v", names)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	table, _ := io.ReadAll(rc)
	_ = rc.Close()
	for _, want := range []string{"export_row_ref,seq,hash,at,actor_id,hospital_id,clearance_id,purpose,period,pseudonym",
		"0123456789abcdef01234567,41,", "week:2026-10-01,P-QZXWVUTS", "made_by,1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "capped,false"} {
		if !strings.Contains(string(table), want) {
			t.Errorf("table lacks %q", want)
		}
	}
	// A letter with no copy refuses the whole file.
	h, hosp, rows, _ := namedFixture()
	tbl, _ := namedCSV(h, hosp, rows)
	if _, err := namedZip(tbl, []letterCopy{{Letter: uuid.New(), Page: 1}}); err != errNoCopy {
		t.Errorf("a letter with no copy: %v", err)
	}
}

func TestExportFileName(t *testing.T) {
	shape := regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}$`)
	id := uuid.New()
	at := time.Date(2026, 11, 14, 16, 5, 0, 0, time.UTC)
	for kind, want := range map[string]string{
		"events":       "audit-events-" + id.String() + "-20261114-1605Z.csv",
		"named_access": "audit-named-access-" + id.String() + "-20261114-1605Z.zip",
	} {
		got := exportFileName(kind, id, at)
		if got != want || !shape.MatchString(got) {
			t.Errorf("%s: %q", kind, got)
		}
	}
}
