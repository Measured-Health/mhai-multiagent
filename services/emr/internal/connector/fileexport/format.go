// Package fileexport is the scheduled file export connector (EMR design
// §5.4, §15.4; plan Tasks 43-44): files in the published mhai_csv_v1
// format (docs/specs/formats/mhai_csv_v1.md), pulled over SFTP through
// egress-emr with a pinned host key, mapped row by row to raw records.
//
// The format is ours, published for the hospital's IT to map their export
// to, and it is read strictly: anything it doesn't name exactly fails
// closed. A file whose shape is wrong (encoding, header, quoting, field
// count, size, a row without a usable id, one id twice with different
// content) is refused whole and nothing of it is stored; a row whose shape
// is right but whose values aren't (a time without an offset, a status
// outside the closed set, a value that isn't a decimal) is kept as a dead
// letter: a rejected raw record with its provenance and no payload, so it
// is counted and visible and never half-read. Nothing here logs; errors are
// closed reasons and line numbers, never a cell, a file name or a host.
package fileexport

import (
	"regexp"
	"slices"
	"time"
)

// Format is the one file format this connector reads.
const Format = "mhai_csv_v1"

// Limits (design §5.4): anything over is refused whole.
const (
	// MaxFile is the largest file read, in bytes.
	MaxFile = 50 << 20
	// MaxRow is the largest row, in bytes, line ending included.
	MaxRow = 8 << 10
	// MaxRows is the most data rows a file may have.
	MaxRows = 200_000
)

// Kinds are the file kinds, in the order a run loads files of the same
// export time: patients before the records that name them.
var Kinds = []string{"patients", "encounters", "results", "vitals", "medications"}

// Columns are each kind's header, exactly (design §5.4, with the departures
// recorded in the format document: updated_at on every kind, and
// source_encounter_id and value_text where the design's example had none).
var Columns = map[string][]string{
	"patients": {"source_patient_id", "updated_at", "mrn", "given_name", "family_name", "birth_date", "sex", "active",
		"replaced_by"},
	"encounters": {"source_encounter_id", "source_patient_id", "updated_at", "class", "status", "start_at", "end_at",
		"source_location_id", "location_name", "location_type", "bed_label"},
	"results": {"source_result_id", "source_patient_id", "source_encounter_id", "collected_at", "resulted_at", "updated_at",
		"status", "local_code", "local_name", "value", "value_text", "unit", "ref_low", "ref_high", "ref_text", "flag"},
	"vitals": {"source_vital_id", "source_patient_id", "source_encounter_id", "measured_at", "updated_at", "status",
		"local_code", "local_name", "value", "unit", "flag"},
	"medications": {"source_medication_id", "source_patient_id", "updated_at", "kind", "status", "local_code", "local_name",
		"dose_value", "dose_unit", "route", "timing_text", "authored_at", "start_at", "end_at"},
}

// The local code systems a file's codes are recorded under (the
// connection's reviewed map keys on them, design §9.4).
const (
	ResultSystem     = "urn:mhai:mhai_csv_v1:results"
	VitalSystem      = "urn:mhai:mhai_csv_v1:vitals"
	MedicationSystem = "urn:mhai:mhai_csv_v1:medications"
)

// The closed sets a row's codes must be in (the normaliser's, so a row the
// parser keeps is never rejected for its code there).
var (
	resultStatus    = []string{"preliminary", "final", "amended", "corrected", "cancelled", "entered-in-error"}
	encounterStatus = []string{"planned", "arrived", "in-progress", "finished", "cancelled", "entered-in-error"}
	medStatus       = []string{"active", "on-hold", "stopped", "completed", "cancelled", "entered-in-error"}
	encounterClass  = []string{"inpatient", "outpatient", "emergency"}
	sexes           = []string{"female", "male", "other", "unknown"}
	flags           = []string{"L", "H", "LL", "HH", "A", "AA", "N"}
	locationTypes   = []string{"ward", "clinic", "emergency", "other"}
	medKinds        = []string{"request", "statement"}
	actives         = []string{"true", "false"}
)

var (
	// fileName is a file the connector reads: <kind>-<UTC time>[-<seq>].csv.
	// Anything else in the folder is left alone and counted.
	fileName = regexp.MustCompile(`^(patients|encounters|results|vitals|medications)-([0-9]{8}T[0-9]{6}Z)(?:-([0-9]{1,4}))?\.csv$`)
	// idShape: a source id. The first character is a letter or digit, so no
	// id reads as a spreadsheet formula.
	idShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	// codeShape: a local code.
	codeShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+%-]{0,63}$`)
	// timeShape: an ISO 8601 date and time with seconds and an offset.
	timeShape = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})$`)
	// dateShape: a date only (birth_date).
	dateShape = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	// valueShape: a decimal with '.', an optional leading '-' and an
	// optional comparator; no exponent, no thousands separator, no unit.
	valueShape = regexp.MustCompile(`^(?:<=|>=|<|>)?-?[0-9]{1,15}(?:\.[0-9]{1,15})?$`)
	// decimalShape: a decimal without a comparator.
	decimalShape = regexp.MustCompile(`^-?[0-9]{1,15}(?:\.[0-9]{1,15})?$`)
	// unitShape: a unit as the source writes it, mapped later; never one
	// that starts like a formula.
	unitShape = regexp.MustCompile(`^[^=+@\-\s][^\s]{0,39}$`)
)

// fileOrder reports whether name is a file to read, and its sort key:
// export time, then kind order, then sequence.
type fileKey struct {
	name, kind, at string
	seq            int
}

func parseName(name string) (fileKey, bool) {
	m := fileName.FindStringSubmatch(name)
	if m == nil {
		return fileKey{}, false
	}
	if _, err := time.Parse("20060102T150405Z", m[2]); err != nil {
		return fileKey{}, false
	}
	seq := 0
	for _, c := range m[3] {
		seq = seq*10 + int(c-'0')
	}
	return fileKey{name: name, kind: m[1], at: m[2], seq: seq}, true
}

func (a fileKey) less(b fileKey) int {
	switch {
	case a.at != b.at:
		if a.at < b.at {
			return -1
		}
		return 1
	case a.kind != b.kind:
		return slices.Index(Kinds, a.kind) - slices.Index(Kinds, b.kind)
	}
	return a.seq - b.seq
}
