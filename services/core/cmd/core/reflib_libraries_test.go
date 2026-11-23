package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// Several reference libraries, each with its own id block and approved
// kinds (reflibLibraries). Synthetic rows only.

func testFormularyRow(i int) testReflibRow {
	return testReflibRow{
		ChunkID: fmt.Sprintf("KB#%d", 2000001+i),
		Content: "Synthlonart is a local brand or name used in Nigeria for synthemether-lumefantrine.",
		Metadata: map[string]any{"kind": "local_formulary", "source": "Measured Health", "title": "Synthlonart",
			"licence": "CC0-1.0", "brand": "Synthlonart", "generic": "synthemether-lumefantrine",
			"drug_class": "synthetic class", "confidence": "high", "audience": "clinician",
			"retrieved": "2026-11-23", "attribution": "Synthetic attribution."},
		Embedding: []float64{0, 0, 1, 0},
	}
}

func testGuidelineRow(i int) testReflibRow {
	return testReflibRow{
		ChunkID: fmt.Sprintf("KB#%d", 3000001+i),
		Content: "Synthetic guideline, section 4.2 Synthetic treatment, p. 12: give the synthetic dose.",
		Metadata: map[string]any{"kind": "guideline", "source": "World Health Organization",
			"publisher": "World Health Organization", "document": "who-malaria", "title": "Synthetic: 4.2",
			"section": "4.2 Synthetic treatment", "edition": "synthetic edition", "page_start": 12, "page_end": 12,
			"licence": "synthetic licence", "audience": "clinician", "retrieved": "2026-11-23",
			"attribution": "Synthetic attribution.", "url": "https://example.org/synthetic.pdf"},
		Embedding: []float64{0, 0, 0, 1},
	}
}

func TestReflibAcceptsEachLibraryInItsOwnBlock(t *testing.T) {
	for name, rows := range map[string][]testReflibRow{
		"local-formulary": {testFormularyRow(0), testFormularyRow(1)},
		"who-malaria":     {testGuidelineRow(0)},
	} {
		path, pins := testReflibArtefactNamed(t, name, "1.0.0", rows)
		data, _ := reflibRead(path)
		h, _, err := reflibCheck(data, pins)
		if err != nil || h.Name != name || h.Count != len(rows) {
			t.Fatalf("%s: %+v %v", name, h, err)
		}
	}
}

func TestReflibRefusesARowOutsideItsLibrary(t *testing.T) {
	cases := map[string]struct {
		name string
		row  testReflibRow
		code string
	}{
		"formulary id in medref":       {"medref", func() testReflibRow { r := testReflibLabel(0); r.ChunkID = "KB#2000001"; return r }(), "id_outside_block"},
		"medref id in formulary":       {"local-formulary", func() testReflibRow { r := testFormularyRow(0); r.ChunkID = "KB#1000001"; return r }(), "id_outside_block"},
		"block's zero id":              {"local-formulary", func() testReflibRow { r := testFormularyRow(0); r.ChunkID = "KB#2000000"; return r }(), "id_outside_block"},
		"formulary row in medref":      {"medref", func() testReflibRow { r := testFormularyRow(0); r.ChunkID = "KB#1000001"; return r }(), "row_kind_not_approved"},
		"label in a guideline":         {"who-malaria", func() testReflibRow { r := testReflibLabel(0); r.ChunkID = "KB#3000001"; return r }(), "row_kind_not_approved"},
		"other publisher":              {"ng-malaria", func() testReflibRow { r := testGuidelineRow(0); r.ChunkID = "KB#4000001"; return r }(), "row_kind_not_approved"},
		"patient field in a formulary": {"local-formulary", func() testReflibRow { r := testFormularyRow(0); r.Metadata["patient_name"] = "Synthetic"; return r }(), "row_patient_or_unknown_field"},
		"patient field in a guideline": {"who-malaria", func() testReflibRow { r := testGuidelineRow(0); r.Metadata["source_subject_id"] = "SYN-1"; return r }(), "row_patient_or_unknown_field"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			path, pins := testReflibArtefactNamed(t, c.name, "1.0.0", []testReflibRow{c.row})
			data, _ := reflibRead(path)
			_, _, err := reflibCheck(data, pins)
			wantRefused(t, err, c.code)
		})
	}
}

func TestReflibRefusesAnUnknownLibrary(t *testing.T) {
	path, pins := testReflibArtefactNamed(t, "synthetic-other", "1.0.0", []testReflibRow{testReflibLabel(0)})
	data, _ := reflibRead(path)
	_, _, err := reflibCheck(data, pins)
	wantRefused(t, err, "bad_header")
}

func TestReflibBlocksAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for name, lib := range reflibLibraries {
		if lib.Block < 1 || lib.Block > 9 {
			t.Errorf("%s: block %d", name, lib.Block)
		}
		if other, ok := seen[lib.Block]; ok {
			t.Errorf("%s and %s share block %d", name, other, lib.Block)
		}
		seen[lib.Block] = name
	}
}

// reflibLibraries mirrors reference_library.LIBRARIES (the Python
// converter's table): the same names and blocks.
func TestReflibLibrariesMatchPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "intelligence", "intelligence", "knowledge_base",
		"reference_library.py"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"([a-z0-9-]+)": LibrarySpec\(\s*(\d),`)
	found := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		n, _ := strconv.Atoi(m[2])
		found[m[1]] = n
	}
	if len(found) != len(reflibLibraries) {
		t.Fatalf("python has %v, core %d libraries", found, len(reflibLibraries))
	}
	for name, lib := range reflibLibraries {
		if found[name] != lib.Block {
			t.Errorf("%s: python block %d, core %d", name, found[name], lib.Block)
		}
	}
}

// testdata/{local-formulary,who-malaria}-python-fixture.jsonl.gz are what
// the Python converters write from synthetic inputs (regenerated and
// compared by tests/unit/test_reflib_go_fixtures.py).
func TestReflibCheckReadsThePythonLibraryArtefacts(t *testing.T) {
	for _, name := range []string{"local-formulary", "who-malaria"} {
		data, err := reflibRead(filepath.Join("testdata", name+"-python-fixture.jsonl.gz"))
		if err != nil {
			t.Fatal(err)
		}
		var header reflibHeader
		_ = reflibLines(data, func(n int, line []byte) error {
			if n == 0 {
				header, err = reflibParseHeader(line)
			}
			return nil
		})
		if err != nil || header.Count == 0 {
			t.Fatalf("%s: header %+v %v", name, header, err)
		}
		sum := sha256.Sum256(data)
		pins := map[string]reflibPin{name: {Version: header.Version, SHA256: hex.EncodeToString(sum[:]), Count: header.Count}}
		h, _, err := reflibCheck(data, pins)
		if err != nil || h.Name != name {
			t.Fatalf("%s: %+v %v", name, h, err)
		}
	}
}
