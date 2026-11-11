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
	mu      sync.Mutex
	dropped map[string]int64
	stored  map[string]int64
	replays int64
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
	if !slices.Contains(Kinds, r.Kind) || r.SourceID == "" || len(r.SourceID) > 256 || !printable(r.SourceID) ||
		len(r.SourceVersion) > 64 || !printable(r.SourceVersion) {
		return ErrRecord
	}
	fields, dropped := keep(r.Kind, r.Fields, s.mrnSystem)
	payload, err := json.Marshal(fields)
	if err != nil {
		return ErrRecord
	}
	sum := sha256.Sum256(append([]byte(r.Kind+"\x00"+r.SourceID+"\x00"+r.SourceVersion+"\x00"), payload...))
	var updated *time.Time
	if !r.SourceUpdatedAt.IsZero() {
		u := r.SourceUpdatedAt.UTC()
		updated = &u
	}
	var id uuid.UUID
	stored := false
	err = dbx.WithHospital(ctx, s.pool, dbx.EMR, s.hospital, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO emr.raw_records (hospital_id, source_kind, connection_id, resource_kind,
			source_id, source_version, source_updated_at, payload, payload_sha256, connector_type, connector_version)
			VALUES ($1, 'connector', $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING RETURNING id`,
			s.hospital, s.connection, r.Kind, r.SourceID, r.SourceVersion, updated, payload, sum[:], s.connType,
			s.connVersion).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		stored = err == nil
		return err
	})
	if err != nil {
		return err
	}
	s.Metrics.add(r.Kind, dropped, stored)
	if !stored {
		return nil
	}
	s.mu.Lock()
	s.pending = append(s.pending, id)
	full := len(s.pending) >= MaxBatch
	s.mu.Unlock()
	if full {
		return s.Flush(ctx)
	}
	return nil
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
