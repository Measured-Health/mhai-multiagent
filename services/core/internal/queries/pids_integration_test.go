//go:build integration

package queries_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// EMR plan Task 48 (PR 13): a question may name an MHAI patient, checked
// against the caller's hospital. Every name and value is synthetic.

const (
	patientName = "Synthetic Perpetua Halloran"
	patientMRN  = "SYN-MRN-Q7X4-K2W9"
)

// newEMREnv is newEnv with CORE_EMR_ENABLED on.
func newEMREnv(t *testing.T, limit int) *env {
	t.Helper()
	e := &env{logs: &syncBuf{}}
	log := logx.New(e.logs)
	svc := &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer, Keys: issuer.PublicKeys(), Log: log,
		MFAKey: []byte(testMFAKey), Argon: password.NewGate(64, time.Minute)}
	guard := &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: log,
		Idem: httpx.IdempotentLog(app, log), IdemAnon: httpx.IdempotentAnonLog(app, log)}
	m := authz.NewMux(guard)
	client := &intel.Client{NC: gwNC, JS: gwJS, Signer: signer, Log: log}
	server.Register(m, server.Deps{Auth: svc, Pool: app, Intel: client, QueryCap: limit, EMREnabled: true})
	e.h = m.Handler([]string{serviceKey})
	return e
}

// seedPatient adds an MHAI patient of hid with status (merged patients
// are merged into into), as the superuser.
func seedPatient(t *testing.T, hid uuid.UUID, status string, into uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, $2, $3, '1966-02-19', 'female') RETURNING id`, hid, patientName, patientMRN).Scan(&id); err != nil {
		t.Fatal(err)
	}
	switch status {
	case "excluded":
		if _, err := super.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'objection'
			WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	case "merged_into":
		if _, err := super.Exec(ctx, `UPDATE emr.patients SET status = 'merged_into', merged_into = $2 WHERE id = $1`, id, into); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func questionAbout(pid string) string {
	var in map[string]any
	_ = json.Unmarshal([]byte(question("Synthetic question about this patient?")), &in)
	in["patient_id"] = pid
	b, _ := json.Marshal(in)
	return string(b)
}

// noTrace checks that a refused question left nothing: no row, no job, no
// query.submitted, and the patient's id, name and MRN in no log.
func noTrace(t *testing.T, e *env, hid uuid.UUID, pid string) {
	t.Helper()
	if n := countRows(t, hid); n != 0 {
		t.Errorf("%d rows", n)
	}
	if n := len(jobs(t, hid)); n != 0 {
		t.Errorf("%d jobs", n)
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'query.submitted' AND hospital_id = $1`,
		hid).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d query.submitted rows (%v)", n, err)
	}
	logs := e.logs.String()
	for _, s := range []string{pid, patientName, patientMRN} {
		if strings.Contains(logs, s) {
			t.Errorf("logs carry %q", s)
		}
	}
}

