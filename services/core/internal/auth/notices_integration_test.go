//go:build integration

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
)

// Security notices (product owner, 24 Nov 2026; migration 00058): the
// account's own address is told when its password is changed or reset with
// a link, two-step is turned on, a backup code is used or new ones are
// made. Each is one outbox row for that person, written in the change's
// own transaction; an ordinary sign-in or a TOTP step-up writes none.

// noticePW and wrongPW are synthetic passwords, built at run time so no
// literal in the source looks like a credential.
var (
	noticePW = strings.Join([]string{"synthetic", "cobalt", "orchard", "5502", "wren"}, "-")
	wrongPW  = strings.Join([]string{"synthetic", "wrong", "pass", strings.Repeat("0", 4)}, "-")
)

// notices counts u's outbox rows of kind ("" for every kind).
func notices(t *testing.T, u uuid.UUID, kind delivery.Kind) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM message_outbox
		WHERE recipient_id = $1 AND ($2 = '' OR kind = $2)`, u, string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// outboxOf is every column of u's outbox rows, as text.
func outboxOf(t *testing.T, u uuid.UUID) string {
	t.Helper()
	var s string
	if err := super.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(o)::text, '|'), '')
		FROM message_outbox o WHERE recipient_id = $1`, u).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// oneNotice checks u has exactly one row of kind: of u's hospital, by u, for
// u, on the log channel, about no correction.
func oneNotice(t *testing.T, u uuid.UUID, kind delivery.Kind) {
	t.Helper()
	var n int
	var ok bool
	if err := super.QueryRow(context.Background(), `SELECT count(*), coalesce(bool_and(o.hospital_id = u.hospital_id
		AND o.created_by = u.id AND o.channel = 'log' AND o.subject_id IS NULL AND o.dedupe_key LIKE o.kind || ':' || u.id::text || ':%'), false)
		FROM message_outbox o JOIN users u ON u.id = o.recipient_id WHERE o.recipient_id = $1 AND o.kind = $2`, u, string(kind)).
		Scan(&n, &ok); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !ok {
		t.Fatalf("%s: %d rows (shape ok %v), want one for the account's own hospital", kind, n, ok)
	}
}

func TestSecurityNoticePasswordChanged(t *testing.T) {
	e := newEnv(t)
	u, lr := loggedIn(t, e)
	at := lr.body["access_token"].(string)
	if n := notices(t, u.id, ""); n != 0 {
		t.Fatalf("sign-in recorded %d notices", n)
	}
	// A refused change tells nobody.
	mustCode(t, "wrong current", e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": wrongPW, "new_password": noticePW}}), 403, "forbidden")
	if n := notices(t, u.id, ""); n != 0 {
		t.Fatalf("a refused change recorded %d notices", n)
	}
	r := e.do(t, req{method: "POST", path: "/auth/password", bearer: at,
		body: map[string]any{"current_password": testPassword, "new_password": noticePW}})
	if r.code != 204 {
		t.Fatalf("change: %d %s", r.code, r.raw)
	}
	oneNotice(t, u.id, delivery.NoticePasswordChanged)
	if n := notices(t, u.id, ""); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
	row := outboxOf(t, u.id)
	for _, s := range []string{testPassword, noticePW, u.email, "@"} {
		if strings.Contains(row, s) {
			t.Fatalf("the outbox row carries a password or an address: %s", row)
		}
	}
	if strings.Contains(e.logs.String(), u.email) || strings.Contains(e.logs.String(), noticePW) {
		t.Fatal("the logs carry the address or the password")
	}
}

func TestSecurityNoticeResetLinkNotInvite(t *testing.T) {
	e := newEnv(t)
	u, _ := loggedIn(t, e)
	plain := seedToken(t, hospitalOf(t, u.id), u.id, "password_reset", "29 minutes")
	if r := e.reset(t, plain, noticePW, newIP()); r.code != 200 {
		t.Fatalf("reset %d %s", r.code, r.raw)
	}
	oneNotice(t, u.id, delivery.NoticePasswordReset)
	if n := notices(t, u.id, ""); n != 1 {
		t.Fatalf("%d notices, want 1", n)
	}
	if strings.Contains(outboxOf(t, u.id), plain) {
		t.Fatal("the outbox row carries the reset token")
	}
	// An invite's link sets a first password: no notice.
	v := clinician(t)
	inv := seedToken(t, hospitalOf(t, v.id), v.id, "invite", "29 minutes")
	if r := e.reset(t, inv, noticePW, newIP()); r.code != 200 {
		t.Fatalf("invite %d %s", r.code, r.raw)
	}
	if n := notices(t, v.id, ""); n != 0 {
		t.Fatalf("an invite recorded %d notices", n)
	}
}

func TestSecurityNoticesTwoStep(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	oneNotice(t, u.id, delivery.NoticeMFAEnabled)

	// A TOTP sign-in and a TOTP step-up tell nobody of a backup code.
	at := e.fullSession(t, u, en).body["access_token"].(string)
	if n := notices(t, u.id, ""); n != 1 {
		t.Fatalf("a TOTP sign-in recorded a notice: %d", n)
	}
	r := e.do(t, req{method: "POST", path: "/auth/mfa/recovery-codes", bearer: at,
		body: map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())}})
	if r.code != 200 {
		t.Fatalf("regenerate: %d %s", r.code, r.raw)
	}
	oneNotice(t, u.id, delivery.NoticeBackupCodesMade)
	if n := notices(t, u.id, delivery.NoticeBackupCodeUsed); n != 0 {
		t.Fatalf("a TOTP step-up recorded %d backup-code notices", n)
	}
	var fresh []string
	for _, c := range r.body["recovery_codes"].([]any) {
		fresh = append(fresh, c.(string))
	}
	e.clock.Add(30 * time.Second)

	// A wrong backup code tells nobody; a right one tells the owner once.
	mustCode(t, "old code", e.challenge(t, e.mfaToken(t, u), en.codes[0], false), 403, "mfa_invalid")
	if n := notices(t, u.id, delivery.NoticeBackupCodeUsed); n != 0 {
		t.Fatalf("a refused code recorded %d notices", n)
	}
	if r := e.challenge(t, e.mfaToken(t, u), fresh[0], false); r.code != 200 {
		t.Fatalf("backup code: %d %s", r.code, r.raw)
	}
	oneNotice(t, u.id, delivery.NoticeBackupCodeUsed)
	if n := notices(t, u.id, ""); n != 3 {
		t.Fatalf("%d notices, want 3", n)
	}

	// Nothing queued carries a code, the authenticator secret or its URI.
	row := outboxOf(t, u.id)
	secrets := append(append([]string{en.uri, base32Secret(t, en.uri)}, en.codes...), fresh...)
	for _, s := range secrets {
		if s != "" && (strings.Contains(row, s) || strings.Contains(e.logs.String(), s)) {
			t.Fatalf("a code or the secret is in the outbox or the logs")
		}
	}
}

