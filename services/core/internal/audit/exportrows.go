package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// An events export's rows (contract §14.9; plan Task 25, departures
// 62-64): the filters' entries up to a snapshot, oldest first, each as the
// catalogue sends it, by ids and codes only. Rebuilt from the append-only
// chain, the same snapshot always gives the same rows.

// ExportMax is the most rows one export holds; past it the export is
// refused (departure 64), never cut.
const ExportMax = 50000

// ErrNoQ: an export takes no q filter.
var ErrNoQ = errors.New("audit: an export takes no q")

// ExportEvent is one row of an events export.
type ExportEvent struct {
	Seq                           int64
	Hash, At, ActorID, HospitalID string
	Type, Kind                    string
	TargetKind, TargetID          string
	Outcome, Flag                 string
}

// Snapshot is the chain's last committed entry: every entry with a lower
// id has committed (ids are taken under the chain lock, appends commit in
// id order), so the entries up to it never change.
func Snapshot(ctx context.Context, tx pgx.Tx) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM audit_log`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("audit: snapshot: %w", err)
	}
	return seq, nil
}

// CountExport counts f's entries up to maxSeq, up to ExportMax + 1.
func CountExport(ctx context.Context, tx pgx.Tx, f Filters, maxSeq int64) (int, error) {
	q, err := exportQuery(f, maxSeq)
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM audit_log WHERE `+q.clause()+
		` LIMIT `+strconv.Itoa(ExportMax+1)+`) s`, q.args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("audit: export count: %w", err)
	}
	return n, nil
}

func exportQuery(f Filters, maxSeq int64) (*query, error) {
	if f.Q != "" {
		return nil, ErrNoQ
	}
	q := &query{}
	if err := q.filter(f); err != nil {
		return nil, err
	}
	q.where = append(q.where, "id <= "+q.arg(maxSeq))
	return q, nil
}

// ExportEvents are f's entries up to maxSeq, oldest first, at most
// ExportMax (more is an error: count first).
func ExportEvents(ctx context.Context, tx pgx.Tx, f Filters, maxSeq int64) ([]ExportEvent, error) {
	q, err := exportQuery(f, maxSeq)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+rowColumns+` FROM audit_log WHERE `+q.clause()+
		` ORDER BY id LIMIT `+strconv.Itoa(ExportMax+1), q.args...)
	if err != nil {
		return nil, fmt.Errorf("audit: export: %w", err)
	}
	raws, err := scanRows(rows)
	if err != nil {
		return nil, fmt.Errorf("audit: export: %w", err)
	}
	if len(raws) > ExportMax {
		return nil, errors.New("audit: export over the cap")
	}
	out := make([]ExportEvent, 0, len(raws))
	for _, r := range raws {
		out = append(out, exportEvent(r))
	}
	return out, nil
}

// exportEvent is the row's export line: its position, fingerprint, time,
// actor and hospital ids, and the catalogue's type, kind, target, outcome
// and flag; the bare entry's type and kind when the catalogue doesn't map
// it, and nothing else of it.
func exportEvent(r rawRow) ExportEvent {
	e := ExportEvent{Seq: r.row.Seq, Hash: r.row.Hash, At: r.row.At.UTC().Format(time.RFC3339Nano),
		ActorID: r.row.Event.ActorID, HospitalID: r.row.Event.HospitalID, Type: UnknownType, Kind: UnknownKind}
	if !hexHash(e.Hash) {
		e.Hash = ""
	}
	if e.ActorID == "" {
		e.ActorID = "system"
	}
	var payload map[string]any
	if err := json.Unmarshal(r.payload, &payload); err != nil || payload == nil {
		return e
	}
	r.row.Event.Payload = payload
	m, ok := Map(r.row.Event)
	if !ok {
		return e
	}
	e.Type, e.Kind, e.Outcome = m.Type, m.Kind, m.Outcome
	if m.Target != nil {
		e.TargetKind, e.TargetID = m.Target.Kind, m.Target.ID
	}
	if m.Flag != nil {
		e.Flag = *m.Flag
	}
	return e
}