// Another hospital's patient, and a patient that doesn't exist, are both
// 404 not_found with the same body: existence never leaks.
func TestPatientIDOfOtherHospital404(t *testing.T) {
	e := newEMREnv(t, 10)
	a, b := seedHospital(t), seedHospital(t)
	tok := mint(t, seedUser(t, a, authz.RoleClinician))
	jobs(t, a)
	theirs := seedPatient(t, b, "active", uuid.Nil).String()
	var codes []string
	for _, pid := range []string{theirs, uuid.NewString()} {
		r := e.do(t, "POST", "/queries", tok, questionAbout(pid), nil)
		if r.code != http.StatusNotFound || r.body["code"] != "not_found" {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		delete(r.body, "request_id")
		b, _ := json.Marshal(r.body)
		codes = append(codes, string(b))
		noTrace(t, e, a, pid)
	}
	if codes[0] != codes[1] {
		t.Fatalf("another hospital's patient and no patient differ: %v", codes)
	}
}

// An excluded or merged patient is 404 not_found, and nothing is stored.
func TestExcludedOrMergedPatient404(t *testing.T) {
	e := newEMREnv(t, 10)
	hid := seedHospital(t)
	tok := mint(t, seedUser(t, hid, authz.RoleClinician))
	jobs(t, hid)
	keep := seedPatient(t, hid, "active", uuid.Nil)
	for _, status := range []string{"excluded", "merged_into"} {
		pid := seedPatient(t, hid, status, keep).String()
		r := e.do(t, "POST", "/queries", tok, questionAbout(pid), nil)
		if r.code != http.StatusNotFound || r.body["code"] != "not_found" {
			t.Fatalf("%s: %d %s", status, r.code, r.raw)
		}
		noTrace(t, e, hid, pid)
	}
}

// Off by default: with CORE_EMR_ENABLED false even the hospital's own
// active patient is 404 not_found, and nothing is stored.
func TestPatientIDWhenEMROff404(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	tok := mint(t, seedUser(t, hid, authz.RoleClinician))
	jobs(t, hid)
	pid := seedPatient(t, hid, "active", uuid.Nil).String()
	r := e.do(t, "POST", "/queries", tok, questionAbout(pid), nil)
	if r.code != http.StatusNotFound || r.body["code"] != "not_found" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	noTrace(t, e, hid, pid)
}

// A patient_id that isn't a canonical uuid is 422 validation_failed.
func TestPatientIDMalformed422(t *testing.T) {
	e := newEMREnv(t, 10)
	hid := seedHospital(t)
	tok := mint(t, seedUser(t, hid, authz.RoleClinician))
	jobs(t, hid)
	own := seedPatient(t, hid, "active", uuid.Nil).String()
	for _, pid := range []string{"", "not-a-uuid", strings.ToUpper(own), strings.ReplaceAll(own, "-", ""),
		"00000000-0000-0000-0000-000000000000", "{" + own + "}"} {
		r := e.do(t, "POST", "/queries", tok, questionAbout(pid), nil)
		if r.code != http.StatusUnprocessableEntity || r.body["code"] != "validation_failed" {
			t.Fatalf("%q: %d %s", pid, r.code, r.raw)
		}
	}
	noTrace(t, e, hid, own)
}

// The hospital's own active patient: 202, the row names it, the audit says
// a patient was named (never which), and the job's body and token carry
// exactly that checked id.
func TestPostQueriesWithPatient(t *testing.T) {
	e := newEMREnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleClinician)
	jobs(t, hid)
	pid := seedPatient(t, hid, "active", uuid.Nil).String()
	r := e.do(t, "POST", "/queries", mint(t, u), questionAbout(pid), nil)
	if r.code != http.StatusAccepted {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	id, _ := r.body["query_id"].(string)
	var stored *string
	if err := super.QueryRow(context.Background(), `SELECT patient_id::text FROM clinical_queries WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == nil || *stored != pid {
		t.Fatalf("row patient %v", stored)
	}
	var payload []byte
	if err := super.QueryRow(context.Background(), `SELECT payload FROM audit_log WHERE action = 'query.submitted' AND entity_id = $1`,
		id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal(payload, &p)
	if p["patient"] != true || len(p) != 3 || strings.Contains(string(payload), pid) {
		t.Fatalf("audit %s", payload)
	}
	ms := jobs(t, hid)
	if len(ms) != 1 {
		t.Fatalf("%d jobs", len(ms))
	}
	var job intelpb.RunQueryJob
	if err := proto.Unmarshal(ms[0].Data(), &job); err != nil || job.QueryId != id || job.PatientId != pid {
		t.Fatalf("job %v %v", &job, err)
	}
	cl, err := reqtoken.VerifyAt(ms[0].Headers().Get("Mhai-Request-Token"), signer.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: ms[0].Subject(), Kind: reqtoken.KindJob}, ms[0].Data(), time.Now())
	if err != nil || !slices.Equal(cl.PatientIDs, []string{pid}) {
		t.Fatalf("token pids %v %v", cl.PatientIDs, err)
	}
	logs := e.logs.String()
	for _, s := range []string{pid, patientName, patientMRN} {
		if strings.Contains(logs, s) {
			t.Errorf("logs carry %q", s)
		}
	}
	// A question with no patient still mints no pids.
	r = e.do(t, "POST", "/queries", mint(t, u), question("Synthetic question?"), nil)
	if r.code != http.StatusAccepted {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	ms = jobs(t, hid)
	if len(ms) != 1 {
		t.Fatalf("%d jobs", len(ms))
	}
	cl, err = reqtoken.VerifyAt(ms[0].Headers().Get("Mhai-Request-Token"), signer.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: ms[0].Subject(), Kind: reqtoken.KindJob}, ms[0].Data(), time.Now())
	if err != nil || len(cl.PatientIDs) != 0 {
		t.Fatalf("pids without a patient: %v %v", cl.PatientIDs, err)
	}
}
