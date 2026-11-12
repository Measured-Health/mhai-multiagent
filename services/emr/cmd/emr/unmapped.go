package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

// unmapped prints a connection's unmapped codes (EMR design §9.4): kind,
// system, code, display text and counts. No patient data: the table holds
// none. It reads as mhai_emr under the connection's hospital.
func unmapped(ctx context.Context, pool *pgxpool.Pool, hospital, connection uuid.UUID, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tSYSTEM\tCODE\tDISPLAY\tCOUNT\tLAST SEEN")
	err := dbx.WithHospital(ctx, pool, dbx.EMR, hospital, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, local_system, local_code, coalesce(local_display, ''), count, last_seen
			FROM emr.unmapped_codes WHERE hospital_id = $1 AND connection_id = $2 ORDER BY count DESC, kind, local_code`,
			hospital, connection)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, system, code, display string
			var count int64
			var last time.Time
			if err := rows.Scan(&kind, &system, &code, &display, &count, &last); err != nil {
				return err
			}
			// The display text came from a source: shown cleaned, never raw.
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", kind, sink.CleanText(system, 80), sink.CleanText(code, 64),
				sink.CleanText(display, 80), count, last.UTC().Format(time.RFC3339))
		}
		return rows.Err()
	})
	if err != nil {
		return errors.New("emr unmapped: cannot read the unmapped codes")
	}
	return w.Flush()
}

func unmappedCmd(getenv func(string) string, hospital, connection string, out io.Writer) error {
	h, err1 := uuid.Parse(hospital)
	c, err2 := uuid.Parse(connection)
	if err1 != nil || err2 != nil {
		return errUsage
	}
	url := getenv("EMR_DATABASE_URL")
	if url == "" {
		return errors.New("EMR_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return errors.New("EMR_DATABASE_URL is invalid")
	}
	defer pool.Close()
	return unmapped(ctx, pool, h, c, out)
}
