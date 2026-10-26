package account

import (
	"errors"
	"testing"
)

func TestNormaliseEmail(t *testing.T) {
	got := NormaliseEmail(" Grace.Nwosu@Havana.example ")
	if got != "grace.nwosu@havana.example" {
		t.Fatalf("got %q", got)
	}
}

func TestPlatformRoleNeedsNoHospital(t *testing.T) {
	if err := ValidateRoles([]string{"platform_admin"}, "HAV"); !errors.Is(err, ErrRoleHospital) {
		t.Fatalf("platform_admin with hospital: %v", err)
	}
	if err := ValidateRoles([]string{"clinician"}, ""); !errors.Is(err, ErrRoleHospital) {
		t.Fatalf("clinician without hospital: %v", err)
	}
	if err := ValidateRoles([]string{"platform_admin"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoles([]string{"auditor"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoles([]string{"clinician", "hospital_admin"}, "HAV"); err != nil {
		t.Fatal(err)
	}
}

func TestRolesCombineOnlyClinical(t *testing.T) {
	for _, hosp := range []string{"", "HAV"} {
		if err := ValidateRoles([]string{"clinician", "platform_admin"}, hosp); err == nil {
			t.Fatalf("clinician+platform_admin accepted (hospital %q)", hosp)
		}
	}
	if err := ValidateRoles([]string{"platform_admin", "auditor"}, ""); !errors.Is(err, ErrRoleMix) {
		t.Fatalf("platform_admin+auditor: %v", err)
	}
}

func TestValidateRolesRejectsUnknownAndEmpty(t *testing.T) {
	for _, r := range [][]string{nil, {}, {"root"}, {"admin"}, {"clinician", "clinician"}} {
		if err := ValidateRoles(r, "HAV"); err == nil {
			t.Errorf("roles %v accepted", r)
		}
	}
}
