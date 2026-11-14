// Package platform serves the platform app's routes (Plan 4 spec §5, §6).
// This file: the audit log reads, GET /platform/audit, /platform/audit/{id},
// /platform/audit/hospitals and /platform/audit/people (spec §5.2, §5.6;
// contract §14.9).
//
// Every route is for platform admins and auditors on the platform
// audience, with two-step always; each runs in authz.Tx, which for a
// platform principal is db.WithPlatform (mhai_platform: no clinical
// table). Entries leave only through the audit catalogue (audit.Map, or
// the bare entry). A drill-down entry's pids leave only through the
// recorded reveal, /{id}/patients (audit_patients.go). The deferred routes
// (/named-access, /exports*) are not registered: the router answers 404
// not_found.
package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/emrctl"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
)

// Deps are what the routes need.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// MasterKey is MFA_ENCRYPTION_KEY (Config.MFAKey); the audit cursor's
	// HMAC key is derived from it (audit.CursorKey). Without a usable one
	// the list fails closed (503 not_ready). Never logged.
	MasterKey []byte
	// SecurityContact is named by the integrity status; unset, the status
	// answers 503 not_ready.
	SecurityContact SecurityContact
	// Walk runs a started chain check's walk (nil: in the background, with
	// the 10-minute budget). Tests set it to hold runs running.
	Walk func(runID uuid.UUID)
	// Health is the health bucket (bus.OpenHealthBucket): GET
	// /platform/health reads it and the sampler writes it. Nil: the route
	// answers 503 unavailable and the sampler doesn't run.
	Health jetstream.KeyValue
	// Intel calls pipeline.health for the sampler (as svc:core).
	Intel *intel.Client
	// Now is the clock the health route judges a reading's age by and the
	// sampler dates it with; nil is time.Now.
	Now func() time.Time
	// Auth checks the step-up code of a job cancel (auth.Service.StepUp).
	// Nil: the cancel answers 503 not_ready.
	Auth *auth.Service
	// QueryCap is the questions in flight per hospital
	// (INTELLIGENCE_QUERY_CAP): a retried question takes a slot like a new
	// one. Under 1 (or no Intel): the retry answers 503 not_ready.
	QueryCap int
	// CloudProviders are the cloud model providers the deployment enables
	// (CORE_CLOUD_PROVIDERS): a policy may name only one of these (409
	// otherwise). Empty: none.
	CloudProviders []string
	// EMREnabled is CORE_EMR_ENABLED: off, the connection writes answer
	// 503 not_ready and the reads answer empty.
	EMREnabled bool
	// EMRTypes are the offered connector types (CORE_EMR_CONNECTOR_TYPES).
	EMRTypes []string
	// EMR calls the emr service's control ops; EMRSealer seals credentials
	// to its public key. Either nil: the writes answer 503 not_ready.
	EMR       *emrctl.Client
	EMRSealer *emrctl.Sealer
	// EMRDevelopment allows loopback IP-literal addresses (test servers), as the
	// emr service does in development; it must match the emr service's
	// APP_ENV so both derive the same credential origin.
	EMRDevelopment bool
	// AfterFollowUpAudit, when set, runs after a follow-up write's audit row
	// and before commit; its error rolls the write and the row back. Tests
	// only (A21); server.Register never sets it.
	AfterFollowUpAudit func() error
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Register adds the audit log reads, the chain check and the auditors'
// writes (verify.go), system health (health.go), the failed jobs (jobs.go),
// the guardrail statistics (guardrails.go), the analytics reads
// (analytics.go, analytics_series.go) and follow-ups
// (analytics_followup.go), the hospitals' cloud model
// policies (modelpolicy.go) and the EMR connections (emrconnections.go).
func Register(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/platform/audit", read, http.HandlerFunc(d.list))
	authz.Route(m, http.MethodGet, "/platform/audit/hospitals", read, http.HandlerFunc(d.hospitals))
	authz.Route(m, http.MethodGet, "/platform/audit/people", read, http.HandlerFunc(d.people))
	authz.Route(m, http.MethodGet, "/platform/audit/{id}", read, http.HandlerFunc(d.detail))
	authz.Route(m, http.MethodGet, "/platform/audit/{id}/patients", read, http.HandlerFunc(d.auditPatients))
	registerVerify(m, d)
	registerHealth(m, d)
	registerJobs(m, d)
	registerGuardrails(m, d)
	registerAnalytics(m, d)
	registerFollowUps(m, d)
	registerModelPolicy(m, d)
	registerEMRConnections(m, d)
	registerEMRRuleSet(m, d)
}

