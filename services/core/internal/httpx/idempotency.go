package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
)

// Principal is who a request acts for. The token middleware sets it with
// WithPrincipal once the access token is verified; Idempotent scopes keys
// by it. HospitalID is uuid.Nil for platform accounts.
type Principal struct {
	UserID     string
	HospitalID uuid.UUID
}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the request's principal, if one was set.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok && p.UserID != ""
}

const (
	// maxKeyLen is the longest Idempotency-Key accepted.
	maxKeyLen = 255
	// maxStoredBody caps the response kept for replay; a larger response
	// is passed through and not stored.
	maxStoredBody = 1 << 20
)

type idemState struct{ noStore bool }

// NoStore marks a route whose responses must never be kept for replay:
// every route that issues a token (sign-in, refresh, two-step challenge).
// Behind Idempotent, its responses are passed through and a retry runs
// the handler again.
func NoStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, ok := r.Context().Value(idemStateKey).(*idemState); ok {
			s.noStore = true
		}
		h.ServeHTTP(w, r)
	})
}

// Idempotent honours Idempotency-Key on POST, PUT, PATCH and DELETE. The
// first request with a key runs and, if it succeeds (2xx), its status and
// body are kept for 72 hours; a retry with the same key, scope, method,
// path and body gets the stored reply without running the handler. The
// same key with a different request, or while the first is still running,
// gets 409 conflict.
//
// The scope is the principal's user id (WithPrincipal); never anything the
// client sends. Idempotent sits inside the principal middleware: a request
// that reaches it without a principal is a wiring fault and gets 500
// internal_error without running. Public routes (sign-in) use
// IdempotentAnon instead. Rows are written as mhai_gateway under the
// principal's hospital, or with no hospital as mhai_platform (platform
// principal).
//
// What happens to the key after the handler:
//   - NoStore route, Cache-Control: no-store, or a non-2xx reply: released,
//     so a retry runs again (token routes are safe to repeat).
//   - 2xx whose body has a credential field (credentialKeys), isn't JSON,
//     or is over 1 MiB: kept as a tombstone, so a retry gets 409 conflict
//     and the handler never runs twice. The body is not stored.
//   - Otherwise the status and body are stored and replayed (as
//     application/json); idempotent routes must not rely on other headers.
func Idempotent(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return IdempotentLog(pool, slog.Default())
}

// IdempotentLog is Idempotent with the logger that records replies which
// could not be stored.
func IdempotentLog(pool *pgxpool.Pool, log *slog.Logger) func(http.Handler) http.Handler {
	return idempotent(pool, log, principalScope)
}

// IdempotentAnon is Idempotent for public routes (sign-in, refresh, the
// two-step challenge): the scope is anon:<client ip> from the BFF, whatever
// else is in the context. A request without a trusted client IP runs
// without idempotency. Rows are written as mhai_auth with no hospital.
func IdempotentAnon(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return IdempotentAnonLog(pool, slog.Default())
}

// IdempotentAnonLog is IdempotentAnon with a logger.
func IdempotentAnonLog(pool *pgxpool.Pool, log *slog.Logger) func(http.Handler) http.Handler {
	return idempotent(pool, log, anonScope)
}

// scopeFunc picks the scope; ok false runs without idempotency, err
// refuses the request.
type scopeFunc func(context.Context) (sc scope, ok bool, err error)

var errNoPrincipal = errors.New("idempotency: no principal")

func idempotent(pool *pgxpool.Pool, log *slog.Logger, scopeOf scopeFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				next.ServeHTTP(w, r)
				return
			}
			keys := r.Header.Values("Idempotency-Key")
			if len(keys) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if len(keys) != 1 || !validIdemKey(keys[0]) {
				Error(w, r, "validation_failed")
				return
			}
			sc, ok, err := scopeOf(r.Context())
			if err != nil {
				log.Error("httpx: idempotent route reached without a principal", "request_id", RequestIDFrom(r.Context()))
				Error(w, r, "internal_error")
				return
			}
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
			if err != nil || len(body) > MaxBody {
				Error(w, r, "validation_failed")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			st := &store{pool: pool, log: log, sc: sc, key: keys[0], hash: requestHash(r, body)}
			st.serve(w, r, next)
		})
	}
}

func validIdemKey(k string) bool {
	if k == "" || len(k) > maxKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

type scope struct {
	name     string
	hospital uuid.UUID
	run      func(context.Context, *pgxpool.Pool, func(pgx.Tx) error) error
}

func principalScope(ctx context.Context) (scope, bool, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return scope{}, false, errNoPrincipal
	}
	if p.HospitalID != uuid.Nil {
		h := p.HospitalID
		return scope{name: p.UserID, hospital: h, run: func(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
			return db.WithHospital(ctx, pool, h, fn)
		}}, true, nil
	}
	return scope{name: p.UserID, run: db.WithPlatform}, true, nil
}

func anonScope(ctx context.Context) (scope, bool, error) {
	ip := ClientIP(ctx)
	if !ip.IsValid() {
		return scope{}, false, nil
	}
	return scope{name: "anon:" + ip.String(), run: db.WithAuth}, true, nil
}

// requestHash binds a key to one request: method, path, query and body.
func requestHash(r *http.Request, body []byte) string {
	h := sha256.New()
	for _, part := range []string{r.Method, r.URL.Path, r.URL.RawQuery} {
		_, _ = io.WriteString(h, strconv.Itoa(len(part))+":"+part+"\n")
	}
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

type store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	sc   scope
	key  string
	hash string
}

