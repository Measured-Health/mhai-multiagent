//go:build integration

package hl7bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/fileexport"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

type nopPub struct{}

func (nopPub) Publish(context.Context, string, []byte, string) error { return nil }

func emrDir(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

// The bridge to the canonical tables: MLLP → the spool → the bridge's SFTP
// → the hl7v2_file connector → the sink → the normaliser: a critical
// potassium sent over MLLP becomes a standardised observation of this
// hospital's patient, once, however often the engine resends it.
func TestBridgeToNormalisedRecords(t *testing.T) {
	_, pool := testpg.Migrated(t)
	c := context.Background()
	s := newSpool(t)
	_, addr := startReceiver(t, s, "127.0.0.1", &logBuf{})
	now := time.Now().UTC().Truncate(time.Second)
	k := hl7test.Result{Code: "99902-9", System: "LN", Name: "Potassium", Type: "NM", Value: "6.9", Unit: "mmol/L",
		Range: "3.5-5.1", Flag: "HH", Status: "F", At: now.Add(-10 * time.Minute)}
	msg := []byte(hl7test.ORU("SYN-CTRL-1", "SYN-ORD-1", now.Add(-5*time.Minute), k))
	mc := dialMLLP(t, addr)
	for i := 0; i < 3; i++ { // the engine resends
		if got := mc.send(t, msg); got != AckAccept {
			t.Fatal(got)
		}
	}
	if err := s.Rotate(0); err != nil {
		t.Fatal(err)
	}
	_, ck, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(ck, "")
	f := newSFTPWith(t, s, ck)
	hid, cid := uuid.New(), uuid.New()
	base := "sftp://bridge.synthetic-hospital.example"
	settings := map[string]string{"folder": Folder, "username": "synthetic-mhai", "expected_every_minutes": "60",
		"host_key_fingerprint": ssh.FingerprintSHA256(f.srv.HostKey.PublicKey()), "mrn_namespace": "urn:synthetic:mrn",
		"mrn_identifier_type": "MR"}
	origins, err := emrurl.OriginsOf(fileexport.HL7ID, base, settings, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(c, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		hid, "B"+hid.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(c, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings,
		credentials_origin, enabled, poll_seconds) VALUES ($1, $2, 'hl7v2_file', 'Lab messages', $3, $4, $5, true, 300)`,
		cid, hid, base, settings, emrurl.Strings(origins)); err != nil {
		t.Fatal(err)
	}
	cfg := connector.Config{ConnectionID: cid, HospitalID: hid, Type: fileexport.HL7ID, BaseURL: base, PollSeconds: 300,
		Origin: origins, Settings: settings}
	conn := fileexport.NewHL7(func(ctx context.Context, _ connector.Config, _ emrurl.Origin, fp, user string,
		auth []ssh.AuthMethod) (*ssh.Client, error) {
		return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{User: user, Auth: auth, Timeout: 5 * time.Second,
			HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
				if ssh.FingerprintSHA256(k) != fp {
					return errors.New("host key")
				}
				return nil
			}})
	})
	tables, err := codes.Load(emrDir("codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(emrDir("maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	n := &normalise.Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: nopPub{}}
	sk := sink.New(pool, hid, cid, fileexport.HL7ID, 1, settings["mrn_namespace"], func(ctx context.Context, h uuid.UUID, ids []uuid.UUID) error {
		return n.Handle(ctx, h, ids)
	})
	creds := connector.NewCreds(map[string]seal.Secret{fileexport.SecretField: seal.NewSecret(pem.EncodeToMemory(blk))})
	ctx, cancel := context.WithTimeout(c, time.Minute)
	defer cancel()
	if _, err := conn.Fetch(ctx, cfg, creds, nil, sk); err != nil {
		t.Fatal(err)
	}
	if err := sk.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var obs int
	var mrn string
	if err := pool.QueryRow(c, `SELECT (SELECT count(*) FROM emr.observations WHERE hospital_id = $1 AND loinc = '99902-9'
		AND value_num = 6.9 AND standardised), (SELECT coalesce(max(mrn), '') FROM emr.patients WHERE hospital_id = $1)`, hid).
		Scan(&obs, &mrn); err != nil {
		t.Fatal(err)
	}
	if obs != 1 || mrn != hl7test.MRN {
		t.Fatalf("observations %d, mrn %q", obs, mrn)
	}
}
