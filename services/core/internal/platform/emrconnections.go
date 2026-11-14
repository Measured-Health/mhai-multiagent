package platform

// The platform's EMR connection routes (EMR design §6.2, §6.3, §13.1,
// §16.1; plan Task 11):
//
//   - GET  /platform/emr/connector-types
//   - GET  /platform/emr/connections?hospital=&state=
//   - GET  /platform/emr/connections/{id}
//   - POST /platform/hospitals/{id}/emr/connections   (step-up; credentials)
//   - PATCH /platform/emr/connections/{id}            (step-up; credentials)
//   - PUT  /platform/emr/connections/{id}/enabled     (step-up)
//   - POST /platform/emr/connections/{id}/test        (no step-up: it changes nothing)
//
// Platform admins write; auditors read. Off by default: unless
// CORE_EMR_ENABLED, every write answers 503 not_ready and the reads answer
// empty (connector-types {types: []}, the list empty, a connection 404).
//
// Credentials are sealed at this edge (gokit/seal), to the emr service's
// public key, bound to the connection, the field and the credential origin,
// before anything else uses them: the body is read into a buffer this
// handler owns, each value is decoded into its own byte slice, sealed, and
// both are zeroed (best effort in Go). Values never reach a log, an error,
// the database, the audit payload, the idempotency store (an HMAC of the
// request, httpx.IdempotentKeyed) or a reply; NATS carries the sealed boxes
// only.
//
// Credentials are bound to their origin (§6.3): a PATCH that changes the
// origin (the base address's or the token address's scheme, host or port)
// wipes them in the emr service before the change commits, and switches the
// connection off; if the wipe isn't confirmed the PATCH fails and nothing
// changes. Credentials sent with a PATCH, on either origin, are stored
// after the commit, so every stored change has its audit row; if the emr
// service doesn't confirm them, the change stays and the answer is
// credentials_missing (on the same origin the old ones are wiped and the
// connection switched off first).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/emrctl"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Limits (design §16.1).
const (
	SourceLabelMax  = 40
	MaxPrivateCIDRs = 8
	MinPollSeconds  = 60
	MaxPollSeconds  = 86400
)

var (
	errBadBody   = errors.New("platform: emr body does not fit")
	errNoType    = errors.New("platform: connector type not offered")
	errConnGone  = errors.New("platform: no such connection")
	errVersion   = errors.New("platform: version conflict")
	errNotTested = errors.New("platform: no passing test of the saved settings")
	errRemoved   = errors.New("platform: hospital removed or being removed")
	errEMR       = errors.New("platform: the emr service didn't confirm")
	errDuplicate = errors.New("platform: the hospital has a connection with that label")
)

func registerEMRConnections(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true, StepUp: true}
	creds := write
	creds.Credentials = true
	test := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	authz.Route(m, http.MethodGet, "/platform/emr/connector-types", read, http.HandlerFunc(d.emrTypes))
	authz.Route(m, http.MethodGet, "/platform/emr/connections", read, http.HandlerFunc(d.emrList))
	authz.Route(m, http.MethodGet, "/platform/emr/connections/{id}", read, http.HandlerFunc(d.emrGet))
	authz.Route(m, http.MethodPost, "/platform/hospitals/{id}/emr/connections", creds, http.HandlerFunc(d.emrCreate))
	authz.Route(m, http.MethodPatch, "/platform/emr/connections/{id}", creds, http.HandlerFunc(d.emrPatch))
	authz.Route(m, http.MethodPut, "/platform/emr/connections/{id}/enabled", write, http.HandlerFunc(d.emrEnabled))
	authz.Route(m, http.MethodPost, "/platform/emr/connections/{id}/test", test, http.HandlerFunc(d.emrTest))
}

// --- Replies (mhai-web's emr-contract.ts, plus the additive fields). ---

// EMRPerson is a hospital or a person: id and name.
type EMRPerson struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EMRLastTest is the last test of the saved settings; code null when it
// passed.
type EMRLastTest struct {
	At   string  `json:"at"`
	OK   bool    `json:"ok"`
	Code *string `json:"code"`
}

// EMRLastSync is the last finished sync.
type EMRLastSync struct {
	At      string `json:"at"`
	OK      bool   `json:"ok"`
	Records int    `json:"records"`
}

// EMRConnectionSummary is a connection in the list: no address, settings or
// credentials.
type EMRConnectionSummary struct {
	ID          string       `json:"id"`
	Hospital    EMRPerson    `json:"hospital"`
	Type        string       `json:"type"`
	TypeLabel   string       `json:"type_label"`
	SourceLabel string       `json:"source_label"`
	Enabled     bool         `json:"enabled"`
	Health      string       `json:"health"`
	LastTest    *EMRLastTest `json:"last_test"`
	LastSync    *EMRLastSync `json:"last_sync"`
	Errors24h   int64        `json:"errors_24h"`
	// Paused is additive: a source the breaker paused (PR 7).
	Paused bool `json:"paused"`
	// CredentialsMissing is additive: a required secret isn't set for the
	// current origin (a create the emr service didn't confirm, or a change
	// of origin). Re-send them with PATCH on the same connection.
	CredentialsMissing bool `json:"credentials_missing"`
}

// EMRCredential is a secret field and when it was set for the current
// origin; never a value.
type EMRCredential struct {
	Key   string  `json:"key"`
	SetAt *string `json:"set_at"`
}

// EMRSyncError is one error code of the last 24 hours.
type EMRSyncError struct {
	At    string `json:"at"`
	Code  string `json:"code"`
	Count int64  `json:"count"`
}

