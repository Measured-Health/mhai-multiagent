//go:build integration

package intel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

var (
	super, app *pgxpool.Pool // superuser for setup; the core login (mhai_app) for behaviour
	sharedNATS *testutil.NATS
	gwNC       *nats.Conn
	gwJS       jetstream.JetStream
	signer     reqtoken.Signer
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	code := func() int {
		pg, stopPG, err := testutil.NewPostgres(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start postgres:", err)
			return 1
		}
		defer stopPG()
		n, stopNATS, err := testutil.NewNATS(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start nats:", err)
			return 1
		}
		defer stopNATS()
		sharedNATS = n
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			return 1
		}
		if super, err = db.Open(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			return 1
		}
		defer super.Close()
		if app, err = testutil.AppLogin(ctx, pg, super); err != nil {
			fmt.Fprintln(os.Stderr, "app login:", err)
			return 1
		}
		defer app.Close()
		if gwNC, gwJS, err = bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{}); err != nil {
			fmt.Fprintln(os.Stderr, "nats connect:", err)
			return 1
		}
		defer gwNC.Close()
		if err := bus.EnsureStreams(ctx, gwJS, bus.StreamConfig{}); err != nil {
			fmt.Fprintln(os.Stderr, "streams:", err)
			return 1
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signer = reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(priv.Public().(ed25519.PublicKey))}
		return m.Run()
	}()
	os.Exit(code)
}

// logBuf is a log sink tests can read.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newClient(t *testing.T) (*Client, *logBuf) {
	t.Helper()
	logs := &logBuf{}
	return &Client{NC: gwNC, JS: gwJS, Signer: signer, Log: logx.New(logs)}, logs
}

func clinician(hid uuid.UUID) Caller {
	return Caller{Sub: uuid.NewString(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
}

func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

// seedHospital adds a running hospital.
func seedHospital(t *testing.T, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := super.QueryRow(context.Background(), `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic General', $1, $2) RETURNING id`,
		"H"+uniq(), status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedUser adds an account at hospital with roles.
func seedUser(t *testing.T, hospital uuid.UUID, active bool, roles ...string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, is_active)
		VALUES ($1, $2, 'synthetic-hash', 'Dr Synthetic', $3) RETURNING id`,
		hospital, "synthetic."+uniq()+"@havana.example", active).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// jobMsg is a copy of one message read off JOBS.
type jobMsg struct {
	subject string
	header  nats.Header
	data    []byte
}

// drainJobs reads and acks every waiting run_query job, as the pipeline
// worker's NATS user on its own durable.
func drainJobs(t *testing.T, url string) []jobMsg {
	t.Helper()
	ctx := context.Background()
	nc, js, err := bus.Connect(ctx, url, "", bus.WorkerInbox("pipeline"), bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	cons, err := js.Consumer(ctx, bus.JobsStream, bus.PipelineConsumer)
	if err != nil {
		t.Fatal(err)
	}
	var out []jobMsg
	for {
		batch, err := cons.Fetch(8, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for m := range batch.Messages() {
			n++
			out = append(out, jobMsg{subject: m.Subject(), header: m.Headers(), data: append([]byte(nil), m.Data()...)})
			if err := m.Ack(); err != nil {
				t.Fatal(err)
			}
		}
		if n == 0 {
			return out
		}
	}
}

func jobsFor(msgs []jobMsg, subject string) []jobMsg {
	var out []jobMsg
	for _, m := range msgs {
		if m.subject == subject {
			out = append(out, m)
		}
	}
	return out
}
