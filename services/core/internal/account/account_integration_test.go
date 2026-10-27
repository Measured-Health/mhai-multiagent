//go:build integration

package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const goodPW = "tangerine-orbit-77-quill"

// setup returns a superuser pool (for assertions and tampering) and a pool
// connected as a plain member of mhai_provision (what the CLI uses).
func setup(t *testing.T) (super, prov *pgxpool.Pool) {
	t.Helper()
	pg, super := testutil.MigratedPostgres(t)
	prov, _, err := testutil.ProvisionLogin(context.Background(), pg, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prov.Close)
	return super, prov
}

func TestEmailUniqueIgnoresCase(t *testing.T) {
	pool, prov := setup(t)
	ctx := context.Background()
	if _, err := CreateHospital(ctx, prov, NewHospital{Name: "Havana General", ShortCode: "HAV", TimeZone: "Africa/Lagos"}); err != nil {
		t.Fatal(err)
	}
	id, err := CreateUser(ctx, prov, NewUser{Email: " Grace.Nwosu@Havana.example ", Name: "Grace Nwosu",
		Roles: []string{"clinician"}, HospitalShortCode: "HAV", Password: goodPW})
	if err != nil {
		t.Fatal(err)
	}
	var stored, hash string
	var must bool
	if err := pool.QueryRow(ctx, `SELECT email::text, password_hash, must_change_password FROM users WHERE id=$1`, id).Scan(&stored, &hash, &must); err != nil {
		t.Fatal(err)
	}
	if stored != "grace.nwosu@havana.example" {
		t.Fatalf("stored email %q", stored)
	}
	if !must {
		t.Error("must_change_password not set")
	}
	if ok, err := password.Verify(goodPW, hash); err != nil || !ok {
		t.Fatalf("stored hash does not verify: %v %v", ok, err)
	}
	_, err = CreateUser(ctx, prov, NewUser{Email: "GRACE.NWOSU@HAVANA.EXAMPLE", Name: "Other",
		Roles: []string{"clinician"}, HospitalShortCode: "HAV", Password: goodPW})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("want ErrEmailTaken, got %v", err)
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "grace") {
		t.Fatalf("error carries the email: %v", err)
	}
	// The database itself also refuses a case-only duplicate.
	if _, err := pool.Exec(ctx, `INSERT INTO users (email, display_name) VALUES ('Grace.Nwosu@havana.example','x')`); err == nil {
		t.Fatal("database accepted a case-only duplicate email")
	}
}

func TestCreateRejectsWeakPassword(t *testing.T) {
	_, prov := setup(t)
	_, err := CreateUser(context.Background(), prov, NewUser{Email: "a@b.example", Name: "A",
		Roles: []string{"platform_admin"}, Password: "password1234"})
	if !errors.Is(err, password.ErrBreached) {
		t.Fatalf("got %v", err)
	}
}

func TestPlatformAndClinicalCreation(t *testing.T) {
	pool, prov := setup(t)
	ctx := context.Background()
	if _, err := CreateUser(ctx, prov, NewUser{Email: "ops@mh.example", Name: "Ops", Roles: []string{"platform_admin"},
		HospitalShortCode: "HAV", Password: goodPW}); !errors.Is(err, ErrRoleHospital) {
		t.Fatalf("platform_admin with hospital: %v", err)
	}
	if _, err := CreateUser(ctx, prov, NewUser{Email: "c@mh.example", Name: "C", Roles: []string{"clinician"},
		Password: goodPW}); !errors.Is(err, ErrRoleHospital) {
		t.Fatalf("clinician without hospital: %v", err)
	}
	if _, err := CreateUser(ctx, prov, NewUser{Email: "c@mh.example", Name: "C", Roles: []string{"clinician"},
		HospitalShortCode: "NOPE", Password: goodPW}); !errors.Is(err, ErrHospitalNotFound) {
		t.Fatalf("unknown hospital: %v", err)
	}
	if _, err := CreateUser(ctx, prov, NewUser{Email: "ops@mh.example", Name: "Ops", Roles: []string{"platform_admin"},
		Password: goodPW}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d users, want 1 (failed creations must leave nothing)", n)
	}
}

// The database triggers still hold if the Go check is bypassed.
func TestTriggersBackstopRolePairing(t *testing.T) {
	pool, _ := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (name, short_code) VALUES ('H','HAV')`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `WITH u AS (INSERT INTO users (email, display_name, hospital_id)
		SELECT 'x@y.example','x',id FROM hospitals RETURNING id)
		INSERT INTO user_roles SELECT id, 'platform_admin' FROM u`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("trigger let a platform role onto a hospital account: %v", err)
	}
}

func TestCreationIsAuditedWithoutEmails(t *testing.T) {
	pool, prov := setup(t)
	ctx := context.Background()
	hid, err := CreateHospital(ctx, prov, NewHospital{Name: "Havana General", ShortCode: "HAV", TimeZone: "UTC",
		RequireMFAForClinicians: true, AllowTrustDevice: true})
	if err != nil {
		t.Fatal(err)
	}
	var req, trust bool
	var status string
	_ = pool.QueryRow(ctx, `SELECT s.require_mfa_for_clinicians, s.allow_trust_device, h.status FROM hospital_settings s JOIN hospitals h ON h.id=s.hospital_id WHERE s.hospital_id=$1`, hid).Scan(&req, &trust, &status)
	if !req || !trust || status != "setup_incomplete" {
		t.Fatalf("settings %v %v %q", req, trust, status)
	}
	uid, err := CreateUser(ctx, prov, NewUser{Email: "grace@havana.example", Name: "Grace", Roles: []string{"clinician", "analyst"},
		HospitalShortCode: "HAV", Password: goodPW})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT action, entity_id::text, payload::text FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var a, e, p string
		_ = rows.Scan(&a, &e, &p)
		actions = append(actions, a)
		if strings.Contains(p, "@") || strings.Contains(p, "grace") {
			t.Fatalf("audit payload carries PII: %s", p)
		}
		if a == "user.created" && e != uid.String() {
			t.Fatalf("entity id %s", e)
		}
	}
	if strings.Join(actions, ",") != "hospital.created,user.created" {
		t.Fatalf("audit actions %v", actions)
	}
	if ok, _, err := audit.Verify(ctx, pool); err != nil || !ok {
		t.Fatalf("audit chain: %v %v", ok, err)
	}
}
