package account

import (
	"errors"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
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

// Every clinical role authz knows is a clinical role here, with a hospital
// (final review M-8: one list, authz.ClinicalRoles).
func TestClinicalRolesAreAuthzs(t *testing.T) {
	for _, r := range authz.ClinicalRoles() {
		if err := ValidateRoles([]string{r}, "SYN1"); err != nil {
			t.Errorf("%s with a hospital: %v", r, err)
		}
		if err := ValidateRoles([]string{r}, ""); err == nil {
			t.Errorf("%s without a hospital accepted", r)
		}
	}
}
