package platform

// Adding a hospital from the platform app (mhai-web's "Add hospital" form;
// product owner, 24 Nov 2026; migration 00059):
//
//   POST /platform/hospitals {name, short_code, country, time_zone,
//   email_domains, first_admin: {name, email}, mfa_code | backup_code} →
//   201 and the hospital's page, exactly as GET /platform/hospitals/{id}
//   then reads it.
//
// Platform admins only (auditors 403), on a two-step session, with a
// step-up code (owner decision, 24 Nov 2026), checked as the other
// platform step-up writes check it (auth.Service.StepUpAudited).
// Idempotency-Key as every platform write: a retry with the same key gets
// the stored reply and adds nothing, without its spent code being checked
// again.
//
// The checks are the CLI's (account.CheckNewHospital: the name, the short
// code's format, an IANA time zone), plus an ISO 3166-1 alpha-2 country,
// up to 20 email domains (trimmed, lower-cased, repeats dropped) and the
// first admin's name and email (the hospital admin invite's rule). A bad
// or missing field is 422 validation_failed, a short code another hospital
// has 409 duplicate (mhai-web highlights the field on that code), an email
// an account already has 409 email_in_use. The first admin's email need
// not be in email_domains (mhai-web's sample API doesn't ask it either).
//
// In one transaction, as mhai_platform (00059 holds every write to the
// hospital this transaction makes): the hospital, setup_incomplete, as the
// CLI leaves it (it is activated as before: core hospitals activate); its
// settings, two-step required for clinicians (as the live site's CLI
// onboarding passes --require-mfa-for-clinicians) and the rest at their
// defaults; the first admin's account, invited (no
// password) with hospital_admin; an invite link valid 3 days (the hospital
// admin's invite: 32 random bytes, only the SHA-256 stored); the outbox
// row; and the audit entries hospital.created {via: platform} and
// staff.invited {roles: [hospital_admin]}, by ids only. Any failure rolls
// back all of it. With no sender (CORE_DELIVERY unset) the route answers
// 503 no_delivery before anything is changed. After commit the link goes
// to the sender (the notifications service mails it); if the sender
// refuses it, the hospital stays added and the reply is 424 not_delivered,
// as the hospital admin's invites answer.
//
// Logs carry ids and codes only: never the name, the email or the link.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const (
	// HospitalAddsPerMinute bounds a session's adds.
	HospitalAddsPerMinute = 10
	// hospitalAddScope is the idempotency_keys scope of the add's rate
	// counter.
	hospitalAddScope = "platform.hospital.add_rate"
	// MaxEmailDomains is a hospital's most email domains (00059's CHECK).
	MaxEmailDomains = 20
	// emailDomainMax is a domain's longest, in bytes.
	emailDomainMax = 253
	// FirstAdminInviteTTL is the first admin's link's life: the hospital
	// admin's invite's (hospital.InviteTTL).
	FirstAdminInviteTTL = 3 * 24 * time.Hour
	// inviteLinkPath is the clinical app's page an invite's link opens
	// (the hospital admin's invites use the same).
	inviteLinkPath = "/reset-password"
)

// emailDomainRE is a lower-case host name of two labels or more.
var emailDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

var (
	errDuplicateCode = errors.New("platform: short code taken")
	errEmailInUse    = errors.New("platform: email in use")
)

