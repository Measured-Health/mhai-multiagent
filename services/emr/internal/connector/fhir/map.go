package fhir

// Resource to raw record, through allow-listed structs (design §14.1): only
// the fields below are decoded at all, so an address, a telecom, a note, an
// extension, contained resources or narrative never reach memory as data
// we keep, let alone a raw record. RawSink applies the allow-list again,
// with its size and shape rules, before anything is stored.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

const (
	categorySystem = "http://terminology.hl7.org/CodeSystem/observation-category"
	ucumSystem     = "http://unitsofmeasure.org"
	maxCodings     = 10
)

type coding struct {
	System  string `json:"system"`
	Code    string `json:"code"`
	Display string `json:"display"`
}

type codeableConcept struct {
	Coding []coding `json:"coding"`
	Text   string   `json:"text"`
}

type reference struct {
	Reference string `json:"reference"`
}

type quantity struct {
	Value      json.Number `json:"value"`
	Comparator string      `json:"comparator"`
	Unit       string      `json:"unit"`
	System     string      `json:"system"`
	Code       string      `json:"code"`
}

type period struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type header struct {
	ResourceType string `json:"resourceType"`
	ID           string `json:"id"`
	Meta         struct {
		VersionID   string `json:"versionId"`
		LastUpdated string `json:"lastUpdated"`
	} `json:"meta"`
}

type observation struct {
	Status               string            `json:"status"`
	Category             []codeableConcept `json:"category"`
	Code                 codeableConcept   `json:"code"`
	Subject              reference         `json:"subject"`
	Encounter            reference         `json:"encounter"`
	EffectiveDateTime    string            `json:"effectiveDateTime"`
	EffectiveInstant     string            `json:"effectiveInstant"`
	EffectivePeriod      *period           `json:"effectivePeriod"`
	Issued               string            `json:"issued"`
	ValueQuantity        *quantity         `json:"valueQuantity"`
	ValueString          *string           `json:"valueString"`
	ValueCodeableConcept *codeableConcept  `json:"valueCodeableConcept"`
	Interpretation       []codeableConcept `json:"interpretation"`
	ReferenceRange       []struct {
		Low  *quantity `json:"low"`
		High *quantity `json:"high"`
		Text string    `json:"text"`
	} `json:"referenceRange"`
}

type dosage struct {
	Route  *codeableConcept `json:"route"`
	Timing *struct {
		Code *codeableConcept `json:"code"`
	} `json:"timing"`
	DoseAndRate []struct {
		DoseQuantity *quantity `json:"doseQuantity"`
	} `json:"doseAndRate"`
}

type medication struct {
	Status                    string           `json:"status"`
	MedicationCodeableConcept *codeableConcept `json:"medicationCodeableConcept"`
	Subject                   reference        `json:"subject"`
	AuthoredOn                string           `json:"authoredOn"`
	EffectiveDateTime         string           `json:"effectiveDateTime"`
	EffectivePeriod           *period          `json:"effectivePeriod"`
	DosageInstruction         []dosage         `json:"dosageInstruction"`
	Dosage                    []dosage         `json:"dosage"`
}

type patient struct {
	Identifier []struct {
		System string `json:"system"`
		Value  string `json:"value"`
	} `json:"identifier"`
	Name []struct {
		Use    string   `json:"use"`
		Family string   `json:"family"`
		Given  []string `json:"given"`
	} `json:"name"`
	Gender    string `json:"gender"`
	BirthDate string `json:"birthDate"`
	Active    *bool  `json:"active"`
	Link      []struct {
		Other reference `json:"other"`
		Type  string    `json:"type"`
	} `json:"link"`
}

type encounter struct {
	Status   string    `json:"status"`
	Class    *coding   `json:"class"`
	Subject  reference `json:"subject"`
	Period   *period   `json:"period"`
	Location []struct {
		Location reference `json:"location"`
	} `json:"location"`
}

type location struct {
	Name         string            `json:"name"`
	PhysicalType *codeableConcept  `json:"physicalType"`
	Type         []codeableConcept `json:"type"`
}

