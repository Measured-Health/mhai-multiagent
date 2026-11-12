package fhir

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

// The allow-list structs: a field we don't read is never decoded, so it
// can't reach a raw record (design §14.1).
func TestUnknownFieldsNeverStored(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	o := obsRes("o-1", "2026-11-12T07:00:00Z", "p-1")
	o["note"] = []any{map[string]any{"text": "MARKER-NOTE free text about the patient"}}
	o["performer"] = []any{map[string]any{"display": "MARKER-PRACTITIONER"}}
	o["extension"] = []any{map[string]any{"url": "urn:x", "valueString": "MARKER-EXTENSION"}}
	o["text"] = map[string]any{"div": "<div>MARKER-NARRATIVE</div>"}
	o["contained"] = []any{map[string]any{"resourceType": "Patient", "id": "c", "name": []any{map[string]any{"family": "MARKER-CONTAINED"}}}}
	f.add("Observation", o)
	p := patientRes("p-1", "2026-11-01T00:00:00Z")
	p["address"] = []any{map[string]any{"line": []any{"MARKER-ADDRESS 1 Synthetic Road"}}}
	p["telecom"] = []any{map[string]any{"system": "phone", "value": "MARKER-PHONE"}}
	p["contact"] = []any{map[string]any{"name": map[string]any{"family": "MARKER-KIN"}}}
	p["identifier"] = append(p["identifier"].([]any), map[string]any{"system": "urn:national-id", "value": "MARKER-NIN"})
	f.add("Patient", p)
	sink := &memSink{}
	if _, err := newFetcher(f).Fetch(context.Background(), f.cfg(), credsWith(key), nil, sink); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(sink.recs)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, m := range []string{"MARKER-NOTE", "MARKER-PRACTITIONER", "MARKER-EXTENSION", "MARKER-NARRATIVE", "MARKER-CONTAINED",
		"MARKER-ADDRESS", "MARKER-PHONE", "MARKER-KIN"} {
		if strings.Contains(out, m) {
			t.Errorf("%s reached a raw record", m)
		}
	}
	// What is read, is read: the observation's mapped fields.
	var obs connector.RawRecord
	for _, r := range sink.recs {
		if r.Kind == "observation" {
			obs = r
		}
	}
	if obs.SourceID != "o-1" || obs.SourceVersion != "1" || obs.SourceUpdatedAt.IsZero() ||
		obs.Fields["patient"] != "p-1" || obs.Fields["category"] != "laboratory" || obs.Fields["status"] != "final" ||
		obs.Fields["value_quantity"] != "5.40" || obs.Fields["unit"] != "mmol/L" || obs.Fields["interpretation"] != "H" ||
		obs.Fields["effective"] != "2026-11-10T07:30:00+01:00" || obs.Fields["ref_low"] != "3.5" || obs.Fields["ref_high"] != "5.1" {
		t.Fatalf("observation fields: %+v", obs)
	}
	if cs, _ := obs.Fields["codings"].([]connector.Coding); len(cs) != 1 || cs[0].Code != "K" {
		t.Fatalf("codings: %+v", obs.Fields["codings"])
	}
}

