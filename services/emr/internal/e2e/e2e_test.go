//go:build e2e

// Package e2e is the synthetic end-to-end run (EMR plan Task 25), built
// into a test binary by scripts/e2e-fhir.sh and run in a container on
// exactly the emr service's networks (backend and emr-proxy, no route
// out), against a migrated Postgres, the real egress-emr and the synthetic
// FHIR server behind it. Synthetic data only; development only. Never run
// against a real FHIR server: the base address is the synthetic server's
// name, which only the test's Docker network resolves.
package e2e

import (
	"bytes"
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/fhir"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/synth"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

func must(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is required", k)
	}
	return v
}

type capture struct {
	mu   sync.Mutex
	data [][]byte
	subj []string
}

func (c *capture) Publish(_ context.Context, subject string, data []byte, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = append(c.data, append([]byte(nil), data...))
	c.subj = append(c.subj, subject)
	return nil
}

func TestSyntheticFHIRThroughEgress(t *testing.T) {
	ctx := context.Background()
	dbURL := must(t, "E2E_DATABASE_URL")
	proxy, err := url.Parse(must(t, "E2E_PROXY_URL"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := proxyconf.ReadKey(must(t, "E2E_PROXY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	name, cidr, synthDir := must(t, "E2E_FHIR_NAME"), must(t, "E2E_FHIR_CIDR"), must(t, "E2E_SYNTH_DIR")
	seed, _ := strconv.ParseInt(must(t, "E2E_SEED"), 10, 64)
	n, _ := strconv.Atoi(must(t, "E2E_PATIENTS"))

	// The database: core's migrations, as the superuser.
	cfgDB, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfgDB)
	prov, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(must(t, "E2E_MIGRATIONS")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = sqlDB.Close()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The synthetic connection, as the platform would have saved it: on,
	// its private range declared (the synthetic server's network).
	base, tokenURL := "https://"+name+"/r4", "https://"+name+"/token"
	origin := []emrurl.Origin{emrurl.Origin("https://" + name + ":443")}
	settings := map[string]string{"client_id": synth.ClientID, "token_url": tokenURL, "mrn_namespace": synth.MRNSystem}
	hid, conn := synth.Hospital, synth.Connection
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic E2E Hospital', 'SYNE2E', 'active')`, hid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, credentials_origin,
		enabled, private_network, private_cidrs, poll_seconds) VALUES ($1, $2, 'fhir_r4', 'Synthetic FHIR', $3, $4, $5, true, true, $6::cidr[], 60)`,
		conn, hid, base, settings, emrurl.Strings(origin), []string{cidr}); err != nil {
		t.Fatal(err)
	}
	// A second connection to the same name that declares another range:
	// the proxy must refuse it the synthetic server's address (A37).
	other := uuid.MustParse("00000000-0000-4000-8000-00000000f41b")
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, credentials_origin,
		enabled, private_network, private_cidrs, poll_seconds) VALUES ($1, $2, 'fhir_r4', 'Other range', $3, $4, $5, true, true, '{10.233.9.0/24}', 60)`,
		other, hid, base, settings, emrurl.Strings(origin)); err != nil {
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
	clientKey, err := os.ReadFile(filepath.Join(synthDir, "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	ver, err := store.Put(ctx, secrets.Path{Env: "development", Hospital: hid, Connection: conn, Field: "private_key"}, seal.NewSecret(clientKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'private_key', now(), $3, $4)`, conn, hid, ver, emrurl.Strings(origin)); err != nil {
		t.Fatal(err)
	}

	// Logs: everything, to check afterwards.
	var logs bytes.Buffer
	var logMu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{w: &logs, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
	slog.SetDefault(log)

	// The allow-list egress-emr reads.
	w := &egress.Writer{Pool: pool, Dir: must(t, "E2E_EGRESS_DIR"), Development: true, Log: log}
	wctx, stopWriter := context.WithCancel(ctx)
	defer stopWriter()
	go func() { _ = w.Run(wctx) }()
	time.Sleep(proxyconf.ReloadEvery + 2*time.Second)

	ca, err := os.ReadFile(filepath.Join(synthDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("ca.pem")
	}
	clients := func(c connector.Config) *http.Client { return egress.HTTPWithRoots(c, proxy, key, roots) }
	fc := fhir.New(clients)

	// No way out but the proxy: a direct request goes nowhere.
	direct := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	if resp, err := direct.Get(base + "/metadata"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("reached the synthetic server without the proxy")
	}

	cfg := connector.Config{ConnectionID: conn, HospitalID: hid, Type: "fhir_r4", BaseURL: base, Settings: settings, Origin: origin, PollSeconds: 60}
	creds := connector.NewCreds(map[string]seal.Secret{"private_key": seal.NewSecret(clientKey)})
	if r := fc.Test(ctx, cfg, creds); !r.OK {
		t.Fatalf("test through egress-emr: %+v", r)
	}
	otherCfg := cfg
	otherCfg.ConnectionID = other
	if r := fc.Test(ctx, otherCfg, creds); r.Code != "unreachable" {
		t.Fatalf("a connection that doesn't declare the range: %+v, want unreachable", r)
	}

	// The sync: scheduler, FHIR connector, sink, normaliser.
	tables, err := codes.Load(must(t, "E2E_CODES"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(must(t, "E2E_MAPS"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	pub := &capture{}
	nz := &normalise.Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: pub, Log: log}
	s := &scheduler.Scheduler{Pool: pool, Store: store, Env: "development", Log: log, Metrics: &scheduler.Metrics{},
		Connectors: connector.NewRegistry(true, fc),
		NewSink: func(c connector.Config, backfill bool) scheduler.Sink {
			sk := sink.New(pool, c.HospitalID, c.ConnectionID, c.Type, 1, c.Settings["mrn_namespace"],
				func(ctx context.Context, h uuid.UUID, ids []uuid.UUID) error {
					if err := normalise.PublishFetched(ctx, pub, h, ids); err != nil {
						return err
					}
					return nz.Handle(ctx, h, ids)
				})
			sk.Backfill = backfill
			return sk
		}}
	if _, err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	var outcome string
	var records int
	if err := pool.QueryRow(ctx, `SELECT outcome, records FROM emr.sync_runs WHERE connection_id = $1 AND finished_at IS NOT NULL`, conn).
		Scan(&outcome, &records); err != nil || outcome != "ok" || records == 0 {
		t.Fatalf("sync run %q %d %v", outcome, records, err)
	}
	var otherOutcome string
	if err := pool.QueryRow(ctx, `SELECT outcome FROM emr.sync_runs WHERE connection_id = $1 AND finished_at IS NOT NULL`, other).
		Scan(&otherOutcome); err != nil || otherOutcome != "sign_in_refused" {
		t.Fatalf("the connection with no credentials: %q %v", otherOutcome, err)
	}
	var obs, meds, pats, backfilled, pending, critical int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM emr.observations WHERE hospital_id = $1),
		(SELECT count(*) FROM emr.medications WHERE hospital_id = $1),
		(SELECT count(*) FROM emr.patients WHERE hospital_id = $1 AND mrn LIKE 'SYN-%'),
		(SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1 AND backfill),
		(SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1 AND status = 'pending'),
		(SELECT count(*) FROM emr.observations WHERE hospital_id = $1 AND loinc = '99902-9' AND value_num >= 6.5)`, hid).
		Scan(&obs, &meds, &pats, &backfilled, &pending, &critical); err != nil {
		t.Fatal(err)
	}
	t.Logf("synced: %d observations, %d medications, %d patients, %d backfill raw rows, %d pending", obs, meds, pats, backfilled, pending)
	if obs == 0 || meds == 0 || pats != n || backfilled == 0 || pending != 0 || critical == 0 {
		t.Fatalf("canonical tables: %d observations, %d medications, %d patients (want %d), %d backfill, %d pending, %d critical",
			obs, meds, pats, n, backfilled, pending, critical)
	}
	var live *time.Time
	if err := pool.QueryRow(ctx, `SELECT live_since FROM emr.sync_state WHERE connection_id = $1`, conn).Scan(&live); err != nil || live == nil {
		t.Fatalf("the first sync read everything, so the backfill ended: %v %v", live, err)
	}

	// A22: no synthetic name, MRN, birth date or value in any log record or
	// message.
	stopWriter()
	logMu.Lock()
	tele := logs.String()
	logMu.Unlock()
	pub.mu.Lock()
	tele += "\n" + strings.Join(pub.subj, "\n") + "\n" + string(bytes.Join(pub.data, []byte("\n")))
	pub.mu.Unlock()
	for _, p := range synth.Generate(seed, n) {
		for _, v := range append([]string{p.Given, p.Family, p.MRN, p.BirthDate, p.ID}, p.Values()...) {
			if v != "" && strings.Contains(tele, v) {
				t.Errorf("telemetry holds %q", v)
			}
		}
	}
	if strings.Contains(tele, "BEGIN") || strings.Contains(tele, name) {
		t.Error("telemetry holds a key or the server's name")
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