// refID is the id a reference names when it is to a typ on this server:
// "Patient/123", "Patient/123/_history/2", or the base address followed by
// that; anything else (another server, a contained "#x", a malformed id)
// is "".
func refID(r reference, typ, base string) string {
	s := r.Reference
	if base != "" {
		if rest, ok := strings.CutPrefix(s, strings.TrimSuffix(base, "/")+"/"); ok {
			s = rest
		}
	}
	rest, ok := strings.CutPrefix(s, typ+"/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/_history/")
	if !idShape.MatchString(id) {
		return ""
	}
	return id
}

func codings(cc *codeableConcept) []connector.Coding {
	if cc == nil {
		return nil
	}
	var out []connector.Coding
	for _, c := range cc.Coding {
		if c.Code == "" || len(out) == maxCodings {
			continue
		}
		out = append(out, connector.Coding{System: c.System, Code: c.Code, Display: c.Display})
	}
	return out
}

// set puts v under k when it is not empty.
func set(m map[string]any, k string, v any) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case []connector.Coding:
		if len(x) == 0 {
			return
		}
	}
	m[k] = v
}

func (q *quantity) value() string {
	if q == nil || q.Value == "" {
		return ""
	}
	switch q.Comparator {
	case "<", "<=", ">=", ">":
		return q.Comparator + q.Value.String()
	case "":
		return q.Value.String()
	}
	return "" // "ad" and the like: not a value we read
}

func (q *quantity) unit() string {
	if q == nil {
		return ""
	}
	if q.System == ucumSystem && q.Code != "" {
		return q.Code
	}
	return q.Unit
}

