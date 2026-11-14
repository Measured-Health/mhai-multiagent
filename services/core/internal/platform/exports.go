package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/letters"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Audit exports (contract §14.9; design §8.8; plan Task 25, departures
// 61-70):
//
//   - POST /platform/audit/exports {kind, filters, reason, note?,
//     reference?, mfa_code | backup_code}: platform admins and auditors
//     (an auditor write), step-up. The record (who, when, why, the
//     filters, the snapshot, the row count, the checksum) and its entry
//     commit before the reply; over 50,000 rows nothing is recorded.
//   - GET /platform/audit/exports?cursor: the records, newest first.
//   - GET /platform/audit/exports/{id}/file: the file, once, to its owner,
//     before it expires; every refusal recorded.
//
// No file is kept anywhere (departure 62): it is built in the record's
// transaction for its checksum and dropped, and built again in the one
// download transaction, and sent only if its checksum is the record's.
// Off by default: every export route, and the named-access list, answers
// 404 until CORE_AUDIT_EXPORTS=true (Deps.ExportsOn).

const (
	// ExportsPerHour, DownloadsPerMinute: each person's limits, reserved
	// before anything is read (departure 75).
	ExportsPerHour     = 10
	DownloadsPerMinute = 30
	// ExportsPage is the records list's page size.
	ExportsPage = 50
	// exportNoteMax and exportReferenceMax are the contract's limits.
	exportNoteMax = 200
)

