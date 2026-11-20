package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/feedback"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
)

const demoUsage = `usage:
  core demo seed
  core demo reset

Seeds the public demo (demo.measured-health.org) with synthetic data: the
demo hospital DEM (hospitals.is_demo), its sample accounts on the published
demo password and two-step secret, the six shared reference guidelines,
sample consultations and corrections, and the hospital's demo Analysis
(core analysis seed-demo). Safe to run again: each run resets every sample
account to its documented state (visitors share them) and revokes its
sessions when anything about signing in changed; content is written once.

Demo host only. It refuses, before writing anything, unless MHAI_DEMO_SEED
is exactly "on" (only the demo compose's one-shot sets it), when any
hospital is not a demo hospital or has a real cloud agreement (a real
hospital may live in this database), when APP_ENV isn't demo, and when a
hospital DEM exists that is not the demo. The platform account's password
and two-step secret are random and thrown away: it exists only so the
clinical app can be shown refusing it. Analysis and named access are
raised only when their switch rows don't exist yet (an operator's "off"
stays off).
DATABASE_URL is the database owner, as core migrate uses it, because the
seed writes rows no single app role may write (user_mfa, decisions);
MFA_ENCRYPTION_KEY is core's, to seal the two-step secret as enrolment does.
It prints the hospital id and counts only.

reset is seed with the same guards, run on a schedule (the "Reset backend
demo" workflow): it also removes what visitors made in DEM (consultations
and their answers, feedback, corrections and their learning jobs, other than
the seeded samples; a question still in flight is kept for the next run),
deactivates accounts visitors added to DEM, signs every sample account out,
and clears the sample accounts' sign-in and two-step counters in NATS
(NATS_URL, NATS_NKEY_SEED_PATH: core's own; without NATS_URL the counters
are left to expire). Audited demo.reset. Then the seed puts everything back.`

// The public demo's published values (deploy/README.md, mhai-web's sign-in
// demo helper). They are demo values, documented and shown on the public
// sign-in page, not secrets: they open synthetic accounts of the synthetic
// demo hospital only. Never reuse them anywhere else.
const (
	DemoHospitalCode     = "DEM"
	DemoHospitalName     = "Demo General Hospital"
	DemoHospitalTimeZone = "Africa/Lagos"
	DemoPassword         = "Sample-demo-only-2026"
	DemoTOTPSecret       = "MHAIDEMOSAMPLEONLYNOTASECRET2345"
	demoTermsVersion     = "2026-10"
	demoSeedEnv          = "MHAI_DEMO_SEED"
)

var (
	errDemoArgs     = fmt.Errorf("%s", demoUsage)
	errDemoOff      = errors.New("refused: MHAI_DEMO_SEED is not on or APP_ENV is not demo; this is not the demo's seed container, nothing was written")
	errDemoKey      = errors.New("MFA_ENCRYPTION_KEY is missing or too short")
	errDemoReal     = errors.New("refused: this database holds a hospital that is not a demo hospital (or a real cloud agreement); the demo seed never runs beside a real hospital, nothing was written")
	errDemoNotDemo  = errors.New("refused: hospital DEM exists and is not the demo hospital; nothing was written")
	errDemoConflict = errors.New("refused: a sample account's email belongs to an account outside the demo hospital; nothing more was written")
	errDemoInactive = errors.New("the demo hospital is not active and can't be activated by the CLI (reactivate it on the platform)")
	errDemoWrite    = errors.New("could not write the demo seed")
	errDemoOwner    = errors.New("refused: DATABASE_URL must be the database owner (it reads every hospital's policy); nothing was written")
)

// demoLockKey serialises seed runs (pg_advisory_lock): two at once would
// race on the hospital and the accounts.
const demoLockKey int64 = 0x6d686169_64656d6f // "mhaidemo"

// demoNS names the seed's deterministic content ids.
var demoNS = uuid.MustParse("4f0b7a4e-9c1e-5d2a-8f3b-6d656d6f2d31")

type pwKind int

const (
	pwDemo   pwKind = iota // the published demo password
	pwRandom               // a random one, thrown away: nobody can sign in
)

type mfaKind int

const (
	mfaNone   mfaKind = iota
	mfaDemo           // enrolled on DemoTOTPSecret
	mfaRandom         // enrolled on a random secret nobody holds
)

// demoAccount is one sample account's documented state.
type demoAccount struct {
	Key, Email, Display, Full, Title, Specialty string
	Roles                                       []string // never "analyst": that goes through the grant
	Analyst, Platform                           bool
	Password                                    pwKind
	MustChange, Terms                           bool
	MFA                                         mfaKind
}

