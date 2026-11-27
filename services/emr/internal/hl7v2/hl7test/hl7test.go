// Package hl7test builds synthetic HL7 v2 messages for tests. Every name,
// id and value is obviously synthetic (SYNTH-…); nothing is real.
package hl7test

import (
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
)

// Synthetic identities, findable in logs by tests that prove they aren't
// there.
const (
	Family   = "SYNTHFAMILYZQ"
	Given    = "SYNTHGIVENZQ"
	MRN      = "SYN-MRN-0001"
	Facility = "SYNTH-HOSP"
)

// Seg is a segment with fields by number (field 1 is the first after the
// id). For MSH use MSH.
func Seg(id string, f map[int]string) string {
	n := 0
	for k := range f {
		n = max(n, k)
	}
	parts := make([]string, n+1)
	parts[0] = id
	for k, v := range f {
		parts[k] = v
	}
	return strings.Join(parts, "|")
}

// MSH is an MSH with the standard delimiters: f by MSH field number (3 and
// up; 1 and 2 are the delimiters).
func MSH(f map[int]string) string {
	n := 2
	for k := range f {
		n = max(n, k)
	}
	parts := make([]string, n)
	parts[0], parts[1] = "MSH", `^~\&`
	for k, v := range f {
		if k >= 3 {
			parts[k-1] = v
		}
	}
	return strings.Join(parts, "|")
}

// Header is a production MSH of type typ (ORU^R01) with control id ctrl,
// sent at at.
func Header(typ, ctrl string, at time.Time) string {
	return MSH(map[int]string{3: "SYNTH-LAB", 4: Facility, 5: "MHAI", 6: "MHAI", 7: hl7v2.FormatTime(at), 9: typ,
		10: ctrl, 11: "P", 12: "2.5.1", 18: "UNICODE UTF-8"})
}

// PID is a synthetic patient: MRN first, then a national id.
func PID() string {
	return Seg("PID", map[int]string{1: "1", 3: MRN + "^^^" + Facility + "^MR~SYN-NAT-9^^^SYNTH-NATIONAL^NI",
		5: Family + "^" + Given + "^^^^^L", 7: "19800101", 8: "F"})
}

// PV1 is an inpatient visit on a synthetic ward.
func PV1() string {
	return Seg("PV1", map[int]string{1: "1", 2: "I", 3: "SYNTH-WARD-A^^BED-3^" + Facility, 19: "SYN-VISIT-1^^^" + Facility,
		44: "20261126080000+0000"})
}

// Result is one OBX: a LOINC code, a numeric value, a unit, a range, a
// flag and a status, taken at at.
type Result struct {
	Code, System, Name, Type, Value, Unit, Range, Flag, Status string
	At                                                         time.Time
}

// Potassium is a high potassium result (6.9 mmol/L, HH, final).
func Potassium(at time.Time) Result {
	return Result{Code: "2823-3", System: "LN", Name: "Potassium SerPl-sCnc", Type: "NM", Value: "6.9", Unit: "mmol/L",
		Range: "3.5-5.1", Flag: "HH", Status: "F", At: at}
}

// OBX is r as an OBX with set id n.
func OBX(n int, r Result) string {
	return Seg("OBX", map[int]string{1: strconv.Itoa(n), 2: r.Type, 3: r.Code + "^" + r.Name + "^" + r.System,
		5: r.Value, 6: r.Unit + "^^UCUM", 7: r.Range, 8: r.Flag, 11: r.Status, 14: hl7v2.FormatTime(r.At)})
}

// ORU is an ORU^R01 for the synthetic patient with one order (filler
// order) holding results.
func ORU(ctrl, order string, at time.Time, results ...Result) string {
	segs := []string{Header("ORU^R01^ORU_R01", ctrl, at), PID(), PV1(),
		Seg("ORC", map[int]string{1: "RE", 2: "PL-" + order + "^SYNTH-EMR", 3: order + "^SYNTH-LAB"}),
		Seg("OBR", map[int]string{1: "1", 2: "PL-" + order + "^SYNTH-EMR", 3: order + "^SYNTH-LAB",
			4: "SYNTH-PANEL^Synthetic panel^L", 7: hl7v2.FormatTime(at.Add(-time.Hour)), 22: hl7v2.FormatTime(at), 25: "F"})}
	for i, r := range results {
		segs = append(segs, OBX(i+1, r))
	}
	return strings.Join(segs, "\r") + "\r"
}

// RDE is an RDE^O11 for the synthetic patient: one new order of an ATC
// coded drug.
func RDE(ctrl, order string, at time.Time) string {
	return strings.Join([]string{Header("RDE^O11^RDE_O11", ctrl, at), PID(), PV1(),
		Seg("ORC", map[int]string{1: "NW", 2: order + "^SYNTH-EMR", 5: "IP", 9: hl7v2.FormatTime(at)}),
		Seg("RXE", map[int]string{2: "C09AA05^Ramipril^WC", 3: "5", 5: "mg^^UCUM"}),
		Seg("RXR", map[int]string{1: "PO^Oral^HL70162"}),
		Seg("TQ1", map[int]string{1: "1", 3: "QD", 7: hl7v2.FormatTime(at)}),
	}, "\r") + "\r"
}

// ADT is an ADT^A08 (a type the connector doesn't read).
func ADT(ctrl string, at time.Time) string {
	return strings.Join([]string{Header("ADT^A08^ADT_A01", ctrl, at), PID(), PV1()}, "\r") + "\r"
}

// File wraps messages in an FHS/BHS envelope, as the bridge writes it.
func File(name string, at time.Time, msgs ...string) []byte {
	return []byte(hl7v2.FileHeader(name, at) + strings.Join(msgs, "") + hl7v2.FileTrailer(len(msgs)))
}