var (
	// ExportReasons are the contract's reasons.
	ExportReasons = []string{"legal_request", "hospital_request", "regulator_request", "internal_review",
		"security_investigation", "other"}
	// exportReference is a reference's characters (departure 63): no
	// quote, comma, formula character or control character.
	exportReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._/:-]{0,59}$`)
)

var (
	errNoExport      = errors.New("platform: no such export")
	errExportChanged = errors.New("platform: the rebuilt file differs from its record")
)

// tooManyRows is an export over ExportMax: 422 too_many_rows {rows}.
type tooManyRows struct{ rows int }

func (tooManyRows) Error() string { return "platform: too many rows" }

func registerExports(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}
	create := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles, AuditorWrite: true}
	authz.Route(m, http.MethodPost, "/platform/audit/exports", create, http.HandlerFunc(d.exportCreate))
	authz.Route(m, http.MethodGet, "/platform/audit/exports", read, http.HandlerFunc(d.exportList))
	authz.Route(m, http.MethodGet, "/platform/audit/exports/{id}/file", read, http.HandlerFunc(d.exportFile))
	authz.Route(m, http.MethodGet, "/platform/audit/named-access", read, http.HandlerFunc(d.namedAccess))
	registerLegal(m, d)
}

// stillHolds re-reads, inside tx, what the guard may have cached for up
// to 30 seconds: the account active, a platform account, and still
// holding one of roles (errNotReader otherwise).
func stillHolds(ctx context.Context, tx pgx.Tx, p authz.Principal, roles ...string) error {
	var usable bool
	var held []string
	err := tx.QueryRow(ctx, `SELECT u.is_active AND u.hospital_id IS NULL,
		ARRAY(SELECT x.role FROM user_roles x WHERE x.user_id = u.id) FROM users u WHERE u.id = $1`, p.UserID).Scan(&usable, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotReader
	}
	if err != nil {
		return err
	}
	for _, want := range roles {
		if usable && slices.Contains(held, want) && slices.Contains(p.Roles, want) {
			return nil
		}
	}
	return errNotReader
}

// reserveUserRate counts one request against the person's window (one
// upsert on the person's counter: parallel requests queue on its row, so
// the limit holds; departure 75). False past limit.
func (d Deps) reserveUserRate(r *http.Request, p authz.Principal, scope string, limit int, window time.Duration) (bool, error) {
	key := scope + ":" + p.UserID.String()
	allowed := false
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(r.Context(), `
			INSERT INTO idempotency_keys AS k (scope, key, hospital_id, request_hash, status, expires_at)
			VALUES ($1, $2, NULL, $2, 1, now() + make_interval(secs => $4))
			ON CONFLICT (scope, key) DO UPDATE
			SET status = CASE WHEN k.expires_at <= now() THEN 1 ELSE k.status + 1 END,
			    expires_at = CASE WHEN k.expires_at <= now() THEN now() + make_interval(secs => $4) ELSE k.expires_at END
			WHERE k.expires_at <= now() OR k.status < $3
			RETURNING k.status`, scope, key, limit, window.Seconds()).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		allowed = true
		return nil
	})
	return allowed, err
}

// rate answers 429 (or 503 when the counter can't be read) and false
// when the person's window is used up.
func (d Deps) rate(w http.ResponseWriter, r *http.Request, p authz.Principal, scope string, limit int, window time.Duration) bool {
	ok, err := d.reserveUserRate(r, p, scope, limit, window)
	switch {
	case err != nil:
		d.log().Error("platform: refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return false
	case !ok:
		httpx.ErrorRetryAfter(w, r, "rate_limited", int(window.Seconds()))
		return false
	}
	return true
}

// exportIn is the create's body: exactly these fields.
type exportIn struct {
	Kind       string          `json:"kind"`
	Filters    json.RawMessage `json:"filters"`
	Reason     string          `json:"reason"`
	Note       *string         `json:"note"`
	Reference  *string         `json:"reference"`
	MFACode    string          `json:"mfa_code"`
	BackupCode string          `json:"backup_code"`
}

// eventsFiltersIn and namedFiltersIn are the two kinds' filters (mhai-web
// AuditFilters without q, and NamedAccessFilters).
type eventsFiltersIn struct {
	Hospital *string  `json:"hospital"`
	Person   *string  `json:"person"`
	Type     []string `json:"type"`
	Outcome  *string  `json:"outcome"`
	Flagged  *bool    `json:"flagged"`
	From     *string  `json:"from"`
	To       *string  `json:"to"`
}

type namedFiltersIn struct {
	Hospital  *string `json:"hospital"`
	Person    *string `json:"person"`
	Clearance *string `json:"clearance"`
	From      *string `json:"from"`
	To        *string `json:"to"`
}

func strictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errInvalid
	}
	if dec.More() {
		return errInvalid
	}
	return nil
}

func optInstant(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	return instant(*s)
}

// exportFilters parses a kind's filters into the audit list's filters.
// A named_access export names one hospital (departure 65).
func exportFilters(kind string, raw json.RawMessage) (audit.Filters, error) {
	var f audit.Filters
	if len(raw) == 0 || raw[0] != '{' {
		return f, errInvalid
	}
	var err error
	switch kind {
	case "events":
		var in eventsFiltersIn
		if err := strictJSON(raw, &in); err != nil {
			return f, err
		}
		if in.Hospital != nil {
			if *in.Hospital != "platform" && !canonicalUUID(*in.Hospital) {
				return f, errInvalid
			}
			f.Hospital = *in.Hospital
		}
		if in.Person != nil {
			if !canonicalUUID(*in.Person) {
				return f, errInvalid
			}
			f.Person = *in.Person
		}
		if in.Type != nil {
			if len(in.Type) == 0 {
				return f, errInvalid
			}
			for _, t := range in.Type {
				if !slices.Contains(audit.AuditTypes, t) {
					return f, errInvalid
				}
			}
			f.Types = in.Type
		}
		if in.Outcome != nil {
			if !slices.Contains(audit.Outcomes, *in.Outcome) {
				return f, errInvalid
			}
			f.Outcome = *in.Outcome
		}
		f.Flagged = in.Flagged
		if f.From, err = optInstant(in.From); err != nil {
			return f, err
		}
		if f.To, err = optInstant(in.To); err != nil {
			return f, err
		}
	case "named_access":
		var in namedFiltersIn
		if err := strictJSON(raw, &in); err != nil {
			return f, err
		}
		f.Named = true
		if in.Hospital == nil || !canonicalUUID(*in.Hospital) {
			return f, errInvalid
		}
		f.Hospital = *in.Hospital
		if in.Person != nil {
			if !canonicalUUID(*in.Person) {
				return f, errInvalid
			}
			f.Person = *in.Person
		}
		if in.Clearance != nil {
			if !canonicalUUID(*in.Clearance) {
				return f, errInvalid
			}
			f.Clearance = *in.Clearance
		}
		if f.From, err = optInstant(in.From); err != nil {
			return f, err
		}
		if f.To, err = optInstant(in.To); err != nil {
			return f, err
		}
	default:
		return f, errInvalid
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return f, errInvalid
	}
	return f, nil
}

// noteOK: 1-200 characters, no control or format character, nothing of
// a pseudonym's shape.
func noteOK(s string) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= 1 && n <= exportNoteMax && !pseudonymIn.MatchString(s) &&
		!strings.ContainsFunc(s, func(c rune) bool { return unicode.Is(unicode.Cc, c) || unicode.Is(unicode.Cf, c) })
}

// exportRecord is a stored record.
type exportRecord struct {
	ID, Owner    uuid.UUID
	Kind         string
	Hospital     *uuid.UUID
	Reason       string
	Note         *string
	Reference    *string
	Filters      map[string]any
	MaxSeq       int64
	Rows         int
	Checksum     string
	FileName     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	DownloadedAt *time.Time
	Live         bool // now() < expires_at, on the database's clock
}

const exportColumns = `id, owner, kind, hospital_id, reason, note, reference, filters, max_seq, rows, checksum, file_name,
	created_at, expires_at, downloaded_at, now() < expires_at`

func scanExport(row pgx.Row) (exportRecord, error) {
	var e exportRecord
	var filters []byte
	err := row.Scan(&e.ID, &e.Owner, &e.Kind, &e.Hospital, &e.Reason, &e.Note, &e.Reference, &filters, &e.MaxSeq, &e.Rows,
		&e.Checksum, &e.FileName, &e.CreatedAt, &e.ExpiresAt, &e.DownloadedAt, &e.Live)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(filters, &e.Filters); err != nil {
		return e, err
	}
	return e, nil
}

// recordedFilters turns a record's filters back into the list's filters.
func recordedFilters(e exportRecord) (audit.Filters, error) {
	raw, err := json.Marshal(e.Filters)
	if err != nil {
		return audit.Filters{}, err
	}
	return exportFilters(e.Kind, raw)
}

// buildFile builds e's file from its snapshot: the same bytes every time
// for the same record (departure 62). A named export's pids are readable
// only in the record's creation transaction or its download transaction
// (00044).
func buildFile(ctx context.Context, tx pgx.Tx, e exportRecord) ([]byte, error) {
	h := exportHeader{ID: e.ID, Kind: e.Kind, By: e.Owner, At: e.CreatedAt, Reason: e.Reason, Reference: e.Reference,
		Filters: e.Filters, Rows: e.Rows}
	f, err := recordedFilters(e)
	if err != nil {
		return nil, err
	}
	if e.Kind == "events" {
		evs, err := audit.ExportEvents(ctx, tx, f, e.MaxSeq)
		if err != nil {
			return nil, err
		}
		if len(evs) != e.Rows {
			return nil, errExportChanged
		}
		return eventsCSV(h, evs)
	}
	if e.Hospital == nil {
		return nil, errExportChanged
	}
	rows, err := tx.Query(ctx, `SELECT ref, seq, hash, ts, actor_id, clearance_id, purpose, level, start, pid
		FROM mhai_audit_export_pids($1)`, e.ID)
	if err != nil {
		return nil, err
	}
	var out []namedExportRow
	var cited []uuid.UUID
	for rows.Next() {
		var r namedExportRow
		if err := rows.Scan(&r.Ref, &r.Seq, &r.Hash, &r.At, &r.Actor, &r.Clearance, &r.Purpose, &r.Level, &r.Start, &r.Pid); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		if !slices.Contains(cited, r.Clearance) {
			cited = append(cited, r.Clearance)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != e.Rows {
		return nil, errExportChanged
	}
	copies, err := letterCopies(ctx, tx, cited)
	if err != nil {
		return nil, err
	}
	table, err := namedCSV(h, *e.Hospital, out)
	if err != nil {
		return nil, err
	}
	return namedZip(table, copies)
}

// letterCopies are the cited letters' pages: the demo's fixture pages for
// a demo letter; any other letter has no copy core holds (errNoCopy,
// departure 66).
func letterCopies(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]letterCopy, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, demo, pages FROM mhai_audit_letters($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []letterCopy
	found := 0
	for rows.Next() {
		var id uuid.UUID
		var demo bool
		var pages int
		if err := rows.Scan(&id, &demo, &pages); err != nil {
			return nil, err
		}
		found++
		if !demo || pages < 1 {
			return nil, errNoCopy
		}
		for n := 1; n <= pages; n++ {
			png := letters.FixturePage(n)
			if png == nil {
				return nil, errNoCopy
			}
			out = append(out, letterCopy{Letter: id, Page: n, PNG: png})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found != len(ids) {
		return nil, errNoCopy
	}
	return out, nil
}

// exportOut is the create's reply (mhai-web ExportOut).
type exportOut struct {
	ExportID  string `json:"export_id"`
	Rows      int    `json:"rows"`
	Capped    bool   `json:"capped"`
	Checksum  string `json:"checksum"`
	ExpiresAt string `json:"expires_at"`
	FileName  string `json:"file_name"`
}

// exportCreate is POST /platform/audit/exports. Order: off (404); the
// body (422); the person's hour (429); the code; then, in one
// transaction: the role re-read (403), the snapshot, the record, the rows
// (422 too_many_rows {rows} over 50,000: nothing kept), the file and its
// checksum, the entry last; commit; the reply.
func (d Deps) exportCreate(w http.ResponseWriter, r *http.Request) {
	if !d.ExportsOn {
		httpx.Error(w, r, "not_found")
		return
	}
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in exportIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	f, err := exportFilters(in.Kind, in.Filters)
	if err != nil || !slices.Contains(ExportReasons, in.Reason) || (in.Note != nil && !noteOK(*in.Note)) ||
		(in.Reason == "other" && in.Note == nil) ||
		(in.Reference != nil && (!exportReference.MatchString(*in.Reference) || pseudonymIn.MatchString(*in.Reference))) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	if !d.rate(w, r, p, "platform.audit.export.v1", ExportsPerHour, time.Hour) || !d.stepUp(w, r, in.MFACode, in.BackupCode) {
		return
	}
	ctx := r.Context()
	var out exportOut
	err = d.tx(ctx, func(tx pgx.Tx) error {
		if err := stillHolds(ctx, tx, p, TeamRoles...); err != nil {
			return err
		}
		maxSeq, err := audit.Snapshot(ctx, tx)
		if err != nil {
			return err
		}
		e := exportRecord{ID: uuid.New(), Owner: p.UserID, Kind: in.Kind, Reason: in.Reason, Note: in.Note,
			Reference: in.Reference, Filters: f.Recorded(), MaxSeq: maxSeq}
		if f.Named {
			h := uuid.MustParse(f.Hospital)
			e.Hospital = &h
		}
		filters, err := json.Marshal(e.Filters)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO audit_exports (id, kind, owner, hospital_id, reason, note, reference, filters,
				max_seq, rows, checksum, file_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, 0, $10, 'pending')
			RETURNING created_at, expires_at`, e.ID, e.Kind, e.Owner, e.Hospital, e.Reason, e.Note, e.Reference, string(filters),
			e.MaxSeq, strings.Repeat("0", 64)).Scan(&e.CreatedAt, &e.ExpiresAt); err != nil {
			return err
		}
		if f.Named {
			// The database takes the rows from the record's own filters.
			if err := tx.QueryRow(ctx, `SELECT mhai_audit_export_named($1, $2)`, e.ID, audit.ExportMax).Scan(&e.Rows); err != nil {
				return err
			}
		} else if e.Rows, err = audit.CountExport(ctx, tx, f, maxSeq); err != nil {
			return err
		}
		if e.Rows > audit.ExportMax {
			return tooManyRows{rows: e.Rows}
		}
		entry := func(payload map[string]any) error {
			action := "audit.exported"
			if f.Named {
				action = "audit.named_access_exported"
			}
			return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: httpx.RequestIDFrom(ctx),
				Action: action, Entity: "audit_export", EntityID: e.ID.String(), Payload: payload})
		}
		// A named export's entry comes before its pids are read (security
		// review): 00044 reads no pid in a transaction without it. The
		// chain lock is then held while the file is built.
		if f.Named {
			if err := entry(map[string]any{"rows": e.Rows, "capped": false, "filters": e.Filters}); err != nil {
				return err
			}
		}
		file, err := buildFile(ctx, tx, e)
		if err != nil {
			return err
		}
		e.Checksum, e.FileName = checksum(file), exportFileName(e.Kind, e.ID, e.CreatedAt)
		if _, err := tx.Exec(ctx, `UPDATE audit_exports SET rows = $2, checksum = $3, file_name = $4 WHERE id = $1`,
			e.ID, e.Rows, e.Checksum, e.FileName); err != nil {
			return err
		}
		// An events export's entry last, with its checksum: the chain
		// lock is held until commit. No entry, no record.
		if !f.Named {
			if err := entry(map[string]any{"rows": e.Rows, "capped": false, "checksum": e.Checksum, "filters": e.Filters}); err != nil {
				return err
			}
		}
		out = exportOut{ExportID: e.ID.String(), Rows: e.Rows, Checksum: e.Checksum,
			ExpiresAt: e.ExpiresAt.UTC().Format(time.RFC3339Nano), FileName: e.FileName}
		return nil
	})
	var many tooManyRows
	switch {
	case err == nil:
		httpx.JSON(w, http.StatusOK, out)
	case errors.As(err, &many):
		httpx.ErrorRows(w, r, many.rows)
	case errors.Is(err, errNotReader):
		httpx.Error(w, r, "forbidden")
	case errors.Is(err, errNoCopy):
		httpx.Error(w, r, "unavailable")
	case pgError(err) != nil && pgError(err).Code == "23503":
		// A hospital that doesn't exist.
		httpx.Error(w, r, "validation_failed")
	default:
		d.fail(w, r, p, "export")
	}
}

