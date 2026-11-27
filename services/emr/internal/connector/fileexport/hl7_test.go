package fileexport

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

var hat = time.Date(2026, 11, 27, 10, 15, 0, 0, time.UTC)

func hl7Config() connector.Config {
	return connector.Config{ConnectionID: uuid.New(), HospitalID: uuid.New(), Type: HL7ID, BaseURL: "sftp://" + sftpHost,
		Origin: []emrurl.Origin{emrurl.Origin("sftp://" + sftpHost + ":22")}, PollSeconds: 300,
		Settings: map[string]string{"folder": folder, "username": user, "expected_every_minutes": "60",
			"host_key_fingerprint": "SHA256:" + strings.Repeat("A", 43), "mrn_namespace": testNS, "mrn_identifier_type": "MR"}}
}

type hl7Parsed struct {
	recs    []connector.RawRecord
	rejects []Reject
	skipped []string
	err     *FileError
}

func parseHL7(t *testing.T, data []byte, cfg connector.Config) hl7Parsed {
	t.Helper()
	var p hl7Parsed
	for row, err := range ParseHL7(data, cfg) {
		if err != nil {
			if !errors.As(err, &p.err) {
				t.Fatalf("not a file error: %v", err)
			}
			return p
		}
		switch {
		case row.Skipped != "":
			p.skipped = append(p.skipped, row.Skipped)
		case row.Reject != nil:
			p.rejects = append(p.rejects, *row.Reject)
		default:
			p.recs = append(p.recs, row.Records...)
		}
	}
	return p
}

func (p hl7Parsed) kind(k string) []connector.RawRecord {
	var out []connector.RawRecord
	for _, r := range p.recs {
		if r.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

func TestHL7ORUMapsToTheCanonicalFields(t *testing.T) {
	cfg := hl7Config()
	p := parseHL7(t, []byte(hl7test.ORU("SYN-CTRL-1", "SYN-ORD-1", hat, hl7test.Potassium(hat))), cfg)
	if p.err != nil || len(p.rejects) != 0 || len(p.skipped) != 0 {
		t.Fatalf("%+v", p)
	}
	pts, obs, enc, loc := p.kind("patient"), p.kind("observation"), p.kind("encounter"), p.kind("location")
	if len(pts) != 1 || len(obs) != 1 || len(enc) != 1 || len(loc) != 1 {
		t.Fatalf("records %d %d %d %d", len(pts), len(obs), len(enc), len(loc))
	}
	pt := pts[0]
	if pt.SourceID != joinID(hl7test.Facility, "MR", hl7test.MRN) || pt.Fields["family"] != hl7test.Family ||
		pt.Fields["given"] != hl7test.Given || pt.Fields["birth_date"] != "1980-01-01" || pt.Fields["sex"] != "female" {
		t.Fatalf("patient %+v", pt)
	}
	ids, _ := pt.Fields["identifier"].([]connector.Coding)
	if len(ids) != 1 || ids[0].System != testNS || ids[0].Code != hl7test.MRN {
		t.Fatalf("mrn %+v", ids)
	}
	o := obs[0].Fields
	cs := o["codings"].([]connector.Coding)
	if cs[0].System != codes.LOINCSystem || cs[0].Code != "2823-3" || o["value_quantity"] != "6.9" || o["unit"] != "mmol/L" ||
		o["ref_low"] != "3.5" || o["ref_high"] != "5.1" || o["interpretation"] != "HH" || o["status"] != "final" ||
		o["category"] != "laboratory" || o["patient"] != pt.SourceID || o["encounter"] != enc[0].SourceID ||
		o["effective"] != "2026-11-27T10:15:00Z" || o["issued"] != "2026-11-27T10:15:00Z" {
		t.Fatalf("observation %+v", o)
	}
	if !obs[0].SourceUpdatedAt.Equal(hat) || len(obs[0].SourceVersion) != 64 {
		t.Fatalf("provenance %+v", obs[0])
	}
	e := enc[0].Fields
	if e["class"] != "inpatient" || e["status"] != "in-progress" || e["location"] != loc[0].SourceID || e["bed_label"] != "BED-3" {
		t.Fatalf("encounter %+v", e)
	}
	if loc[0].Fields["name"] != "SYNTH-WARD-A" || loc[0].Fields["type"] != "ward" {
		t.Fatalf("location %+v", loc[0].Fields)
	}
}

// Without the MRN type and namespace no MRN is stored; another type is not
// the MRN.
func TestHL7MRNOnlyFromTheNamedType(t *testing.T) {
	for name, set := range map[string]map[string]string{
		"no settings":     {},
		"other type":      {"mrn_namespace": testNS, "mrn_identifier_type": "PI"},
		"other authority": {"mrn_namespace": testNS, "mrn_identifier_type": "MR", "mrn_assigning_authority": "OTHER"},
	} {
		cfg := hl7Config()
		delete(cfg.Settings, "mrn_namespace")
		delete(cfg.Settings, "mrn_identifier_type")
		for k, v := range set {
			cfg.Settings[k] = v
		}
		p := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat))), cfg)
		if _, ok := p.kind("patient")[0].Fields["identifier"]; ok {
			t.Errorf("%s: an MRN was kept", name)
		}
	}
}

