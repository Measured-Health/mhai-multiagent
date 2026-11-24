package account

import (
	"errors"
	"strings"
	"testing"
)

// The one check of a new hospital (the CLI and POST /platform/hospitals).
func TestCheckNewHospital(t *testing.T) {
	name, code, err := CheckNewHospital("  Synthetic General  ", "plt", "Africa/Lagos")
	if err != nil || name != "Synthetic General" || code != "PLT" {
		t.Fatalf("got %q %q %v", name, code, err)
	}
	long := strings.Repeat("é", HospitalNameMax)
	if _, _, err := CheckNewHospital(long, "PLT", "UTC"); err != nil {
		t.Errorf("%d characters refused: %v", HospitalNameMax, err)
	}
	for _, c := range []struct {
		name, code, tz string
		want           error
	}{
		{"", "PLT", "UTC", ErrBadHospitalName},
		{"   ", "PLT", "UTC", ErrBadHospitalName},
		{long + "e", "PLT", "UTC", ErrBadHospitalName},
		{"Synthetic\tGeneral", "PLT", "UTC", ErrBadHospitalName},
		{"Synthetic \xff", "PLT", "UTC", ErrBadHospitalName},
		{"Synthetic", "P", "UTC", ErrBadShortCode},
		{"Synthetic", "P LT", "UTC", ErrBadShortCode},
		{"Synthetic", "PLT", "", ErrBadTimeZone},
		{"Synthetic", "PLT", "Local", ErrBadTimeZone},
		{"Synthetic", "PLT", "Mars/Olympus", ErrBadTimeZone},
		{"Synthetic", "PLT", "../../etc/passwd", ErrBadTimeZone},
	} {
		if _, _, err := CheckNewHospital(c.name, c.code, c.tz); !errors.Is(err, c.want) {
			t.Errorf("%q %q %q: %v, want %v", c.name, c.code, c.tz, err, c.want)
		}
	}
}