// exportFile is GET /platform/audit/exports/{id}/file: in one
// transaction, the record locked; not the owner, 403 and
// export_download_refused {not_owner}; spent or expired, 410 and
// {gone}; otherwise marked downloaded, the file rebuilt and checked
// against its checksum (503, not spent, when it differs), and
// export_downloaded; commit; then the file.
func (d Deps) exportFile(w http.ResponseWriter, r *http.Request) {
	if !d.ExportsOn {
		httpx.Error(w, r, "not_found")
		return
	}
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || id.String() != r.PathValue("id") || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	if !d.rate(w, r, p, "platform.audit.download.v1", DownloadsPerMinute, time.Minute) {
		return
	}
	ctx := r.Context()
	var refused string
	var file []byte
	var kind string
	err = d.tx(ctx, func(tx pgx.Tx) error {
		if err := stillHolds(ctx, tx, p, TeamRoles...); err != nil {
			return err
		}
		e, err := scanExport(tx.QueryRow(ctx, `SELECT `+exportColumns+` FROM audit_exports WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoExport
		}
		if err != nil {
			return err
		}
		entry := func(action string, payload map[string]any) error {
			return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: httpx.RequestIDFrom(ctx),
				Action: action, Entity: "audit_export", EntityID: e.ID.String(), Payload: payload})
		}
		switch {
		case e.Owner != p.UserID:
			refused = "not_owner"
		case e.DownloadedAt != nil || !e.Live:
			refused = "gone"
		}
		if refused != "" {
			return entry("audit.export_download_refused", map[string]any{"reason": refused})
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_exports SET downloaded_at = now(), download_xact = pg_current_xact_id()
			WHERE id = $1`, e.ID); err != nil {
			return err
		}
		// The entry before the file is built (a named file's pids are read
		// only once it is in this transaction); a file that differs rolls
		// both back.
		if err := entry("audit.export_downloaded", map[string]any{}); err != nil {
			return err
		}
		if file, err = buildFile(ctx, tx, e); err != nil {
			return err
		}
		if checksum(file) != e.Checksum {
			return errExportChanged
		}
		kind = e.Kind
		return nil
	})
	switch {
	case err == nil && refused == "not_owner":
		httpx.Error(w, r, "forbidden")
	case err == nil && refused == "gone":
		httpx.Error(w, r, "gone")
	case err == nil:
		h := w.Header()
		h.Set("Content-Type", "text/csv; charset=utf-8")
		if kind == "named_access" {
			h.Set("Content-Type", "application/zip")
		}
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(file)
	case errors.Is(err, errNoExport):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errNotReader):
		httpx.Error(w, r, "forbidden")
	case errors.Is(err, errExportChanged), errors.Is(err, errNoCopy):
		// Not spent (the transaction rolled back), but recorded in its own
		// (security review).
		rerr := d.tx(context.WithoutCancel(ctx), func(tx pgx.Tx) error {
			return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: httpx.RequestIDFrom(ctx),
				Action: "audit.export_download_refused", Entity: "audit_export", EntityID: id.String(),
				Payload: map[string]any{"reason": "unavailable"}})
		})
		d.log().Error("platform: export file not sent", "request_id", httpx.RequestIDFrom(ctx), "user_id", p.UserID.String(),
			"export_id", id.String(), "reason", "unavailable", "recorded", rerr == nil)
		httpx.Error(w, r, "unavailable")
	default:
		d.fail(w, r, p, "export file")
	}
}

