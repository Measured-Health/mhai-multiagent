//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
)

// core users roles, as the provisioning login: the same database function
// and rules as the app (T26, CLI). Every account is synthetic.

type rolesWorld struct {
	super      *pgxpool.Pool
	url        string
	h, h2      uuid.UUID
	ids        map[string]uuid.UUID // by email
	notified   []uuid.UUID
	notifiedAt []uuid.UUID
}

func newRolesWorld(t *testing.T) *rolesWorld {
	t.Helper()
	super, url, _ := analysisEnv(t)
	w := &rolesWorld{super: super, url: url, ids: map[string]uuid.UUID{}}
	ctx := context.Background()
	for _, h := range []*uuid.UUID{&w.h, &w.h2} {
		if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status, time_zone)
			VALUES ('Synthetic Hospital', $1, 'active', 'Africa/Lagos') RETURNING id`, "R"+uuid.NewString()[:8]).Scan(h); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal) VALUES (NULL, 'on', 'SO-2026-0001', 'SO-2026-0001', 'SO-2026-0001')`,
		`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal) VALUES ($1, 'on', 'SO-2026-0002', 'SO-2026-0002', 'SO-2026-0002')`,
		`INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal) VALUES ($2, 'on', 'SO-2026-0003', 'SO-2026-0003', 'SO-2026-0003')`,
	} {
		args := []any{}
		if strings.Contains(q, "$1") {
			args = []any{w.h}
		} else if strings.Contains(q, "$2") {
			q = strings.Replace(q, "$2", "$1", 1)
			args = []any{w.h2}
		}
		if _, err := super.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	w.add(t, "admin@r.example.test", w.h, true, true, "clinician", "hospital_admin")
	w.add(t, "ada@r.example.test", w.h, true, true, "clinician")
	w.add(t, "nomfa@r.example.test", w.h, true, false, "clinician")
	w.add(t, "plain@r.example.test", w.h, true, true, "clinician")
	w.add(t, "admin2@r.example.test", w.h2, true, true, "hospital_admin")
	w.add(t, "gone@r.example.test", w.h, false, true, "hospital_admin")
	return w
}

func (w *rolesWorld) add(t *testing.T, email string, h uuid.UUID, active, mfa bool, roles ...string) {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := w.super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, is_active)
		VALUES ($1, $2, 'synthetic-hash', 'Synthetic Staff', $3) RETURNING id`, h, email, active).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := w.super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.super.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', $2)`, id, mfa); err != nil {
		t.Fatal(err)
	}
	w.ids[email] = id
}

func (w *rolesWorld) run(args ...string) (string, error) {
	var out bytes.Buffer
	err := usersRoles(context.Background(), args, w.url, &out, func(_ context.Context, h, p uuid.UUID) {
		w.notifiedAt = append(w.notifiedAt, h)
		w.notified = append(w.notified, p)
	})
	return out.String(), err
}

func (w *rolesWorld) analyst(t *testing.T, email string) bool {
	t.Helper()
	return scalar(t, w.super, `SELECT count(*) FROM user_roles WHERE user_id = $1 AND role = 'analyst'`, w.ids[email]) == 1
}

