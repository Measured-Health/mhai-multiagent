package fileexport

// The hl7v2_file format (design docs/designs/2026-11-27-hl7v2-file-
// connector-and-bridge.md §3; format docs/specs/formats/hl7v2_file_v1.md):
// batch files of HL7 v2 messages, parsed by internal/hl7v2 and mapped here
// to the same raw records fhir_r4 and mhai_csv_v1 produce. ORU^R01 gives
// observations, RDE^O11 medication requests; each message's PID gives the
// patient and its PV1 the encounter and location. Every record is bound to
// the PID before it in the same message, and to the connection (its
// hospital) only: no field of a message ever names a hospital.
//
// As for the CSV: a file whose shape is wrong is refused whole; a record
// whose shape is right but a value isn't is a dead letter; a message or
// result we don't read (another type, a test message, narrative) is
// skipped and counted. Nothing here logs.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // time_zone settings in a distroless image
	"unicode"
	"unicode/utf8"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
)

// HL7ID is the connector type that reads HL7 v2 message files.
const HL7ID = "hl7v2_file"

// HL7Kind is the one file kind of the format.
const HL7Kind = "messages"

// The reasons an HL7 file is refused whole beyond the parser's (whose
// reasons pass through as they are).
const ReasonFacility = "facility"

// The reasons a message, or a result in it, is skipped (counted, not an
// error).
const (
	SkipType       = "message_type"
	SkipProcessing = "processing_id"
	SkipValueType  = "value_type"
	SkipNoResult   = "no_result"
	SkipClass      = "encounter_class"
)

// The local code system prefix: urn:mhai:hl7v2:<HL7 table 0396 value>.
const hl7SystemPrefix = "urn:mhai:hl7v2:"

// vitalLOINC are the LOINC codes read as vital signs: the FHIR vital-signs
// profile's, and those the rule set's vital-sign analytes name.
var vitalLOINC = []string{"85353-1", "9279-1", "8867-4", "2708-6", "59408-5", "8310-5", "8302-2", "9843-4", "29463-7",
	"39156-5", "85354-9", "8480-6", "8462-4", "8478-0"}

// obsStatusMap is OBX-11 to the FHIR status, as HL7's v2-to-FHIR concept
// map; "" is skipped (no result yet).
var obsStatusMap = map[string]string{"F": "final", "U": "final", "C": "corrected", "A": "amended", "P": "preliminary",
	"R": "preliminary", "S": "preliminary", "X": "cancelled", "D": "entered-in-error", "W": "entered-in-error",
	"I": "", "O": "", "N": ""}

// orderStatus (ORC-5) and orderControl (ORC-1) to the medication status.
var (
	orderStatus = map[string]string{"IP": "active", "SC": "active", "A": "active", "CM": "completed", "DC": "stopped",
		"CA": "cancelled", "HD": "on-hold", "ER": "entered-in-error"}
	orderControl = map[string]string{"NW": "active", "XO": "active", "RP": "active", "RL": "active", "SC": "active",
		"CA": "cancelled", "OC": "cancelled", "CR": "cancelled", "DC": "stopped", "OD": "stopped", "DR": "stopped",
		"HD": "on-hold", "OH": "on-hold"}
	hl7Sex      = map[string]string{"F": "female", "M": "male", "O": "other", "A": "other", "U": "unknown", "N": "unknown"}
	hl7Class    = map[string]string{"I": "inpatient", "O": "outpatient", "E": "emergency"}
	classToUnit = map[string]string{"inpatient": "ward", "outpatient": "clinic", "emergency": "emergency"}
)