// References are read from the same server only, by _id in batches; an
// absolute reference elsewhere, a malformed id or an entry not asked for
// is ignored. The patient is put before the records that name it.
func TestReferencesFetched(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	o1 := obsRes("o-1", "2026-11-12T07:00:00Z", "p-1")
	o1["encounter"] = map[string]any{"reference": "Encounter/e-1"}
	o2 := obsRes("o-2", "2026-11-12T07:01:00Z", "x")
	o2["subject"] = map[string]any{"reference": "https://evil.example.org/fhir/Patient/p-9"}
	o3 := obsRes("o-3", "2026-11-12T07:02:00Z", "x")
	o3["subject"] = map[string]any{"reference": "Patient/bad id,p-2"}
	o4 := obsRes("o-4", "2026-11-12T07:03:00Z", "x")
	o4["subject"] = map[string]any{"reference": f.base() + "/Patient/p-3"}
	for _, o := range []map[string]any{o1, o2, o3, o4} {
		f.add("Observation", o)
	}
	f.add("Patient", patientRes("p-1", "2026-11-01T00:00:00Z"))
	f.add("Patient", patientRes("p-2", "2026-11-01T00:00:00Z"))
	f.add("Patient", patientRes("p-3", "2026-11-01T00:00:00Z"))
	f.add("Encounter", map[string]any{"resourceType": "Encounter", "id": "e-1", "meta": map[string]any{"versionId": "1", "lastUpdated": "2026-11-11T00:00:00Z"},
		"status": "in-progress", "class": map[string]any{"code": "IMP"}, "subject": map[string]any{"reference": "Patient/p-1"},
		"period":   map[string]any{"start": "2026-11-11T00:00:00Z"},
		"location": []any{map[string]any{"location": map[string]any{"reference": "Location/l-1"}}}})
	f.add("Location", map[string]any{"resourceType": "Location", "id": "l-1", "meta": map[string]any{"versionId": "1", "lastUpdated": "2026-01-01T00:00:00Z"},
		"name": "Ward 3", "physicalType": map[string]any{"coding": []any{map[string]any{"code": "wa"}}}})
	sink := &memSink{}
	if _, err := newFetcher(f).Fetch(context.Background(), f.cfg(), credsWith(key), nil, sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.ids("patient"); !slices.Equal(got, []string{"p-1", "p-3"}) {
		t.Fatalf("patients %v", got)
	}
	if got := sink.ids("encounter"); !slices.Equal(got, []string{"e-1"}) {
		t.Fatalf("encounters %v", got)
	}
	if got := sink.ids("location"); !slices.Equal(got, []string{"l-1"}) {
		t.Fatalf("locations %v", got)
	}
	// Order: locations, patients, encounters, then the page's records.
	var kinds []string
	for _, r := range sink.recs {
		if len(kinds) == 0 || kinds[len(kinds)-1] != r.Kind {
			kinds = append(kinds, r.Kind)
		}
	}
	if !slices.Equal(kinds, []string{"location", "patient", "encounter", "observation"}) {
		t.Fatalf("put order %v", kinds)
	}
	for _, r := range f.reqs() {
		if strings.Contains(r, "evil") || strings.Contains(r, "bad") {
			t.Fatalf("asked for %s", r)
		}
	}
	for _, r := range sink.recs {
		if r.SourceID == "o-2" || r.SourceID == "o-3" {
			if _, has := r.Fields["patient"]; has {
				t.Fatalf("%s kept a patient reference it shouldn't: %v", r.SourceID, r.Fields["patient"])
			}
		}
		if r.SourceID == "e-1" && (r.Fields["patient"] != "p-1" || r.Fields["location"] != "l-1" || r.Fields["class"] != "IMP") {
			t.Fatalf("encounter fields %+v", r.Fields)
		}
		if r.SourceID == "l-1" && (r.Fields["name"] != "Ward 3" || r.Fields["type"] != "ward") {
			t.Fatalf("location fields %+v", r.Fields)
		}
		if r.SourceID == "p-1" && (r.Fields["given"] != "Synthia" || r.Fields["family"] != "Testperson" || r.Fields["sex"] != "female" ||
			r.Fields["birth_date"] != "1970-01-02") {
			t.Fatalf("patient fields %+v", r.Fields)
		}
	}
}

// The medication mapping.
func TestMedicationMapped(t *testing.T) {
	r, ok := mapResource([]byte(mustJSON(t, medRes("m-1", "2026-11-12T06:00:00Z", "p-1"))), "MedicationRequest", "")
	if !ok || r.Kind != "medication" || r.Fields["kind"] != "request" || r.Fields["status"] != "active" ||
		r.Fields["dose_value"] != "40" || r.Fields["dose_unit"] != "mg" || r.Fields["route"] != "oral" ||
		r.Fields["timing_text"] != "twice daily" || r.Fields["authored"] != "2026-11-11T10:00:00Z" || r.Fields["med_text"] != "Loop diuretic one" {
		t.Fatalf("%v %+v", ok, r)
	}
	// A resource of another type than asked for, or without an id, is not
	// mapped.
	if _, ok := mapResource([]byte(mustJSON(t, medRes("m-1", "2026-11-12T06:00:00Z", "p-1"))), "Observation", ""); ok {
		t.Fatal("mapped a MedicationRequest as an Observation")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