// Corrections are new versions of the same record; an identical resend is
// the same version.
func TestHL7CorrectionsAreVersions(t *testing.T) {
	cfg := hl7Config()
	k := hl7test.Potassium(hat)
	first := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg).kind("observation")[0]
	again := parseHL7(t, []byte(hl7test.ORU("C2", "O1", hat, k)), cfg).kind("observation")[0]
	k.Status, k.Value = "C", "5.0"
	corr := parseHL7(t, []byte(hl7test.ORU("C3", "O1", hat.Add(time.Hour), k)), cfg).kind("observation")[0]
	if first.SourceID != again.SourceID || first.SourceVersion != again.SourceVersion {
		t.Fatal("a resend is a new version")
	}
	if corr.SourceID != first.SourceID || corr.SourceVersion == first.SourceVersion || corr.Fields["status"] != "corrected" {
		t.Fatalf("correction %+v", corr)
	}
}

func TestHL7Statuses(t *testing.T) {
	cfg := hl7Config()
	for in, want := range map[string]string{"F": "final", "C": "corrected", "P": "preliminary", "R": "preliminary",
		"X": "cancelled", "D": "entered-in-error", "W": "entered-in-error", "U": "final", "A": "amended"} {
		k := hl7test.Potassium(hat)
		k.Status = in
		obs := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg).kind("observation")
		if len(obs) != 1 || obs[0].Fields["status"] != want {
			t.Errorf("%s: %+v", in, obs)
		}
	}
	for _, in := range []string{"I", "O", "N"} {
		k := hl7test.Potassium(hat)
		k.Status = in
		p := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg)
		if len(p.kind("observation")) != 0 || len(p.skipped) != 1 {
			t.Errorf("%s: not skipped", in)
		}
	}
	k := hl7test.Potassium(hat)
	k.Status = "Q"
	if p := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg); len(p.rejects) != 1 || p.rejects[0].Reason != RowCode {
		t.Errorf("unknown status: %+v", p)
	}
}