// T26 (CLI): the database's refusals, through the same function.
func TestCLIRolesUsesTheFunction(t *testing.T) {
	w := newRolesWorld(t)
	for _, c := range []struct {
		person, by string
		want       error
	}{
		{"admin@r.example.test", "admin@r.example.test", analysis.ErrAnalystSelf},
		{"nomfa@r.example.test", "admin@r.example.test", analysis.ErrAnalystNotEligible},
		{"ada@r.example.test", "plain@r.example.test", analysis.ErrAnalystForbidden},
		{"ada@r.example.test", "gone@r.example.test", analysis.ErrAnalystForbidden},
		{"nobody@r.example.test", "admin@r.example.test", analysis.ErrAnalystNotFound},
	} {
		out, err := w.run(c.person, "--add", "analyst", "--by", c.by)
		if !errors.Is(err, c.want) || out != "" {
			t.Errorf("%s by %s: want %v, got %v %q", c.person, c.by, c.want, err, out)
		}
		if err != nil && strings.Contains(err.Error(), "@") {
			t.Errorf("an error echoes an email: %v", err)
		}
	}
	// The switch off.
	if _, err := w.super.Exec(context.Background(), `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, w.h); err != nil {
		t.Fatal(err)
	}
	if _, err := w.run("ada@r.example.test", "--add", "analyst", "--by", "admin@r.example.test"); !errors.Is(err, analysis.ErrAnalystOff) {
		t.Errorf("switch off: %v", err)
	}
	if _, err := w.super.Exec(context.Background(), `UPDATE analysis_switch SET position = 'on' WHERE hospital_id = $1`, w.h); err != nil {
		t.Fatal(err)
	}
	if w.analyst(t, "ada@r.example.test") || len(w.notified) != 0 {
		t.Fatal("a refused grant changed something")
	}

	out, err := w.run("ADA@r.example.test", "--add", "analyst", "--by", "admin@r.example.test")
	if err != nil || out != "roles "+w.ids["ada@r.example.test"].String()+" analyst added\n" || !w.analyst(t, "ada@r.example.test") {
		t.Fatalf("grant: %q %v", out, err)
	}
	if n := scalar(t, w.super, `SELECT count(*) FROM audit_log WHERE action = 'analyst.granted' AND entity = 'user' AND entity_id = $1
		AND actor_id = $2 AND hospital_id = $3 AND payload->>'via' = 'cli' AND payload::text NOT LIKE '%@%'`,
		w.ids["ada@r.example.test"].String(), w.ids["admin@r.example.test"], w.h); n != 1 {
		t.Errorf("%d grant entries, want 1", n)
	}
	out, err = w.run("ada@r.example.test", "--add", "analyst", "--by", "admin@r.example.test")
	if err != nil || !strings.HasSuffix(out, " analyst unchanged\n") {
		t.Errorf("second grant: %q %v", out, err)
	}
	out, err = w.run("ada@r.example.test", "--remove", "analyst", "--by", "admin@r.example.test")
	if err != nil || !strings.HasSuffix(out, " analyst removed\n") || w.analyst(t, "ada@r.example.test") {
		t.Fatalf("removal: %q %v", out, err)
	}
	if n := scalar(t, w.super, `SELECT count(*) FROM audit_log WHERE action = 'analyst.removed' AND entity_id = $1
		AND payload->>'reason' = 'removed' AND payload->>'via' = 'cli'`, w.ids["ada@r.example.test"].String()); n != 1 {
		t.Errorf("%d removal entries, want 1", n)
	}
}

// --by must be a hospital admin of the person's own hospital.
func TestCLIRefusesForeignGrantor(t *testing.T) {
	w := newRolesWorld(t)
	for _, verb := range []string{"--add", "--remove"} {
		if _, err := w.run("ada@r.example.test", verb, "analyst", "--by", "admin2@r.example.test"); !errors.Is(err, analysis.ErrAnalystForbidden) {
			t.Errorf("%s by another hospital's admin: %v", verb, err)
		}
	}
	if w.analyst(t, "ada@r.example.test") || scalar(t, w.super, `SELECT count(*) FROM analyst_changes`) != 0 {
		t.Error("a foreign grantor changed something")
	}
}

// T26 (puppet account): each grant notifies once, after commit; a refused
// or repeated grant and a removal notify nobody.
func TestNoticeCalledOncePerGrant(t *testing.T) {
	w := newRolesWorld(t)
	_, _ = w.run("nomfa@r.example.test", "--add", "analyst", "--by", "admin@r.example.test")
	if _, err := w.run("ada@r.example.test", "--add", "analyst", "--by", "admin@r.example.test"); err != nil {
		t.Fatal(err)
	}
	_, _ = w.run("ada@r.example.test", "--add", "analyst", "--by", "admin@r.example.test")
	_, _ = w.run("ada@r.example.test", "--remove", "analyst", "--by", "admin@r.example.test")
	if len(w.notified) != 1 || w.notified[0] != w.ids["ada@r.example.test"] || w.notifiedAt[0] != w.h {
		t.Errorf("notices %v at %v, want one for the grant", w.notified, w.notifiedAt)
	}
}