// caller is the verified platform principal, or a 500 (the guard admits
// only platform accounts here).
func caller(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, ok := authz.FromContext(r.Context())
	if !ok || !p.Platform || p.HospitalID != uuid.Nil || p.SessionID == "" {
		httpx.Error(w, r, "internal_error")
		return p, false
	}
	return p, true
}

// fail logs by ids and the code only (a driver error can carry row
// values) and answers internal_error.
func (d Deps) fail(w http.ResponseWriter, r *http.Request, p authz.Principal, what string) {
	d.log().Error("platform: audit "+what+" failed", "request_id", httpx.RequestIDFrom(r.Context()),
		"user_id", p.UserID.String(), "reason", "internal_error")
	httpx.Error(w, r, "internal_error")
}

// reader is who reads, from the verified principal only.
func reader(r *http.Request, p authz.Principal) audit.Reader {
	return audit.Reader{UserID: p.UserID.String(), SessionID: p.SessionID, RequestID: httpx.RequestIDFrom(r.Context())}
}

func (d Deps) tx(ctx context.Context, fn func(pgx.Tx) error) error { return authz.Tx(ctx, d.Pool, fn) }

var (
	// requestIDPattern is a searched request id (spec §5.2).
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	// pseudonymIn is mhai-web's PSEUDONYM_IN: a pseudonym anywhere.
	pseudonymIn = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)
	listKeys    = []string{"hospital", "person", "type", "outcome", "flagged", "from", "to", "q", "cursor", "limit"}
)

var errInvalid = errors.New("platform: invalid query")

// params parses the query: every key known, each at most once and never
// empty.
func params(r *http.Request, known []string) (map[string]string, error) {
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errInvalid
	}
	out := map[string]string{}
	for k, v := range vals {
		if !slices.Contains(known, k) || len(v) != 1 || v[0] == "" {
			return nil, errInvalid
		}
		out[k] = v[0]
	}
	return out, nil
}

func canonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

func instant(s string) (*time.Time, error) {
	if !audit.Instant(s) {
		return nil, errInvalid
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, errInvalid
	}
	return &t, nil
}

// listQuery parses the list's query into its filters, cursor and limit.
// A q that is neither an event id nor a request id, or that has the
// pseudonym's shape (no pseudonym search until Plan 6), is refused.
func listQuery(r *http.Request) (f audit.Filters, cursor string, limit int, err error) {
	v, err := params(r, listKeys)
	if err != nil {
		return f, "", 0, err
	}
	if h, ok := v["hospital"]; ok {
		if h != "platform" && !canonicalUUID(h) {
			return f, "", 0, errInvalid
		}
		f.Hospital = h
	}
	if p, ok := v["person"]; ok {
		if !canonicalUUID(p) {
			return f, "", 0, errInvalid
		}
		f.Person = p
	}
	if t, ok := v["type"]; ok {
		for _, x := range strings.Split(t, ",") {
			if !slices.Contains(audit.AuditTypes, x) {
				return f, "", 0, errInvalid
			}
			f.Types = append(f.Types, x)
		}
	}
	if o, ok := v["outcome"]; ok {
		if !slices.Contains(audit.Outcomes, o) {
			return f, "", 0, errInvalid
		}
		f.Outcome = o
	}
	if fl, ok := v["flagged"]; ok {
		b, perr := strconv.ParseBool(fl)
		if perr != nil || (fl != "true" && fl != "false") {
			return f, "", 0, errInvalid
		}
		f.Flagged = &b
	}
	if s, ok := v["from"]; ok {
		if f.From, err = instant(s); err != nil {
			return f, "", 0, err
		}
	}
	if s, ok := v["to"]; ok {
		if f.To, err = instant(s); err != nil {
			return f, "", 0, err
		}
	}
	if q, ok := v["q"]; ok {
		_, isEvent := audit.EventID(q)
		if (!isEvent && !requestIDPattern.MatchString(q)) || pseudonymIn.MatchString(q) {
			return f, "", 0, errInvalid
		}
		f.Q = q
	}
	limit = audit.DefaultLimit
	if l, ok := v["limit"]; ok {
		n, perr := strconv.Atoi(l)
		if perr != nil || strconv.Itoa(n) != l || n < 1 || n > audit.MaxLimit {
			return f, "", 0, errInvalid
		}
		limit = n
	}
	return f, v["cursor"], limit, nil
}