const demoDomain = "@demo-hospital.example"

// DemoAccounts is the sign-in page's list (mhai-web's demo helper). The
// clinical app asks every account without two-step to set it up at sign-in,
// so the ready-to-use accounts carry the published demo secret; only the
// "new" accounts set their own.
var DemoAccounts = []demoAccount{
	{Key: "clinician", Email: "a.reyes" + demoDomain, Display: "Dr. Reyes", Full: "Dr. Ana Reyes", Title: "Attending physician",
		Specialty: "Internal Medicine", Roles: []string{"clinician"}, Terms: true, MFA: mfaDemo},
	{Key: "twoStep", Email: "mfa.okafor" + demoDomain, Display: "Dr. Okafor", Full: "Dr. Chidi Okafor", Title: "Consultant",
		Specialty: "Paediatrics", Roles: []string{"clinician"}, Terms: true, MFA: mfaDemo},
	{Key: "newAccount", Email: "new.bello" + demoDomain, Display: "Nurse Bello", Full: "Amina Bello", Title: "Staff nurse",
		Specialty: "General ward", Roles: []string{"clinician"}, MustChange: true},
	{Key: "newAdmin", Email: "newadmin.okoro" + demoDomain, Display: "Chike Okoro", Full: "Chike Okoro", Title: "Hospital IT lead",
		Roles: []string{"hospital_admin"}, MustChange: true},
	{Key: "hospitalAdmin", Email: "admin.nwosu" + demoDomain, Display: "Grace Nwosu", Full: "Grace Nwosu", Title: "Hospital IT lead",
		Roles: []string{"hospital_admin"}, Terms: true, MFA: mfaDemo},
	{Key: "director", Email: "director.obi" + demoDomain, Display: "Dr. Obi", Full: "Dr. Ngozi Obi", Title: "Medical director",
		Specialty: "Emergency medicine", Roles: []string{"clinician", "hospital_admin"}, Terms: true, MFA: mfaDemo},
	{Key: "analyst", Email: "analyst.adeyemi" + demoDomain, Display: "Dr. Adeyemi", Full: "Dr. Kemi Adeyemi", Title: "Consultant physician",
		Specialty: "Internal Medicine", Roles: []string{"clinician"}, Analyst: true, Terms: true, MFA: mfaDemo},
	{Key: "platformAdmin", Email: "platform.bakare@measured-health.example", Display: "Tunde Bakare", Full: "Tunde Bakare",
		Title: "Platform admin", Roles: []string{"platform_admin"}, Platform: true, Password: pwRandom, Terms: true, MFA: mfaRandom},
	{Key: "locked", Email: "locked.eze" + demoDomain, Display: "Dr. Eze", Full: "Dr. Obinna Eze", Title: "Registrar",
		Specialty: "Surgery", Roles: []string{"clinician"}, Password: pwRandom, Terms: true},
}

func demoAccountByKey(key string) demoAccount {
	for _, a := range DemoAccounts {
		if a.Key == key {
			return a
		}
	}
	panic("demo: no account " + key)
}

// demoCmd is "core demo seed". getenv reads MHAI_DEMO_SEED and
// MFA_ENCRYPTION_KEY; every check runs before connecting.
func demoCmd(ctx context.Context, args []string, url string, getenv func(string) string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "seed" && args[0] != "reset") {
		return errDemoArgs
	}
	// Both switches, exactly: the flag only the demo's seed container sets,
	// and APP_ENV=demo (unset is production).
	if getenv(demoSeedEnv) != "on" || getenv("APP_ENV") != "demo" {
		return errDemoOff
	}
	mfaKey := []byte(strings.TrimSpace(getenv("MFA_ENCRYPTION_KEY")))
	sealer, err := auth.NewTOTPSealer(mfaKey)
	if err != nil {
		return errDemoKey
	}
	demoSecret, err := auth.DecodeTOTPSecret(DemoTOTPSecret)
	if err != nil {
		return err
	}
	if err := password.Preload(); err != nil {
		return err
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	s := &demoSeeder{pool: pool, url: url, sealer: sealer, secret: demoSecret, out: out}
	if args[0] == "reset" {
		s.reset = &demoReset{counters: &auth.Service{MFAKey: mfaKey}, natsURL: getenv("NATS_URL"),
			natsSeed: getenv("NATS_NKEY_SEED_PATH"), natsTLS: bus.TLS{CAPath: getenv("NATS_TLS_CA_PATH"),
				CertPath: getenv("NATS_TLS_CERT_PATH"), KeyPath: getenv("NATS_TLS_KEY_PATH")}}
	}
	return s.run(ctx)
}