// EMRConnection is one connection in full.
type EMRConnection struct {
	EMRConnectionSummary
	BaseURL     string            `json:"base_url"`
	Settings    map[string]string `json:"settings"`
	Credentials []EMRCredential   `json:"credentials"`
	Errors      []EMRSyncError    `json:"errors"`
	UpdatedBy   *EMRPerson        `json:"updated_by"`
	UpdatedAt   *string           `json:"updated_at"`
	Version     int               `json:"version"`
	// Additive (design §16.1).
	PollSeconds    int      `json:"poll_seconds"`
	PrivateNetwork bool     `json:"private_network"`
	PrivateCIDRs   []string `json:"private_cidrs"`
}

// --- Reading. ---

// connRow is a connection as stored.
type connRow struct {
	id, hospital                 uuid.UUID
	hospitalName, hospitalStatus string
	typ, label, baseURL          string
	settings                     map[string]string
	origin                       []emrurl.Origin
	enabled, paused, privateNet  bool
	cidrs                        []netip.Prefix
	poll, version                int
	testAt                       *time.Time
	testOK                       *bool
	testCode                     *string
	updatedBy                    *uuid.UUID
	updatedByName                *string
	updatedAt                    time.Time
}

const connColumns = `c.id, c.hospital_id, h.name, h.status, c.type, c.source_label, c.base_url, c.settings,
	c.credentials_origin, c.enabled, c.paused, c.private_network, c.private_cidrs, c.poll_seconds, c.version,
	c.last_test_at, c.last_test_ok, c.last_test_code, c.updated_by, u.display_name, c.updated_at`

const connFrom = ` FROM emr_connections c JOIN hospitals h ON h.id = c.hospital_id
	LEFT JOIN users u ON u.id = c.updated_by`

func scanConn(row pgx.Row) (connRow, error) {
	var c connRow
	var origin []string
	err := row.Scan(&c.id, &c.hospital, &c.hospitalName, &c.hospitalStatus, &c.typ, &c.label, &c.baseURL, &c.settings,
		&origin, &c.enabled, &c.paused, &c.privateNet, &c.cidrs, &c.poll, &c.version, &c.testAt, &c.testOK, &c.testCode,
		&c.updatedBy, &c.updatedByName, &c.updatedAt)
	for _, o := range origin {
		c.origin = append(c.origin, emrurl.Origin(o))
	}
	return c, err
}