// ExportRecordOut is one record as the list sends it (mhai-web
// ExportRecord).
type ExportRecordOut struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	By          Person         `json:"by"`
	At          string         `json:"at"`
	Reason      string         `json:"reason"`
	Note        *string        `json:"note"`
	Reference   *string        `json:"reference"`
	Filters     map[string]any `json:"filters"`
	FilterNames struct {
		Hospital  *string `json:"hospital"`
		Person    *string `json:"person"`
		Clearance *string `json:"clearance"`
	} `json:"filter_names"`
	Rows         int     `json:"rows"`
	Capped       bool    `json:"capped"`
	Checksum     string  `json:"checksum"`
	ExpiresAt    string  `json:"expires_at"`
	DownloadedAt *string `json:"downloaded_at"`
}

// exportList is GET /platform/audit/exports?cursor: the records, newest
// first, 50 a page; recorded as audit.searched {searched_kind: export}
// (departure 70). The cursor is the last record's id.
func (d Deps) exportList(w http.ResponseWriter, r *http.Request) {
	if !d.ExportsOn {
		httpx.Error(w, r, "not_found")
		return
	}
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"cursor"})
	var after *uuid.UUID
	if c, has := v["cursor"]; err == nil && has {
		if !canonicalUUID(c) {
			err = errInvalid
		} else {
			x := uuid.MustParse(c)
			after = &x
		}
	}
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	out := struct {
		Exports    []ExportRecordOut `json:"exports"`
		NextCursor *string           `json:"next_cursor"`
	}{Exports: []ExportRecordOut{}}
	err = d.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e.id, e.kind, e.owner, u.display_name, e.created_at, e.reason, e.note, e.reference,
				e.filters, e.rows, e.checksum, e.expires_at, e.downloaded_at,
				(SELECT h.name FROM hospitals h WHERE h.id::text = e.filters ->> 'hospital'),
				(SELECT x.display_name FROM users x WHERE x.id::text = e.filters ->> 'person')
			FROM audit_exports e JOIN users u ON u.id = e.owner
			WHERE $1::uuid IS NULL OR (e.created_at, e.id) < (SELECT c.created_at, c.id FROM audit_exports c WHERE c.id = $1)
			ORDER BY e.created_at DESC, e.id DESC LIMIT $2`, after, ExportsPage+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o ExportRecordOut
			var owner uuid.UUID
			var name string
			var at, expires time.Time
			var downloaded *time.Time
			var filters []byte
			var hname, pname *string
			if err := rows.Scan(&o.ID, &o.Kind, &owner, &name, &at, &o.Reason, &o.Note, &o.Reference, &filters, &o.Rows,
				&o.Checksum, &expires, &downloaded, &hname, &pname); err != nil {
				return err
			}
			if err := json.Unmarshal(filters, &o.Filters); err != nil {
				return err
			}
			o.By = Person{ID: owner.String(), Name: name}
			if !audit.ShowableName(name) {
				o.By.Name = "Unknown"
			}
			o.At, o.ExpiresAt = at.UTC().Format(time.RFC3339Nano), expires.UTC().Format(time.RFC3339Nano)
			if downloaded != nil {
				s := downloaded.UTC().Format(time.RFC3339Nano)
				o.DownloadedAt = &s
			}
			if hname != nil && audit.ShowableName(*hname) {
				o.FilterNames.Hospital = hname
			}
			if pname != nil && audit.ShowableName(*pname) {
				o.FilterNames.Person = pname
			}
			out.Exports = append(out.Exports, o)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = audit.RecordExportsRead(ctx, tx, reader(r, p))
		return err
	})
	if err != nil {
		d.fail(w, r, p, "exports")
		return
	}
	if len(out.Exports) > ExportsPage {
		out.Exports = out.Exports[:ExportsPage]
		c := out.Exports[ExportsPage-1].ID
		out.NextCursor = &c
	}
	httpx.JSON(w, http.StatusOK, out)
}

// namedAccess is GET /platform/audit/named-access?hospital&person&
// clearance&from&to&cursor: the named-view entries, newest first, each
// with its letter (departure 69). Every page is recorded (audit.viewed
// {list: named_access}, deduplicated as the events list), and the cursor
// is bound to the session and the filters.
func (d Deps) namedAccess(w http.ResponseWriter, r *http.Request) {
	if !d.ExportsOn {
		httpx.Error(w, r, "not_found")
		return
	}
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"hospital", "person", "clearance", "from", "to", "cursor"})
	f := audit.Filters{Named: true}
	if err == nil {
		for k, dst := range map[string]*string{"hospital": &f.Hospital, "person": &f.Person, "clearance": &f.Clearance} {
			if s, has := v[k]; has {
				if !canonicalUUID(s) {
					err = errInvalid
				}
				*dst = s
			}
		}
	}
	if err == nil {
		if s, has := v["from"]; has {
			f.From, err = instant(s)
		}
	}
	if err == nil {
		if s, has := v["to"]; has {
			f.To, err = instant(s)
		}
	}
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	key, err := audit.CursorKey(d.MasterKey)
	if err != nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	cur := audit.Cursors{Key: key, Session: p.SessionID}
	var after int64
	if c, has := v["cursor"]; has {
		if after, err = cur.Decode(c, f); err != nil {
			httpx.Error(w, r, "validation_failed")
			return
		}
	}
	var page audit.NamedPage
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if page, err = audit.ListNamed(r.Context(), tx, cur, f, after, audit.DefaultLimit); err != nil {
			return err
		}
		_, err = audit.RecordRead(r.Context(), tx, reader(r, p), f)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "named access")
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}
