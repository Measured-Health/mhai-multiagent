//go:build integration

package emrctl_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/emrctl"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// TestConnectionLifecycleWithStub (plan Task 11, step 4): core's client
// against the real emr binary (built from ../emr, APP_ENV development, the
// stub connector), on one NATS and one Postgres. Sealed at core's edge,
// opened by the emr service only; step-up carried in the token; a wipe, or
// an origin the credentials weren't entered for, makes the test
// sign_in_refused with nothing sent. Synthetic everything.
func TestConnectionLifecycleWithStub(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the emr binary")
	}
	ctx := context.Background()
	dir := t.TempDir()

	// The emr binary.
	bin := filepath.Join(dir, "emr")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = filepath.Join("..", "..", "..", "emr", "cmd", "emr")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build emr: %v\n%s", err, out)
	}

	pg := testutil.StartPostgres(t)
	if err := db.MigrateUp(ctx, pg.URL); err != nil {
		t.Fatal(err)
	}
	super, err := db.Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer super.Close()
	const emrPW = "test-only-emr-login-password-0001"
	if err := db.EnsureLogin(ctx, super, "emr_app", emrPW, "mhai_emr_app"); err != nil {
		t.Fatal(err)
	}
	dbURL, _ := url.Parse(pg.URL)
	dbURL.User = url.UserPassword("emr_app", emrPW)

	n, stopNATS, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stopNATS()
	natsURL, _ := url.Parse(n.EMRURL)
	natsPW, _ := natsURL.User.Password()
	natsURL.User = nil

	// Core's signing key, published to the emr service's trusted keys.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(priv.Public().(ed25519.PublicKey))}
	trusted := filepath.Join(dir, "trusted.json")
	if err := reqtoken.WriteTrusted(trusted, signer.PublicKeys()); err != nil {
		t.Fatal(err)
	}
	// The sealing key pair, made by the binary itself.
	keys := filepath.Join(dir, "seal")
	secrets := filepath.Join(dir, "secrets")
	for _, d := range []string{keys, secrets} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command(bin, "keys", "seal-init", "--out", keys).CombinedOutput(); err != nil {
		t.Fatalf("seal-init: %v\n%s", err, out)
	}
	pubs, _ := filepath.Glob(filepath.Join(keys, "seal-*.pub"))
	if len(pubs) != 1 {
		t.Fatalf("pub files %v", pubs)
	}
	sealer, err := emrctl.LoadSealer(pubs[0])
	if err != nil {
		t.Fatal(err)
	}

	var logs lockedBuffer
	serve := exec.Command(bin, "serve")
	serve.Env = []string{"APP_ENV=development", "EMR_NATS_URL=" + natsURL.String(), "EMR_NATS_PASSWORD=" + natsPW,
		"EMR_DATABASE_URL=" + dbURL.String(), "EMR_TRUSTED_KEYS_PATH=" + trusted, "EMR_SEAL_KEY_PATH=" + keys,
		"EMR_SECRETS_DIR=" + secrets}
	serve.Stdout, serve.Stderr = &logs, &logs
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serve.Process.Kill(); _ = serve.Wait() }()

	nc, _, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	client := &emrctl.Client{NC: nc, Signer: signer}

	hid, admin := uuid.New(), uuid.New()
	if _, err := super.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic General', $2, 'active')`,
		hid, "L"+hid.String()[:6]); err != nil {
		t.Fatal(err)
	}
	// Health as svc:core, once the service is up.
	deadline := time.Now().Add(60 * time.Second)
	for {
		var h emrpb.HealthReply
		err := client.Request(ctx, emrctl.System(hid.String(), "rid-health"), "health", &emrpb.HealthRequest{}, &h)
		if err == nil {
			if !h.GetDatabaseReady() || !h.GetSecretStoreReady() {
				t.Fatalf("health %v", &h)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emr service never answered: %v\n%s", err, logs.String())
		}
		time.Sleep(500 * time.Millisecond)
	}

	conn := uuid.New()
	origin := []emrurl.Origin{"https://stub.example.org:443"}
	if _, err := super.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, credentials_origin)
		VALUES ($1, $2, 'stub', 'Main record', 'https://stub.example.org/api', '{"client_id": "synthetic-client"}', $3)`,
		conn, hid, emrurl.Strings(origin)); err != nil {
		t.Fatal(err)
	}
	caller := func(stepUp bool) emrctl.Caller {
		return emrctl.Caller{Sub: admin.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString()[:8],
			Roles: []string{"platform_admin"}, StepUp: stepUp}
	}
	const canary = "synthetic-test-secret-not-real-0007"
	put := func(o []emrurl.Origin) error {
		f, err := sealer.Seal(conn, "api_key", o, []byte(canary))
		if err != nil {
			t.Fatal(err)
		}
		var rep emrpb.PutCredentialsReply
		return client.Request(ctx, caller(true), "put_credentials",
			&emrpb.PutCredentialsRequest{ConnectionId: conn.String(), Fields: []*emrpb.SealedField{f}}, &rep)
	}
	test := func() string {
		var rep emrpb.TestReply
		if err := client.Request(ctx, caller(false), "test", &emrpb.TestRequest{ConnectionId: conn.String()}, &rep); err != nil {
			t.Fatalf("test: %v", err)
		}
		return rep.GetCode()
	}

	if got := test(); got != "sign_in_refused" {
		t.Fatalf("test before credentials: %q", got)
	}
	if err := put(origin); err != nil {
		t.Fatalf("put: %v\n%s", err, logs.String())
	}
	if got := test(); got != "ok" {
		t.Fatalf("test after put: %q", got)
	}
	// A box for another origin is refused: no answer.
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	f, _ := sealer.Seal(conn, "api_key", []emrurl.Origin{"https://evil.example.org:443"}, []byte(canary))
	var rep emrpb.PutCredentialsReply
	err = client.Request(short, caller(true), "put_credentials",
		&emrpb.PutCredentialsRequest{ConnectionId: conn.String(), Fields: []*emrpb.SealedField{f}}, &rep)
	cancel()
	if !errors.Is(err, emrctl.ErrTimeout) {
		t.Fatalf("a box for another origin: %v", err)
	}
	// The address moves (the routes switch it off and wipe first): the
	// credentials aren't for it.
	if _, err := super.Exec(ctx, `UPDATE emr_connections SET base_url = 'https://new-host.example.org/api',
		credentials_origin = '{https://new-host.example.org:443}' WHERE id = $1`, conn); err != nil {
		t.Fatal(err)
	}
	if got := test(); got != "sign_in_refused" {
		t.Fatalf("test after an origin change: %q", got)
	}
	var wiped emrpb.WipeCredentialsReply
	if err := client.Request(ctx, caller(false), "wipe_credentials", &emrpb.WipeCredentialsRequest{ConnectionId: conn.String()}, &wiped); err != nil || !wiped.GetWiped() {
		t.Fatalf("wipe: %v", err)
	}
	var rows int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM emr.credentials WHERE connection_id = $1`, conn).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("metadata after wipe: %d %v", rows, err)
	}
	if err := put([]emrurl.Origin{"https://new-host.example.org:443"}); err != nil {
		t.Fatalf("put for the new origin: %v", err)
	}
	if got := test(); got != "ok" {
		t.Fatalf("test with the new credentials: %q", got)
	}
	// Nothing of the credential in the emr service's log or the database.
	if strings.Contains(logs.String(), canary) {
		t.Fatal("the emr service logged the credential")
	}
	var leaked int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM emr.credentials c WHERE row_to_json(c)::text LIKE '%' || $1 || '%'`, canary).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("database: %d %v", leaked, err)
	}
}

// lockedBuffer is the emr process's output: written by exec's copier,
// read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
