//go:build integration

package intel

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

// EMR plan Task 48 (PR 13): the pids claim carries only patient ids the
// database checked for the caller's hospital. Every value is synthetic.

// seedEMRPatient adds an MHAI patient of hospital with status, as the
// superuser; a merged patient is merged into into.
func seedEMRPatient(t *testing.T, hospital uuid.UUID, status string, into uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, 'Synthetic Ondine Marrable', $2, '1958-11-23', 'other') RETURNING id`,
		hospital, "SYN-PD-"+uuid.NewString()[:13]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	switch status {
	case "excluded":
		if _, err := super.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'other'
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

// checkIn runs CheckPatient for c and pid in a transaction bound to bound.
func checkIn(t *testing.T, bound uuid.UUID, c Caller, pid uuid.UUID) (Caller, error) {
	t.Helper()
	var out Caller
	err := db.WithHospital(context.Background(), app, bound, func(tx pgx.Tx) error {
		var err error
		out, err = CheckPatient(context.Background(), tx, c, pid)
		return err
	})
	return out, err
}

func TestPidsClaimOnlyCheckedIDs(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	a, b := seedHospital(t, "active"), seedHospital(t, "active")
	own := seedEMRPatient(t, a, "active", uuid.Nil)
	excluded := seedEMRPatient(t, a, "excluded", uuid.Nil)
	merged := seedEMRPatient(t, a, "merged_into", own)
	theirs := seedEMRPatient(t, b, "active", uuid.Nil)
	caller := clinician(a)
	subject := "jobs.v1." + a.String() + ".run_query"
	deadline := time.Now().Add(5 * time.Minute)

	// A Caller nobody checked carries no patient, so a body that names one
	// is refused before anything is signed.
	if got := caller.PatientIDs(); len(got) != 0 {
		t.Fatalf("unchecked caller carries %v", got)
	}
	q := uuid.New()
	if err := c.PublishJob(ctx, caller, "pipeline.run_query", q, &intelpb.RunQueryJob{QueryId: q.String(), PatientId: own.String()},
		deadline); !errors.Is(err, ErrMisuse) {
		t.Fatalf("unchecked patient in the body: %v", err)
	}
	// Another hospital's, an excluded, a merged or an unknown patient is
	// ErrNoPatient, all the same; the caller is unchanged.
	for name, pid := range map[string]uuid.UUID{"theirs": theirs, "excluded": excluded, "merged": merged, "unknown": uuid.New()} {
		got, err := checkIn(t, a, caller, pid)
		if !errors.Is(err, ErrNoPatient) || len(got.PatientIDs()) != 0 {
			t.Errorf("%s: %v %v", name, err, got.PatientIDs())
		}
	}
	// A transaction bound to another hospital can't check for this caller.
	if _, err := checkIn(t, b, caller, theirs); !errors.Is(err, ErrMisuse) {
		t.Errorf("check under another hospital: %v", err)
	}
	checked, err := checkIn(t, a, caller, own)
	if err != nil || !slices.Equal(checked.PatientIDs(), []string{own.String()}) {
		t.Fatalf("checked caller: %v %v", err, checked.PatientIDs())
	}
	if len(caller.PatientIDs()) != 0 {
		t.Fatal("CheckPatient changed its argument")
	}
	// Checked again: still one id.
	if again, err := checkIn(t, a, checked, own); err != nil || len(again.PatientIDs()) != 1 {
		t.Fatalf("checked twice: %v %v", err, again.PatientIDs())
	}
	// The checked caller can't name a different patient in the body.
	if err := c.PublishJob(ctx, checked, "pipeline.run_query", q, &intelpb.RunQueryJob{QueryId: q.String(), PatientId: theirs.String()},
		deadline); !errors.Is(err, ErrMisuse) {
		t.Fatalf("other patient in the body: %v", err)
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), subject)); n != 0 {
		t.Fatalf("%d jobs published by refused calls", n)
	}
	// The checked patient: the token's pids are exactly that id.
	if err := c.PublishJob(ctx, checked, "pipeline.run_query", q, &intelpb.RunQueryJob{QueryId: q.String(), PatientId: own.String()},
		deadline); err != nil {
		t.Fatalf("checked publish: %v", err)
	}
	ms := jobsFor(drainJobs(t, sharedNATS.PipelineURL), subject)
	if len(ms) != 1 {
		t.Fatalf("%d jobs", len(ms))
	}
	cl := verifyJob(t, ms[0])
	if !slices.Equal(cl.PatientIDs, []string{own.String()}) {
		t.Fatalf("pids %v", cl.PatientIDs)
	}
	// An unchecked caller's token has no pids at all.
	q2 := uuid.New()
	if err := c.PublishJob(ctx, caller, "pipeline.run_query", q2, &intelpb.RunQueryJob{QueryId: q2.String()}, deadline); err != nil {
		t.Fatal(err)
	}
	ms = jobsFor(drainJobs(t, sharedNATS.PipelineURL), subject)
	if len(ms) != 1 || len(verifyJob(t, ms[0]).PatientIDs) != 0 {
		t.Fatalf("unchecked token carries pids: %d jobs", len(ms))
	}
}

// seedPatientQuery is seedQuery for a question that names patient.
func seedPatientQuery(t *testing.T, hid, createdBy, patient uuid.UUID, deadline time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := super.Exec(context.Background(), `INSERT INTO clinical_queries
		(id, hospital_id, clinical_question, urgency, status, created_by, created_at, deadline_at, request_id, patient_id)
		VALUES ($1, $2, 'synthetic question', 'routine', 'queued', $3, now() - interval '20 seconds', $4, $5, $6)`,
		id, hid, createdBy, deadline, "rid-"+id.String(), patient); err != nil {
		t.Fatal(err)
	}
	return id
}

// The sweeper re-mints a question about a patient with that patient,
// checked again; a patient excluded since is no longer named (00033
// unlinked it), so the job carries no patient and no pids.
func TestSweeperRechecksPatient(t *testing.T) {
	c, _ := newClient(t)
	c.Patients = true
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Truncate(time.Second).Add(5 * time.Minute)
	stays := seedEMRPatient(t, hid, "active", uuid.Nil)
	leaves := seedEMRPatient(t, hid, "active", uuid.Nil)
	qStays := seedPatientQuery(t, hid, creator, stays, deadline)
	qLeaves := seedPatientQuery(t, hid, creator, leaves, deadline)
	if _, err := super.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'objection'
		WHERE id = $1`, leaves); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepOnce(ctx, app, c); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	msgs := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(msgs) != 2 {
		t.Fatalf("%d jobs, want 2", len(msgs))
	}
	for _, m := range msgs {
		var job intelpb.RunQueryJob
		if err := proto.Unmarshal(m.data, &job); err != nil {
			t.Fatal(err)
		}
		cl := verifyJob(t, m)
		switch job.QueryId {
		case qStays.String():
			if job.PatientId != stays.String() || !slices.Equal(cl.PatientIDs, []string{stays.String()}) {
				t.Errorf("stays: body %q pids %v", job.PatientId, cl.PatientIDs)
			}
		case qLeaves.String():
			if job.PatientId != "" || len(cl.PatientIDs) != 0 {
				t.Errorf("excluded patient still named: body %q pids %v", job.PatientId, cl.PatientIDs)
			}
		default:
			t.Errorf("unexpected job %s", job.QueryId)
		}
	}
}

