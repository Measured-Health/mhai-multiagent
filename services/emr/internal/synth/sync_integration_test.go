//go:build integration

package synth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/fhir"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

func repoDir(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

// capture records every message published (the fetched and created
// announcements, signals).
type capture struct {
	mu   sync.Mutex
	msgs [][]byte
	subj []string
}

func (c *capture) Publish(_ context.Context, subject string, data []byte, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, append([]byte(nil), data...))
	c.subj = append(c.subj, subject)
	return nil
}

// A22, connector to canonical: a full synthetic sync (the FHIR connector,
// the scheduler, the sink, the normaliser) leaves no synthetic name, MRN or
// value in any log record or message, and the records reach the canonical
// tables.
func TestSyntheticSyncNoPHIInTelemetry(t *testing.T) {
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	ps := Generate(7, 6)
	srv := Server(ps)
	defer srv.Close()
	cfg := srv.Config()
	hid, conn := cfg.HospitalID, cfg.ConnectionID
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		hid, "Y"+hid.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings,
		credentials_origin, enabled, poll_seconds) VALUES ($1, $2, 'fhir_r4', 'Synthetic FHIR', $3, $4, $5, true, 120)`,
		conn, hid, cfg.BaseURL, cfg.Settings, emrurl.Strings(cfg.Origin)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.NewFileStore(dir, "development")
	if err != nil {
		t.Fatal(err)
	}
	ver, err := store.Put(ctx, secrets.Path{Env: "development", Hospital: hid, Connection: conn, Field: "private_key"}, seal.NewSecret(srv.ClientKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'private_key', now(), $3, $4)`, conn, hid, ver, emrurl.Strings(cfg.Origin)); err != nil {
		t.Fatal(err)
	}
	tables, err := codes.Load(repoDir("codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(repoDir("maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
	prev := slog.Default()
	slog.SetDefault(log)
	defer slog.SetDefault(prev)
	pub := &capture{}
	n := &normalise.Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: pub, Log: log}
	s := &scheduler.Scheduler{Pool: pool, Store: store, Env: "development", Log: log, Metrics: &scheduler.Metrics{},
		Connectors: connector.NewRegistry(true, fhir.New(func(connector.Config) *http.Client { return srv.Client() })),
		NewSink: func(c connector.Config, backfill bool) scheduler.Sink {
			sk := sink.New(pool, c.HospitalID, c.ConnectionID, c.Type, 1, c.Settings["mrn_namespace"],
				func(ctx context.Context, h uuid.UUID, ids []uuid.UUID) error {
					if err := normalise.PublishFetched(ctx, pub, h, ids); err != nil {
						return err
					}
					return n.Handle(ctx, h, ids)
				})
			sk.Backfill = backfill
			return sk
		}}
	if ran, err := s.Once(ctx); ran != 1 || err != nil {
		t.Fatalf("sync: %d %v", ran, err)
	}
	var outcome string
	var records int
	if err := pool.QueryRow(ctx, `SELECT outcome, records FROM emr.sync_runs WHERE connection_id = $1`, conn).Scan(&outcome, &records); err != nil ||
		outcome != "ok" || records == 0 {
		t.Fatalf("run %q %d %v", outcome, records, err)
	}
	var obs, meds, pats, pending int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.observations WHERE hospital_id = $1),
		(SELECT count(*) FROM emr.medications WHERE hospital_id = $1),
		(SELECT count(*) FROM emr.patients WHERE hospital_id = $1 AND mrn LIKE 'SYN-%'),
		(SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1 AND status = 'pending')`, hid).Scan(&obs, &meds, &pats, &pending); err != nil {
		t.Fatal(err)
	}
	if obs == 0 || meds == 0 || pats != len(ps) || pending != 0 {
		t.Fatalf("canonical: %d observations, %d medications, %d patients, %d pending", obs, meds, pats, pending)
	}
	var critical int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM emr.observations WHERE hospital_id = $1 AND loinc = '99902-9' AND value_num >= 6.5`, hid).Scan(&critical); err != nil || critical == 0 {
		t.Fatalf("the critical potassium didn't arrive: %d %v", critical, err)
	}
	// The telemetry: logs and every message.
	pub.mu.Lock()
	tele := logs.String() + "\n" + strings.Join(pub.subj, "\n") + "\n" + string(bytes.Join(pub.msgs, []byte("\n")))
	pub.mu.Unlock()
	for _, p := range ps {
		for _, s := range append([]string{p.Given, p.Family, p.MRN, p.BirthDate, p.ID}, p.Values()...) {
			if s != "" && strings.Contains(tele, s) {
				t.Errorf("telemetry holds %q", s)
			}
		}
	}
}
