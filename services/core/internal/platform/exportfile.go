package platform

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// An export's file (contract §14.9; plan Task 25, departures 62-66): built
// from ids, codes and the export record only, so the same record and
// snapshot always give the same bytes. Never a name, a note or anything a
// person typed except the reference, whose characters the record limits.
// Every cell that a spreadsheet could read as a formula is prefixed with
// an apostrophe.

// exportHeader is what the file's first lines say: who made it, when,
// why, its filters and its row count.
type exportHeader struct {
	ID        uuid.UUID
	Kind      string
	By        uuid.UUID
	At        time.Time
	Reason    string
	Reference *string
	Filters   map[string]any // as recorded
	Rows      int
}

// namedExportRow is one row of a named-access export: the entry's
// position, fingerprint, time and actor, the list's letter, purpose and
// period, and the pid the hospital saw.
type namedExportRow struct {
	Ref, Hash, Purpose, Level, Pid string
	Seq                            int64
	At                             time.Time
	Actor, Clearance               uuid.UUID
	Start                          time.Time
}

// letterCopy is one page of a cited letter's copy.
type letterCopy struct {
	Letter uuid.UUID
	Page   int
	PNG    []byte
}

// errNoCopy: a cited letter has no copy core can include (not a demo
// letter; the documents service keeps live letters, H5).
var errNoCopy = errors.New("platform: a cited letter has no copy")

// zipTime is every ZIP entry's time: fixed, so a rebuilt file is the same.
var zipTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// cell neutralises a value a spreadsheet could take as a formula.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r|%", rune(s[0])) {
		return "'" + s
	}
	return s
}

func writeRow(w *csv.Writer, vals ...string) {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = cell(v)
	}
	_ = w.Write(out) // a bytes.Buffer never fails; Flush reports any error
}

// writeHeader writes the key-value rows and the blank row.
func writeHeader(w *csv.Writer, h exportHeader) {
	writeRow(w, "export_id", h.ID.String())
	writeRow(w, "kind", h.Kind)
	writeRow(w, "made_by", h.By.String())
	writeRow(w, "made_at", h.At.UTC().Format(time.RFC3339))
	writeRow(w, "reason", h.Reason)
	if h.Reference != nil {
		writeRow(w, "reference", *h.Reference)
	}
	keys := make([]string, 0, len(h.Filters))
	for k := range h.Filters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		writeRow(w, "filter_"+k, filterValue(h.Filters[k]))
	}
	writeRow(w, "rows", strconv.Itoa(h.Rows))
	writeRow(w, "capped", "false")
	writeRow(w, "")
}

func filterValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = fmt.Sprint(p)
		}
		return strings.Join(parts, "|")
	case []string:
		return strings.Join(x, "|")
	}
	return ""
}

// eventsCSV is an events export: the header, then one line per entry.
func eventsCSV(h exportHeader, rows []audit.ExportEvent) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	writeHeader(w, h)
	writeRow(w, "seq", "hash", "at", "actor_id", "hospital_id", "type", "kind", "target_kind", "target_id", "outcome", "flag")
	for _, r := range rows {
		writeRow(w, strconv.FormatInt(r.Seq, 10), r.Hash, r.At, r.ActorID, r.HospitalID, r.Type, r.Kind, r.TargetKind, r.TargetID,
			r.Outcome, r.Flag)
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

// namedCSV is a named-access export's table.
func namedCSV(h exportHeader, hospital uuid.UUID, rows []namedExportRow) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	writeHeader(w, h)
	writeRow(w, "export_row_ref", "seq", "hash", "at", "actor_id", "hospital_id", "clearance_id", "purpose", "period", "pseudonym")
	for _, r := range rows {
		writeRow(w, r.Ref, strconv.FormatInt(r.Seq, 10), r.Hash, r.At.UTC().Format(time.RFC3339Nano), r.Actor.String(),
			hospital.String(), r.Clearance.String(), r.Purpose, r.Level+":"+r.Start.Format(time.DateOnly), r.Pid)
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

// namedZip is a named-access export: the CSV and each cited letter's
// pages, stored (not compressed) with fixed times, in a fixed order.
func namedZip(table []byte, letters []letterCopy) ([]byte, error) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	add := func(name string, data []byte) error {
		f, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: zipTime})
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	if err := add("named-access.csv", table); err != nil {
		return nil, err
	}
	sorted := slices.Clone(letters)
	slices.SortFunc(sorted, func(a, c letterCopy) int {
		if x := strings.Compare(a.Letter.String(), c.Letter.String()); x != 0 {
			return x
		}
		return a.Page - c.Page
	})
	for _, l := range sorted {
		if len(l.PNG) == 0 {
			return nil, errNoCopy
		}
		if err := add(fmt.Sprintf("letters/%s/page-%d.png", l.Letter, l.Page), l.PNG); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// checksum is the file's SHA-256, lowercase hex.
func checksum(file []byte) string {
	s := sha256.Sum256(file)
	return hex.EncodeToString(s[:])
}

// exportFileName is the contract's suggested name.
func exportFileName(kind string, id uuid.UUID, at time.Time) string {
	k, ext := "events", "csv"
	if kind == "named_access" {
		k, ext = "named-access", "zip"
	}
	return "audit-" + k + "-" + id.String() + "-" + at.UTC().Format("20060102-1504") + "Z." + ext
}
