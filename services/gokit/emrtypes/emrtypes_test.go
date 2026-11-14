package emrtypes

import (
	"regexp"
	"slices"
	"testing"
)

// The closed vocabulary (plan Global Constraints): connector type ids, the
// outcome codes; each type's fields are [a-z_]+ keys of a known kind.
func TestTypesClosed(t *testing.T) {
	ids := []string{}
	for _, ti := range Types {
		ids = append(ids, ti.ID)
	}
	if !slices.Equal(ids, []string{"fhir_r4", "document_archive", "file_export", "stub"}) {
		t.Fatalf("types %v", ids)
	}
	key := regexp.MustCompile(`^[a-z_]+$`)
	for _, ti := range Types {
		secrets := 0
		for _, f := range ti.Fields {
			if !key.MatchString(f.Key) || (f.Kind != KindText && f.Kind != KindSecret) || f.Max < 1 || f.Label == "" {
				t.Errorf("%s: field %+v", ti.ID, f)
			}
			if f.Kind == KindSecret {
				secrets++
				if f.Max != 4000 {
					t.Errorf("%s: secret %s max %d, want 4000", ti.ID, f.Key, f.Max)
				}
			}
		}
		if secrets == 0 {
			t.Errorf("%s declares no secret", ti.ID)
		}
	}
	if !slices.Equal(Codes, []string{"ok", "unreachable", "sign_in_refused", "timed_out", "unreadable_data",
		"unmapped_codes", "too_many_requests"}) {
		t.Fatalf("codes %v", Codes)
	}
	// FHIR R4 is built (EMR PR 6) and the file export (PR 11); the stub
	// only in development.
	if b := Built(false); !slices.Equal(b, []string{"fhir_r4", "file_export"}) {
		t.Fatalf("built outside development: %v", b)
	}
	if b := Built(true); !slices.Equal(b, []string{"fhir_r4", "file_export", "stub"}) {
		t.Fatalf("built in development: %v", b)
	}
	// The MRN namespace (design §10.2, §16.1): an optional text setting,
	// so the sink keeps the hospital's MRN and nothing else.
	fhir, _ := Lookup("fhir_r4")
	if f, ok := fhir.Field("mrn_namespace"); !ok || f.Kind != KindText || f.Required || f.Max != 200 {
		t.Fatalf("fhir_r4 mrn_namespace: %+v %v", f, ok)
	}
	// The file export (design §5.4, §15.4): a user name to sign in with,
	// the declared export interval (required: staleness needs it), the MRN
	// namespace optional, the SSH key the one secret.
	fe, _ := Lookup("file_export")
	var keys []string
	for _, f := range fe.Fields {
		keys = append(keys, f.Key)
	}
	if !slices.Equal(keys, []string{"folder", "username", "host_key_fingerprint", "format", "expected_every_minutes",
		"mrn_namespace", "access_key"}) || !slices.Equal(fe.Secrets(), []string{"access_key"}) {
		t.Fatalf("file_export fields %v", keys)
	}
	if f, _ := fe.Field("expected_every_minutes"); !f.Required || f.Kind != KindText {
		t.Fatalf("expected_every_minutes: %+v", f)
	}
	if f, _ := fe.Field("mrn_namespace"); f.Required {
		t.Fatalf("file_export mrn_namespace required")
	}
	if _, ok := Lookup("hl7v2_feed"); ok {
		t.Fatal("hl7v2_feed is not built and not offered")
	}
}

// Type ids, labels and summaries name a standard or a method, never a
// product (ADR 0003).
func TestTypeIDsNameNoProduct(t *testing.T) {
	for _, ti := range Types {
		for _, s := range []string{ti.ID, ti.Label, ti.Summary} {
			if v := VendorIn(s); v != "" {
				t.Errorf("%s names %q", ti.ID, v)
			}
		}
		for _, f := range ti.Fields {
			if v := VendorIn(f.Key + " " + f.Label); v != "" {
				t.Errorf("%s.%s names %q", ti.ID, f.Key, v)
			}
		}
	}
	for s, want := range map[string]string{
		"Main record": "", "EpicClient": "epic", "epic_fhir": "epic", "DHIS2 feed": "dhis", "Cerner lab": "cerner",
		"Epicentre": "", "Oracle": "oracle", "the athenahealth sync": "athenahealth", "Lab results": "",
	} {
		if got := VendorIn(s); got != want {
			t.Errorf("VendorIn(%q) = %q, want %q", s, got, want)
		}
	}
	if len(vendors) < 12 {
		t.Fatalf("vendor list has %d names", len(vendors))
	}
}
