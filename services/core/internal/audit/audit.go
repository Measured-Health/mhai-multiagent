// Package audit appends to, and verifies, the tamper-evident audit chain
// (design §7.7).
//
// The chain is global across hospitals: each row's hash is
// sha256(prev_hash || canonical JSON of the row), where prev_hash is the
// hash of the row before it in id order. Appends are serialised with a
// transaction-level advisory lock, and the previous hash comes from the
// SECURITY DEFINER function audit_chain_head(), because the gateway can see
// only its own hospital's rows.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
)

// Event is one audit entry. ActorID and HospitalID are uuids, or "" for
// none. Payload must never hold PHI, credentials or email addresses.
type Event struct {
	ActorID, HospitalID, RequestID, Action, Entity, EntityID string
	Payload                                                  map[string]any
}

// chainLock is the pg_advisory_xact_lock key that serialises appends
// ("mhaiaudt").
const chainLock int64 = 0x6d68616961756474

// genesis is prev_hash for the first row.
var genesis = strings.Repeat("0", 64)

// ErrIsolation is returned when Write runs in a transaction above READ
// COMMITTED: its snapshot could predate the chain head, forking the chain.
var ErrIsolation = errors.New("audit: Write needs a READ COMMITTED transaction")

// ErrSensitiveKey is returned when the payload has a field the log
// redaction list treats as sensitive (email, token, code, password, ...),
// at any depth. Nothing is written.
var ErrSensitiveKey = errors.New("audit: payload has a sensitive key")

const tsLayout = "2006-01-02T15:04:05.000000Z07:00"

// record is a row as hashed: the same fields whether built by Write or read
// back by Verify.
type record struct {
	ts                                                       time.Time
	actorID, hospitalID, requestID, action, entity, entityID string
	payload                                                  []byte // JSON as Postgres returns it
}

// hash is hex(sha256(prev || canonical JSON)). The payload is decoded and
// re-encoded with sorted keys and numbers kept as written, so jsonb's key
// order and spacing don't matter.
func (r record) hash(prev string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(r.payload))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return "", fmt.Errorf("audit: payload: %w", err)
	}
	opt := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// A map encodes with sorted keys, at every depth.
	if err := enc.Encode(map[string]any{
		"ts":          r.ts.UTC().Format(tsLayout),
		"actor_id":    opt(r.actorID),
		"hospital_id": opt(r.hospitalID),
		"request_id":  opt(r.requestID),
		"action":      r.action,
		"entity":      opt(r.entity),
		"entity_id":   opt(r.entityID),
		"payload":     payload,
	}); err != nil {
		return "", fmt.Errorf("audit: encode: %w", err)
	}
	sum := sha256.Sum256(append([]byte(prev), buf.Bytes()...))
	return hex.EncodeToString(sum[:]), nil
}

func normUUID(field, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return "", fmt.Errorf("audit: %s is not a uuid", field)
	}
	return u.String(), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Write appends e to the chain inside tx, which must be a READ COMMITTED
// transaction under an app role allowed to insert this row (mhai_gateway
// for its own hospital, mhai_auth, mhai_platform). The advisory lock is
// held until tx ends, so commit promptly.
func Write(ctx context.Context, tx pgx.Tx, e Event) error {
	if e.Action == "" {
		return errors.New("audit: event has no action")
	}
	actor, err := normUUID("actor id", e.ActorID)
	if err != nil {
		return err
	}
	hospital, err := normUUID("hospital id", e.HospitalID)
	if err != nil {
		return err
	}
	var iso string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
		return fmt.Errorf("audit: isolation: %w", err)
	}
	if iso != "read committed" {
		return ErrIsolation
	}
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return errors.New("audit: payload is not JSON")
	}
	// Scan the marshalled JSON, so struct values are checked by their JSON
	// field names too. The key name may be reported; its value never is.
	if k, bad := logx.SensitiveKeyInJSON(raw); bad {
		return fmt.Errorf("%w: %q", ErrSensitiveKey, k)
	}
	// Hash the payload as Postgres will store it, so Verify, reading it
	// back, computes the same hash.
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT $1::jsonb`, string(raw)).Scan(&stored); err != nil {
		return fmt.Errorf("audit: payload: %w", err)
	}
	r := record{
		actorID: actor, hospitalID: hospital, requestID: e.RequestID,
		action: e.Action, entity: e.Entity, entityID: e.EntityID,
		payload: stored,
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, chainLock); err != nil {
		return fmt.Errorf("audit: lock: %w", err)
	}
	// A new statement after the lock: under READ COMMITTED it sees the
	// head committed by the previous lock holder.
	var headHash *string
	var headID *int64
	if err := tx.QueryRow(ctx, `SELECT id, hash FROM audit_chain_head()`).Scan(&headID, &headHash); err != nil {
		return fmt.Errorf("audit: chain head: %w", err)
	}
	// Taken under the lock, so ts never goes backwards in id order.
	r.ts = time.Now().UTC().Truncate(time.Microsecond)
	prev := genesis
	if headHash != nil {
		prev = *headHash
	}
	h, err := r.hash(prev)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (ts, actor_id, hospital_id, request_id, action, entity, entity_id, payload, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)`,
		r.ts, nullable(r.actorID), nullable(r.hospitalID), nullable(r.requestID),
		r.action, nullable(r.entity), nullable(r.entityID), string(stored), prev, h); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// Verify walks the whole chain in id order and reports the first row whose
// prev_hash doesn't match the row before it, or whose hash doesn't match
// its contents. It reads as mhai_platform, the one app role that can see
// every hospital's audit rows. ok is true, and firstBroken 0, for an
// intact (or empty) chain.
func Verify(ctx context.Context, pool *pgxpool.Pool) (ok bool, firstBroken int64, err error) {
	err = db.WithPlatform(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, ts, actor_id::text, hospital_id::text, request_id, action, entity, entity_id,
			       payload, prev_hash, hash
			FROM audit_log ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		prev := genesis
		for rows.Next() {
			var (
				id                                           int64
				r                                            record
				actor, hospital, requestID, entity, entityID *string
				prevHash, hash                               string
			)
			if err := rows.Scan(&id, &r.ts, &actor, &hospital, &requestID, &r.action, &entity, &entityID,
				&r.payload, &prevHash, &hash); err != nil {
				return err
			}
			r.actorID, r.hospitalID, r.requestID = deref(actor), deref(hospital), deref(requestID)
			r.entity, r.entityID = deref(entity), deref(entityID)
			want, err := r.hash(prevHash)
			if err != nil || prevHash != prev || want != hash {
				firstBroken = id
				return nil
			}
			prev = hash
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return false, 0, fmt.Errorf("audit: verify: %w", err)
	}
	return ok, firstBroken, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
