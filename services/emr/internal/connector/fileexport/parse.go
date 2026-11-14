package fileexport

// The mhai_csv_v1 parser (plan Task 43). RFC 4180, UTF-8 without a byte
// order mark, the kind's header exactly, every row the header's width;
// file, row and row-count limits; each row mapped to raw records through
// the allow-list's field names. Text cells go through sink.CleanText and
// sink.CSVCell (A7, A8), as the sink does again.

import (
	"io"
	"iter"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
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
	return func(func(Row, error) bool) {} // not yet
}