var errConflict = errors.New("idempotency conflict")

type stored struct {
	status int
	body   []byte
}

func (s *store) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx := r.Context()
	prior, err := s.reserve(ctx)
	switch {
	case errors.Is(err, errConflict):
		Error(w, r, "conflict")
		return
	case err != nil:
		Error(w, r, "internal_error")
		return
	case prior != nil:
		w.Header().Set("Idempotent-Replayed", "true")
		if len(prior.body) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		w.WriteHeader(prior.status)
		_, _ = w.Write(prior.body)
		return
	}

	// The key is reserved: finish it whatever the handler does, panics
	// included, on a context that outlives a cancelled request.
	state := &idemState{}
	rec := &recorder{ResponseWriter: w}
	finished := false
	defer func() {
		if !finished {
			s.release(context.WithoutCancel(ctx))
		}
	}()
	next.ServeHTTP(rec, r.WithContext(context.WithValue(ctx, idemStateKey, state)))
	finished = true

	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	cctx := context.WithoutCancel(ctx)
	switch {
	case state.noStore || hasNoStore(w.Header().Values("Cache-Control")) || status < 200 || status >= 300:
		// Safe to repeat (token routes) or failed: free the key for a retry.
		s.release(cctx)
	case rec.overflow || !storable(rec.buf.Bytes()):
		// Succeeded, but the reply can't be kept (a credential, not JSON,
		// too large). Leave the reservation as a tombstone: a retry gets
		// 409 conflict and the handler never runs twice.
	default:
		if err := s.finish(cctx, status, rec.buf.Bytes()); err != nil {
			// The key stays reserved (409 until it expires). Log only a
			// hash of the scope and the request id, never the driver error.
			sum := sha256.Sum256([]byte(s.sc.name))
			s.log.Error("idempotency: could not store the reply; key left reserved",
				"scope_hash", hex.EncodeToString(sum[:8]), "request_id", RequestIDFrom(ctx))
		}
	}
}

// credentialKeys are the fields a stored reply must never hold, normalised
// (lowercase, "-" and "_" removed). Narrower than the log redaction list:
// email and code are not credentials, and stored bodies are under RLS and
// expire.
var credentialKeys = map[string]bool{
	"password": true, "secret": true, "token": true, "accesstoken": true,
	"refreshtoken": true, "mfatoken": true, "devicetoken": true, "mfacode": true,
	"backupcode": true, "backupcodes": true, "recoverycodes": true, "otp": true,
	"totp": true, "authorization": true, "cookie": true, "setcookie": true,
	"apikey": true,
}

func isCredentialKey(k string) bool {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "-", "")
	k = strings.ReplaceAll(k, "_", "")
	return credentialKeys[k]
}

// storable reports whether a reply body may be kept: empty, or JSON with no
// credential field at any depth.
func storable(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	_, bad := logx.KeyInJSON(body, isCredentialKey)
	return !bad
}

// reserve returns the stored reply for a completed request, or nil after
// reserving the key for this one. errConflict means the key belongs to a
// different request, or to one still running.
func (s *store) reserve(ctx context.Context) (*stored, error) {
	var prior *stored
	var hospital any
	if s.sc.hospital != uuid.Nil {
		hospital = s.sc.hospital
	}
	err := s.sc.run(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM idempotency_keys WHERE scope = $1 AND key = $2 AND expires_at <= now()`,
			s.sc.name, s.key); err != nil {
			return err
		}
		var (
			hash   string
			status *int32
			body   []byte
		)
		err := tx.QueryRow(ctx,
			`SELECT request_hash, status, body FROM idempotency_keys WHERE scope = $1 AND key = $2`,
			s.sc.name, s.key).Scan(&hash, &status, &body)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx,
				`INSERT INTO idempotency_keys (scope, key, hospital_id, request_hash, expires_at)
				 VALUES ($1, $2, $3, $4, now() + interval '72 hours')`,
				s.sc.name, s.key, hospital, s.hash)
			return err
		case err != nil:
			return err
		case hash != s.hash || status == nil:
			return errConflict
		default:
			prior = &stored{status: int(*status), body: body}
			return nil
		}
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, errConflict
	}
	return prior, err
}

// finish keeps the reply. On error the reservation stays: the handler has
// already succeeded, so a retry must not run it again.
func (s *store) finish(ctx context.Context, status int, body []byte) error {
	return s.sc.run(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE idempotency_keys SET status = $3, body = $4 WHERE scope = $1 AND key = $2 AND status IS NULL`,
			s.sc.name, s.key, status, body)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("idempotency: reservation missing")
		}
		return err
	})
}

func (s *store) release(ctx context.Context) {
	_ = s.sc.run(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM idempotency_keys WHERE scope = $1 AND key = $2 AND status IS NULL`,
			s.sc.name, s.key)
		return err
	})
}

func hasNoStore(vals []string) bool {
	for _, v := range vals {
		for _, d := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(d), "no-store") {
				return true
			}
		}
	}
	return false
}

// recorder passes the response through and keeps a copy of the body, up to
// maxStoredBody.
type recorder struct {
	http.ResponseWriter
	status   int
	buf      bytes.Buffer
	overflow bool
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if !r.overflow {
		if r.buf.Len()+len(b) > maxStoredBody {
			r.overflow = true
			r.buf.Reset()
		} else {
			r.buf.Write(b)
		}
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
