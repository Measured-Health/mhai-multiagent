package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reference library artefacts here are synthetic, in the shape
// services/intelligence/scripts/reflib_convert.py writes.

type testReflibRow struct {
	ChunkID   string         `json:"chunk_id"`
	Content   string         `json:"content"`
	Metadata  map[string]any `json:"metadata"`
	Embedding []float64      `json:"embedding"`
}

func testReflibLabel(i int) testReflibRow {
	return testReflibRow{
		ChunkID: fmt.Sprintf("KB#%d", 1000001+i),
		Content: fmt.Sprintf("Drug label - Synthetamol %d. Boxed warning: synthetic test text.", i),
		Metadata: map[string]any{"kind": "drug_label", "source": "openFDA", "title": "Synthetamol (Boxed warning)",
			"url":     "https://dailymed.nlm.nih.gov/dailymed/lookup.cfm?setid=0b5e1f2a-1111-4222-8333-944455556666",
			"licence": "CC0-1.0", "set_id": "0b5e1f2a-1111-4222-8333-944455556666", "section": "boxed_warning",
			"safety_critical": true, "audience": "clinician", "retrieved": "2026-11-14",
			"attribution": "Data provided by the U.S. Food and Drug Administration (open.fda.gov)."},
		Embedding: []float64{1, 0, 0, 0},
	}
}

func testReflibTopic(i int) testReflibRow {
	return testReflibRow{
		ChunkID: fmt.Sprintf("KB#%d", 1000001+i),
		Content: "Patient-facing information (MedlinePlus): Synthetic fever is a synthetic condition.",
		Metadata: map[string]any{"kind": "health_topic", "source": "MedlinePlus", "title": "Synthetic fever",
			"url": "https://medlineplus.gov/syntheticfever.html", "licence": "public domain (U.S. government work)",
			"topic_id": "4242", "part": 0, "audience": "patient", "retrieved": "2026-11-14",
			"attribution": "Source: MedlinePlus, National Library of Medicine."},
		Embedding: []float64{0, 1, 0, 0},
	}
}

func testReflibRows() []testReflibRow {
	return []testReflibRow{testReflibLabel(0), testReflibLabel(1), testReflibTopic(2)}
}