func TestHL7ValueTypes(t *testing.T) {
	cfg := hl7Config()
	run := func(k hl7test.Result) hl7Parsed {
		return parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg)
	}
	sn := func(v string) hl7test.Result {
		k := hl7test.Potassium(hat)
		k.Type, k.Value = "SN", v
		return k
	}
	for v, want := range map[string]string{"^5.2": "5.2", "<^5": "<5", ">=^10": ">=10", "=^3": "3"} {
		if o := run(sn(v)).kind("observation"); len(o) != 1 || o[0].Fields["value_quantity"] != want {
			t.Errorf("SN %s: %+v", v, o)
		}
	}
	if o := run(sn("^1^:^128")).kind("observation"); len(o) != 1 || o[0].Fields["value_text"] != "1:128" {
		t.Errorf("SN ratio: %+v", o)
	}
	if p := run(sn("^abc")); len(p.rejects) != 1 {
		t.Error("SN not a number")
	}
	st := hl7test.Potassium(hat)
	st.Type, st.Value, st.Unit = "ST", "Haemolysed sample", ""
	if o := run(st).kind("observation"); len(o) != 1 || o[0].Fields["value_text"] != "Haemolysed sample" {
		t.Errorf("ST %+v", o)
	}
	cwe := hl7test.Potassium(hat)
	cwe.Type, cwe.Value, cwe.Unit = "CWE", "260373001^Detected^SCT", ""
	if o := run(cwe).kind("observation"); len(o) != 1 {
		t.Fatal("CWE")
	} else if vc := o[0].Fields["value_codings"].([]connector.Coding); vc[0].System != "urn:mhai:hl7v2:SCT" || vc[0].Code != "260373001" {
		t.Errorf("CWE %+v", vc)
	}
	for _, typ := range []string{"TX", "FT", "ED", "RP", "DT"} {
		k := hl7test.Potassium(hat)
		k.Type = typ
		if p := run(k); len(p.kind("observation")) != 0 || len(p.skipped) != 1 || p.skipped[0] != SkipValueType {
			t.Errorf("%s not skipped: %+v", typ, p)
		}
	}
	nm := hl7test.Potassium(hat)
	nm.Value = "6,9"
	if p := run(nm); len(p.rejects) != 1 || p.rejects[0].Reason != RowValue {
		t.Errorf("NM with a comma: %+v", p)
	}
	nm.Value = "+6.9"
	if o := run(nm).kind("observation"); len(o) != 1 || o[0].Fields["value_quantity"] != "6.9" {
		t.Error("NM with a plus")
	}
}

func TestHL7ReferenceRanges(t *testing.T) {
	cfg := hl7Config()
	for in, want := range map[string][3]string{"3.5-5.1": {"3.5", "5.1", ""}, "-2 - 2": {"-2", "2", ""},
		"<5": {"", "5", ""}, ">=10": {"10", "", ""}, "negative": {"", "", "negative"}} {
		k := hl7test.Potassium(hat)
		k.Range = in
		o := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg).kind("observation")[0].Fields
		got := [3]string{}
		got[0], _ = o["ref_low"].(string)
		got[1], _ = o["ref_high"].(string)
		got[2], _ = o["ref_text"].(string)
		if got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}

// Codes: LN is LOINC; a local system takes the unmapped path through its
// own system; a vital sign's LOINC makes it a vital sign.
func TestHL7CodeSystems(t *testing.T) {
	cfg := hl7Config()
	k := hl7test.Potassium(hat)
	k.Code, k.System = "K-SER", "99SYNLAB"
	o := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg).kind("observation")[0]
	if cs := o.Fields["codings"].([]connector.Coding); cs[0].System != "urn:mhai:hl7v2:99SYNLAB" {
		t.Errorf("local %+v", cs)
	}
	k.System = ""
	o = parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, k)), cfg).kind("observation")[0]
	if cs := o.Fields["codings"].([]connector.Coding); cs[0].System != "urn:mhai:hl7v2:local" {
		t.Errorf("no system %+v", cs)
	}
	hr := hl7test.Result{Code: "8867-4", System: "LN", Name: "Heart rate", Type: "NM", Value: "128", Unit: "/min", Status: "F", At: hat}
	o = parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, hr)), cfg).kind("observation")[0]
	if o.Fields["category"] != "vital-signs" {
		t.Errorf("vital sign %+v", o.Fields)
	}
}

