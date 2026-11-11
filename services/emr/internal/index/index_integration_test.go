//go:build integration

package index

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
)

const mrnNS = "urn:synthetic:mrn"

func setup(t *testing.T) (*pgxpool.Pool, connector.Config, connector.Config) {
	t.Helper()
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	h := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', 'IDX1', 'active')`, h); err != nil {
		t.Fatal(err)
	}
	var cfgs []connector.Config
	for i, label := range []string{"Main record", "Second record"} {
		c := connector.Config{ConnectionID: uuid.New(), HospitalID: h, Type: "stub", Settings: map[string]string{"mrn_namespace": mrnNS}}
		if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings)
			VALUES ($1, $2, 'stub', $3, 'https://stub.example.org', $4)`, c.ConnectionID, h, label, map[string]any{"mrn_namespace": mrnNS}); err != nil {
			t.Fatal(i, err)
		}
		cfgs = append(cfgs, c)
	}
	return pool, cfgs[0], cfgs[1]
}

func date(s string) *time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return &d
}

func resolve(t *testing.T, pool *pgxpool.Pool, c connector.Config, src string, f *PatientFields) (Patient, int) {
	t.Helper()
	var p Patient
	var sigs int
	if err := dbx.WithHospital(context.Background(), pool, dbx.EMR, c.HospitalID, func(tx pgx.Tx) error {
		var err error
		if p, err = Resolve(context.Background(), tx, c, src, f, nil); err != nil {
			return err
		}
		s, err := Propose(context.Background(), tx, c, p.ID)
		sigs = len(s)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p, sigs
}

// A11: equal MRN, different birth date, across connections: two patients
// and a proposal, never one patient; within one connection the source id
// decides, and the same source id is the same patient.
func TestEqualMRNDifferentBirthDateTwoPatients(t *testing.T) {
	pool, c1, c2 := setup(t)
	a, s1 := resolve(t, pool, c1, "p-1", &PatientFields{MRN: "MRN-5", BirthDate: date("1970-01-01"), Sex: "female"})
	b, s2 := resolve(t, pool, c2, "p-1", &PatientFields{MRN: "mrn-5", BirthDate: date("1980-06-06"), Sex: "female"})
	if a.ID == b.ID || s1 != 0 || s2 != 1 {
		t.Fatalf("a %v b %v signals %d %d", a.ID, b.ID, s1, s2)
	}
	again, _ := resolve(t, pool, c1, "p-1", nil)
	if again.ID != a.ID {
		t.Fatal("the same source id resolved to another patient")
	}
	var reason string
	if err := pool.QueryRow(context.Background(), `SELECT reason FROM emr.possible_matches`).Scan(&reason); err != nil || reason != "mrn_only" {
		t.Fatalf("reason %q %v", reason, err)
	}
	// Equal on all three: the stronger reason, a separate proposal.
	c, s3 := resolve(t, pool, c2, "p-2", &PatientFields{MRN: "MRN-5", BirthDate: date("1970-01-01"), Sex: "female"})
	if c.ID == a.ID || s3 != 1 {
		t.Fatalf("third patient %v signals %d", c.ID, s3)
	}
	// Without a shared MRN namespace: nothing proposed.
	c1.Settings = map[string]string{}
	if _, s := resolve(t, pool, c1, "p-9", &PatientFields{MRN: "MRN-5"}); s != 0 {
		t.Errorf("proposed with no namespace: %d", s)
	}
}

// §10.2: names never match patients: equal names, no MRN, two patients
// and nothing proposed.
func TestNoNameMatching(t *testing.T) {
	pool, c1, c2 := setup(t)
	f := &PatientFields{DisplayName: "Synthetic Same-Name", BirthDate: date("1970-01-01"), Sex: "male"}
	a, s1 := resolve(t, pool, c1, "p-1", f)
	b, s2 := resolve(t, pool, c2, "p-2", f)
	if a.ID == b.ID || s1+s2 != 0 {
		t.Fatalf("a %v b %v signals %d", a.ID, b.ID, s1+s2)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM emr.possible_matches`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("matches %d %v", n, err)
	}
}
