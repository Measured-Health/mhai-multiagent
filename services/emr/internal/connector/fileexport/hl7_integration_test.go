//go:build integration

package fileexport

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/rules"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/scheduler"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

func emrDir(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", "..", ".."}, parts...)...)
}

type pubCapture struct {
	mu   sync.Mutex
	subj []string
	msgs [][]byte
}

func (c *pubCapture) Publish(_ context.Context, subject string, data []byte, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subj = append(c.subj, subject)
	c.msgs = append(c.msgs, append([]byte(nil), data...))
	return nil
}

// HL7Pipeline is the whole hl7v2_file path on synthetic messages: SFTP →
// the scheduler and the connector → the sink → the normaliser → the rules.
// A critical potassium becomes a critical finding for this hospital's
// patient, a correction supersedes it, a second hospital sees nothing,
// and no synthetic name, MRN, control id or value reaches a log or a
// message.
func TestHL7PipelineToFindings(t *testing.T) {
	_, pool := testpg.Migrated(t)
	c := context.Background()
	s := newServer(t)
	cfg := s.hl7Config()
	hid, other := uuid.New(), uuid.New()
	cfg.HospitalID = hid
	origins, err := emrurl.OriginsOf(HL7ID, cfg.BaseURL, cfg.Settings, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range []uuid.UUID{hid, other} {
		if _, err := pool.Exec(c, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
			h, "H"+string(rune('A'+i))+h.String()[:5]); err != nil {
			t.Fatal(err)
		}
	}
	settings := map[string]any{}
	for k, v := range cfg.Settings {
		settings[k] = v
	}
	if _, err := pool.Exec(c, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings,
		credentials_origin, enabled, poll_seconds) VALUES ($1, $2, 'hl7v2_file', 'Lab messages', $3, $4, $5, true, 300)`,
		cfg.ConnectionID, hid, cfg.BaseURL, settings, emrurl.Strings(origins)); err != nil {
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
	ver, err := store.Put(c, secrets.Path{Env: "development", Hospital: hid, Connection: cfg.ConnectionID, Field: SecretField},
		seal.NewSecret(s.clientPK))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(c, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, $3, now(), $4, $5)`, cfg.ConnectionID, hid, SecretField, ver, emrurl.Strings(origins)); err != nil {
		t.Fatal(err)
	}
	tables, err := codes.Load(emrDir("codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(emrDir("maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Load(emrDir("rules", "testdata", "ruleset-0-synthetic.yaml"), true)
	if err != nil {
		t.Fatal(err)
	}
	sha := rs.SHA256()
	if _, err := pool.Exec(c, `INSERT INTO emr_rule_set_activation (hospital_id, version, sha256) VALUES ($1, $2, $3)`,
		hid, rs.Version(), sha[:]); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(log)
	defer slog.SetDefault(prev)
	npub, rpub := &pubCapture{}, &pubCapture{}
	n := &normalise.Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: npub, Log: log}
	eng := &rules.Engine{Pool: pool, RuleSet: rs, Maps: maps, Pub: rpub, Active: rs.Version(), Log: log}
	conn := s.hl7Connector()
	sched := &scheduler.Scheduler{Pool: pool, Store: store, Env: "development", Log: log, Metrics: &scheduler.Metrics{},
		Connectors: connector.NewRegistry(true, conn),
		NewSink: func(cc connector.Config, backfill bool) scheduler.Sink {
			sk := sink.New(pool, cc.HospitalID, cc.ConnectionID, cc.Type, 1, cc.Settings["mrn_namespace"],
				func(ctx context.Context, h uuid.UUID, ids []uuid.UUID) error {
					if err := normalise.PublishFetched(ctx, npub, h, ids); err != nil {
						return err
					}
					return n.Handle(ctx, h, ids)
				})
			sk.Backfill = false
			return sk
		}}
	created := func() []uuid.UUID {
		npub.mu.Lock()
		defer npub.mu.Unlock()
		var ids []uuid.UUID
		for i, subj := range npub.subj {
			if !strings.HasSuffix(subj, ".created") {
				continue
			}
			var cr emrpb.Created
			if err := proto.Unmarshal(npub.msgs[i], &cr); err != nil {
				t.Fatal(err)
			}
			for _, id := range cr.RecordIds {
				ids = append(ids, uuid.MustParse(id))
			}
		}
		npub.subj, npub.msgs = nil, nil
		return ids
	}

	// The synthetic code table's potassium (99902-9), critical high.
	now := time.Now().UTC().Truncate(time.Second)
	k := hl7test.Result{Code: "99902-9", System: "LN", Name: "Potassium", Type: "NM", Value: "6.9", Unit: "mmol/L",
		Range: "3.5-5.1", Flag: "HH", Status: "F", At: now.Add(-10 * time.Minute)}
	s.put(hl7v2.FileName(now, 1), hl7test.File("f1", now, hl7test.ORU("SYN-CTRL-1", "SYN-ORD-1", now.Add(-5*time.Minute), k),
		hl7test.ADT("SYN-CTRL-2", now)))
	if ran, err := sched.Once(c); ran != 1 || err != nil {
		t.Fatalf("sync: %d %v", ran, err)
	}
	if err := eng.Handle(c, hid, created()); err != nil {
		t.Fatal(err)
	}
	var obs, findings, pats int
	var mrn string
	if err := pool.QueryRow(c, `SELECT (SELECT count(*) FROM emr.observations WHERE hospital_id = $1 AND loinc = '99902-9'
			AND value_num = 6.9 AND superseded_by IS NULL),
		(SELECT count(*) FROM emr.findings WHERE hospital_id = $1 AND severity = 'critical'),
		(SELECT count(*) FROM emr.patients WHERE hospital_id = $1),
		(SELECT coalesce(max(mrn), '') FROM emr.patients WHERE hospital_id = $1)`, hid).Scan(&obs, &findings, &pats, &mrn); err != nil {
		t.Fatal(err)
	}
	if obs != 1 || findings != 1 || pats != 1 || mrn != hl7test.MRN {
		t.Fatalf("observations %d, critical findings %d, patients %d, mrn %q", obs, findings, pats, mrn)
	}
	// The other hospital: nothing.
	var leaked int
	if err := pool.QueryRow(c, `SELECT (SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1) +
		(SELECT count(*) FROM emr.patients WHERE hospital_id = $1)`, other).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("other hospital: %d %v", leaked, err)
	}

	// The correction: one current value, the corrected one.
	k.Status, k.Value, k.Flag = "C", "4.4", "N"
	s.put(hl7v2.FileName(now.Add(time.Second), 1), hl7test.File("f2", now, hl7test.ORU("SYN-CTRL-3", "SYN-ORD-1", now, k)))
	time.Sleep(10 * time.Millisecond)
	if _, err := pool.Exec(c, `UPDATE emr.sync_state SET next_due_at = now() - interval '1 second' WHERE connection_id = $1`,
		cfg.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if ran, err := sched.Once(c); ran != 1 || err != nil {
		t.Fatalf("second sync: %d %v", ran, err)
	}
	if err := eng.Handle(c, hid, created()); err != nil {
		t.Fatal(err)
	}
	var current string
	var versions int
	if err := pool.QueryRow(c, `SELECT (SELECT status || ':' || value_num::text FROM emr.observations WHERE hospital_id = $1
			AND superseded_by IS NULL), (SELECT count(*) FROM emr.observations WHERE hospital_id = $1)`, hid).Scan(&current, &versions); err != nil {
		t.Fatal(err)
	}
	if current != "corrected:4.4" || versions != 2 {
		t.Fatalf("after the correction: %q, %d versions", current, versions)
	}
	if got := s.list(Processed); len(got) != 2 {
		t.Fatalf("processed %v", got)
	}

	// Telemetry: no synthetic identity or value.
	npub.mu.Lock()
	rpub.mu.Lock()
	tele := logs.String() + strings.Join(npub.subj, "\n") + string(bytes.Join(npub.msgs, nil)) + string(bytes.Join(rpub.msgs, nil))
	rpub.mu.Unlock()
	npub.mu.Unlock()
	for _, sv := range []string{hl7test.Family, hl7test.Given, hl7test.MRN, "SYN-CTRL-1", "SYN-ORD-1", "19800101", "SYNTH-WARD-A"} {
		if strings.Contains(tele, sv) {
			t.Errorf("telemetry holds %q", sv)
		}
	}
}