type demoSeeder struct {
	pool   *pgxpool.Pool
	url    string
	sealer *auth.TOTPSealer
	secret []byte
	out    io.Writer
	// demoHash is one argon2id hash of DemoPassword for the accounts this
	// run creates or resets (hashing is slow; a shared salt across demo
	// accounts with a public password costs nothing).
	demoHash string
	hid      uuid.UUID
	ids      map[string]uuid.UUID
	// reset is set for "core demo reset" (demo_reset.go), nil for seed.
	reset *demoReset
}

func (s *demoSeeder) run(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return demoErr(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, demoLockKey); err != nil {
		return demoErr(err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, demoLockKey) }()

	if err := demoGuards(ctx, s.pool); err != nil {
		return err
	}
	if s.hid, err = s.hospital(ctx); err != nil {
		return err
	}
	// Again after DEM exists, before any account is written: a hospital made
	// meanwhile stops the run here.
	if err := demoGuards(ctx, s.pool); err != nil {
		return err
	}
	if s.reset != nil {
		// Under the same lock and after the same guards: only ever DEM, in
		// a database of demo hospitals only.
		if err := s.resetVisitors(ctx); err != nil {
			return err
		}
	}
	s.ids = map[string]uuid.UUID{}
	created, reset := 0, 0
	for _, a := range DemoAccounts {
		id, c, r, err := s.account(ctx, a)
		if err != nil {
			return err
		}
		s.ids[a.Key] = id
		if c {
			created++
		} else if r {
			reset++
		}
	}
	if err := s.audit(ctx, created, reset); err != nil {
		return err
	}
	subjects, err := s.analysis(ctx)
	if err != nil {
		return err
	}
	if err := s.analysts(ctx); err != nil {
		return err
	}
	chunks, err := s.guidelines(ctx)
	if err != nil {
		return err
	}
	consults, err := s.consultations(ctx)
	if err != nil {
		return err
	}
	corrections, err := s.corrections(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "demo seed hospital %s: accounts %d created %d reset; analysis %s; guidelines %d new; consultations %d new; corrections %d new\n",
		s.hid, created, reset, subjects, chunks, consults, corrections)
	return nil
}