var (
	tzShape       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+/-]{0,63}$`)
	facilityShape = regexp.MustCompile(`^[\x21-\x7e](?:[\x20-\x7e]{0,58}[\x21-\x7e])?$`)
	idTypeShape   = regexp.MustCompile(`^[A-Za-z0-9]{1,5}$`)
	systemClean   = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	rangeShape    = regexp.MustCompile(`^(-?[0-9]{1,15}(?:\.[0-9]{1,15})?)\s*-\s*(-?[0-9]{1,15}(?:\.[0-9]{1,15})?)$`)
	boundShape    = regexp.MustCompile(`^(<=|>=|<|>)\s*(-?[0-9]{1,15}(?:\.[0-9]{1,15})?)$`)
)

// hl7Settings are a connection's settings, read once per file.
type hl7Settings struct {
	loc                    *time.Location
	facility               string
	mrnNS, mrnType, mrnAut string
}

// validateHL7 checks the type's own settings (Validate checked the shared
// ones).
func validateHL7(cfg connector.Config) bool {
	s := cfg.Settings
	if tz, ok := s["time_zone"]; ok {
		if !tzShape.MatchString(tz) || tz == "Local" {
			return false
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return false
		}
	}
	if f, ok := s["sending_facility"]; ok && !facilityShape.MatchString(f) {
		return false
	}
	if a, ok := s["mrn_assigning_authority"]; ok && !facilityShape.MatchString(a) {
		return false
	}
	ty, hasType := s["mrn_identifier_type"]
	if hasType && !idTypeShape.MatchString(ty) {
		return false
	}
	_, hasNS := s["mrn_namespace"]
	_, hasAut := s["mrn_assigning_authority"]
	// An MRN needs both the namespace and the type; the authority only
	// narrows it.
	return hasType == hasNS && (!hasAut || hasType)
}

func settingsOf(cfg connector.Config) hl7Settings {
	s := cfg.Settings
	out := hl7Settings{facility: s["sending_facility"], mrnNS: s["mrn_namespace"], mrnType: s["mrn_identifier_type"],
		mrnAut: s["mrn_assigning_authority"]}
	if tz := s["time_zone"]; tz != "" {
		out.loc, _ = time.LoadLocation(tz)
	}
	return out
}

func parseHL7Name(name string) (fileKey, bool) {
	at, seq, ok := hl7v2.ParseFileName(name)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{name: name, kind: HL7Kind, at: at, seq: seq}, true
}

// ParseHL7 reads one hl7v2_file file. It yields rows as ParseBytes does,
// plus skipped ones; a *FileError ends the sequence and the caller stores
// nothing of the file.
func ParseHL7(data []byte, cfg connector.Config) iter.Seq2[Row, error] {
	st := settingsOf(cfg)
	return func(yield func(Row, error) bool) {
		b, err := hl7v2.ParseBatch(data)
		if err != nil {
			var reason string
			line := 0
			if he, ok := err.(*hl7v2.Error); ok {
				reason, line = he.Reason, he.Message
			} else {
				reason = ReasonShape
			}
			yield(Row{}, &FileError{Reason: reason, Line: line})
			return
		}
		if st.facility != "" {
			for i, m := range b.Messages {
				if f, ok := m.V(m.MSH(), 4, 1); !ok || !strings.EqualFold(f, st.facility) {
					yield(Row{}, &FileError{Reason: ReasonFacility, Line: i + 1})
					return
				}
			}
		}
		for i, m := range b.Messages {
			for _, r := range mapMessage(m, st) {
				r.Line = i + 1
				if !yield(r, nil) {
					return
				}
			}
		}
	}
}

// joinID joins parts length-prefixed ("3:LAB/5:12345"), so no two different
// part lists give one id; one too long, or not printable, is its hash.
func joinID(parts ...string) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	s := b.String()
	if len(s) > 256 || !printableID(s) {
		sum := sha256.Sum256([]byte(s))
		return "h:" + hex.EncodeToString(sum[:])
	}
	return s
}

func printableID(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// recordVersion is the hash of a record's mapped content: an unchanged
// resend is the same version (a no-op), a correction a new one.
func recordVersion(kind string, fields map[string]any) string {
	b, _ := json.Marshal(fields) // map keys sorted; values are strings, bools and codings
	h := sha256.New()
	h.Write([]byte(hl7v2.Format + "\x00" + kind + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// msgCtx is one message being mapped.
type msgCtx struct {
	m       *hl7v2.Message
	st      hl7Settings
	updated time.Time
	timeBad bool
	ctrl    string
	rows    []Row
}

// patientCtx is the patient group a record belongs to.
type patientCtx struct {
	id        string // "" when the PID gave none
	encounter string
}

// fieldReader collects one record's fields and the first problem.
type fieldReader struct {
	mc     *msgCtx
	fields map[string]any
	reason string
}

func (r *fieldReader) bad(reason string) {
	if r.reason == "" {
		r.reason = reason
	}
}

// val reads a value; an unreadable one makes the record a dead letter.
func (r *fieldReader) val(seg *hl7v2.Segment, f, rep, c, sub int) string {
	v, err := r.mc.m.Value(seg, f, rep, c, sub)
	if err != nil {
		r.bad(RowValue)
		return ""
	}
	return v
}

func (r *fieldReader) v(seg *hl7v2.Segment, f, c int) string { return r.val(seg, f, 0, c, 1) }

// text puts display text under key: cleaned and neutralised; longer than
// max is a dead letter, never cut.
func (r *fieldReader) text(key, v string, max int) string {
	if v == "" {
		return ""
	}
	c := sink.CSVCell(sink.CleanText(v, -1))
	if utf8.RuneCountInString(c) > max {
		r.bad(RowTooLong)
		return ""
	}
	if c != "" && key != "" {
		r.fields[key] = c
	}
	return c
}

func (r *fieldReader) time(key, v string) string {
	if v == "" {
		return ""
	}
	t, _, err := hl7v2.ParseTime(v, r.mc.st.loc)
	if err != nil {
		r.bad(RowTime)
		return ""
	}
	if key != "" {
		r.fields[key] = t
	}
	return t
}

// codeSystem maps an HL7 table 0396 coding system to the system the
// normaliser reads: LOINC for observations, ATC for medications, every
// other one as urn:mhai:hl7v2:<system>.
func codeSystem(sys string, medication bool) string {
	u := strings.ToUpper(sys)
	switch {
	case u == "LN" && !medication:
		return codes.LOINCSystem
	case (u == "WC" || u == "ATC") && medication:
		return codes.ATCSystem
	}
	c := systemClean.ReplaceAllString(sys, "")
	if len(c) > 40 {
		c = c[:40]
	}
	if c == "" {
		c = "local"
	}
	return hl7SystemPrefix + c
}

// cwe reads a CWE (or CE) at field f, repetition rep: its primary and
// alternate codings. A coding the sink would refuse is left out.
func (r *fieldReader) cwe(seg *hl7v2.Segment, f, rep int, medication bool) []connector.Coding {
	var out []connector.Coding
	for _, base := range []int{1, 4} {
		code := r.val(seg, f, rep, base, 1)
		if code == "" {
			continue
		}
		if len(code) > 64 || !printableID(code) {
			r.bad(RowCode)
			continue
		}
		disp := r.val(seg, f, rep, base+1, 1)
		out = append(out, connector.Coding{System: codeSystem(r.val(seg, f, rep, base+2, 1), medication), Code: code,
			Display: sink.CleanText(disp, 200)})
	}
	return out
}

// orderID is an order number (EI): its entity id and namespace.
func (r *fieldReader) orderID(seg *hl7v2.Segment, f int) string {
	if seg == nil {
		return ""
	}
	id := r.v(seg, f, 1)
	if id == "" {
		return ""
	}
	return joinID(r.v(seg, f, 2), id)
}

func (mc *msgCtx) reject(kind, id, reason string) {
	if id == "" {
		id = joinID("msg", mc.ctrl, strconv.Itoa(len(mc.rows)))
	}
	sum := sha256.Sum256([]byte(mc.m.Text + "\x00" + kind + "\x00" + id))
	mc.rows = append(mc.rows, Row{Reject: &Reject{Kind: kind, SourceID: id, SourceVersion: hex.EncodeToString(sum[:]),
		SourceUpdatedAt: mc.updated, Reason: reason}})
}

func (mc *msgCtx) emit(kind, id string, r *fieldReader) {
	if mc.timeBad {
		r.bad(RowTime)
	}
	if r.reason != "" {
		mc.reject(kind, id, r.reason)
		return
	}
	mc.rows = append(mc.rows, Row{Records: []connector.RawRecord{{Kind: kind, SourceID: id,
		SourceVersion: recordVersion(kind, r.fields), SourceUpdatedAt: mc.updated, Fields: r.fields}}})
}

func (mc *msgCtx) reader() *fieldReader { return &fieldReader{mc: mc, fields: map[string]any{}} }

func (mc *msgCtx) skip(reason string) { mc.rows = append(mc.rows, Row{Skipped: reason}) }

// mapMessage maps one parsed message to its rows.
func mapMessage(m *hl7v2.Message, st hl7Settings) []Row {
	mc := &msgCtx{m: m, st: st, ctrl: m.ControlID()}
	h := m.MSH()
	if p, _ := m.V(h, 11, 1); p != "P" {
		mc.skip(SkipProcessing)
		return mc.rows
	}
	typ, ev := m.Type()
	medication := false
	switch {
	case typ == "ORU" && ev == "R01":
	case typ == "RDE" && ev == "O11":
		medication = true
	default:
		mc.skip(SkipType)
		return mc.rows
	}
	if ts, ok := m.V(h, 7, 1); ok {
		if t, dateOnly, err := hl7v2.ParseTime(ts, st.loc); err == nil && !dateOnly {
			mc.updated, _ = time.Parse(time.RFC3339Nano, t)
			mc.updated = mc.updated.UTC()
		}
	}
	mc.timeBad = mc.updated.IsZero()

	var (
		pt       *patientCtx
		orc, obr *hl7v2.Segment
		order    *medOrder
	)
	flushOrder := func() {
		if order != nil {
			mc.medication(pt, order)
			order = nil
		}
	}
	for i := 1; i < len(m.Segs); i++ {
		seg := &m.Segs[i]
		switch seg.ID {
		case "PID":
			flushOrder()
			pt = mc.patient(seg)
			orc, obr = nil, nil
		case "PV1":
			if pt != nil {
				mc.encounter(pt, seg)
			}
		case "ORC":
			flushOrder()
			orc, obr = seg, nil
			if medication {
				order = &medOrder{orc: seg}
			}
		case "OBR":
			obr = seg
		case "OBX":
			if !medication {
				mc.observation(pt, orc, obr, seg)
			}
		case "RXE":
			if order != nil && order.rxe == nil {
				order.rxe = seg
			}
		case "RXR":
			if order != nil && order.rxr == nil {
				order.rxr = seg
			}
		case "TQ1":
			if order != nil && order.tq1 == nil {
				order.tq1 = seg
			}
		}
	}
	flushOrder()
	return mc.rows
}

// patient maps a PID. Its source id is the first PID-3 repetition with a
// value (authority, type and id joined); the MRN, when the connection names
// its type, under the connection's namespace.
func (mc *msgCtx) patient(seg *hl7v2.Segment) *patientCtx {
	m := mc.m
	r := mc.reader()
	pc := &patientCtx{}
	var mrn []connector.Coding
	for rep := 0; rep < m.Reps(seg, 3); rep++ {
		id := r.val(seg, 3, rep, 1, 1)
		if id == "" {
			continue
		}
		aut, typ := r.val(seg, 3, rep, 4, 1), r.val(seg, 3, rep, 5, 1)
		if pc.id == "" {
			pc.id = joinID(aut, typ, id)
		}
		if mrn == nil && mc.st.mrnType != "" && strings.EqualFold(typ, mc.st.mrnType) &&
			(mc.st.mrnAut == "" || strings.EqualFold(aut, mc.st.mrnAut)) {
			if len(id) <= 64 && printableID(id) {
				mrn = []connector.Coding{{System: mc.st.mrnNS, Code: id}}
			} else {
				r.bad(RowCode)
			}
		}
	}
	if pc.id == "" {
		// No identifier: this patient's records are dead letters (their
		// own ids, below), never attached to anyone.
		mc.reject("patient", "", RowRequired)
		return pc
	}
	if mrn != nil {
		r.fields["identifier"] = mrn
	}
	name := 0
	for rep := 0; rep < m.Reps(seg, 5); rep++ {
		if r.val(seg, 5, rep, 7, 1) == "L" {
			name = rep
			break
		}
	}
	given := strings.TrimSpace(r.val(seg, 5, name, 2, 1) + " " + r.val(seg, 5, name, 3, 1))
	r.text("given", given, 60)
	r.text("family", r.val(seg, 5, name, 1, 1), 60)
	if b := r.v(seg, 7, 1); b != "" {
		if len(b) >= 8 {
			if d, _, err := hl7v2.ParseTime(b[:8], nil); err == nil {
				r.fields["birth_date"] = d
			} else {
				r.bad(RowTime)
			}
		} else {
			r.bad(RowTime)
		}
	}
	if s := r.v(seg, 8, 1); s != "" {
		if v, ok := hl7Sex[strings.ToUpper(s)]; ok {
			r.fields["sex"] = v
		} else {
			r.bad(RowCode)
		}
	}
	mc.emit("patient", pc.id, r)
	return pc
}

// encounter maps a PV1 that names a visit (PV1-19), and its location.
func (mc *msgCtx) encounter(pt *patientCtx, seg *hl7v2.Segment) {
	r := mc.reader()
	visit := r.v(seg, 19, 1)
	if visit == "" || pt.id == "" {
		return
	}
	id := joinID(pt.id, r.v(seg, 19, 4), visit)
	class, ok := hl7Class[strings.ToUpper(r.v(seg, 2, 1))]
	if !ok {
		mc.skip(SkipClass)
		return
	}
	pt.encounter = id
	r.fields["patient"] = pt.id
	r.fields["class"] = class
	r.time("period_start", r.v(seg, 44, 1))
	if r.time("period_end", r.v(seg, 45, 1)) != "" {
		r.fields["status"] = "finished"
	} else {
		r.fields["status"] = "in-progress"
	}
	if bed := r.v(seg, 3, 3); bed != "" {
		r.text("bed_label", bed, 20)
	}
	if poc := r.v(seg, 3, 1); poc != "" {
		lr := mc.reader()
		loc := joinID(lr.val(seg, 3, 0, 4, 1), poc)
		lr.text("name", poc, 60)
		lr.fields["type"] = classToUnit[class]
		r.fields["location"] = loc
		mc.emit("location", loc, lr)
	}
	mc.emit("encounter", id, r)
}

// observation maps one OBX of an ORU^R01.
func (mc *msgCtx) observation(pt *patientCtx, orc, obr, seg *hl7v2.Segment) {
	r := mc.reader()
	order := r.orderID(obr, 3)
	if order == "" {
		order = r.orderID(obr, 2)
	}
	if order == "" {
		order = r.orderID(orc, 3)
	}
	if order == "" {
		order = r.orderID(orc, 2)
	}
	codings := r.cwe(seg, 3, 0, false)
	var id string
	// The patient is part of the id: an order number reused for another
	// patient is another record, never a version superseding this one's.
	if order != "" && len(codings) > 0 && pt != nil && pt.id != "" {
		id = joinID(pt.id, order, codings[0].System, codings[0].Code, r.v(seg, 4, 1))
	}
	status, known := obsStatusMap[strings.ToUpper(r.v(seg, 11, 1))]
	switch {
	case known && status == "":
		mc.skip(SkipNoResult)
		return
	case !known:
		r.bad(RowCode)
	}
	vt := strings.ToUpper(r.v(seg, 2, 1))
	switch vt {
	case "NM", "SN", "ST", "CE", "CWE", "":
	default:
		mc.skip(SkipValueType) // narrative, reports, attachments: never kept
		return
	}
	if pt == nil || pt.id == "" {
		r.bad(RowRequired)
	}
	if id == "" {
		r.bad(RowRequired)
	}
	if r.reason != "" {
		mc.reject("observation", id, r.reason)
		return
	}
	r.fields["patient"] = pt.id
	if pt.encounter != "" {
		r.fields["encounter"] = pt.encounter
	}
	r.fields["status"] = status
	r.fields["codings"] = codings
	r.text("code_text", r.v(seg, 3, 2), 200)
	category := "laboratory"
	for _, c := range codings {
		if c.System == codes.LOINCSystem && slices.Contains(vitalLOINC, c.Code) {
			category = "vital-signs"
		}
	}
	r.fields["category"] = category
	switch vt {
	case "NM":
		v := strings.TrimPrefix(r.v(seg, 5, 1), "+")
		if v != "" {
			if !decimalShape.MatchString(v) {
				r.bad(RowValue)
			} else {
				r.fields["value_quantity"] = v
			}
		}
	case "SN":
		r.structuredNumeric(seg)
	case "ST":
		r.text("value_text", r.v(seg, 5, 1), 120)
	case "CE", "CWE":
		if cs := r.cwe(seg, 5, 0, false); len(cs) > 0 {
			r.fields["value_codings"] = cs
		}
	}
	if u := r.v(seg, 6, 1); u != "" || r.v(seg, 6, 2) != "" {
		if u == "" {
			u = r.v(seg, 6, 2)
		}
		if !unitOK(u) {
			r.bad(RowValue)
		} else {
			r.fields["unit"] = u
		}
	}
	r.referenceRange(r.v(seg, 7, 1))
	for rep := 0; rep < mc.m.Reps(seg, 8); rep++ {
		if f := r.val(seg, 8, rep, 1, 1); slices.Contains(flags, f) {
			r.fields["interpretation"] = f
			break
		}
	}
	eff := r.v(seg, 14, 1)
	if eff == "" && obr != nil {
		eff = r.v(obr, 7, 1)
	}
	if eff == "" {
		r.bad(RowRequired)
	}
	r.time("effective", eff)
	if obr != nil {
		r.time("issued", r.v(obr, 22, 1))
	}
	mc.emit("observation", id, r)
}

// structuredNumeric reads an SN: comparator^num1^separator^num2.
func (r *fieldReader) structuredNumeric(seg *hl7v2.Segment) {
	cmp, n1, sep, n2 := r.v(seg, 5, 1), r.v(seg, 5, 2), r.v(seg, 5, 3), r.v(seg, 5, 4)
	if n1 == "" && n2 == "" {
		return
	}
	n1 = strings.TrimPrefix(n1, "+")
	if !decimalShape.MatchString(n1) {
		r.bad(RowValue)
		return
	}
	if sep == "" && n2 == "" {
		switch cmp {
		case "", "=":
			r.fields["value_quantity"] = n1
		case "<", ">", "<=", ">=":
			r.fields["value_quantity"] = cmp + n1
		case "<>":
			r.fields["value_text"] = "<>" + n1
		default:
			r.bad(RowValue)
		}
		return
	}
	n2 = strings.TrimPrefix(n2, "+")
	if !decimalShape.MatchString(n2) || !slices.Contains([]string{"-", "+", "/", ".", ":"}, sep) ||
		!slices.Contains([]string{"", "=", "<", ">", "<=", ">=", "<>"}, cmp) {
		r.bad(RowValue)
		return
	}
	r.text("value_text", cmp+n1+sep+n2, 120)
}

// referenceRange reads OBX-7: a-b, <b, <=b, >a, >=a as bounds, anything
// else as text.
func (r *fieldReader) referenceRange(v string) {
	if v == "" {
		return
	}
	if p := rangeShape.FindStringSubmatch(v); p != nil {
		r.fields["ref_low"], r.fields["ref_high"] = p[1], p[2]
		return
	}
	if p := boundShape.FindStringSubmatch(v); p != nil {
		if strings.HasPrefix(p[1], "<") {
			r.fields["ref_high"] = p[2]
		} else {
			r.fields["ref_low"] = p[2]
		}
		return
	}
	r.text("ref_text", v, 120)
}

// medOrder is one ORC of an RDE^O11 and the segments of its order.
type medOrder struct {
	orc, rxe, rxr, tq1 *hl7v2.Segment
}

// medication maps one order of an RDE^O11 to a medication request.
func (mc *msgCtx) medication(pt *patientCtx, o *medOrder) {
	r := mc.reader()
	order := r.orderID(o.orc, 2)
	if order == "" {
		order = r.orderID(o.orc, 3)
	}
	id := ""
	if order != "" && pt != nil && pt.id != "" {
		id = joinID(pt.id, order) // the patient is part of the id, as for results
	}
	status := ""
	if s := strings.ToUpper(r.v(o.orc, 5, 1)); s != "" {
		status = orderStatus[s]
	} else {
		status = orderControl[strings.ToUpper(r.v(o.orc, 1, 1))]
	}
	if status == "" {
		r.bad(RowCode)
	}
	if pt == nil || pt.id == "" || id == "" || o.rxe == nil {
		r.bad(RowRequired)
	}
	if r.reason != "" {
		mc.reject("medication", id, r.reason)
		return
	}
	r.fields["patient"] = pt.id
	r.fields["kind"] = "request"
	r.fields["status"] = status
	cs := r.cwe(o.rxe, 2, 0, true)
	if len(cs) > 0 {
		r.fields["codings"] = cs
	}
	name := r.text("med_text", r.v(o.rxe, 2, 2), 200)
	if len(cs) == 0 && name == "" {
		r.bad(RowRequired)
	}
	if d := strings.TrimPrefix(r.v(o.rxe, 3, 1), "+"); d != "" {
		if decimalShape.MatchString(d) {
			r.fields["dose_value"] = d
		} else {
			r.bad(RowValue)
		}
	}
	u := r.v(o.rxe, 5, 1)
	if u == "" {
		u = r.v(o.rxe, 5, 2)
	}
	if u != "" {
		if unitOK(u) {
			r.fields["dose_unit"] = u
		} else {
			r.bad(RowValue)
		}
	}
	if o.rxr != nil {
		route := r.v(o.rxr, 1, 2)
		if route == "" {
			route = r.v(o.rxr, 1, 1)
		}
		r.text("route", route, 40)
	}
	timing, start, end := "", "", ""
	if o.tq1 != nil {
		timing, start, end = r.v(o.tq1, 3, 1), r.v(o.tq1, 7, 1), r.v(o.tq1, 8, 1)
	}
	if timing == "" {
		timing = r.val(o.rxe, 1, 0, 2, 1)
	}
	if start == "" {
		start = r.v(o.rxe, 1, 4)
	}
	if end == "" {
		end = r.v(o.rxe, 1, 5)
	}
	r.text("timing_text", timing, 60)
	r.time("authored", r.v(o.orc, 9, 1))
	r.time("period_start", start)
	r.time("period_end", end)
	mc.emit("medication", id, r)
}
