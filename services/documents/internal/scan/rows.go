package scan

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// PGRows calls the documents_scan_* definers (00048) as mhai_documents,
// under the hospital (RLS): another hospital's document is never claimed.
type PGRows struct{ Pool *pgxpool.Pool }

func (p PGRows) tx(ctx context.Context, hospital uuid.UUID, fn func(pgx.Tx) error) error {
	return dbx.WithHospital(ctx, p.Pool, dbx.Documents, hospital, fn)
}

// Claim calls documents_scan_claim.
func (p PGRows) Claim(ctx context.Context, hospital, document, lease uuid.UUID) (Claim, error) {
	var (
		c           Claim
		purpose, mt *string
		size        *int64
		sum         []byte
	)
	err := p.tx(ctx, hospital, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT outcome, purpose, mime_type, size_bytes, sha256
			FROM public.documents_scan_claim($1, $2)`, document, lease).Scan(&c.Outcome, &purpose, &mt, &size, &sum)
	})
	if err != nil {
		return Claim{}, err
	}
	if purpose != nil {
		c.Purpose = *purpose
	}
	if mt != nil {
		c.MimeType = *mt
	}
	c.SHA256 = sum
	return c, nil
}

func (p PGRows) boolCall(ctx context.Context, hospital uuid.UUID, q string, args ...any) (bool, error) {
	var ok bool
	err := p.tx(ctx, hospital, func(tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&ok) })
	return ok, err
}

// Renew calls documents_scan_renew.
func (p PGRows) Renew(ctx context.Context, hospital, document, lease uuid.UUID) (bool, error) {
	return p.boolCall(ctx, hospital, `SELECT public.documents_scan_renew($1, $2)`, document, lease)
}

// Release calls documents_scan_release.
func (p PGRows) Release(ctx context.Context, hospital, document, lease uuid.UUID) (bool, error) {
	return p.boolCall(ctx, hospital, `SELECT public.documents_scan_release($1, $2)`, document, lease)
}

// Quarantine calls documents_scan_quarantine.
func (p PGRows) Quarantine(ctx context.Context, hospital, document, lease uuid.UUID, reason string, sum *[32]byte) (bool, error) {
	var b []byte
	if sum != nil {
		b = sum[:]
	}
	return p.boolCall(ctx, hospital, `SELECT public.documents_scan_quarantine($1, $2, $3, $4)`, document, lease, reason, b)
}

// Ready calls documents_scan_ready with the objects' rows.
func (p PGRows) Ready(ctx context.Context, hospital, document, lease uuid.UUID, sum [32]byte, pages int, objects []Object) (bool, error) {
	n := len(objects)
	kinds, ns, wrapped := make([]string, n), make([]int32, n), make([][]byte, n)
	versions, sums, sizes := make([]int32, n), make([][]byte, n), make([]int64, n)
	for i, o := range objects {
		kinds[i], ns[i], wrapped[i] = string(o.Kind), int32(o.N), o.Sealed.WrappedDEK
		versions[i], sums[i], sizes[i] = int32(o.Sealed.KEKVersion), append([]byte(nil), o.Sealed.SHA256[:]...), o.Sealed.Size
	}
	return p.boolCall(ctx, hospital, `SELECT public.documents_scan_ready($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		document, lease, sum[:], pages, kinds, ns, wrapped, versions, sums, sizes)
}

// Due is one row the sweeper picks up.
type Due struct{ Hospital, Document uuid.UUID }

// SweepAfter is how long an uploaded row waits for its message before the
// sweeper scans it (§4.2: 2 minutes).
const SweepAfter = 2 * time.Minute

// Due lists, across hospitals, rows a lost message left behind: uploaded
// for SweepAfter, or scanning with an expired lease (§8: the documents
// service's sweeper). It reads as mhai_documents_sweep: ids, status and
// times only.
func (p PGRows) Due(ctx context.Context, limit int) ([]Due, error) {
	if p.Pool == nil {
		return nil, errors.New("scan: no database")
	}
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_documents_sweep`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT hospital_id, id FROM public.documents
		WHERE (status = 'uploaded' AND received_at < now() - make_interval(secs => $1))
		   OR (status = 'scanning' AND scan_lease_until <= now())
		ORDER BY received_at LIMIT $2`, SweepAfter.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Due, error) {
		var d Due
		err := r.Scan(&d.Hospital, &d.Document)
		return d, err
	})
}