// loadConn reads one connection. lock takes FOR NO KEY UPDATE on the row:
// it serialises the platform's writes, and still lets the emr service's
// credential rows reference it (their key check takes KEY SHARE) while
// core waits on its answer.
func loadConn(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (connRow, error) {
	q := `SELECT ` + connColumns + connFrom + ` WHERE c.id = $1`
	if lock {
		q += ` FOR NO KEY UPDATE OF c`
	}
	c, err := scanConn(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errConnGone
	}
	return c, err
}

type healthRow struct {
	health  string
	at      *time.Time
	ok      *bool
	records *int32
	errs    int64
}

func (d Deps) summary(c connRow, h healthRow) EMRConnectionSummary {
	ti, _ := emrtypes.Lookup(c.typ)
	s := EMRConnectionSummary{ID: c.id.String(), Hospital: EMRPerson{ID: c.hospital.String(), Name: c.hospitalName},
		Type: c.typ, TypeLabel: ti.Label, SourceLabel: c.label, Enabled: c.enabled, Health: h.health,
		Errors24h: h.errs, Paused: c.paused}
	if s.Health == "" {
		s.Health = "never"
	}
	if c.testAt != nil && c.testOK != nil {
		lt := &EMRLastTest{At: stamp(*c.testAt), OK: *c.testOK}
		if !*c.testOK && c.testCode != nil {
			code := *c.testCode
			lt.Code = &code
		}
		s.LastTest = lt
	}
	if h.at != nil && h.ok != nil {
		ls := &EMRLastSync{At: stamp(*h.at), OK: *h.ok}
		if h.records != nil {
			ls.Records = int(*h.records)
		}
		s.LastSync = ls
	}
	return s
}

const healthQuery = `SELECT connection_id, health, last_sync_at, last_sync_ok, last_sync_records, errors_24h FROM emr_platform_health()`

func readHealth(ctx context.Context, tx pgx.Tx, only *uuid.UUID) (map[uuid.UUID]healthRow, error) {
	q, args := healthQuery, []any{}
	if only != nil {
		q, args = q+` WHERE connection_id = $1`, []any{*only}
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]healthRow{}
	for rows.Next() {
		var id uuid.UUID
		var h healthRow
		if err := rows.Scan(&id, &h.health, &h.at, &h.ok, &h.records, &h.errs); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// readConnection is the full reply for id.
func (d Deps) readConnection(ctx context.Context, tx pgx.Tx, id uuid.UUID) (EMRConnection, error) {
	c, err := loadConn(ctx, tx, id, false)
	if err != nil {
		return EMRConnection{}, err
	}
	hs, err := readHealth(ctx, tx, &id)
	if err != nil {
		return EMRConnection{}, err
	}
	out := EMRConnection{EMRConnectionSummary: d.summary(c, hs[id]), BaseURL: c.baseURL, Settings: map[string]string{},
		Credentials: []EMRCredential{}, Errors: []EMRSyncError{}, Version: c.version, PollSeconds: c.poll,
		PrivateNetwork: c.privateNet, PrivateCIDRs: []string{}}
	ti, _ := emrtypes.Lookup(c.typ)
	for k, v := range c.settings {
		if f, ok := ti.Field(k); ok && f.Kind == emrtypes.KindText {
			out.Settings[k] = v
		}
	}
	set := map[string]time.Time{}
	rows, err := tx.Query(ctx, `SELECT field, set_at FROM emr_credentials_meta($1)`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var f string
		var at time.Time
		if err := rows.Scan(&f, &at); err != nil {
			rows.Close()
			return out, err
		}
		set[f] = at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, k := range ti.Secrets() {
		cred := EMRCredential{Key: k}
		if at, ok := set[k]; ok {
			s := stamp(at)
			cred.SetAt = &s
		} else if f, _ := ti.Field(k); f.Required {
			out.CredentialsMissing = true
		}
		out.Credentials = append(out.Credentials, cred)
	}
	rows, err = tx.Query(ctx, `SELECT at, code, count FROM emr_platform_errors($1)`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e EMRSyncError
		var at time.Time
		if err := rows.Scan(&at, &e.Code, &e.Count); err != nil {
			rows.Close()
			return out, err
		}
		e.At = stamp(at)
		out.Errors = append(out.Errors, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, p := range c.cidrs {
		out.PrivateCIDRs = append(out.PrivateCIDRs, p.String())
	}
	if c.updatedBy != nil {
		name := ""
		if c.updatedByName != nil {
			name = *c.updatedByName
		}
		out.UpdatedBy = &EMRPerson{ID: c.updatedBy.String(), Name: name}
	}
	at := stamp(c.updatedAt)
	out.UpdatedAt = &at
	return out, nil
}

// connState is mhai-web's connectionState.
func connState(s EMRConnectionSummary) string {
	switch {
	case !s.Enabled && (s.LastTest == nil || !s.LastTest.OK):
		return "not_tested"
	case !s.Enabled:
		return "off"
	case s.Health == "errors" || s.Paused:
		return "needs_attention"
	}
	return "connected"
}

// --- Routes. ---

// emrTypes is GET /platform/emr/connector-types: the offered types
// (CORE_EMR_CONNECTOR_TYPES), none when EMR is off.
func (d Deps) emrTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	out := []emrtypes.TypeInfo{}
	if d.EMREnabled {
		for _, t := range emrtypes.Types {
			if slices.Contains(d.EMRTypes, t.ID) {
				out = append(out, t)
			}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"types": out})
}

// EMRConnectionsOut is the list reply.
type EMRConnectionsOut struct {
	Connections  []EMRConnectionSummary `json:"connections"`
	NotConnected []EMRPerson            `json:"not_connected"`
}

var connStates = []string{"all", "needs_attention", "off", "not_connected"}

// emrList is GET /platform/emr/connections.
func (d Deps) emrList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	q, err := params(r, []string{"hospital", "state"})
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	state := "all"
	if s, ok := q["state"]; ok {
		if !slices.Contains(connStates, s) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		state = s
	}
	var hospital *uuid.UUID
	if h, ok := q["hospital"]; ok {
		if !canonicalUUID(h) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		id := uuid.MustParse(h)
		hospital = &id
	}
	out := EMRConnectionsOut{Connections: []EMRConnectionSummary{}, NotConnected: []EMRPerson{}}
	if !d.EMREnabled {
		httpx.JSON(w, http.StatusOK, out)
		return
	}
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		hs, err := readHealth(r.Context(), tx, nil)
		if err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT `+connColumns+connFrom+`
			WHERE $1::uuid IS NULL OR c.hospital_id = $1 ORDER BY h.name, c.source_label, c.id`, hospital)
		if err != nil {
			return err
		}
		var conns []connRow
		for rows.Next() {
			c, err := scanConn(rows)
			if err != nil {
				rows.Close()
				return err
			}
			conns = append(conns, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range conns {
			s := d.summary(c, hs[c.id])
			if s.CredentialsMissing, err = credsMissing(r.Context(), tx, c); err != nil {
				return err
			}
			switch st := connState(s); {
			case state == "not_connected":
			case state == "needs_attention" && st != "needs_attention":
			case state == "off" && s.Enabled:
			default:
				out.Connections = append(out.Connections, s)
			}
		}
		if state != "all" && state != "not_connected" {
			return nil
		}
		hrows, err := tx.Query(r.Context(), `SELECT h.id, h.name FROM hospitals h
			WHERE h.status NOT IN ('removal_pending', 'removal_scheduled', 'removed')
			  AND ($1::uuid IS NULL OR h.id = $1)
			  AND NOT EXISTS (SELECT 1 FROM emr_connections c WHERE c.hospital_id = h.id)
			ORDER BY h.name, h.id`, hospital)
		if err != nil {
			return err
		}
		defer hrows.Close()
		for hrows.Next() {
			var id uuid.UUID
			var name string
			if err := hrows.Scan(&id, &name); err != nil {
				return err
			}
			out.NotConnected = append(out.NotConnected, EMRPerson{ID: id.String(), Name: name})
		}
		return hrows.Err()
	})
	if err != nil {
		d.emrFail(w, r, p, "list", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// pathConn is the path's connection id, or false (404).
func pathConn(r *http.Request) (uuid.UUID, bool) {
	s := r.PathValue("id")
	if !canonicalUUID(s) {
		return uuid.Nil, false
	}
	id := uuid.MustParse(s)
	return id, id != uuid.Nil
}

// emrGet is GET /platform/emr/connections/{id}.
func (d Deps) emrGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathConn(r)
	if !ok || !d.EMREnabled {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out EMRConnection
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = d.readConnection(r.Context(), tx, id)
		return err
	})
	if err != nil {
		d.emrFail(w, r, p, "read", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// emrReady reports whether the writes can run: EMR on, the emr client, the
// sealer and step-up wired. Otherwise 503 not_ready.
func (d Deps) emrReady(w http.ResponseWriter, r *http.Request, sealing bool) bool {
	if !d.EMREnabled || d.EMR == nil || d.Auth == nil || (sealing && d.EMRSealer == nil) {
		httpx.Error(w, r, "not_ready")
		return false
	}
	return true
}

// emrFail maps the closed errors to their codes and logs the rest by
// reason only (a driver error can carry row values).
func (d Deps) emrFail(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, err error) {
	code := "internal_error"
	switch {
	case errors.Is(err, errBadBody):
		code = "validation_failed"
	case errors.Is(err, errNoType):
		code = "no_type"
	case errors.Is(err, errConnGone), errors.Is(err, ErrNoSuchHospital):
		code = "not_found"
	case errors.Is(err, errVersion), errors.Is(err, errDuplicate):
		code = "conflict"
	case errors.Is(err, errNotTested):
		code = "not_tested"
	case errors.Is(err, errRemoved):
		code = "hospital_removed"
	case errors.Is(err, errEMR):
		code = "unavailable"
	}
	if code == "internal_error" || code == "unavailable" {
		d.log().Error("platform: emr connection "+what+" failed", "request_id", httpx.RequestIDFrom(r.Context()),
			"user_id", p.UserID.String(), "reason", code)
	}
	httpx.Error(w, r, code)
}

func removedStatus(s string) bool {
	return s == "removal_pending" || s == "removal_scheduled" || s == "removed"
}

// stepUp checks the code (403 mfa_required with none; mfa_invalid audited).
func (d Deps) stepUp(w http.ResponseWriter, r *http.Request, mfa, backup string) bool {
	if mfa == "" && backup == "" {
		httpx.Error(w, r, "mfa_required")
		return false
	}
	if err := d.Auth.StepUpAudited(r, mfa, backup); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return false
	}
	return true
}

func (d Deps) writeAudit(ctx context.Context, tx pgx.Tx, p authz.Principal, rid, action string, conn uuid.UUID, payload map[string]any) error {
	return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: rid, Action: action,
		Entity: "emr_connection", EntityID: conn.String(), Payload: payload})
}

// --- Bodies. ---

// secretValue is a credential value, decoded from its JSON string straight
// into bytes this handler can zero (encoding/json would make a string).
type secretValue []byte

// UnmarshalJSON unescapes a JSON string into the slice.
func (s *secretValue) UnmarshalJSON(b []byte) error {
	v, ok := unquoteJSON(b)
	if !ok {
		return errBadBody
	}
	*s = v
	return nil
}

// secretValues are the credentials of a body, by field.
type secretValues map[string]secretValue

func (m secretValues) zero() {
	for k, v := range m {
		clear(v)
		delete(m, k)
	}
}

// unquoteJSON decodes a JSON string literal into bytes (RFC 8259 escapes,
// surrogate pairs); false for anything else.
func unquoteJSON(b []byte) ([]byte, bool) {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return nil, false
	}
	b = b[1 : len(b)-1]
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '"' || c < 0x20:
			return nil, false
		case c != '\\':
			out = append(out, c)
			continue
		}
		i++
		if i >= len(b) {
			return nil, false
		}
		switch b[i] {
		case '"', '\\', '/':
			out = append(out, b[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r1, ok := hex4(b, i+1)
			if !ok {
				return nil, false
			}
			i += 4
			r := rune(r1)
			if utf16.IsSurrogate(r) {
				if i+6 < len(b) && b[i+1] == '\\' && b[i+2] == 'u' {
					if r2, ok := hex4(b, i+3); ok {
						if d := utf16.DecodeRune(r, rune(r2)); d != utf8.RuneError {
							r = d
							i += 6
						}
					}
				}
				if utf16.IsSurrogate(r) {
					return nil, false
				}
			}
			out = utf8.AppendRune(out, r)
		default:
			return nil, false
		}
	}
	if !utf8.Valid(out) {
		return nil, false
	}
	return out, true
}

func hex4(b []byte, at int) (uint16, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	var v uint16
	for _, c := range b[at : at+4] {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint16(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// readBody reads the request body into a buffer the handler zeroes, and
// decodes it strictly (unknown keys, trailing data: refused).
func readBody(r *http.Request, v any) (raw []byte, err error) {
	raw, err = io.ReadAll(io.LimitReader(r.Body, httpx.MaxBody+1))
	if err != nil || len(raw) > httpx.MaxBody {
		return raw, errBadBody
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return raw, errBadBody
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return raw, errBadBody
	}
	return raw, nil
}

type connectionIn struct {
	Type           string            `json:"type"`
	SourceLabel    *string           `json:"source_label"`
	BaseURL        *string           `json:"base_url"`
	Settings       map[string]string `json:"settings"`
	Credentials    secretValues      `json:"credentials"`
	PollSeconds    *int              `json:"poll_seconds"`
	PrivateNetwork *bool             `json:"private_network"`
	PrivateCIDRs   *[]string         `json:"private_cidrs"`
	Version        *int              `json:"version"`
	MFACode        string            `json:"mfa_code"`
	BackupCode     string            `json:"backup_code"`
}

// plainText is a setting or label: no control character, valid UTF-8,
// trimmed, at most max runes.
func plainText(s string, max int) bool {
	if s != strings.TrimSpace(s) || !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// labelOK: 1 to 40 characters, plain, naming no product (design §16.1).
func labelOK(s string) bool {
	return s != "" && plainText(s, SourceLabelMax) && emrtypes.VendorIn(s) == ""
}

// settingsOK checks settings against the type's text fields: declared
// keys only, plain values within their maximum, every required one set.
func settingsOK(ti emrtypes.TypeInfo, settings map[string]string) bool {
	for k, v := range settings {
		f, ok := ti.Field(k)
		if !ok || f.Kind != emrtypes.KindText || v == "" || !plainText(v, f.Max) {
			return false
		}
	}
	for _, f := range ti.Fields {
		if f.Kind == emrtypes.KindText && f.Required && settings[f.Key] == "" {
			return false
		}
	}
	return true
}

// cidrsOK parses the declared ranges: each a private IPv4 network (RFC
// 1918) of at most a /24, written as its network, at most 8 (00020 checks
// again, and that no other hospital's overlaps).
func cidrsOK(raw []string) ([]netip.Prefix, bool) {
	if len(raw) > MaxPrivateCIDRs {
		return nil, false
	}
	private := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16")}
	var out []netip.Prefix
	for _, s := range raw {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p.Bits() < 24 || p.Masked() != p || slices.Contains(out, p) {
			return nil, false
		}
		in := false
		for _, n := range private {
			in = in || (n.Contains(p.Addr()) && p.Bits() >= n.Bits())
		}
		if !in {
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}

// sealAll seals every credential for (conn, origin) and zeroes the values.
// Keys must be the type's secret fields, values 1 to 4000 bytes.
func (d Deps) sealAll(ti emrtypes.TypeInfo, conn uuid.UUID, origin []emrurl.Origin, creds secretValues) ([]*emrpb.SealedField, error) {
	defer creds.zero()
	keys := make([]string, 0, len(creds))
	for k := range creds {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []*emrpb.SealedField
	for _, k := range keys {
		v := creds[k]
		if !slices.Contains(ti.Secrets(), k) || len(v) < 1 || len(v) > seal.MaxValue {
			return nil, errBadBody
		}
		f, err := d.EMRSealer.Seal(conn, k, origin, v)
		if err != nil {
			return nil, errBadBody
		}
		out = append(out, f)
	}
	return out, nil
}

// putCredentials sends the sealed boxes. The reply names each field.
func (d Deps) putCredentials(ctx context.Context, p authz.Principal, hid, conn uuid.UUID, rid string, fields []*emrpb.SealedField) error {
	var rep emrpb.PutCredentialsReply
	err := d.EMR.Request(ctx, emrctl.FromPrincipal(p, hid, rid, true), "put_credentials",
		&emrpb.PutCredentialsRequest{ConnectionId: conn.String(), Fields: fields}, &rep)
	if err != nil || len(rep.GetFields()) != len(fields) {
		return errEMR
	}
	return nil
}

// emrCreate is POST /platform/hospitals/{id}/emr/connections. Order: the
// path (404), the body (422), the type (422 no_type), the addresses and
// origin (422), sealing, the rest of the body (422), the hospital (404, 409
// hospital_removed), the code; then the row and its audit entry, committed;
// then the credentials. A connection is saved off. A label the hospital
// already has is 409 conflict (ruling E1).
func (d Deps) emrCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pathHospital(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if !d.emrReady(w, r, true) {
		return
	}
	var body connectionIn
	raw, err := readBody(r, &body)
	defer clear(raw)
	defer body.Credentials.zero()
	if err != nil || r.URL.RawQuery != "" || body.Version != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ti, known := emrtypes.Lookup(body.Type)
	if !known || !slices.Contains(d.EMRTypes, body.Type) {
		httpx.Error(w, r, "no_type")
		return
	}
	if body.BaseURL == nil || body.SourceLabel == nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	settings := body.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	origin, err := emrurl.OriginsOf(body.Type, *body.BaseURL, settings, d.EMRDevelopment)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	id := uuid.New()
	for _, k := range ti.Secrets() {
		if f, _ := ti.Field(k); f.Required && len(body.Credentials[k]) == 0 {
			httpx.Error(w, r, "validation_failed")
			return
		}
	}
	sealed, err := d.sealAll(ti, id, origin, body.Credentials)
	clear(raw)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	poll := 300
	if body.PollSeconds != nil {
		poll = *body.PollSeconds
	}
	private := body.PrivateNetwork != nil && *body.PrivateNetwork
	var cidrs []netip.Prefix
	if body.PrivateCIDRs != nil {
		var ok bool
		if cidrs, ok = cidrsOK(*body.PrivateCIDRs); !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
	}
	if !labelOK(*body.SourceLabel) || !settingsOK(ti, settings) || poll < MinPollSeconds || poll > MaxPollSeconds ||
		(len(cidrs) > 0 && !private) || len(*body.BaseURL) > emrurl.MaxLen {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if cidrs == nil {
		cidrs = []netip.Prefix{}
	}
	var status string
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `SELECT status FROM hospitals WHERE id = $1`, hid).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSuchHospital
		}
		return err
	}); err != nil {
		d.emrFail(w, r, p, "create", err)
		return
	}
	if removedStatus(status) {
		httpx.Error(w, r, "hospital_removed")
		return
	}
	if !d.stepUp(w, r, body.MFACode, body.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		// The hospital again, under the write: it may have gone since.
		if err := tx.QueryRow(r.Context(), `SELECT status FROM hospitals WHERE id = $1`, hid).Scan(&status); err != nil {
			return err
		}
		if removedStatus(status) {
			return errRemoved
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url,
				settings, credentials_origin, private_network, private_cidrs, poll_seconds, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())`,
			id, hid, body.Type, *body.SourceLabel, *body.BaseURL, settings, emrurl.Strings(origin), private, cidrs, poll,
			p.UserID); err != nil {
			return err
		}
		return d.writeAudit(r.Context(), tx, p, rid, "emr.connection_added", id,
			map[string]any{"hospital_id": hid.String(), "connection_id": id.String(), "type": body.Type})
	})
	if err != nil {
		d.emrFail(w, r, p, "create", mapPG(err))
		return
	}
	if len(sealed) > 0 {
		if err := d.putCredentials(r.Context(), p, hid, id, rid, sealed); err != nil {
			// Ruling E1: the connection stays created, off, with
			// credentials_missing; the answer is a success, so the
			// idempotency store keeps it and a retry with the same key gets
			// this connection, never a second one. The credentials are
			// re-sent with PATCH on it.
			d.log().Warn("platform: emr connection created without credentials", "request_id", rid,
				"connection_id", id.String(), "reason", "credentials_missing")
		}
	}
	d.answer(w, r, p, id)
}

// mapPG turns the constraint refusals a valid-looking body can still meet
// into codes: another hospital's overlapping range is validation_failed; a
// label the hospital already has (emr_connections_label) is conflict.
func mapPG(err error) error {
	pe := pgError(err)
	switch {
	case pe == nil:
		return err
	case pe.Code == "23505" && pe.ConstraintName == "emr_connections_label":
		return errDuplicate
	case pe.Code == "23514", pe.Code == "23P01", pe.Code == "23505":
		return errBadBody
	}
	return err
}

// credsMissing reports whether a required secret of c's type isn't set for
// its current origin.
func credsMissing(ctx context.Context, tx pgx.Tx, c connRow) (bool, error) {
	ti, _ := emrtypes.Lookup(c.typ)
	rows, err := tx.Query(ctx, `SELECT field FROM emr_credentials_meta($1)`, c.id)
	if err != nil {
		return false, err
	}
	set, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	for _, f := range ti.Fields {
		if f.Kind == emrtypes.KindSecret && f.Required && !slices.Contains(set, f.Key) {
			return true, nil
		}
	}
	return false, nil
}

// answer replies with the connection as stored.
func (d Deps) answer(w http.ResponseWriter, r *http.Request, p authz.Principal, id uuid.UUID) {
	var out EMRConnection
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = d.readConnection(r.Context(), tx, id)
		return err
	}); err != nil {
		d.emrFail(w, r, p, "read", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// emrPatch is PATCH /platform/emr/connections/{id}: the fields being
// changed, and version. A change of the base address, settings,
// credentials or private network clears the last test; a change of origin
// also wipes the credentials and switches the connection off.
func (d Deps) emrPatch(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathConn(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if !d.emrReady(w, r, true) {
		return
	}
	var body connectionIn
	raw, err := readBody(r, &body)
	defer clear(raw)
	defer body.Credentials.zero()
	if err != nil || r.URL.RawQuery != "" || body.Type != "" || body.Version == nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var cur connRow
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		cur, err = loadConn(r.Context(), tx, id, false)
		return err
	}); err != nil {
		d.emrFail(w, r, p, "patch", err)
		return
	}
	ti, _ := emrtypes.Lookup(cur.typ)
	next := cur
	next.settings = map[string]string{}
	for k, v := range cur.settings {
		next.settings[k] = v
	}
	for k, v := range body.Settings {
		if v == "" {
			delete(next.settings, k)
		} else {
			next.settings[k] = v
		}
	}
	if body.BaseURL != nil {
		next.baseURL = *body.BaseURL
	}
	origin, err := emrurl.OriginsOf(cur.typ, next.baseURL, next.settings, d.EMRDevelopment)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	originChanged := !slices.Equal(emrurl.Strings(origin), emrurl.Strings(cur.origin))
	credKeys := make([]string, 0, len(body.Credentials))
	for k := range body.Credentials {
		credKeys = append(credKeys, k)
	}
	slices.Sort(credKeys)
	sealed, err := d.sealAll(ti, id, origin, body.Credentials)
	clear(raw)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var changed []string
	if body.SourceLabel != nil && *body.SourceLabel != cur.label {
		next.label = *body.SourceLabel
		changed = append(changed, "source_label")
	}
	if next.baseURL != cur.baseURL {
		changed = append(changed, "base_url")
	}
	if body.PollSeconds != nil && *body.PollSeconds != cur.poll {
		next.poll = *body.PollSeconds
		changed = append(changed, "poll_seconds")
	}
	if body.PrivateNetwork != nil && *body.PrivateNetwork != cur.privateNet {
		next.privateNet = *body.PrivateNetwork
		changed = append(changed, "private_network")
	}
	if body.PrivateCIDRs != nil {
		cidrs, ok := cidrsOK(*body.PrivateCIDRs)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		if cidrs == nil {
			cidrs = []netip.Prefix{}
		}
		if !slices.Equal(cidrs, cur.cidrs) {
			changed = append(changed, "private_cidrs")
		}
		next.cidrs = cidrs
	}
	settingKeys := map[string]bool{}
	for k := range cur.settings {
		settingKeys[k] = true
	}
	for k := range next.settings {
		settingKeys[k] = true
	}
	for _, k := range sortedKeys(settingKeys) {
		if cur.settings[k] != next.settings[k] {
			changed = append(changed, k)
		}
	}
	changed = append(changed, credKeys...)
	if !labelOK(next.label) || !settingsOK(ti, next.settings) || next.poll < MinPollSeconds || next.poll > MaxPollSeconds ||
		(len(next.cidrs) > 0 && !next.privateNet) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if *body.Version != cur.version {
		httpx.Error(w, r, "conflict")
		return
	}
	if removedStatus(cur.hospitalStatus) {
		httpx.Error(w, r, "hospital_removed")
		return
	}
	if !d.stepUp(w, r, body.MFACode, body.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	// Only the label or the poll interval: the last test still describes the
	// saved settings.
	clearsTest := slices.ContainsFunc(changed, func(k string) bool { return k != "source_label" && k != "poll_seconds" })
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		locked, err := loadConn(r.Context(), tx, id, true)
		if err != nil {
			return err
		}
		if locked.version != *body.Version {
			return errVersion
		}
		if removedStatus(locked.hospitalStatus) {
			return errRemoved
		}
		enabled := locked.enabled && !originChanged
		if _, err := tx.Exec(r.Context(), `UPDATE emr_connections SET source_label = $2, base_url = $3, settings = $4,
				credentials_origin = $5, private_network = $6, private_cidrs = $7, poll_seconds = $8, enabled = $9,
				last_test_at = CASE WHEN $10 THEN NULL ELSE last_test_at END,
				last_test_ok = CASE WHEN $10 THEN NULL ELSE last_test_ok END,
				last_test_code = CASE WHEN $10 THEN NULL ELSE last_test_code END,
				version = version + 1, updated_by = $11, updated_at = now()
			WHERE id = $1`, id, next.label, next.baseURL, next.settings, emrurl.Strings(origin), next.privateNet, next.cidrs,
			next.poll, enabled, clearsTest, p.UserID); err != nil {
			return err
		}
		fields := make([]any, 0, len(changed))
		for _, k := range changed {
			fields = append(fields, k)
		}
		if err := d.writeAudit(r.Context(), tx, p, rid, "emr.connection_changed", id, map[string]any{
			"hospital_id": cur.hospital.String(), "connection_id": id.String(), "fields_changed": fields,
			"credentials_replaced": len(sealed) > 0, "credentials_wiped": originChanged}); err != nil {
			return err
		}
		if locked.enabled && !enabled {
			if err := d.writeAudit(r.Context(), tx, p, rid, "emr.connection_switched_off", id, map[string]any{
				"hospital_id": cur.hospital.String(), "connection_id": id.String()}); err != nil {
				return err
			}
		}
		if originChanged {
			// The wipe, after the row's checks and before the commit: no
			// confirmation, no change.
			var rep emrpb.WipeCredentialsReply
			if err := d.EMR.Request(r.Context(), emrctl.FromPrincipal(p, cur.hospital, rid, false), "wipe_credentials",
				&emrpb.WipeCredentialsRequest{ConnectionId: id.String()}, &rep); err != nil || !rep.GetWiped() {
				return errEMR
			}
		}
		return nil
	})
	if err != nil {
		d.emrFail(w, r, p, "patch", mapPG(err))
		return
	}
	// The credentials go after the commit, so every stored change has its
	// audit row (and a new origin is the one the emr service reads). If the
	// emr service doesn't confirm them, the change stays saved and the
	// answer says what is stored: credentials_missing (ruling E1, as at
	// create). On the same origin the old credentials are wiped first and
	// the connection switched off, so it never runs on credentials the
	// admin meant to replace.
	if len(sealed) > 0 {
		if err := d.putCredentials(r.Context(), p, cur.hospital, id, rid, sealed); err != nil {
			if !originChanged {
				if err := d.dropUnconfirmed(r.Context(), p, cur.hospital, id, rid, *body.Version+1); err != nil {
					d.emrFail(w, r, p, "patch credentials", err)
					return
				}
			}
			d.log().Warn("platform: emr connection changed without credentials", "request_id", rid,
				"connection_id", id.String(), "reason", "credentials_missing")
		}
	}
	d.answer(w, r, p, id)
}