func TestHL7Medication(t *testing.T) {
	cfg := hl7Config()
	p := parseHL7(t, []byte(hl7test.RDE("C1", "SYN-RX-1", hat)), cfg)
	meds := p.kind("medication")
	if len(meds) != 1 || len(p.rejects) != 0 {
		t.Fatalf("%+v", p)
	}
	f := meds[0].Fields
	cs := f["codings"].([]connector.Coding)
	if cs[0].System != codes.ATCSystem || cs[0].Code != "C09AA05" || f["kind"] != "request" || f["status"] != "active" ||
		f["dose_value"] != "5" || f["dose_unit"] != "mg" || f["route"] != "Oral" || f["timing_text"] != "QD" ||
		f["authored"] != "2026-11-27T10:15:00Z" || f["period_start"] != "2026-11-27T10:15:00Z" || f["med_text"] != "Ramipril" {
		t.Fatalf("medication %+v", f)
	}
	// Discontinued; an RxNorm code is not ATC.
	s := strings.Replace(hl7test.RDE("C2", "SYN-RX-1", hat), "|IP|", "|DC|", 1)
	s = strings.Replace(s, "C09AA05^Ramipril^WC", "29046^Ramipril^RXNORM", 1)
	m := parseHL7(t, []byte(s), cfg).kind("medication")[0]
	if m.Fields["status"] != "stopped" || m.Fields["codings"].([]connector.Coding)[0].System != "urn:mhai:hl7v2:RXNORM" ||
		m.SourceID != meds[0].SourceID || m.SourceVersion == meds[0].SourceVersion {
		t.Fatalf("discontinued %+v", m)
	}
}

// Unsupported and non-production messages are skipped; the rest of the
// file still loads.
func TestHL7SkipsWhatItDoesNotRead(t *testing.T) {
	cfg := hl7Config()
	test := strings.Replace(hl7test.ORU("C3", "O3", hat, hl7test.Potassium(hat)), "|P|2.5.1|", "|T|2.5.1|", 1)
	data := hl7test.File("f", hat, hl7test.ADT("C1", hat), hl7test.ORU("C2", "O2", hat, hl7test.Potassium(hat)), test,
		strings.Replace(hl7test.ADT("C4", hat), "ADT^A08^ADT_A01", "RAS^O17^RAS_O17", 1))
	p := parseHL7(t, data, cfg)
	if p.err != nil || len(p.kind("observation")) != 1 || len(p.skipped) != 3 {
		t.Fatalf("%+v", p)
	}
}

// Times without an offset: refused, never assumed UTC; read in the
// connection's zone when it has one.
func TestHL7TimeZones(t *testing.T) {
	noOffset := strings.ReplaceAll(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)), "+0000", "")
	cfg := hl7Config()
	p := parseHL7(t, []byte(noOffset), cfg)
	if len(p.recs) != 0 || len(p.rejects) == 0 {
		t.Fatalf("without a zone: %+v", p)
	}
	for _, r := range p.rejects {
		if r.Reason != RowTime {
			t.Errorf("reason %s", r.Reason)
		}
	}
	cfg.Settings["time_zone"] = "Africa/Lagos"
	o := parseHL7(t, []byte(noOffset), cfg).kind("observation")
	if len(o) != 1 || o[0].Fields["effective"] != "2026-11-27T10:15:00+01:00" {
		t.Fatalf("in Lagos: %+v", o)
	}
}