// list is GET /platform/audit: a page of entries. Every page is recorded,
// once per session and filters per 5 minutes: audit.viewed, or
// audit.searched for q. The cursor is HMAC-bound to the session and the
// filters.
func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	f, cursor, limit, err := listQuery(r)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	key, err := audit.CursorKey(d.MasterKey)
	if err != nil {
		d.log().Error("platform: audit list refused: no cursor key", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	cur := audit.Cursors{Key: key, Session: p.SessionID}
	var after int64
	if cursor != "" {
		if after, err = cur.Decode(cursor, f); err != nil {
			httpx.Error(w, r, "validation_failed")
			return
		}
	}
	var page audit.Page
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if page, err = audit.ListEvents(r.Context(), tx, cur, f, after, limit); err != nil {
			return err
		}
		// Every page, cursor or not: the 5-minute dedupe collapses a page
		// load into one row, and a cursor never decides whether a read
		// is recorded. Last in the transaction: the chain lock is held
		// until commit.
		_, err = audit.RecordRead(r.Context(), tx, reader(r, p), f)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "list")
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

// detail is GET /platform/audit/{id}. The read is recorded as the exact-id
// search for that id (audit.searched, searched_kind event), in the same
// transaction: no entry is shown unrecorded.
func (d Deps) detail(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	seq, ok := audit.EventID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var out audit.Detail
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = audit.GetEvent(r.Context(), tx, seq); err != nil {
			return err
		}
		_, err = audit.RecordRead(r.Context(), tx, reader(r, p), audit.Filters{Q: strconv.FormatInt(seq, 10)})
		return err
	})
	switch {
	case errors.Is(err, audit.ErrNotFound):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "detail")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}

// hospitals is GET /platform/audit/hospitals, recorded as audit.searched
// {searched_kind: hospital}.
func (d Deps) hospitals(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if _, err := params(r, nil); err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out []audit.HospitalRef
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = audit.Hospitals(r.Context(), tx); err != nil {
			return err
		}
		_, err = audit.RecordHospitalsRead(r.Context(), tx, reader(r, p))
		return err
	})
	if err != nil {
		d.fail(w, r, p, "hospitals")
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		Hospitals []audit.HospitalRef `json:"hospitals"`
	}{out})
}

// peopleQueryMax bounds the people search text.
const peopleQueryMax = 100

// people is GET /platform/audit/people?q: q of 2 to 100 characters once
// trimmed, with no control or format character. Recorded as audit.searched
// {searched_kind: person}, never the text.
func (d Deps) people(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"q"})
	q := strings.TrimSpace(v["q"])
	if err != nil || !utf8.ValidString(q) || utf8.RuneCountInString(q) < 2 || utf8.RuneCountInString(q) > peopleQueryMax ||
		strings.ContainsFunc(q, func(c rune) bool { return unicode.Is(unicode.Cc, c) || unicode.Is(unicode.Cf, c) }) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out []audit.PersonOut
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = audit.SearchPeople(r.Context(), tx, q); err != nil {
			return err
		}
		_, err = audit.RecordPeopleSearch(r.Context(), tx, reader(r, p), q)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "people")
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		People []audit.PersonOut `json:"people"`
	}{out})
}
