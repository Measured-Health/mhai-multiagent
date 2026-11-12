// Package sink is RawSink (EMR design §8.1, §14.1; plan Task 17): the one
// way a connector's records reach the database. It keeps only the
// allow-listed fields of each kind, cleans display text, stores each record
// version once (a replay is a no-op) and announces the new rows' ids, at
// most 500 a message, on emr.v1.<hid>.fetched for the normaliser.
//
// A record it can't store (an unknown kind, an over-long or unprintable
// source id or version) is refused whole with ErrRecord; nothing about its
// content is logged or returned.
package sink

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/index"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// MaxBatch is the most raw ids one fetched message carries.
const MaxBatch = 500

// ErrRecord: the record can't be stored as given (fixed text).
var ErrRecord = errors.New("sink: record refused")

// Publish announces new raw ids (one message, at most MaxBatch ids).
type Publish func(ctx context.Context, hospital uuid.UUID, rawIDs []uuid.UUID) error

// Metrics counts what the sink drops and stores, by kind; no content.
type Metrics struct {
	mu       sync.Mutex
	dropped  map[string]int64
	stored   map[string]int64
	replays  int64
	excluded int64
}

func (m *Metrics) add(kind string, dropped int, stored bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dropped == nil {
		m.dropped, m.stored = map[string]int64{}, map[string]int64{}
	}
	m.dropped[kind] += int64(dropped)
	if stored {
		m.stored[kind]++
	} else {
		m.replays++
	}
}

func (m *Metrics) addExcluded() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.excluded++
}

// Excluded is how many records of excluded patients were kept without a
// payload.
func (m *Metrics) Excluded() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.excluded
}

// Dropped is emr_sink_dropped_fields_total{kind}.
func (m *Metrics) Dropped(kind string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped[kind]
}

// Replays is how many records were already stored.
func (m *Metrics) Replays() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.replays
}

// Sink stores one connection's records.
type Sink struct {
	pool                 *pgxpool.Pool
	hospital, connection uuid.UUID
	connType             string
	connVersion          int
	mrnSystem            string
	publish              Publish
	Metrics              *Metrics
	// Backfill flags every record this sink stores as a first sync's
	// (design §7.1): findings from it are historical and never alert.
	Backfill bool

	mu      sync.Mutex
	pending []uuid.UUID
}

var _ connector.RawSink = (*Sink)(nil)

// New returns the sink for one connection. mrnSystem is the connection's
// MRN namespace (settings.mrn_namespace): the one identifier kept.
func New(pool *pgxpool.Pool, hospital, connection uuid.UUID, connType string, connVersion int, mrnSystem string, publish Publish) *Sink {
	return &Sink{pool: pool, hospital: hospital, connection: connection, connType: connType, connVersion: connVersion,
		mrnSystem: mrnSystem, publish: publish, Metrics: &Metrics{}}
}

// Put stores r's allow-listed fields once, and queues its id to announce.
func (s *Sink) Put(ctx context.Context, r connector.RawRecord) error {
	_, err := s.PutStored(ctx, r)
	return err
}

// PutStored is Put, reporting whether r was new (stored, not a replay).
func (s *Sink) PutStored(ctx context.Context, r connector.RawRecord) (bool, error) {
	if !slices.Contains(Kinds, r.Kind) || r.SourceID == "" || len(r.SourceID) > 256 || !printable(r.SourceID) ||
		len(r.SourceVersion) > 64 || !printable(r.SourceVersion) {
		return false, ErrRecord
	}
	fields, dropped := keep(r.Kind, r.Fields, s.mrnSystem)
	payload, err := json.Marshal(fields)
	if err != nil {
		return false, ErrRecord
	}
	sum := sha256.Sum256(append([]byte(r.Kind+"\x00"+r.SourceID+"\x00"+r.SourceVersion+"\x00"), payload...))
	var updated *time.Time
	if !r.SourceUpdatedAt.IsZero() {
		u := r.SourceUpdatedAt.UTC()
		updated = &u
	}
	var id uuid.UUID
	stored, excluded := false, false
	src := r.SourceID
	if r.Kind != "patient" {
		src, _ = fields["patient"].(string)
	}
	err = dbx.WithHospital(ctx, s.pool, dbx.EMR, s.hospital, func(tx pgx.Tx) error {
		// An excluded patient's record is kept as a rejected row with no
		// payload (design §10.6), so a replay stays a no-op and nothing of
		// it is held. The patient is held FOR SHARE until this commits, so
		// an exclusion either waits for this row (and purges it) or has
		// committed and is seen here.
		if src != "" {
			p, found, err := index.Lookup(ctx, tx, s.hospital, s.connection, src)
			if err != nil {
				return err
			}
			if found && p.Status == "excluded" {
				_, err := tx.Exec(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind,
					source_id, source_version, source_updated_at, payload, payload_sha256, connector_type, connector_version,
					status, reject_code, backfill)
					VALUES ($1, 'connector', $2, $3, $4, $5, $6, NULL, $7, $8, $9, 'rejected', 'excluded', $10) ON CONFLICT DO NOTHING`,
					s.hospital, s.connection, r.Kind, r.SourceID, r.SourceVersion, updated, sum[:], s.connType, s.connVersion, s.Backfill)
				excluded = err == nil
				return err
			}
		}
		err := tx.QueryRow(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind,
			source_id, source_version, source_updated_at, payload, payload_sha256, connector_type, connector_version, backfill)
			VALUES ($1, 'connector', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT DO NOTHING RETURNING id`,
			s.hospital, s.connection, r.Kind, r.SourceID, r.SourceVersion, updated, payload, sum[:], s.connType,
			s.connVersion, s.Backfill).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		stored = err == nil
		return err
	})
	if err != nil {
		return false, err
	}
	if excluded {
		s.Metrics.addExcluded()
		return false, nil
	}
	s.Metrics.add(r.Kind, dropped, stored)
	if !stored {
		return false, nil
	}
	s.mu.Lock()
	s.pending = append(s.pending, id)
	full := len(s.pending) >= MaxBatch
	s.mu.Unlock()
	if full {
		return true, s.Flush(ctx)
	}
	return true, nil
}

// Flush announces the queued ids, MaxBatch a message, and marks them
// published. On a refused publish the rows stay unpublished and the
// normaliser's sweep announces them later.
func (s *Sink) Flush(ctx context.Context) error {
	s.mu.Lock()
	ids := s.pending
	s.pending = nil
	s.mu.Unlock()
	for len(ids) > 0 {
		n := min(len(ids), MaxBatch)
		batch := ids[:n]
		ids = ids[n:]
		if err := s.publish(ctx, s.hospital, batch); err != nil {
			return err
		}
		if err := MarkPublished(ctx, s.pool, s.hospital, batch); err != nil {
			return err
		}
	}
	return nil
}

// MarkPublished records that ids were announced.
func MarkPublished(ctx context.Context, pool *pgxpool.Pool, hospital uuid.UUID, ids []uuid.UUID) error {
	return dbx.WithHospital(ctx, pool, dbx.EMR, hospital, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE emr.raw_records SET published_at = now()
			WHERE hospital_id = $1 AND id = ANY ($2) AND published_at IS NULL`, hospital, ids)
		return err
	})
}