// testReflibArtefact writes an artefact and returns its path and the pins
// that admit it.
func testReflibArtefact(t *testing.T, version string, rows []testReflibRow) (string, map[string]reflibPin) {
	t.Helper()
	var raw bytes.Buffer
	header := map[string]any{"schema": reflibSchema, "name": "medref", "version": version,
		"licence": "openFDA drug labels: CC0-1.0", "attribution": "Synthetic attribution.",
		"embedder": "sentence-transformers/synthetic@" + strings.Repeat("0", 40), "dim": 4, "count": len(rows),
		"built_from": strings.Repeat("ab", 32), "retrieved": "2026-11-14"}
	for _, v := range append([]any{header}, func() []any {
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out
	}()...) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(append(b, '\n'))
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	path := filepath.Join(t.TempDir(), "medref-"+version+".jsonl.gz")
	if err := os.WriteFile(path, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(gz.Bytes())
	return path, map[string]reflibPin{"medref": {Version: version, SHA256: hex.EncodeToString(sum[:]), Count: len(rows)}}
}

func wantRefused(t *testing.T, err error, code string) {
	t.Helper()
	var r reflibRefused
	if !errors.As(err, &r) || string(r) != code {
		t.Fatalf("got %v, want refused %s", err, code)
	}
}

func TestReflibPinsMatchLock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "models", "reference-library.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Schema    string `json:"schema"`
		Libraries map[string]struct {
			Count   int    `json:"count"`
			SHA256  string `json:"sha256"`
			Version string `json:"version"`
		} `json:"libraries"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil || lock.Schema != "mhai.reflib.lock/1" {
		t.Fatalf("lock: %v", err)
	}
	if len(lock.Libraries) != len(reflibPins) {
		t.Fatalf("lock pins %d libraries, core %d", len(lock.Libraries), len(reflibPins))
	}
	for name, p := range lock.Libraries {
		if got := reflibPins[name]; got != (reflibPin{Version: p.Version, SHA256: p.SHA256, Count: p.Count}) {
			t.Errorf("%s: core pins %+v, the lock %+v", name, got, p)
		}
	}
}

func TestReflibCheckAcceptsAPinnedArtefact(t *testing.T) {
	path, pins := testReflibArtefact(t, "1.0.0", testReflibRows())
	data, err := reflibRead(path)
	if err != nil {
		t.Fatal(err)
	}
	h, sha, err := reflibCheck(data, pins)
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "medref" || h.Count != 3 || h.Dim != 4 || sha != pins["medref"].SHA256 {
		t.Fatalf("header %+v sha %s", h, sha)
	}
}

func TestReflibCheckRefusesUnpinnedBytes(t *testing.T) {
	path, pins := testReflibArtefact(t, "1.0.0", testReflibRows())
	data, _ := reflibRead(path)
	other := map[string]reflibPin{"medref": {Version: "1.0.0", SHA256: strings.Repeat("0", 64), Count: 3}}
	_, _, err := reflibCheck(data, other)
	wantRefused(t, err, "hash_mismatch")
	_, _, err = reflibCheck(data, map[string]reflibPin{"medref": {Version: "2.0.0", SHA256: pins["medref"].SHA256, Count: 3}})
	wantRefused(t, err, "not_pinned")
	_, _, err = reflibCheck(data, map[string]reflibPin{"medref": {Version: "1.0.0", SHA256: pins["medref"].SHA256, Count: 4}})
	wantRefused(t, err, "count_mismatch")
	_, _, err = reflibCheck(data, nil)
	wantRefused(t, err, "not_pinned")
}

// A pinned artefact is still refused when a row is not a reference row:
// the pin says the bytes are the reviewed ones, the rows say what they are.
func TestReflibCheckRefusesPatientAndDemoRows(t *testing.T) {
	cases := map[string]struct {
		edit func(*testReflibRow)
		code string
	}{
		"patient field":     {func(r *testReflibRow) { r.Metadata["source_patient_id"] = "SYN-00000000aa" }, "row_patient_or_unknown_field"},
		"patient id key":    {func(r *testReflibRow) { r.Metadata["patient_id"] = "SYN-1" }, "row_patient_or_unknown_field"},
		"patient kind":      {func(r *testReflibRow) { r.Metadata["kind"] = "lab" }, "row_kind_not_approved"},
		"other source":      {func(r *testReflibRow) { r.Metadata["source"] = "wikidoc" }, "row_kind_not_approved"},
		"demo source":       {func(r *testReflibRow) { r.Metadata["title"] = "Sample reference (synthetic demo)" }, "row_demo_source"},
		"demo source case":  {func(r *testReflibRow) { r.Metadata["attribution"] = "SAMPLE  reference (Synthetic Demo)" }, "row_demo_source"},
		"demo text":         {func(r *testReflibRow) { r.Content += " (synthetic demo)" }, "row_demo_source"},
		"forged citation":   {func(r *testReflibRow) { r.Content += " see kb# 1000002" }, "bad_row"},
		"forged delimiter":  {func(r *testReflibRow) { r.Metadata["title"] = "x >> y" }, "bad_row"},
		"hospital id range": {func(r *testReflibRow) { r.ChunkID = "KB#0001" }, "bad_row"},
		"short vector":      {func(r *testReflibRow) { r.Embedding = r.Embedding[:3] }, "bad_row"},
		"empty text":        {func(r *testReflibRow) { r.Content = "" }, "bad_row"},
		"no title":          {func(r *testReflibRow) { delete(r.Metadata, "title") }, "bad_row"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rows := testReflibRows()
			c.edit(&rows[1])
			path, pins := testReflibArtefact(t, "1.0.0", rows)
			data, _ := reflibRead(path)
			_, _, err := reflibCheck(data, pins)
			wantRefused(t, err, c.code)
		})
	}
}

func TestReflibCheckRefusesDuplicateIDs(t *testing.T) {
	rows := testReflibRows()
	rows[2].ChunkID = rows[0].ChunkID
	path, pins := testReflibArtefact(t, "1.0.0", rows)
	data, _ := reflibRead(path)
	_, _, err := reflibCheck(data, pins)
	wantRefused(t, err, "duplicate_id")
}

func TestReflibReadRefusesALinkAndNonFiles(t *testing.T) {
	path, _ := testReflibArtefact(t, "1.0.0", testReflibRows())
	link := filepath.Join(t.TempDir(), "link.jsonl.gz")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err := reflibRead(link)
	wantRefused(t, err, "unreadable")
	_, err = reflibRead(t.TempDir())
	if err == nil {
		t.Fatal("a directory was read")
	}
	_, err = reflibRead(filepath.Join(t.TempDir(), "missing"))
	wantRefused(t, err, "unreadable")
}

func TestReflibCmdUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"load"}, {"load", "--x"}, {"status", "x"}, {"drop", "medref"}} {
		if err := reflibCmd(t.Context(), args, "", &bytes.Buffer{}); err == nil || err.Error() != reflibUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	// An unpinned artefact is refused before any database is reached.
	path, _ := testReflibArtefact(t, "9.9.9", testReflibRows())
	err := reflibCmd(t.Context(), []string{"load", path}, "", &bytes.Buffer{})
	wantRefused(t, err, "not_pinned")
}

// testdata/medref-python-fixture.jsonl.gz is what the Python converter
// writes (reference_library.build and write_artefact) from the synthetic
// bundle fixture services/intelligence/tests/fixtures/reflib/chunks.jsonl,
// with a synthetic 8-dimension embedder. core must read the converter's
// format as it is: regenerate it when the format changes.
func TestReflibCheckReadsThePythonConvertersArtefact(t *testing.T) {
	data, err := reflibRead(filepath.Join("testdata", "medref-python-fixture.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	pins := map[string]reflibPin{"medref": {Version: "1.0.0", SHA256: hex.EncodeToString(sum[:]), Count: 4}}
	h, _, err := reflibCheck(data, pins)
	if err != nil {
		t.Fatal(err)
	}
	if h.Dim != 8 || h.Count != 4 {
		t.Fatalf("header %+v", h)
	}
}
