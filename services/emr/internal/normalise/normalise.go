// Package normalise is the one normaliser (EMR design §8, §9; plan Task
// 19): it turns raw records into canonical rows, strictly, with provenance
// and versions, in the emr service. Connectors never normalise.
//
// For each emr.v1.<hid>.fetched batch, under the subject's hospital (RLS
// hides every other hospital's row), it reads the pending raw records,
// resolves their patients through the index, writes canonical rows, marks
// each raw record normalised or rejected with a closed code, and announces
// the new observations and medications on records.v1.<hid>.created. It
// never guesses: an unknown code or unit is kept, flagged unstandardised
// and counted; a value or time it can't read rejects the record.
package normalise

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/signals"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// Version is the normaliser's version, recorded on every canonical row.
const Version = "n1"

// MaxBatch is the most record ids one created message carries.
const MaxBatch = 500

// Metrics are counts only.
type Metrics struct {
	Batches, Normalised, Rejected, Refused, Unmapped, ExternalLinks atomic.Int64
}

// Normaliser holds what a batch needs; it is safe for concurrent use.
type Normaliser struct {
	Pool    *pgxpool.Pool
	Tables  *codes.Tables
	Maps    *codes.Maps
	Pub     signals.Publisher
	Log     *slog.Logger
	Now     func() time.Time
	Metrics *Metrics
}

func (n *Normaliser) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Normaliser) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (n *Normaliser) metrics() *Metrics {
	if n.Metrics == nil {
		n.Metrics = &Metrics{}
	}
	return n.Metrics
}

// raw is one raw record as read back.
type raw struct {
	id, conn                uuid.UUID
	kind, sourceID, version string
	updated                 *time.Time
	received                time.Time
	payload                 map[string]any
	connType                string
	connVersion             int
	status, sourceKind      string
}

var kindOrder = []string{"patient", "location", "encounter", "observation", "medication"}

// Handle normalises one batch of hospital hid's raw ids. It returns an
// error only for a failure worth retrying (the database, the publish); a
// record that can't be read is rejected, not retried. Ids of another
// hospital are invisible and counted as refused.
func (n *Normaliser) Handle(ctx context.Context, hid uuid.UUID, rawIDs []uuid.UUID) error {
	m := n.metrics()
	m.Batches.Add(1)
	var created []uuid.UUID
	var sigs []signals.Signal
	err := dbx.WithHospital(ctx, n.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		created, sigs = nil, nil
		conns, err := loadConnections(ctx, tx, hid)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, connection_id, resource_kind, source_id, source_version, source_updated_at,
			received_at, payload, connector_type, connector_version, status, source_kind
			FROM emr.raw_records WHERE hospital_id = $1 AND id = ANY ($2)`, hid, rawIDs)
		if err != nil {
			return err
		}
		recs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (raw, error) {
			var x raw
			var conn *uuid.UUID
			var payload []byte
			err := r.Scan(&x.id, &conn, &x.kind, &x.sourceID, &x.version, &x.updated, &x.received, &payload,
				&x.connType, &x.connVersion, &x.status, &x.sourceKind)
			if conn != nil {
				x.conn = *conn
			}
			if err == nil && payload != nil {
				if json.Unmarshal(payload, &x.payload) != nil {
					x.payload = nil
				}
			}
			return x, err
		})
		if err != nil {
			return err
		}
		if refused := len(uniq(rawIDs)) - len(recs); refused > 0 {
			m.Refused.Add(int64(refused))
		}
		slices.SortStableFunc(recs, func(a, b raw) int {
			return slices.Index(kindOrder, a.kind) - slices.Index(kindOrder, b.kind)
		})
		for _, r := range recs {
			if r.status != "pending" {
				continue
			}
			cfg, ok := conns[r.conn]
			if r.sourceKind != "connector" || !ok {
				// Document records arrive with PR 12; until then nothing
				// normalises them.
				continue
			}
			out, err := n.record(ctx, tx, cfg, r)
			if err != nil {
				return err
			}
			created = append(created, out.created...)
			sigs = append(sigs, out.signals...)
			if out.reject != "" {
				m.Rejected.Add(1)
			} else {
				m.Normalised.Add(1)
			}
		}
		// Rows from a delivery that committed but failed to announce.
		rows, err = tx.Query(ctx, `SELECT id FROM emr.observations WHERE hospital_id = $1 AND raw_id = ANY ($2) AND superseded_by IS NULL
			UNION SELECT id FROM emr.medications WHERE hospital_id = $1 AND raw_id = ANY ($2) AND superseded_by IS NULL`, hid, rawIDs)
		if err != nil {
			return err
		}
		again, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range again {
			if !slices.Contains(created, id) {
				created = append(created, id)
			}
		}
		return nil
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			n.log().Error("normalise: database error", "sqlstate", pe.Code, "constraint", pe.ConstraintName)
		} else {
			n.log().Error("normalise: batch failed", "ids", len(rawIDs))
		}
		return err
	}
	for len(created) > 0 {
		k := min(len(created), MaxBatch)
		if err := n.publishCreated(ctx, hid, created[:k]); err != nil {
			return err
		}
		created = created[k:]
	}
	failed := 0
	for _, s := range sigs {
		if signals.Send(ctx, n.Pub, hid, s) != nil {
			failed++
		}
	}
	if failed > 0 {
		n.log().Warn("normalise: signals not sent; the rows stand", "count", failed)
	}
	return nil
}

func uniq(ids []uuid.UUID) []uuid.UUID {
	s := slices.Clone(ids)
	slices.SortFunc(s, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(s)
}

func (n *Normaliser) publishCreated(ctx context.Context, hid uuid.UUID, ids []uuid.UUID) error {
	msg := &emrpb.Created{RecordIds: make([]string, len(ids))}
	for i, id := range ids {
		msg.RecordIds[i] = id.String()
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return n.Pub.Publish(ctx, "records.v1."+hid.String()+".created", b, ids[0].String())
}

// loadConnections reads the hospital's connections (configuration only).
func loadConnections(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (map[uuid.UUID]connector.Config, error) {
	rows, err := tx.Query(ctx, `SELECT id, type, settings FROM public.emr_connections WHERE hospital_id = $1`, hid)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]connector.Config{}
	for rows.Next() {
		var c connector.Config
		var settings map[string]any
		if err := rows.Scan(&c.ConnectionID, &c.Type, &settings); err != nil {
			rows.Close()
			return nil, err
		}
		c.HospitalID = hid
		c.Settings = map[string]string{}
		for k, v := range settings {
			if s, ok := v.(string); ok {
				c.Settings[k] = s
			}
		}
		out[c.ConnectionID] = c
	}
	return out, rows.Err()
}