// Hospital isolation: a PID naming another hospital's authority is still
// this connection's patient (nothing in a message picks a hospital), and a
// sending_facility mismatch refuses the whole file.
func TestHL7NoMessageFieldPicksAHospital(t *testing.T) {
	cfg := hl7Config()
	other := strings.ReplaceAll(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)), hl7test.Facility, "OTHER-HOSP")
	p := parseHL7(t, []byte(other), cfg)
	for _, r := range p.recs {
		for k := range r.Fields {
			if k == "hospital" || k == "hospital_id" {
				t.Fatalf("a hospital field: %+v", r)
			}
		}
	}
	if _, ok := p.kind("patient")[0].Fields["identifier"]; !ok {
		t.Fatal("the MRN of type MR is kept whatever the authority when none is set")
	}
	cfg.Settings["sending_facility"] = hl7test.Facility
	data := hl7test.File("f", hat, hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)), other)
	if p := parseHL7(t, data, cfg); p.err == nil || p.err.Reason != ReasonFacility || len(p.recs) != 0 {
		t.Fatalf("facility: %+v", p)
	}
	if p := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat))), cfg); p.err != nil {
		t.Fatalf("matching facility refused: %v", p.err)
	}
}

// Each OBX is bound to the PID before it in the same message; an OBX with
// no PID before it is a dead letter, never attached to anyone.
func TestHL7RecordsBindToTheirOwnPID(t *testing.T) {
	cfg := hl7Config()
	second := strings.Join([]string{
		hl7test.Seg("PID", map[int]string{1: "2", 3: "SYN-MRN-0002^^^" + hl7test.Facility + "^MR", 5: "SYNTHOTHER^ZQ"}),
		hl7test.Seg("OBR", map[int]string{1: "1", 3: "O2^SYNTH-LAB", 7: hl7v2.FormatTime(hat)}),
		hl7test.OBX(1, hl7test.Potassium(hat)),
	}, "\r") + "\r"
	// Two patient groups in one message: no MSH between them.
	msg := hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)) + second
	p := parseHL7(t, []byte(msg), cfg)
	obs := p.kind("observation")
	if len(obs) != 2 || obs[0].Fields["patient"] == obs[1].Fields["patient"] {
		t.Fatalf("groups %+v", obs)
	}
	if obs[1].Fields["patient"] != joinID(hl7test.Facility, "MR", "SYN-MRN-0002") {
		t.Fatalf("second group bound to %v", obs[1].Fields["patient"])
	}
	if _, ok := obs[1].Fields["encounter"]; ok {
		t.Fatal("the first group's encounter leaked into the second")
	}
	noPID := strings.Replace(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)), hl7test.PID()+"\r", "", 1)
	p = parseHL7(t, []byte(noPID), cfg)
	if len(p.kind("observation")) != 0 || len(p.rejects) != 1 || p.rejects[0].Kind != "observation" {
		t.Fatalf("no PID: %+v", p)
	}
	noID := strings.Replace(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat)), "|"+hl7test.MRN+"^^^"+hl7test.Facility+"^MR~SYN-NAT-9^^^SYNTH-NATIONAL^NI|", "||", 1)
	p = parseHL7(t, []byte(noID), cfg)
	if len(p.kind("observation")) != 0 || len(p.kind("patient")) != 0 || len(p.rejects) != 2 {
		t.Fatalf("no PID-3: %+v", p)
	}
}

