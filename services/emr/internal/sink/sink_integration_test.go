//go:build integration

package sink

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
)

// A19 part, §8.1: the same record version stored once; announced once; a
// new version is a new row. Only allow-listed fields reach the database.
func TestSinkReplayNoOp(t *testing.T) {
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	h, c := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', 'SNK1', 'active')`, h); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url)
		VALUES ($1, $2, 'stub', 'Main record', 'https://stub.example.org')`, c, h); err != nil {
		t.Fatal(err)
	}
	var announced [][]uuid.UUID
	s := New(pool, h, c, "stub", 1, mrnNS, func(_ context.Context, hid uuid.UUID, ids []uuid.UUID) error {
		if hid != h {
			t.Errorf("announced for %v", hid)
		}
		announced = append(announced, ids)
		return nil
	})
	rec := connector.RawRecord{Kind: "observation", SourceID: "obs-1", SourceVersion: "1", SourceUpdatedAt: time.Now(),
		Fields: map[string]any{"patient": "p-1", "status": "final", "value_quantity": "5.5", "address": "1 Synthetic Street"}}
	for i := 0; i < 5; i++ {
		if err := s.Put(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	rec.SourceVersion = "2"
	if err := s.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(announced) != 1 || len(announced[0]) != 2 {
		t.Fatalf("announced %v, want one message with two ids", announced)
	}
	if s.Metrics.Replays() != 4 || s.Metrics.Dropped("observation") != 6 {
		t.Errorf("replays %d dropped %d", s.Metrics.Replays(), s.Metrics.Dropped("observation"))
	}
	var n, unpublished int
	var payload string
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE published_at IS NULL), max(payload::text)
		FROM emr.raw_records WHERE connection_id = $1`, c).Scan(&n, &unpublished, &payload); err != nil {
		t.Fatal(err)
	}
	if n != 2 || unpublished != 0 || strings.Contains(payload, "Synthetic Street") {
		t.Errorf("rows %d unpublished %d payload %s", n, unpublished, payload)
	}
	for _, bad := range []connector.RawRecord{
		{Kind: "practitioner", SourceID: "x"},
		{Kind: "observation", SourceID: ""},
		{Kind: "observation", SourceID: strings.Repeat("x", 257)},
		{Kind: "observation", SourceID: "x", SourceVersion: strings.Repeat("9", 65)},
		{Kind: "observation", SourceID: "x\x00y"},
	} {
		if err := s.Put(ctx, bad); err != ErrRecord {
			t.Errorf("Put(%q, %d chars): %v, want ErrRecord", bad.Kind, len(bad.SourceID), err)
		}
	}
}
