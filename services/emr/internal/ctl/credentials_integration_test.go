//go:build integration

package ctl

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/stub"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Synthetic, obviously fake.
const credCanary = "synthetic-test-secret-not-real-0004"

var stubOrigin = []emrurl.Origin{"https://stub.example.test:443"}

type credFixture struct {
	pool     *pgxpool.Pool
	svc      *Credentials
	stub     *stub.Stub
	kp       seal.KeyPair
	dir      string
	hospital uuid.UUID
	conn     uuid.UUID
	now      time.Time
}

func newCredFixture(t *testing.T) *credFixture {
	t.Helper()
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	f := &credFixture{pool: pool, stub: stub.New(), hospital: uuid.New(), conn: uuid.New(),
		now: time.Now().UTC().Truncate(time.Second)}
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		f.hospital, "S"+f.hospital.String()[:6]); err != nil {
		t.Fatal(err)
	}
	f.seedConn(t, f.hospital, f.conn)
	kp, err := seal.GenerateKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.kp = kp
	f.dir = filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.NewFileStore(f.dir, "development")
	if err != nil {
		t.Fatal(err)
	}
	f.svc = &Credentials{Pool: pool, Store: store, Keys: seal.Keys{seal.KeyID(kp.Public[:]): kp}, Env: "development",
		Connectors: connector.NewRegistry(true, f.stub), Now: func() time.Time { return f.now }}
	return f
}

func (f *credFixture) seedConn(t *testing.T, hospital, id uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url,
		settings, credentials_origin) VALUES ($1, $2, 'stub', 'Main record', 'https://stub.example.test/api',
		'{"client_id": "synthetic-client"}', $3)`, id, hospital, emrurl.Strings(stubOrigin)); err != nil {
		t.Fatal(err)
	}
}

func (f *credFixture) claims(op string) reqtoken.Claims {
	return reqtoken.Claims{Aud: "emr", Sub: admin, Hid: f.hospital.String(), Roles: []string{"platform_admin"}, Rid: rid,
		Kind: reqtoken.KindReq, Subject: "emr.v1.ctl." + op, ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore,
		StepUp: op == "put_credentials"}
}

func (f *credFixture) box(t *testing.T, conn uuid.UUID, field, value string, origin []emrurl.Origin, at time.Time) *emrpb.SealedField {
	t.Helper()
	kid := seal.KeyID(f.kp.Public[:])
	b, err := seal.Seal(&f.kp.Public, kid, seal.Binding{ConnectionID: conn, Field: field, Origin: origin}, []byte(value), at)
	if err != nil {
		t.Fatal(err)
	}
	return &emrpb.SealedField{Field: field, Kid: kid, Box: b}
}

func (f *credFixture) put(t *testing.T, fields ...*emrpb.SealedField) (*emrpb.PutCredentialsReply, error) {
	t.Helper()
	out, err := f.svc.Put(context.Background(), f.claims("put_credentials"),
		&emrpb.PutCredentialsRequest{ConnectionId: f.conn.String(), Fields: fields})
	if err != nil {
		return nil, err
	}
	return out.(*emrpb.PutCredentialsReply), nil
}

func (f *credFixture) test(t *testing.T) *emrpb.TestReply {
	t.Helper()
	out, err := f.svc.Test(context.Background(), f.claims("test"), &emrpb.TestRequest{ConnectionId: f.conn.String()})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	return out.(*emrpb.TestReply)
}

// stored reports what the store and the metadata hold for the connection.
func (f *credFixture) stored(t *testing.T) (files, rows int) {
	t.Helper()
	_ = filepath.WalkDir(f.dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM emr.credentials WHERE connection_id = $1`, f.conn).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return files, rows
}

// A35, the emr side: one refused box among the fields stores nothing.
func TestPutCredentialsAllOrNothing(t *testing.T) {
	f := newCredFixture(t)
	other := uuid.New()
	for name, fields := range map[string][]*emrpb.SealedField{
		"a field the type doesn't have": {f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now),
			f.box(t, f.conn, "private_key", credCanary, stubOrigin, f.now)},
		"a box for another connection": {f.box(t, other, "api_key", credCanary, stubOrigin, f.now)},
		"a box for another origin": {f.box(t, f.conn, "api_key", credCanary,
			[]emrurl.Origin{"https://evil.example.org:443"}, f.now)},
		"a box 61 s old": {f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now.Add(-61*time.Second))},
		"the same field twice": {f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now),
			f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now)},
		"a kid that isn't the box's": func() []*emrpb.SealedField {
			b := f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now)
			b.Kid = "0000000000000000"
			return []*emrpb.SealedField{b}
		}(),
		"no fields": nil,
	} {
		if _, err := f.put(t, fields...); err == nil {
			t.Errorf("%s: stored", name)
		}
		if files, rows := f.stored(t); files != 0 || rows != 0 {
			t.Fatalf("%s: %d files, %d rows left", name, files, rows)
		}
	}
	// Another hospital's token can't reach the connection.
	c := f.claims("put_credentials")
	c.Hid = uuid.NewString()
	if _, err := f.svc.Put(context.Background(), c, &emrpb.PutCredentialsRequest{ConnectionId: f.conn.String(),
		Fields: []*emrpb.SealedField{f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now)}}); err == nil {
		t.Fatal("another hospital's token stored a credential")
	}
	if files, rows := f.stored(t); files != 0 || rows != 0 {
		t.Fatalf("another hospital: %d files, %d rows", files, rows)
	}
}