// A row that still names a patient who is no longer active (a race the
// trigger closes, forced here) is never minted with it: Republish says
// ErrNoPatient and publishes nothing.
func TestRepublishRefusesInactivePatient(t *testing.T) {
	c, _ := newClient(t)
	c.Patients = true
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	gone := seedEMRPatient(t, hid, "excluded", uuid.Nil)
	row := JobRow{ID: uuid.New(), HospitalID: hid, CreatedBy: creator, RequestID: "rid-race",
		Deadline: time.Now().Add(5 * time.Minute), PatientID: gone}
	if err := Republish(ctx, app, c, row, row.ID.String()); !errors.Is(err, ErrNoPatient) {
		t.Fatalf("republish: %v", err)
	}
	if err := Publishable(ctx, app, c, row); !errors.Is(err, ErrNoPatient) {
		t.Fatalf("publishable: %v", err)
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 0 {
		t.Fatalf("%d jobs published", n)
	}
}

// With EMR switched off (Client.Patients false), a question saved while it
// was on is re-minted with no patient: none in the body, no pids, and the
// patient isn't looked up at all.
func TestRemintWithoutPatientWhenEMROff(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Truncate(time.Second).Add(5 * time.Minute)
	q := seedPatientQuery(t, hid, creator, seedEMRPatient(t, hid, "active", uuid.Nil), deadline)
	if _, err := SweepOnce(ctx, app, c); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	msgs := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(msgs) != 1 {
		t.Fatalf("%d jobs", len(msgs))
	}
	var job intelpb.RunQueryJob
	if err := proto.Unmarshal(msgs[0].data, &job); err != nil || job.QueryId != q.String() {
		t.Fatalf("job %v %v", &job, err)
	}
	if cl := verifyJob(t, msgs[0]); job.PatientId != "" || len(cl.PatientIDs) != 0 {
		t.Fatalf("EMR off, still named: body %q pids %v", job.PatientId, cl.PatientIDs)
	}
}