// demoGuards refuses a database that holds a real hospital, or a DEM that
// isn't the demo. Read-only; nothing has been written when it refuses.
func demoGuards(ctx context.Context, pool *pgxpool.Pool) error {
	// The checks below must see every row: a login under row-level
	// security would see none and pass them. Only the owner (a superuser,
	// or BYPASSRLS) runs the seed.
	var sees bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&sees); err != nil {
		return demoErr(err)
	}
	if !sees {
		return errDemoOwner
	}
	var isDemo bool
	err := pool.QueryRow(ctx, `SELECT is_demo FROM hospitals WHERE short_code = $1`, DemoHospitalCode).Scan(&isDemo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return demoErr(err)
	}
	if err == nil && !isDemo {
		return errDemoNotDemo
	}
	// Fail closed: a demo database holds demo hospitals only (is_demo is set
	// at creation and never changes). Any other hospital, whatever its
	// policy, or any real cloud agreement, means a real hospital may live
	// here, and the published accounts must never exist beside it.
	var real bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hospitals WHERE NOT is_demo)
		OR EXISTS (SELECT 1 FROM hospital_model_policy WHERE agreement_ref ~ '^(AGR|DPA)-')`).Scan(&real); err != nil {
		return demoErr(err)
	}
	if real {
		return errDemoReal
	}
	return nil
}

// hospital creates DEM as the demo (audited hospital.created) when missing,
// activates it (audited), and puts back the two sign-in settings visitors
// may have changed.
func (s *demoSeeder) hospital(ctx context.Context) (uuid.UUID, error) {
	var hid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM hospitals WHERE short_code = $1 AND is_demo`, DemoHospitalCode).Scan(&hid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if hid, err = account.CreateHospital(ctx, s.pool, account.NewHospital{Name: DemoHospitalName, ShortCode: DemoHospitalCode,
			TimeZone: DemoHospitalTimeZone, Demo: true}); err != nil {
			return uuid.Nil, err
		}
	case err != nil:
		return uuid.Nil, demoErr(err)
	}
	if _, _, err := account.ActivateHospital(ctx, s.pool, DemoHospitalCode); err != nil {
		if errors.Is(err, account.ErrNotActivatable) {
			return uuid.Nil, errDemoInactive
		}
		return uuid.Nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE hospital_settings SET require_mfa_for_clinicians = false, allow_trust_device = false
		WHERE hospital_id = $1 AND (require_mfa_for_clinicians OR allow_trust_device)`, hid); err != nil {
		return uuid.Nil, demoErr(err)
	}
	return hid, nil
}

func (s *demoSeeder) hash(kind pwKind) (string, error) {
	if kind == pwRandom {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return "", demoErr(err)
		}
		return password.Hash("random-" + hex.EncodeToString(b))
	}
	if s.demoHash == "" {
		h, err := password.Hash(DemoPassword)
		if err != nil {
			return "", err
		}
		s.demoHash = h
	}
	return s.demoHash, nil
}

// inTx runs fn in a READ COMMITTED transaction as the owner (audit.Write
// needs READ COMMITTED).
func (s *demoSeeder) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return demoErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return demoErr(err)
	}
	return nil
}

// account makes a or puts it back to its documented state. reset reports
// any change to an existing account; a change to how it signs in (password,
// first-sign-in steps, roles, two-step, active) also revokes its sessions
// and trusted devices.
func (s *demoSeeder) account(ctx context.Context, a demoAccount) (id uuid.UUID, created, reset bool, err error) {
	var hospital *uuid.UUID
	if !a.Platform {
		hospital = &s.hid
	}
	email := account.NormaliseEmail(a.Email)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var curHosp *uuid.UUID
		var hash *string
		var display string
		var full, title, spec *string
		var active, must, terms bool
		err := tx.QueryRow(ctx, `SELECT id, hospital_id, password_hash, display_name, full_name, role_title, specialty,
			is_active, must_change_password, terms_accepted_at IS NOT NULL FROM users WHERE email = $1 FOR UPDATE`, email).
			Scan(&id, &curHosp, &hash, &display, &full, &title, &spec, &active, &must, &terms)
		if errors.Is(err, pgx.ErrNoRows) {
			created = true
			return s.create(ctx, tx, a, email, hospital, &id)
		}
		if err != nil {
			return demoErr(err)
		}
		if (curHosp == nil) != (hospital == nil) || (curHosp != nil && *curHosp != *hospital) {
			return errDemoConflict
		}
		sec := false
		if newHash, change, err := s.passwordFor(a.Password, hash); err != nil {
			return err
		} else if change {
			if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, id, newHash); err != nil {
				return demoErr(err)
			}
			sec = true
		}
		if must != a.MustChange || terms != a.Terms || !active {
			if _, err := tx.Exec(ctx, `UPDATE users SET must_change_password = $2, is_active = true,
				terms_version = CASE WHEN $3::boolean THEN $4::text END, terms_accepted_at = CASE WHEN $3::boolean THEN now() END
				WHERE id = $1`, id, a.MustChange, a.Terms, demoTermsVersion); err != nil {
				return demoErr(err)
			}
			sec = true
		}
		if display != a.Display || deref(full) != a.Full || deref(title) != a.Title || deref(spec) != a.Specialty {
			if _, err := tx.Exec(ctx, `UPDATE users SET display_name = $2, full_name = $3, role_title = $4, specialty = $5
				WHERE id = $1`, id, a.Display, a.Full, a.Title, nullStr(a.Specialty)); err != nil {
				return demoErr(err)
			}
			reset = true
		}
		rolesChanged, err := s.roles(ctx, tx, id, a)
		if err != nil {
			return err
		}
		mfaChanged, err := s.mfa(ctx, tx, id, a.MFA)
		if err != nil {
			return err
		}
		if sec || rolesChanged || mfaChanged {
			reset = true
			if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
				return demoErr(err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM trusted_devices WHERE user_id = $1`, id); err != nil {
				return demoErr(err)
			}
		}
		return nil
	})
	return id, created, reset, err
}

