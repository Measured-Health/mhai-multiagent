package scan

import (
	"archive/zip"
	"bytes"
	"testing"
)

func zipFile(t *testing.T, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("synthetic"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// §5 step 2, B4, DS7: the type is the magic bytes', and must be the
// declared one and one the purpose takes.
func TestTypeByMagicBytes(t *testing.T) {
	pdf := []byte("%PDF-1.7\n")
	png := append([]byte(nil), pngSignature...)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	tiffLE, tiffBE := []byte("II*\x00\x08\x00"), []byte("MM\x00*\x00\x08")
	for _, c := range []struct {
		name, purpose, declared string
		b                       []byte
		want                    bool
	}{
		{"pdf", "clinical", PDF, pdf, true},
		{"png", "clinical", PNG, png, true},
		{"jpeg", "archive", JPEG, jpeg, true},
		{"tiff le", "clinical", TIFF, tiffLE, true},
		{"tiff be", "archive", TIFF, tiffBE, true},
		{"clearance pdf", "clearance", PDF, pdf, true},
		{"clearance tiff", "clearance", TIFF, tiffLE, false},
		{"docx as pdf", "clinical", PDF, zipFile(t, "word/document.xml"), false},
		{"zip as pdf", "clinical", PDF, zipFile(t, "a.txt"), false},
		{"html as pdf", "clinical", PDF, []byte("<!doctype html><html>"), false},
		{"png declared pdf", "clinical", PDF, png, false},
		{"pdf declared png", "clinical", PNG, pdf, false},
		{"pdf not at offset 0", "clinical", PDF, append([]byte(" "), pdf...), false},
		{"bigtiff", "clinical", TIFF, []byte("II+\x00\x08\x00"), false},
		{"empty", "clinical", PDF, nil, false},
		{"unknown purpose", "letter", PDF, pdf, false},
	} {
		if got := TypeAllowed(c.purpose, c.declared, c.b); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPurposeLimits(t *testing.T) {
	if l := PurposeLimits["clinical"]; l.MaxBytes != 25<<20 || l.MaxPages != 50 {
		t.Fatalf("clinical %+v", l)
	}
	if l := PurposeLimits["clearance"]; l.MaxBytes != 10<<20 || l.MaxPages != 20 || l.Allows(TIFF) {
		t.Fatalf("clearance %+v", l)
	}
}