func TestHL7FileRefusals(t *testing.T) {
	cfg := hl7Config()
	good := hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat))
	for name, data := range map[string]string{
		"encoding": strings.Replace(good, hl7test.Family, "SYN\xc3\x28", 1),
		"header":   strings.Replace(good, "|C1|", "||", 1),
		"envelope": hl7v2.FileHeader("x", hat) + good + hl7v2.FileTrailer(2),
	} {
		if p := parseHL7(t, []byte(data), cfg); p.err == nil || len(p.recs) != 0 {
			t.Errorf("%s: %+v", name, p)
		}
	}
	// An unreadable escape is a dead letter of that record, not the file.
	esc := strings.Replace(good, hl7test.Family, `SYN\Cxx\`, 1)
	if p := parseHL7(t, []byte(esc), cfg); p.err != nil || len(p.rejects) != 1 || p.rejects[0].Kind != "patient" {
		t.Errorf("escape: %+v", p)
	}
}

func TestHL7Validate(t *testing.T) {
	c := NewHL7(nil)
	if c.Type().ID != HL7ID || len(c.Type().Fields) == 0 {
		t.Fatal("type")
	}
	if err := c.Validate(hl7Config()); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(map[string]string){
		"format setting":     func(s map[string]string) { s["format"] = Format },
		"namespace, no type": func(s map[string]string) { delete(s, "mrn_identifier_type") },
		"type, no namespace": func(s map[string]string) { delete(s, "mrn_namespace") },
		"authority, no type": func(s map[string]string) {
			delete(s, "mrn_identifier_type")
			delete(s, "mrn_namespace")
			s["mrn_assigning_authority"] = "X"
		},
		"bad zone":             func(s map[string]string) { s["time_zone"] = "Mars/Olympus" },
		"local zone":           func(s map[string]string) { s["time_zone"] = "Local" },
		"bad type":             func(s map[string]string) { s["mrn_identifier_type"] = "M R" },
		"facility with spaces": func(s map[string]string) { s["sending_facility"] = " X" },
		"folder traversal":     func(s map[string]string) { s["folder"] = "/outbound/../etc" },
	} {
		cfg := hl7Config()
		mut(cfg.Settings)
		if c.Validate(cfg) == nil {
			t.Errorf("%s: valid", name)
		}
	}
	csv := s0config()
	if c.Validate(csv) == nil {
		t.Error("a file_export config validates as hl7v2_file")
	}
}

func s0config() connector.Config {
	cfg := hl7Config()
	cfg.Type = ID
	return cfg
}

// The mapper never panics and never yields a record outside the five
// kinds, or a source id the sink would refuse.
func FuzzParseHL7(f *testing.F) {
	f.Add([]byte(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat))))
	f.Add([]byte(hl7test.RDE("C2", "R1", hat)))
	f.Add(hl7test.File("f", hat, hl7test.ADT("C3", hat), hl7test.ORU("C4", "O4", hat, hl7test.Potassium(hat))))
	cfg := hl7Config()
	cfg.Settings["time_zone"] = "Africa/Lagos"
	f.Fuzz(func(t *testing.T, data []byte) {
		for row, err := range ParseHL7(data, cfg) {
			if err != nil {
				return
			}
			for _, r := range row.Records {
				if !strings.Contains("patient encounter location observation medication", r.Kind) || r.SourceID == "" ||
					len(r.SourceID) > 256 || !printableID(r.SourceID) || len(r.SourceVersion) != 64 {
					t.Fatalf("record %q %q", r.Kind, r.SourceID)
				}
			}
			if row.Reject != nil && (row.Reject.SourceID == "" || len(row.Reject.SourceID) > 256) {
				t.Fatalf("reject %q", row.Reject.SourceID)
			}
		}
	})
}

// Security review: a record's source id includes its patient, so an order
// number a feed reuses for another patient is another record, never a new
// version that would supersede the first patient's row.
func TestHL7SourceIDsNameThePatient(t *testing.T) {
	cfg := hl7Config()
	a := parseHL7(t, []byte(hl7test.ORU("C1", "O1", hat, hl7test.Potassium(hat))), cfg)
	other := strings.Replace(hl7test.ORU("C2", "O1", hat, hl7test.Potassium(hat)), hl7test.MRN, "SYN-MRN-0002", 1)
	b := parseHL7(t, []byte(other), cfg)
	for _, kind := range []string{"observation", "encounter"} {
		if a.kind(kind)[0].SourceID == b.kind(kind)[0].SourceID {
			t.Errorf("%s: one source id for two patients", kind)
		}
	}
	ra := parseHL7(t, []byte(hl7test.RDE("C3", "R1", hat)), cfg).kind("medication")[0]
	rb := parseHL7(t, []byte(strings.Replace(hl7test.RDE("C4", "R1", hat), hl7test.MRN, "SYN-MRN-0002", 1)), cfg).kind("medication")[0]
	if ra.SourceID == rb.SourceID {
		t.Error("medication: one source id for two patients")
	}
}
