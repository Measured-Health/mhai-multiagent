package dblogin

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeRow answers the check's one row.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *bool:
			*p = r.vals[i].(bool)
		case *int64:
			*p = r.vals[i].(int64)
		}
	}
	return nil
}

type fakeQ struct{ row fakeRow }

func (q fakeQ) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

// fit is a fit login's answer: nothing but mhai_notify.
func fit() []any {
	return []any{false, false, false, false, false, false, false, int64(0), int64(0), int64(0), true}
}

func TestCheckRefusesEachPrivilege(t *testing.T) {
	ctx := context.Background()
	if err := Check(ctx, fakeQ{fakeRow{vals: fit()}}); err != nil {
		t.Fatalf("fit login: %v", err)
	}
	for i, reason := range []string{"superuser", "bypassrls", "createrole", "createdb", "replication", "migrate_member",
		"database_owner", "owns_schemas", "owns_relations", "other_roles", "not_notify"} {
		v := fit()
		switch x := v[i].(type) {
		case bool:
			v[i] = !x
		case int64:
			v[i] = int64(2)
		}
		err := Check(ctx, fakeQ{fakeRow{vals: v}})
		var pe *PrivilegedError
		if !errors.As(err, &pe) || pe.Reason != reason || !errors.Is(err, ErrPrivileged) {
			t.Errorf("%s: %v", reason, err)
		}
	}
	if err := Check(ctx, fakeQ{fakeRow{err: errors.New("synthetic: connection refused")}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no answer: %v", err)
	}
}
