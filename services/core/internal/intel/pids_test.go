package intel

import (
	"context"
	"errors"
	"testing"

	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

// Synthetic patient ids, never short numbers.
const (
	pidOwn   = "6f1c2a7e-93b4-4d0f-8e21-5a9c7d3b1f42"
	pidOther = "b83e0d51-2c6a-47f9-9d10-e47a6c58b0f3"
)

// patientsCarried is core's copy of the worker's check 8 for patients:
// every patient id a body names, at any depth, is one the caller carries.
func TestPatientsCarried(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		pids []string
		job  *intelpb.RunQueryJob
	}{
		{"no patient, no pids", true, nil, &intelpb.RunQueryJob{QueryId: "q"}},
		{"no patient, pids", true, []string{pidOwn}, &intelpb.RunQueryJob{QueryId: "q"}},
		{"carried", true, []string{pidOwn}, &intelpb.RunQueryJob{QueryId: "q", PatientId: pidOwn}},
		{"not carried", false, []string{pidOwn}, &intelpb.RunQueryJob{QueryId: "q", PatientId: pidOther}},
		{"none carried", false, nil, &intelpb.RunQueryJob{QueryId: "q", PatientId: pidOwn}},
	} {
		if got := patientsCarried(tc.job, tc.pids); got != tc.ok {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// A repeated field: every element must be carried, and an empty one
	// never is.
	g := &intelpb.GrantRequest{PatientIds: []string{pidOwn, pidOther}}
	if patientsCarried(g, []string{pidOwn}) || !patientsCarried(g, []string{pidOwn, pidOther}) {
		t.Error("repeated patient_ids")
	}
	if patientsCarried(&intelpb.GrantRequest{PatientIds: []string{""}}, []string{pidOwn}) {
		t.Error("an empty element was carried")
	}
}

// Only CheckPatient adds a patient; it needs a hospital and a transaction.
func TestCheckPatientNeedsHospitalAndTx(t *testing.T) {
	c := Caller{Sub: pidOther, Rid: "rid-synthetic", Roles: []string{"clinician"}}
	if _, err := CheckPatient(context.Background(), nil, c, [16]byte{1}); !errors.Is(err, ErrMisuse) {
		t.Fatalf("no hospital: %v", err)
	}
	c.Hid = pidOwn
	if _, err := CheckPatient(context.Background(), nil, c, [16]byte{1}); !errors.Is(err, ErrMisuse) {
		t.Fatalf("no transaction: %v", err)
	}
	if code, _ := Code(ErrNoPatient); code != "not_found" {
		t.Fatalf("ErrNoPatient maps to %q", code)
	}
}