// A1, the emr side: the reply is each field and its set time; the value is
// in the store only, and the database row is metadata.
func TestPutCredentialsReplyHasSetAtOnly(t *testing.T) {
	f := newCredFixture(t)
	rep, err := f.put(t, f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.GetFields()) != 1 || rep.GetFields()[0].GetField() != "api_key" ||
		rep.GetFields()[0].GetSetAt() != f.now.Format(time.RFC3339) {
		t.Fatalf("reply %v", rep)
	}
	b, err := proto.Marshal(rep)
	if err != nil || bytes.Contains(b, []byte(credCanary)) {
		t.Fatalf("the reply carries the value (%v)", err)
	}
	var version, setBy string
	var origin []string
	if err := f.pool.QueryRow(context.Background(), `SELECT secret_version, set_by::text, origin FROM emr.credentials
		WHERE connection_id = $1 AND field = 'api_key'`, f.conn).Scan(&version, &setBy, &origin); err != nil {
		t.Fatal(err)
	}
	if setBy != admin || strings.Join(origin, ",") != string(stubOrigin[0]) || strings.Contains(version, credCanary) {
		t.Fatalf("metadata %q %q %v", version, setBy, origin)
	}
	var dump string
	if err := f.pool.QueryRow(context.Background(), `SELECT string_agg(row_to_json(c)::text, ' ') FROM emr.credentials c`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, credCanary) || strings.Contains(dump, "synthetic-test-secret") {
		t.Fatal("the database holds the value")
	}
	v, ver, err := f.svc.Store.Get(context.Background(), secrets.Path{Env: "development", Hospital: f.hospital,
		Connection: f.conn, Field: "api_key"})
	if err != nil || ver != version || string(v.Bytes()) != credCanary {
		t.Fatalf("store: %q %v", ver, err)
	}
	if r := f.test(t); !r.GetOk() || r.GetCode() != "ok" || f.stub.Calls() != 1 {
		t.Fatalf("test after put: %v calls %d", r, f.stub.Calls())
	}
}

// A28, the emr side: after a wipe, or once the saved addresses no longer
// give the origin the credentials were entered for, test answers
// sign_in_refused and the connector is never called.
func TestWipeThenTestSendsNothing(t *testing.T) {
	f := newCredFixture(t)
	ctx := context.Background()
	if _, err := f.put(t, f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now)); err != nil {
		t.Fatal(err)
	}
	out, err := f.svc.Wipe(ctx, f.claims("wipe_credentials"), &emrpb.WipeCredentialsRequest{ConnectionId: f.conn.String()})
	if err != nil || !out.(*emrpb.WipeCredentialsReply).GetWiped() {
		t.Fatalf("wipe: %v %v", out, err)
	}
	if files, rows := f.stored(t); files != 0 || rows != 0 {
		t.Fatalf("after wipe: %d files, %d rows", files, rows)
	}
	if r := f.test(t); r.GetOk() || r.GetCode() != "sign_in_refused" || f.stub.Calls() != 0 {
		t.Fatalf("test after wipe: %v calls %d", r, f.stub.Calls())
	}
	// Idempotent.
	if _, err := f.svc.Wipe(ctx, f.claims("wipe_credentials"), &emrpb.WipeCredentialsRequest{ConnectionId: f.conn.String()}); err != nil {
		t.Fatalf("second wipe: %v", err)
	}
	// Set again, then move the base address without the origin: the
	// credentials were entered for the old one.
	if _, err := f.put(t, f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE emr_connections SET base_url = 'https://new.example.test/api' WHERE id = $1`, f.conn); err != nil {
		t.Fatal(err)
	}
	if r := f.test(t); r.GetCode() != "sign_in_refused" || f.stub.Calls() != 0 {
		t.Fatalf("test after an address change: %v calls %d", r, f.stub.Calls())
	}
	// And with the origin moved too: the metadata's origin is the old one.
	if _, err := f.pool.Exec(ctx, `UPDATE emr_connections SET credentials_origin = '{https://new.example.test:443}' WHERE id = $1`, f.conn); err != nil {
		t.Fatal(err)
	}
	if r := f.test(t); r.GetCode() != "sign_in_refused" || f.stub.Calls() != 0 {
		t.Fatalf("test after an origin change: %v calls %d", r, f.stub.Calls())
	}
	// Another hospital's token: not found, no answer.
	c := f.claims("test")
	c.Hid = uuid.NewString()
	if _, err := f.svc.Test(ctx, c, &emrpb.TestRequest{ConnectionId: f.conn.String()}); err == nil {
		t.Fatal("another hospital's token tested the connection")
	}
}

// Design §17.4: an empty or placeholder secret is sign_in_refused without
// calling out.
func TestPlaceholderSecretRefused(t *testing.T) {
	f := newCredFixture(t)
	for _, v := range []string{"changeme", "CHANGE-ME", " placeholder "} {
		if _, err := f.put(t, f.box(t, f.conn, "api_key", v, stubOrigin, f.now)); err != nil {
			t.Fatal(err)
		}
		if r := f.test(t); r.GetOk() || r.GetCode() != "sign_in_refused" || f.stub.Calls() != 0 {
			t.Fatalf("%q: %v calls %d", v, r, f.stub.Calls())
		}
	}
}