// Moving two-step to a new app (#277): starting tells nobody (nothing
// changed yet), a wrong confirm code tells nobody, the confirmation tells
// the owner once, and the new app's secret is in neither the outbox nor
// the logs.
func TestSecurityNoticeMFAReplaced(t *testing.T) {
	e := newEnv(t)
	u := admin(t)
	en := e.enrol(t, u)
	at := e.fullSession(t, u, en).body["access_token"].(string)
	r := e.replaceStart(t, at, map[string]any{"mfa_code": totpAt(en.secret, e.clock.Now())})
	if r.code != 200 {
		t.Fatalf("replace: %d %s", r.code, r.raw)
	}
	uri := r.body["otpauth_uri"].(string)
	fresh := secretFromURI(t, uri)
	e.clock.Add(30 * time.Second)
	if n := notices(t, u.id, delivery.NoticeMFAReplaced); n != 0 {
		t.Fatalf("starting a move recorded %d notices", n)
	}
	mustCode(t, "wrong confirm", e.replaceConfirm(t, at, wrongCode(fresh, e.clock.Now())), 403, "mfa_invalid")
	if n := notices(t, u.id, delivery.NoticeMFAReplaced); n != 0 {
		t.Fatalf("a refused confirm recorded %d notices", n)
	}
	if c := e.replaceConfirm(t, at, totpAt(fresh, e.clock.Now())); c.code != 204 {
		t.Fatalf("confirm: %d %s", c.code, c.raw)
	}
	oneNotice(t, u.id, delivery.NoticeMFAReplaced)
	row := outboxOf(t, u.id)
	for _, s := range []string{uri, base32Secret(t, uri)} {
		if strings.Contains(row, s) || strings.Contains(e.logs.String(), s) {
			t.Fatal("the new app's secret is in the outbox or the logs")
		}
	}
}

// base32Secret is the otpauth URI's secret parameter, as shown to the user.
func base32Secret(t *testing.T, uri string) string {
	t.Helper()
	_, q, _ := strings.Cut(uri, "secret=")
	s, _, _ := strings.Cut(q, "&")
	if s == "" {
		t.Fatal("no secret in the otpauth uri")
	}
	return s
}

