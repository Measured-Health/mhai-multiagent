package dbx

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Plan departure 6: the role is one of a closed set. Anything else, and a
// nil hospital or user, is refused before any SQL: the nil pool here would
// panic if it were touched.
func TestWithHospitalRoleIsClosed(t *testing.T) {
	ctx := context.Background()
	hid, uid := uuid.New(), uuid.New()
	ran := func(pgx.Tx) error { t.Fatal("fn ran"); return nil }
	for _, r := range []Role{"", "mhai_platform", "mhai_migrate", "postgres", "MHAI_GATEWAY", "mhai_gateway; RESET ROLE",
		"mhai_emr_app", "mhai_worker"} {
		if err := WithHospital(ctx, nil, r, hid, ran); !errors.Is(err, ErrRole) {
			t.Errorf("role %q: %v, want ErrRole", r, err)
		}
		if err := WithHospitalUser(ctx, nil, r, hid, uid, ran); !errors.Is(err, ErrRole) {
			t.Errorf("role %q with user: %v, want ErrRole", r, err)
		}
	}
	for _, r := range Roles {
		if err := WithHospital(ctx, nil, r, uuid.Nil, ran); !errors.Is(err, ErrNoHospital) {
			t.Errorf("%s nil hospital: %v", r, err)
		}
		if err := WithHospitalUser(ctx, nil, r, hid, uuid.Nil, ran); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s nil user: %v", r, err)
		}
	}
	if len(Roles) != 3 || Gateway != "mhai_gateway" || EMR != "mhai_emr" || EMRCore != "mhai_emr_core" {
		t.Fatalf("roles changed: %v", Roles)
	}
}