func (s *demoSeeder) create(ctx context.Context, tx pgx.Tx, a demoAccount, email string, hospital *uuid.UUID, id *uuid.UUID) error {
	h, err := s.hash(a.Password)
	if err != nil {
		return err
	}
	*id = uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO users (id, hospital_id, email, password_hash, display_name, full_name, role_title, specialty,
		must_change_password, terms_version, terms_accepted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $10::boolean THEN $11::text END, CASE WHEN $10::boolean THEN now() END)`,
		*id, hospital, email, h, a.Display, a.Full, a.Title, nullStr(a.Specialty), a.MustChange, a.Terms, demoTermsVersion); err != nil {
		return demoErr(err)
	}
	for _, r := range a.Roles {
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, *id, r); err != nil {
			return demoErr(err)
		}
	}
	if _, err := s.mfa(ctx, tx, *id, a.MFA); err != nil {
		return err
	}
	ev := audit.Event{Action: "user.created", Entity: "user", EntityID: id.String(), Payload: map[string]any{"via": "cli"}}
	if hospital != nil {
		ev.HospitalID = hospital.String()
	}
	if err := audit.Write(ctx, tx, ev); err != nil {
		return demoErr(err)
	}
	return nil
}

// passwordFor says whether the stored hash must change: the demo password
// must verify; a random one must not be the demo password (or missing).
func (s *demoSeeder) passwordFor(kind pwKind, cur *string) (string, bool, error) {
	demoOK := false
	if cur != nil && *cur != "" {
		demoOK, _ = password.Verify(DemoPassword, *cur)
	}
	if kind == pwDemo && demoOK {
		return "", false, nil
	}
	if kind == pwRandom && cur != nil && *cur != "" && !demoOK {
		return "", false, nil
	}
	h, err := s.hash(kind)
	return h, err == nil, err
}

// roles puts the account's roles (other than analyst, which the grant
// handles) back to exactly a.Roles.
func (s *demoSeeder) roles(ctx context.Context, tx pgx.Tx, id uuid.UUID, a demoAccount) (bool, error) {
	var cur []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(role ORDER BY role), '{}') FROM user_roles
		WHERE user_id = $1 AND role <> 'analyst'`, id).Scan(&cur); err != nil {
		return false, demoErr(err)
	}
	changed := false
	for _, r := range a.Roles {
		if !slices.Contains(cur, r) {
			if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
				return false, demoErr(err)
			}
			changed = true
		}
	}
	for _, r := range cur {
		if !slices.Contains(a.Roles, r) {
			if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role = $2`, id, r); err != nil {
				return false, demoErr(err)
			}
			changed = true
		}
	}
	return changed, nil
}

// mfa puts user_mfa back: none, enrolled on the demo secret, or enrolled
// on a random secret (kept once made, so nobody, the seed included, holds
// it). Backup codes are always cleared.
func (s *demoSeeder) mfa(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind mfaKind) (bool, error) {
	var sealed []byte
	var enabled bool
	var backups int
	err := tx.QueryRow(ctx, `SELECT secret_enc, enabled, cardinality(backup_code_hashes) FROM user_mfa WHERE user_id = $1`, id).
		Scan(&sealed, &enabled, &backups)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, demoErr(err)
	}
	if kind == mfaNone {
		if !exists {
			return false, nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_mfa WHERE user_id = $1`, id); err != nil {
			return false, demoErr(err)
		}
		return true, nil
	}
	if exists && enabled && backups == 0 {
		if secret, err := s.sealer.Open(id, sealed); err == nil {
			isDemo := bytes.Equal(secret, s.secret)
			if (kind == mfaDemo) == isDemo {
				return false, nil
			}
		}
	}
	secret := s.secret
	if kind == mfaRandom {
		if secret, err = auth.RandomTOTPSecret(); err != nil {
			return false, demoErr(err)
		}
	}
	enc, err := s.sealer.Seal(id, secret)
	if kind == mfaRandom {
		clear(secret) // nobody keeps the random secret
	}
	if err != nil {
		return false, demoErr(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled, last_used_step, backup_code_hashes)
		VALUES ($1, $2, true, NULL, '{}')
		ON CONFLICT (user_id) DO UPDATE SET secret_enc = EXCLUDED.secret_enc, enabled = true, last_used_step = NULL,
			backup_code_hashes = '{}'`, id, enc); err != nil {
		return false, demoErr(err)
	}
	return true, nil
}

func (s *demoSeeder) audit(ctx context.Context, created, reset int) error {
	if created == 0 && reset == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := audit.Write(ctx, tx, audit.Event{HospitalID: s.hid.String(), Action: "demo.seeded", Entity: "hospital",
			EntityID: s.hid.String(), Payload: map[string]any{"via": "cli", "created": created, "reset": reset}}); err != nil {
			return demoErr(err)
		}
		return nil
	})
}

// analysis raises DEM's demo Analysis with its synthetic cohort
// (analysisSeedDemo) and named access to letters_only, each once.
func (s *demoSeeder) analysis(ctx context.Context) (string, error) {
	// Raised only while DEM has no switch row of its own and the global row,
	// if there is one (another demo hospital may have made it), isn't off: a
	// switch an operator lowered (core analysis switch off) stays off, so a
	// deploy never undoes a kill switch.
	var demRow, globalOff bool
	if err := s.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM analysis_switch WHERE hospital_id = $1),
		EXISTS (SELECT 1 FROM analysis_switch WHERE hospital_id IS NULL AND position = 'off')`, s.hid).Scan(&demRow, &globalOff); err != nil {
		return "", demoErr(err)
	}
	result := "kept"
	if !demRow && !globalOff {
		if err := analysisSeedDemo(ctx, []string{DemoHospitalCode}, s.url, io.Discard, nil); err != nil {
			return "", err
		}
		result = "seeded"
	}
	// Named access the same way, and only on top of DEM's demo Analysis.
	var namedRow, namedGlobalOff, demOn bool
	if err := s.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM named_access_switch WHERE hospital_id = $1),
		EXISTS (SELECT 1 FROM named_access_switch WHERE hospital_id IS NULL AND position = 'off'),
		EXISTS (SELECT 1 FROM analysis_switch WHERE hospital_id = $1 AND position = 'on' AND signoff_clinical = 'DEMO-SYNTHETIC')`,
		s.hid).Scan(&namedRow, &namedGlobalOff, &demOn); err != nil {
		return "", demoErr(err)
	}
	if !namedRow && !namedGlobalOff && demOn {
		if err := namedAccessDemo(ctx, DemoHospitalCode, s.url, io.Discard); err != nil {
			return "", err
		}
	}
	return result, nil
}

// analysts grants the analyst role to the analyst account and takes it
// from every other sample account, through the CLI's audited path (core
// users roles), by the sample hospital admin.
func (s *demoSeeder) analysts(ctx context.Context) error {
	by := demoAccountByKey("hospitalAdmin").Email
	for _, a := range DemoAccounts {
		if a.Platform {
			continue
		}
		var has bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = 'analyst')`,
			s.ids[a.Key]).Scan(&has); err != nil {
			return demoErr(err)
		}
		if has == a.Analyst {
			continue
		}
		verb := "--remove"
		if a.Analyst {
			verb = "--add"
		}
		if err := usersRoles(ctx, []string{a.Email, verb, "analyst", "--by", by}, s.url, io.Discard, nil); err != nil {
			// An operator switched Analysis off (the seed keeps it off): the
			// analyst role waits until it is on again.
			if a.Analyst && errors.Is(err, analysis.ErrAnalystOff) {
				fmt.Fprintln(s.out, "demo seed: analysis is off; the analyst role is not granted")
				continue
			}
			return err
		}
	}
	return nil
}