// A change that rolls back tells nobody; one minute holds at most one
// notice of a kind per account; a platform account (no hospital) and a
// deactivated one get none.
func TestSecurityNoticeTransactional(t *testing.T) {
	ctx := context.Background()
	u := clinician(t)
	s := delivery.Log{}
	errBoom := errors.New("synthetic: the change fails")
	err := db.WithAuth(ctx, app, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET must_change_password = true WHERE id = $1`, u.id); err != nil {
			return err
		}
		h, err := delivery.Notice(ctx, tx, s, u.id, delivery.NoticePasswordChanged)
		if err != nil || h != hospitalOf(t, u.id) {
			t.Errorf("notice: %s %v", h, err)
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if n := notices(t, u.id, ""); n != 0 {
		t.Fatalf("a rolled-back change left %d notices", n)
	}
	// Twice in one transaction (one minute): one row; another kind: its own.
	if err := db.WithAuth(ctx, app, func(tx pgx.Tx) error {
		for i, k := range []delivery.Kind{delivery.NoticeBackupCodeUsed, delivery.NoticeBackupCodeUsed, delivery.NoticeBackupCodesMade} {
			h, err := delivery.Notice(ctx, tx, s, u.id, k)
			if err != nil {
				return err
			}
			if (h == uuid.Nil) != (i == 1) {
				t.Errorf("notice %d (%s): hospital %s", i, k, h)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if notices(t, u.id, delivery.NoticeBackupCodeUsed) != 1 || notices(t, u.id, delivery.NoticeBackupCodesMade) != 1 {
		t.Fatal("dedupe within the minute")
	}
	p := seedUser(t, uuid.Nil, false, "platform_admin")
	gone := clinician(t)
	if _, err := super.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, gone.id); err != nil {
		t.Fatal(err)
	}
	for _, who := range []uuid.UUID{p.id, gone.id} {
		if err := db.WithAuth(ctx, app, func(tx pgx.Tx) error {
			h, err := delivery.Notice(ctx, tx, s, who, delivery.NoticePasswordChanged)
			if err == nil && h != uuid.Nil {
				t.Errorf("a notice for %s", who)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if n := notices(t, who, ""); n != 0 {
			t.Fatalf("%d notices for a platform or inactive account", n)
		}
	}
}

// badChannel is a sender whose channel the outbox refuses: recording the
// notice fails.
type badChannel struct{ delivery.Log }

func (badChannel) Channel() string { return "smtp" }

// A notice that can't be recorded never blocks or undoes the change: the
// password still changes (204), and the failure is logged by ids.
func TestSecurityNoticeFailureNeverBlocks(t *testing.T) {
	e := newEnv(t)
	e.svc.Notices = badChannel{}
	u, lr := loggedIn(t, e)
	r := e.do(t, req{method: "POST", path: "/auth/password", bearer: lr.body["access_token"].(string),
		body: map[string]any{"current_password": testPassword, "new_password": noticePW}})
	if r.code != 204 {
		t.Fatalf("change: %d %s", r.code, r.raw)
	}
	if x := e.login(t, u.email, noticePW, newIP()); x.code != 200 {
		t.Fatalf("the new password doesn't work: %d", x.code)
	}
	if n := notices(t, u.id, ""); n != 0 {
		t.Fatalf("%d notices", n)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "security notice not recorded") || strings.Contains(logs, u.email) {
		t.Fatalf("logs: %s", logs)
	}
	// With no sender at all, the same.
	e2 := newEnv(t)
	e2.svc.Notices = nil
	v, lr2 := loggedIn(t, e2)
	if r := e2.do(t, req{method: "POST", path: "/auth/password", bearer: lr2.body["access_token"].(string),
		body: map[string]any{"current_password": testPassword, "new_password": noticePW}}); r.code != 204 {
		t.Fatalf("change without a sender: %d", r.code)
	}
	if n := notices(t, v.id, ""); n != 0 {
		t.Fatalf("%d notices without a sender", n)
	}
}

// Only mhai_auth runs the definer; it takes security kinds only; mhai_auth
// can't write the outbox directly, nor the gateway call the definer.
func TestSecurityNoticeDefinerReach(t *testing.T) {
	ctx := context.Background()
	u := clinician(t)
	as := func(role, q string, args ...any) error {
		tx, err := super.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, q, args...)
		return err
	}
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}
	for _, role := range []string{"mhai_gateway", "mhai_platform", "mhai_notify", "mhai_worker"} {
		if err := as(role, `SELECT mhai_security_notice($1, 'security_password_changed', 'log')`, u.id); code(err) != "42501" {
			t.Errorf("%s runs the definer: %v", role, err)
		}
	}
	if err := as("mhai_auth", `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by)
		SELECT hospital_id, id, 'security_password_changed', 'log', id FROM users WHERE id = $1`, u.id); code(err) != "42501" {
		t.Errorf("mhai_auth writes the outbox: %v", err)
	}
	for _, k := range []string{"invite", "password_reset", "mfa_reset", "review_reminder", "security_x"} {
		if err := as("mhai_auth", `SELECT mhai_security_notice($1, $2, 'log')`, u.id, k); code(err) != "22023" {
			t.Errorf("the definer takes %s: %v", k, err)
		}
	}
	if err := as("mhai_auth", `SELECT mhai_security_notice($1, 'security_mfa_enabled', 'smtp')`, u.id); code(err) != "22023" {
		t.Errorf("the definer takes an unknown channel: %v", err)
	}
}