// mapResource reads raw as a typ and maps it to a raw record. ok is false
// when it isn't a typ, or has no usable id or lastUpdated.
func mapResource(raw []byte, typ, base string) (connector.RawRecord, bool) {
	var h header
	if json.Unmarshal(raw, &h) != nil || h.ResourceType != typ || !idShape.MatchString(h.ID) || len(h.Meta.VersionID) > 64 {
		return connector.RawRecord{}, false
	}
	lu, err := time.Parse(time.RFC3339Nano, h.Meta.LastUpdated)
	if err != nil || !strings.Contains(h.Meta.LastUpdated, "T") {
		return connector.RawRecord{}, false
	}
	r := connector.RawRecord{SourceID: h.ID, SourceVersion: h.Meta.VersionID, SourceUpdatedAt: lu.UTC(), Fields: map[string]any{}}
	f := r.Fields
	switch typ {
	case "Observation":
		var o observation
		if json.Unmarshal(raw, &o) != nil {
			return connector.RawRecord{}, false
		}
		r.Kind = "observation"
		for _, cat := range o.Category {
			for _, c := range cat.Coding {
				if (c.System == categorySystem || c.System == "") && (c.Code == "laboratory" || c.Code == "vital-signs") {
					set(f, "category", c.Code)
				}
			}
		}
		set(f, "status", o.Status)
		set(f, "codings", codings(&o.Code))
		set(f, "code_text", o.Code.Text)
		set(f, "patient", refID(o.Subject, "Patient", base))
		set(f, "encounter", refID(o.Encounter, "Encounter", base))
		eff := o.EffectiveDateTime
		if eff == "" {
			eff = o.EffectiveInstant
		}
		if eff == "" && o.EffectivePeriod != nil {
			eff = o.EffectivePeriod.Start
		}
		set(f, "effective", eff)
		set(f, "issued", o.Issued)
		set(f, "value_quantity", o.ValueQuantity.value())
		set(f, "unit", o.ValueQuantity.unit())
		if o.ValueString != nil {
			set(f, "value_text", *o.ValueString)
		}
		set(f, "value_codings", codings(o.ValueCodeableConcept))
		if len(o.Interpretation) > 0 && len(o.Interpretation[0].Coding) > 0 {
			set(f, "interpretation", o.Interpretation[0].Coding[0].Code)
		}
		if len(o.ReferenceRange) > 0 {
			rr := o.ReferenceRange[0]
			set(f, "ref_low", rr.Low.value())
			set(f, "ref_high", rr.High.value())
			set(f, "ref_text", rr.Text)
		}
	case "MedicationRequest", "MedicationStatement":
		var m medication
		if json.Unmarshal(raw, &m) != nil {
			return connector.RawRecord{}, false
		}
		r.Kind = "medication"
		ds := m.DosageInstruction
		if typ == "MedicationRequest" {
			set(f, "kind", "request")
			set(f, "authored", m.AuthoredOn)
		} else {
			set(f, "kind", "statement")
			ds = m.Dosage
			set(f, "period_start", m.EffectiveDateTime)
			if m.EffectivePeriod != nil {
				set(f, "period_start", m.EffectivePeriod.Start)
				set(f, "period_end", m.EffectivePeriod.End)
			}
		}
		set(f, "status", m.Status)
		set(f, "patient", refID(m.Subject, "Patient", base))
		set(f, "codings", codings(m.MedicationCodeableConcept))
		if m.MedicationCodeableConcept != nil {
			set(f, "med_text", m.MedicationCodeableConcept.Text)
		}
		if len(ds) > 0 {
			d := ds[0]
			if d.Route != nil {
				route := d.Route.Text
				if route == "" && len(d.Route.Coding) > 0 {
					route = d.Route.Coding[0].Display
				}
				set(f, "route", route)
			}
			if d.Timing != nil && d.Timing.Code != nil {
				set(f, "timing_text", d.Timing.Code.Text)
			}
			if len(d.DoseAndRate) > 0 {
				set(f, "dose_value", d.DoseAndRate[0].DoseQuantity.value())
				set(f, "dose_unit", d.DoseAndRate[0].DoseQuantity.unit())
			}
		}
	case "Patient":
		var p patient
		if json.Unmarshal(raw, &p) != nil {
			return connector.RawRecord{}, false
		}
		r.Kind = "patient"
		var ids []connector.Coding
		for _, id := range p.Identifier {
			if id.Value != "" && len(ids) < maxCodings {
				ids = append(ids, connector.Coding{System: id.System, Code: id.Value})
			}
		}
		set(f, "identifier", ids)
		name := -1
		for i, n := range p.Name {
			if n.Use == "official" {
				name = i
				break
			}
		}
		if name < 0 && len(p.Name) > 0 {
			name = 0
		}
		if name >= 0 {
			set(f, "given", strings.Join(p.Name[name].Given, " "))
			set(f, "family", p.Name[name].Family)
		}
		set(f, "sex", p.Gender)
		set(f, "birth_date", p.BirthDate)
		if p.Active != nil {
			f["active"] = *p.Active
		}
		for _, l := range p.Link {
			if l.Type != "replaced-by" {
				continue
			}
			if id := refID(l.Other, "Patient", base); id != "" {
				set(f, "link_replaced_by", id)
			} else {
				f["link_replaced_by_external"] = true
			}
		}
	case "Encounter":
		var e encounter
		if json.Unmarshal(raw, &e) != nil {
			return connector.RawRecord{}, false
		}
		r.Kind = "encounter"
		set(f, "status", e.Status)
		if e.Class != nil {
			set(f, "class", e.Class.Code)
		}
		set(f, "patient", refID(e.Subject, "Patient", base))
		if e.Period != nil {
			set(f, "period_start", e.Period.Start)
			set(f, "period_end", e.Period.End)
		}
		if len(e.Location) > 0 {
			set(f, "location", refID(e.Location[0].Location, "Location", base))
		}
	case "Location":
		var l location
		if json.Unmarshal(raw, &l) != nil {
			return connector.RawRecord{}, false
		}
		r.Kind = "location"
		set(f, "name", l.Name)
		set(f, "type", locationKind(l))
	default:
		return connector.RawRecord{}, false
	}
	return r, true
}

// locationKind maps the FHIR location codes we know to a unit kind (the
// normaliser's ward, clinic, emergency); anything else is left out.
func locationKind(l location) string {
	if l.PhysicalType != nil {
		for _, c := range l.PhysicalType.Coding {
			if c.Code == "wa" {
				return "ward"
			}
		}
	}
	for _, t := range l.Type {
		for _, c := range t.Coding {
			switch c.Code {
			case "ER", "ETU":
				return "emergency"
			case "OF", "AMB":
				return "clinic"
			}
		}
	}
	return ""
}