// demoGuidelines are the six shared reference guidelines (as
// deploy/pilot/enable-pilot.sh seeds them), with a title and a source so an
// answer that cites one lists it under its sources.
var demoGuidelines = []struct{ id, title, content string }{
	{"KB#0001", "Gestational diabetes: diagnosis", "Gestational Diabetes Mellitus (GDM) diagnosed when fasting glucose >= 92 mg/dL, 1-hr >= 180, or 2-hr >= 153 on 75g OGTT."},
	{"KB#0002", "Malaria: confirmation and severe features", "Malaria requires parasitological confirmation by microscopy or RDT. Severe features: Hb < 5, respiratory distress, parasitemia > 10%."},
	{"KB#0003", "Hypertension: definition and first-line treatment", "Hypertension defined as systolic >= 140 or diastolic >= 90 on two occasions. First-line: thiazides, ACEi, ARBs, CCBs."},
	{"KB#0004", "Neonatal jaundice: physiological or pathological", "Neonatal jaundice: physiological onset > 24hr, peaks day 3-5, total bili < 15. Pathological: onset < 24hr, total > 17."},
	{"KB#0005", "Upper respiratory tract infection: supportive care", "URTI: viral majority. Supportive: paracetamol, hydration. Antibiotics only if bacterial superinfection."},
	{"KB#0006", "Type 2 diabetes: diagnosis and first-line treatment", "T2DM: fasting glucose >= 126, 2-hr OGTT >= 200, HbA1c >= 6.5%. First-line: Metformin 500mg BID."},
}

const demoSourceName = "Sample reference (synthetic demo)"

func guidelineMeta(title string) string {
	b, _ := json.Marshal(map[string]string{"title": title, "source": demoSourceName})
	return string(b)
}

// guidelines adds the six shared guidelines if missing, and gives a title
// and source to a copy that has none (pilot-seeded rows); nothing else.
func (s *demoSeeder) guidelines(ctx context.Context) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for _, g := range demoGuidelines {
			tag, err := tx.Exec(ctx, `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, metadata_json)
				VALUES ($1, NULL, $2, 'guideline', $3::jsonb)
				ON CONFLICT (hospital_id, chunk_id) DO UPDATE SET metadata_json = EXCLUDED.metadata_json
				WHERE kb_chunks.metadata_json IS NULL AND kb_chunks.library_id IS NULL`, g.id, g.content, guidelineMeta(g.title))
			if err != nil {
				return demoErr(err)
			}
			n += int(tag.RowsAffected())
		}
		return nil
	})
	return n, err
}