// dropUnconfirmed wipes a connection's credentials and switches it off,
// after a same-origin replacement the emr service didn't confirm. version is
// the one the PATCH committed: a later change wins, and nothing is done.
// The wipe comes before the commit: no confirmation, no change.
func (d Deps) dropUnconfirmed(ctx context.Context, p authz.Principal, hid, id uuid.UUID, rid string, version int) error {
	return d.tx(ctx, func(tx pgx.Tx) error {
		locked, err := loadConn(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if locked.version != version {
			return nil
		}
		var rep emrpb.WipeCredentialsReply
		if err := d.EMR.Request(ctx, emrctl.FromPrincipal(p, hid, rid, false), "wipe_credentials",
			&emrpb.WipeCredentialsRequest{ConnectionId: id.String()}, &rep); err != nil || !rep.GetWiped() {
			return errEMR
		}
		if err := d.writeAudit(ctx, tx, p, rid, "emr.connection_changed", id, map[string]any{
			"hospital_id": hid.String(), "connection_id": id.String(), "fields_changed": []any{},
			"credentials_replaced": false, "credentials_wiped": true}); err != nil {
			return err
		}
		if !locked.enabled {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE emr_connections SET enabled = false, version = version + 1, updated_by = $2,
			updated_at = now() WHERE id = $1`, id, p.UserID); err != nil {
			return err
		}
		return d.writeAudit(ctx, tx, p, rid, "emr.connection_switched_off", id, map[string]any{
			"hospital_id": hid.String(), "connection_id": id.String()})
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

type enabledBody struct {
	Enabled    *bool  `json:"enabled"`
	Version    *int   `json:"version"`
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// emrEnabled is PUT /platform/emr/connections/{id}/enabled. On needs an
// offered type and a passing test of the saved settings (409 not_tested).
func (d Deps) emrEnabled(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathConn(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if !d.emrReady(w, r, false) {
		return
	}
	var body enabledBody
	if !httpx.Decode(w, r, &body) {
		return
	}
	if body.Enabled == nil || body.Version == nil || r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	on := *body.Enabled
	var cur connRow
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		cur, err = loadConn(r.Context(), tx, id, false)
		return err
	}); err != nil {
		d.emrFail(w, r, p, "enable", err)
		return
	}
	switch {
	case *body.Version != cur.version:
		httpx.Error(w, r, "conflict")
		return
	case on && removedStatus(cur.hospitalStatus):
		httpx.Error(w, r, "hospital_removed")
		return
	case on && !slices.Contains(d.EMRTypes, cur.typ):
		httpx.Error(w, r, "no_type")
		return
	case on && (cur.testOK == nil || !*cur.testOK):
		httpx.Error(w, r, "not_tested")
		return
	}
	if !d.stepUp(w, r, body.MFACode, body.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		locked, err := loadConn(r.Context(), tx, id, true)
		if err != nil {
			return err
		}
		if locked.version != *body.Version {
			return errVersion
		}
		if on && (locked.testOK == nil || !*locked.testOK) {
			return errNotTested
		}
		if _, err := tx.Exec(r.Context(), `UPDATE emr_connections SET enabled = $2, version = version + 1,
			updated_by = $3, updated_at = now() WHERE id = $1`, id, on, p.UserID); err != nil {
			return err
		}
		action := "emr.connection_switched_off"
		if on {
			action = "emr.connection_switched_on"
		}
		return d.writeAudit(r.Context(), tx, p, rid, action, id, map[string]any{
			"hospital_id": locked.hospital.String(), "connection_id": id.String()})
	})
	if err != nil {
		d.emrFail(w, r, p, "enable", err)
		return
	}
	if on {
		d.syncNow(r.Context(), cur.hospital, id, rid)
	}
	d.answer(w, r, p, id)
}

// syncNowWait bounds the sync_now call a switch-on makes.
const syncNowWait = 3 * time.Second

// syncNow asks the emr service to poll a connection just switched on at its
// next tick (emr.v1.ctl.sync_now as svc:core, the token's hospital the
// connection's), so the first fetch doesn't wait a whole poll interval.
// Best effort, after the commit: the switch stands whatever happens here;
// an emr service that is down or busy polls it on its own schedule. Logged
// by ids and the outcome code only.
func (d Deps) syncNow(ctx context.Context, hid, conn uuid.UUID, rid string) {
	if d.EMR == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncNowWait)
	defer cancel()
	var rep emrpb.SyncNowReply
	if err := d.EMR.Request(ctx, emrctl.System(hid.String(), rid), "sync_now",
		&emrpb.SyncNowRequest{ConnectionId: conn.String()}, &rep); err != nil {
		d.log().Warn("platform: sync_now after switch-on not answered; the connection polls on its schedule",
			"request_id", rid, "connection_id", conn.String(), "reason", "unavailable")
		return
	}
	d.log().Info("platform: sync_now after switch-on", "request_id", rid, "connection_id", conn.String(),
		"queued", rep.GetQueued())
}

// emrTest is POST /platform/emr/connections/{id}/test: the emr service
// tries the saved settings; the outcome is recorded against the version it
// tested (a change since is 409 conflict, nothing recorded).
func (d Deps) emrTest(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathConn(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if !d.emrReady(w, r, false) {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !emptyBody(r) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var cur connRow
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		cur, err = loadConn(r.Context(), tx, id, false)
		return err
	}); err != nil {
		d.emrFail(w, r, p, "test", err)
		return
	}
	switch {
	case removedStatus(cur.hospitalStatus):
		httpx.Error(w, r, "hospital_removed")
		return
	case !slices.Contains(d.EMRTypes, cur.typ):
		httpx.Error(w, r, "no_type")
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var rep emrpb.TestReply
	if err := d.EMR.Request(r.Context(), emrctl.FromPrincipal(p, cur.hospital, rid, false), "test",
		&emrpb.TestRequest{ConnectionId: id.String()}, &rep); err != nil || !emrtypes.IsCode(rep.GetCode()) ||
		rep.GetOk() != (rep.GetCode() == "ok") {
		d.emrFail(w, r, p, "test", errEMR)
		return
	}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE emr_connections SET last_test_at = now(), last_test_ok = $3, last_test_code = $4
			WHERE id = $1 AND version = $2`, id, cur.version, rep.GetOk(), rep.GetCode())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errVersion
		}
		return d.writeAudit(r.Context(), tx, p, rid, "emr.connection_tested", id, map[string]any{
			"hospital_id": cur.hospital.String(), "connection_id": id.String(), "outcome": rep.GetCode()})
	})
	if err != nil {
		d.emrFail(w, r, p, "test", err)
		return
	}
	d.answer(w, r, p, id)
}

func pgError(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}
