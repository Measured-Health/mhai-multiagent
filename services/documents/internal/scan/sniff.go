package scan

import "bytes"

// The types a document may be (§4.1), as documents.mime_type holds them.
const (
	PDF  = "application/pdf"
	PNG  = "image/png"
	JPEG = "image/jpeg"
	TIFF = "image/tiff"
)

// Limits are a purpose's (§4.1): the largest file, the most pages and the
// types it takes.
type Limits struct {
	MaxBytes int64
	MaxPages int
	Types    []string
}

// Allows reports whether the purpose takes mime.
func (l Limits) Allows(mime string) bool {
	for _, t := range l.Types {
		if t == mime {
			return true
		}
	}
	return false
}

// PurposeLimits are §4.1's, the same as the documents_limits CHECK and
// upload.MaxBytes.
var PurposeLimits = map[string]Limits{
	"clinical":  {MaxBytes: 25 << 20, MaxPages: 50, Types: []string{PDF, PNG, JPEG, TIFF}},
	"archive":   {MaxBytes: 25 << 20, MaxPages: 50, Types: []string{PDF, PNG, JPEG, TIFF}},
	"clearance": {MaxBytes: 10 << 20, MaxPages: 20, Types: []string{PDF, PNG, JPEG}},
}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// Sniff is the file's type from its first bytes (§5 step 2), or "" for
// anything else: a DOCX, ZIP or HTML file, a BigTIFF (II+ or MM\0+), an
// empty file. Only the magic bytes at offset 0 count, never a name or a
// declared type.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return PDF
	case bytes.HasPrefix(b, pngSignature):
		return PNG
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return JPEG
	case bytes.HasPrefix(b, []byte("II*\x00")), bytes.HasPrefix(b, []byte("MM\x00*")):
		return TIFF
	}
	return ""
}

// TypeAllowed reports whether a file whose first bytes are b may be stored
// as declared for purpose: the sniffed type must be the declared one and
// one the purpose takes. Anything else is unsupported_type.
func TypeAllowed(purpose, declared string, b []byte) bool {
	l, ok := PurposeLimits[purpose]
	t := Sniff(b)
	return ok && t != "" && t == declared && l.Allows(t)
}