// demoConsultation is one past question of the sample clinician, answered
// as the sample answer worker would answer it.
type demoConsultation struct {
	key, question, category, urgency, chunk string
	age                                     int
	sex                                     string
	daysAgo                                 int
	decision, band                          string
	trust                                   float64
	answer                                  string
	guard                                   []string
}

const samplePrefix = "Sample answer (synthetic demo data, not clinical advice): "

var demoConsultations = []demoConsultation{
	{key: "gdm", question: "Pregnant at 26 weeks with a 2-hour OGTT of 160 mg/dL. Does this meet gestational diabetes?", category: "Obstetrics",
		urgency: "routine", chunk: "KB#0001", age: 31, sex: "female", daysAgo: 6, decision: "PASS", band: "high", trust: 0.91,
		answer: "Yes. A 2-hour value of 153 mg/dL or more on a 75 g OGTT meets the gestational diabetes threshold. Confirm the test was a 75 g load and plan follow-up with the antenatal team."},
	{key: "malaria", question: "Fever for three days, positive rapid test. What confirms malaria and which features would make it severe?", category: "Internal Medicine",
		urgency: "urgent", chunk: "KB#0002", age: 24, sex: "male", daysAgo: 5, decision: "PASS", band: "high", trust: 0.88,
		answer: "A positive RDT or microscopy confirms malaria before treatment. Treat as severe if haemoglobin is below 5, there is respiratory distress, or parasitaemia is above 10%."},
	{key: "bp", question: "Two clinic readings of 150/95. Is this hypertension and what is first-line treatment?", category: "Internal Medicine",
		urgency: "routine", chunk: "KB#0003", age: 52, sex: "female", daysAgo: 3, decision: "REGENERATE", band: "mid", trust: 0.67,
		answer: "Readings of 140/90 or more on two occasions define hypertension. First-line options include a thiazide, an ACE inhibitor, an ARB or a calcium channel blocker; the choice depends on the rest of the history."},
	{key: "jaundice", question: "Newborn, 18 hours old, visibly yellow. Total bilirubin 12. Is this physiological?", category: "Paediatrics",
		urgency: "emergency", chunk: "KB#0004", age: 0, sex: "female", daysAgo: 2, decision: "ESCALATE", band: "low", trust: 0.41,
		answer: "Jaundice that starts in the first 24 hours is pathological, not physiological. This needs senior review now.", guard: []string{"red_flag"}},
	{key: "urti", question: "Cough and sore throat for four days, no fever. Are antibiotics needed?", category: "General Practice",
		urgency: "routine", chunk: "KB#0005", age: 38, sex: "male", daysAgo: 1, decision: "PASS", band: "high", trust: 0.93,
		answer: "Most upper respiratory infections are viral; give supportive care with paracetamol and fluids. Antibiotics only if a bacterial superinfection develops."},
}

func (s *demoSeeder) consultationID(key string) uuid.UUID {
	return uuid.NewSHA1(demoNS, []byte(s.hid.String()+"/consultation/"+key))
}