type firstAdminIn struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// hospitalIn is the body: every field required.
type hospitalIn struct {
	Name         *string       `json:"name"`
	ShortCode    *string       `json:"short_code"`
	Country      *string       `json:"country"`
	TimeZone     *string       `json:"time_zone"`
	EmailDomains *[]string     `json:"email_domains"`
	FirstAdmin   *firstAdminIn `json:"first_admin"`
	// The caller's step-up code: exactly one of the two.
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// newHospital is a checked, normalised body.
type newHospital struct {
	name, code, country, timeZone string
	domains                       []string
	adminName, adminEmail         string
}

// isoCountries are ISO 3166-1's 249 officially assigned alpha-2 codes:
// no user-assigned (AA, QM-QZ, XA-XZ, ZZ), exceptionally reserved (UK,
// EU, UN) or withdrawn code.
var isoCountries = strings.Fields(`
	AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ
	BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ
	CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ
	DE DJ DK DM DO DZ
	EC EE EG EH ER ES ET
	FI FJ FK FM FO FR
	GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY
	HK HM HN HR HT HU
	ID IE IL IM IN IO IQ IR IS IT
	JE JM JO JP
	KE KG KH KI KM KN KP KR KW KY KZ
	LA LB LC LI LK LR LS LT LU LV LY
	MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ
	NA NC NE NF NG NI NL NO NP NR NU NZ
	OM
	PA PE PF PG PH PK PL PM PN PR PS PT PW PY
	QA
	RE RO RS RU RW
	SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ
	TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ
	UA UG UM US UY UZ
	VA VC VE VG VI VN VU
	WF WS
	YE YT
	ZA ZM ZW`)

// ISOCountryCount is how many codes NormaliseCountry accepts.
var ISOCountryCount = len(isoCountries)

// NormaliseCountry upper-cases an ISO 3166-1 alpha-2 country code; ok is
// false for anything but an officially assigned one.
func NormaliseCountry(s string) (string, bool) {
	c := strings.ToUpper(strings.TrimSpace(s))
	if len(c) != 2 || !slices.Contains(isoCountries, c) {
		return "", false
	}
	return c, true
}

// NormaliseEmailDomains trims, lower-cases and drops repeats; ok is false
// for more than MaxEmailDomains or any entry that isn't a host name.
func NormaliseEmailDomains(in []string) ([]string, bool) {
	out := []string{}
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if len(d) > emailDomainMax || !emailDomainRE.MatchString(d) {
			return nil, false
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out, len(out) <= MaxEmailDomains
}

func (in hospitalIn) check() (newHospital, bool) {
	var h newHospital
	if in.Name == nil || in.ShortCode == nil || in.Country == nil || in.TimeZone == nil || in.EmailDomains == nil ||
		in.FirstAdmin == nil || in.FirstAdmin.Name == nil || in.FirstAdmin.Email == nil {
		return h, false
	}
	var err error
	if h.name, h.code, err = account.CheckNewHospital(*in.Name, *in.ShortCode, *in.TimeZone); err != nil {
		return h, false
	}
	h.timeZone = *in.TimeZone
	var ok bool
	if h.country, ok = NormaliseCountry(*in.Country); !ok {
		return h, false
	}
	if h.domains, ok = NormaliseEmailDomains(*in.EmailDomains); !ok {
		return h, false
	}
	// The admin's name follows a hospital name's rule (1 to 120
	// characters, mhai-web's NAME_MAX for both; no control characters).
	if h.adminName, ok = account.NormaliseHospitalName(*in.FirstAdmin.Name); !ok {
		return h, false
	}
	h.adminEmail = account.NormaliseEmail(*in.FirstAdmin.Email)
	if !account.ValidEmail(h.adminEmail) {
		return h, false
	}
	return h, true
}

// inviteLink is the link an invite token goes out in (relative when no app
// URL is configured), as the hospital admin's invites build it.
func (d Deps) inviteLink(plain string) string {
	return strings.TrimRight(d.AppURL, "/") + inviteLinkPath + "?token=" + url.QueryEscape(plain)
}

// addHospital writes h, its first admin and their invite, and reads the
// page back, in tx. by is the platform admin.
func (d Deps) addHospital(ctx context.Context, tx pgx.Tx, h newHospital, by uuid.UUID, rid string) (Hospital, delivery.Message, error) {
	hid, admin := uuid.New(), uuid.New()
	var msg delivery.Message
	// The first admin is named now; their row follows (00059's deferred
	// key).
	if _, err := tx.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, time_zone, country, email_domains, first_admin_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, hid, h.name, h.code, h.timeZone, h.country, h.domains, admin); err != nil {
		if uniqueOn(err, "hospitals_short_code_key") {
			return Hospital{}, msg, errDuplicateCode
		}
		return Hospital{}, msg, err
	}
	// Two-step for clinicians from the start, as the live site's CLI
	// onboarding (--require-mfa-for-clinicians); 00059 holds it true.
	if _, err := tx.Exec(ctx, `INSERT INTO hospital_settings (hospital_id, require_mfa_for_clinicians) VALUES ($1, true)`,
		hid); err != nil {
		return Hospital{}, msg, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO users (id, hospital_id, email, display_name, invited_at) VALUES ($1, $2, $3, $4, now())`,
		admin, hid, h.adminEmail, h.adminName); err != nil {
		if uniqueOn(err, "users_email_key") {
			return Hospital{}, msg, errEmailInUse
		}
		return Hospital{}, msg, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, admin, authz.RoleHospitalAdmin); err != nil {
		return Hospital{}, msg, err
	}
	plain, hash := token.NewRefresh()
	var exp time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES ($1, $2, 'invite', $3, $4, now() + make_interval(secs => $5)) RETURNING expires_at`,
		hid, admin, hash, by, FirstAdminInviteTTL.Seconds()).Scan(&exp); err != nil {
		return Hospital{}, msg, err
	}
	msg = delivery.Message{Kind: delivery.Invite, HospitalID: hid, Recipient: admin, By: by,
		Link: d.inviteLink(plain), ExpiresAt: exp}
	if err := delivery.Record(ctx, tx, d.Sender, msg); err != nil {
		return Hospital{}, msg, err
	}
	// Ids only: no name, code or email in the audit trail.
	if err := audit.Write(ctx, tx, audit.Event{ActorID: by.String(), HospitalID: hid.String(), RequestID: rid,
		Action: "hospital.created", Entity: "hospital", EntityID: hid.String(),
		Payload: map[string]any{"via": ViaPlatform}}); err != nil {
		return Hospital{}, msg, err
	}
	if err := audit.Write(ctx, tx, audit.Event{ActorID: by.String(), HospitalID: hid.String(), RequestID: rid,
		Action: "staff.invited", Entity: "user", EntityID: admin.String(),
		Payload: map[string]any{"roles": []string{authz.RoleHospitalAdmin}}}); err != nil {
		return Hospital{}, msg, err
	}
	if d.AfterHospitalAdd != nil {
		if err := d.AfterHospitalAdd(); err != nil {
			return Hospital{}, msg, err
		}
	}
	out, err := readHospital(ctx, tx, hid)
	return out, msg, err
}

// uniqueOn reports whether err is a unique violation of constraint.
func uniqueOn(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == constraint
}

// taken reports which of h's unique values another row already has
// (errDuplicateCode, errEmailInUse), so a step-up code is never spent on
// an add that can't succeed. The write checks again (unique keys).
func taken(ctx context.Context, tx pgx.Tx, h newHospital) error {
	var code, email bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hospitals WHERE short_code = $1),
		EXISTS (SELECT 1 FROM users WHERE email = $2)`, h.code, h.adminEmail).Scan(&code, &email); err != nil {
		return err
	}
	switch {
	case code:
		return errDuplicateCode
	case email:
		return errEmailInUse
	}
	return nil
}

// hospitalAdd is POST /platform/hospitals. Order: the query (422), the
// body (422), the sender and the step-up check (503), a short code or
// email already taken (409, before a code is spent), the session's minute
// (429), the code (403 mfa_required with none; mfa_invalid when wrong or
// both are sent, audited auth.mfa.failed; 429 mfa_locked, audited), then
// the write (409 again on a race), then the link to the sender (424
// not_delivered, the hospital kept).
func (d Deps) hospitalAdd(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in hospitalIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	h, ok := in.check()
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	rid := httpx.RequestIDFrom(ctx)
	if d.Sender == nil {
		d.log().Error("platform: hospital add refused: no sender", "request_id", rid, "reason", "no_delivery")
		httpx.Error(w, r, "no_delivery")
		return
	}
	if d.Auth == nil {
		d.log().Error("platform: hospital add refused: no step-up", "request_id", rid, "reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	err := d.tx(ctx, func(tx pgx.Tx) error { return taken(ctx, tx, h) })
	switch {
	case errors.Is(err, errDuplicateCode):
		httpx.Error(w, r, "duplicate")
		return
	case errors.Is(err, errEmailInUse):
		httpx.Error(w, r, "email_in_use")
		return
	case err != nil:
		d.log().Error("platform: hospital add lookup failed", "request_id", rid, "user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
		return
	}
	allowed, err := d.reserveRate(r, p, hospitalAddScope, "platform.hospital.add.v1", HospitalAddsPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: hospital add refused: rate counter unavailable", "request_id", rid, "reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	if in.MFACode == "" && in.BackupCode == "" {
		httpx.Error(w, r, "mfa_required")
		return
	}
	// A refused code is audited (auth.mfa.failed or auth.mfa.locked); both
	// codes at once are refused as invalid. The code is spent here: an
	// Idempotency-Key retry is answered from the stored reply, never
	// checked again.
	if err := d.Auth.StepUpAudited(r, in.MFACode, in.BackupCode); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return
	}
	var out Hospital
	var msg delivery.Message
	err = d.tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, msg, err = d.addHospital(ctx, tx, h, p.UserID, rid)
		return err
	})
	switch {
	case errors.Is(err, errDuplicateCode):
		httpx.Error(w, r, "duplicate")
		return
	case errors.Is(err, errEmailInUse):
		httpx.Error(w, r, "email_in_use")
		return
	case err != nil:
		d.log().Error("platform: hospital add failed", "request_id", rid, "user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
		return
	}
	if err := d.Sender.Send(context.WithoutCancel(ctx), msg); err != nil {
		d.log().Error("platform: hospital added but the invite was not handed over", "request_id", rid,
			"hospital_id", out.ID, "user_id", p.UserID.String(), "reason", "not_delivered")
		httpx.Error(w, r, "not_delivered")
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}
