package account

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestParseHospitalRef(t *testing.T) {
	id := uuid.New()
	r, err := parseHospitalRef(" " + id.String() + " ")
	if err != nil || r.id != id || r.code != "" {
		t.Fatalf("uuid: %+v %v", r, err)
	}
	if c, a := r.where(); c != "id = $1" || a != id {
		t.Fatalf("uuid where: %s %v", c, a)
	}
	r, err = parseHospitalRef("hav")
	if err != nil || r.code != "HAV" || r.id != uuid.Nil {
		t.Fatalf("code: %+v %v", r, err)
	}
	if c, a := r.where(); c != "short_code = $1" || a != "HAV" {
		t.Fatalf("code where: %s %v", c, a)
	}
}

func TestParseHospitalRefRefusesOddInput(t *testing.T) {
	for _, s := range []string{"", " ", "H", "HAV'; --", "urn:uuid:" + uuid.NewString(),
		"{" + uuid.NewString() + "}", "a b", "Synthetic General", "abc\xff"} {
		if err := ValidateHospitalRef(s); !errors.Is(err, ErrBadHospitalRef) {
			t.Errorf("%q: %v", s, err)
		}
	}
}