// consultations writes the sample clinician's past questions once: the
// queued-then-completed row and its decision, as the pipeline writes them.
func (s *demoSeeder) consultations(ctx context.Context) (int, error) {
	reyes := s.ids["clinician"]
	n := 0
	title := map[string]string{}
	for _, g := range demoGuidelines {
		title[g.id] = g.title
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for _, c := range demoConsultations {
			qid := s.consultationID(c.key)
			at := time.Now().UTC().Add(-time.Duration(c.daysAgo)*24*time.Hour - 3*time.Hour).Truncate(time.Second)
			patient, _ := json.Marshal(map[string]any{"age": c.age, "sex": c.sex})
			tag, err := tx.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, patient_data_json, clinical_question, urgency, category,
				status, progress, current_stage, started_at, completed_at, created_by, created_at, attempts, published_at, deadline_at)
				VALUES ($1, $2, $3::jsonb, $4, $5, $6, 'completed', 1, 'decision', $7::timestamptz + interval '1 second', $7::timestamptz + interval '9 seconds',
				        $8, $7, 1, $7, $7::timestamptz + interval '10 minutes')
				ON CONFLICT (id) DO NOTHING`, qid, s.hid, string(patient), c.question, c.urgency, c.category, at, reyes)
			if err != nil {
				return demoErr(err)
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			n++
			cites, _ := json.Marshal([]string{c.chunk})
			sources, _ := json.Marshal([]map[string]any{{"chunk_id": c.chunk, "title": title[c.chunk], "url": nil,
				"source": demoSourceName, "attribution": nil}})
			guard := c.guard
			if guard == nil {
				guard = []string{}
			}
			report, _ := json.Marshal(map[string]any{"failed_agents": []string{}, "flags": [][]string{}, "model_used": "sample",
				"guard_reasons": guard, "guard_rules": guard})
			versions, _ := json.Marshal(map[string]string{"sample": "1", "cca_route": "local"})
			timings, _ := json.Marshal(map[string]int{"privacy": 40, "guidelines": 120, "answer": 2100, "evidence": 900})
			if _, err := tx.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, response_text, citations_json, trust_score,
				hga_decision, guardrail_report_json, retrieval_ms, generation_ms, verified, degraded, failed_agents, model_versions,
				trust_band, masked_question, masked_patient_json, timings_ms, cited_sources_json, created_at)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8::jsonb, 120, 2100, true, false, '{}', $9::jsonb, $10, $11, $12::jsonb,
				        $13::jsonb, $14::jsonb, $15::timestamptz + interval '9 seconds')`,
				uuid.NewSHA1(demoNS, []byte(qid.String()+"/decision")), s.hid, qid, samplePrefix+c.answer, string(cites), c.trust,
				c.decision, string(report), string(versions), c.band, c.question, string(patient), string(timings), string(sources), at); err != nil {
				return demoErr(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO hga_evaluations (hospital_id, query_id, target_agent, trust, decision, nli_score,
				coherence_score, numerical_score, se_score, flag_penalty, weights_snapshot_json, cascade_layers_json, ts)
				VALUES ($1, $2, 'CCA', $3, $4, $5, 1.0, 0.85, 0.7, 0, '{"alpha":0.35,"beta":0.25,"gamma":0.2,"delta":0.2}', '[]',
				        $6::timestamptz + interval '9 seconds')`, s.hid, qid, c.trust, c.decision, c.trust, at); err != nil {
				return demoErr(err)
			}
		}
		return nil
	})
	return n, err
}

// corrections, through the app's own store (audited as the app audits
// them): one waiting for review (the sample clinician's, on the blood
// pressure answer: the director's queue) and one approved by the director
// (on the malaria answer: the clinician's "my feedback"). Each is made once;
// one a visitor decided is kept, one a visitor withdrew is made again.
func (s *demoSeeder) corrections(ctx context.Context) (int, error) {
	store := feedback.Store{Pool: s.pool}
	reyes, director := s.ids["clinician"], s.ids["director"]
	n := 0
	for _, c := range demoCorrections {
		qid := s.consultationID(c.key)
		var cid uuid.UUID
		var status string
		err := s.pool.QueryRow(ctx, `SELECT id, status FROM feedback_events WHERE query_id = $1 AND status <> 'withdrawn'`, qid).
			Scan(&cid, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			d, r := c.diagnosis, c.reason
			sub, err := store.Submit(ctx, s.hid, reyes, "", qid, feedback.FeedbackIn{QueryID: qid.String(), Action: feedbackCorrect,
				CorrectedDiagnosis: &d, Reason: &r})
			if err != nil {
				return n, demoErr(err)
			}
			cid, status = sub.CorrectionID, sub.Status
			n++
		} else if err != nil {
			return n, demoErr(err)
		}
		if c.approve && status == "pending_review" {
			if _, err := store.Decide(ctx, s.hid, director, cid, "", feedback.Decision{Kind: "approve"}); err != nil {
				return n, demoErr(err)
			}
		}
	}
	return n, nil
}

// demoCorrections are the two sample corrections: the sample clinician's,
// one left waiting for review and one approved by the director.
var demoCorrections = []struct {
	key, diagnosis, reason string
	approve                bool
}{
	{"bp", "Hypertension, confirm with home readings", "Sample correction: confirm with home or ambulatory readings before starting treatment.", false},
	{"malaria", "Uncomplicated malaria", "Sample correction: state that no severe features were present before oral treatment.", true},
}

// feedbackCorrect is POST /feedback's action for a correction.
const feedbackCorrect = "correct"

// demoErr is errDemoWrite with the database's error code and constraint
// name (never a value), so an operator can tell what refused the write.
func demoErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("%w (sqlstate %s %s)", errDemoWrite, pe.Code, pe.ConstraintName)
	}
	return errDemoWrite
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
