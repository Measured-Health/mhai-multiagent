//go:build integration

package normalise

import (
	"context"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// synthConn is the connection id of maps/testdata/synthetic/fhir.yaml.
var synthConn = uuid.MustParse("00000000-0000-4000-8000-00000000f41a")

const mrnNS = "urn:synthetic:mrn"

type published struct {
	subject, msgID string
	data           []byte
}

type capture struct {
	mu   sync.Mutex
	msgs []published
	fail bool
}

func (c *capture) Publish(_ context.Context, subject string, data []byte, msgID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return context.DeadlineExceeded
	}
	c.msgs = append(c.msgs, published{subject, msgID, data})
	return nil
}

func (c *capture) on(suffix string) []published {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []published
	for _, m := range c.msgs {
		if strings.HasSuffix(m.subject, suffix) {
			out = append(out, m)
		}
	}
	return out
}

type fixture struct {
	pool     *pgxpool.Pool
	n        *Normaliser
	pub      *capture
	hospital uuid.UUID
	conn     uuid.UUID
	now      time.Time
}

func repoDir(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, pool := testpg.Migrated(t)
	tables, err := codes.Load(repoDir("codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(repoDir("maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, pub: &capture{}, hospital: uuid.New(), conn: synthConn,
		now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	f.n = &Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: f.pub, Now: func() time.Time { return f.now }}
	f.addHospital(t, f.hospital, f.conn)
	return f
}

func (f *fixture) addHospital(t *testing.T, h, conn uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		h, "N"+h.String()[:6]); err != nil {
		t.Fatal(err)
	}
	f.addConn(t, h, conn, "Main record")
}

func (f *fixture) addConn(t *testing.T, h, conn uuid.UUID, label string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, enabled)
		VALUES ($1, $2, 'stub', $3, 'https://stub.example.org', $4, true)`, conn, h, label,
		map[string]any{"mrn_namespace": mrnNS}); err != nil {
		t.Fatal(err)
	}
}

// put stores records through the sink and returns their raw ids.
func (f *fixture) put(t *testing.T, h, conn uuid.UUID, recs ...connector.RawRecord) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	s := sink.New(f.pool, h, conn, "stub", 1, mrnNS, func(_ context.Context, _ uuid.UUID, b []uuid.UUID) error {
		ids = append(ids, b...)
		return nil
	})
	for _, r := range recs {
		if err := s.Put(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ids
}

func (f *fixture) normalise(t *testing.T, ids []uuid.UUID) {
	t.Helper()
	if err := f.n.Handle(context.Background(), f.hospital, ids); err != nil {
		t.Fatalf("normalise: %v", err)
	}
}

func patientRec(id, mrn, birth, sex string) connector.RawRecord {
	return connector.RawRecord{Kind: "patient", SourceID: id, SourceVersion: "1", Fields: map[string]any{
		"given": "Synthetic", "family": "Patient " + id, "birth_date": birth, "sex": sex,
		"identifier": []connector.Coding{{System: mrnNS, Code: mrn}}}}
}

func obsRec(id, version, patient string, fields map[string]any) connector.RawRecord {
	f := map[string]any{"patient": patient, "category": "laboratory", "status": "final",
		"codings":        []connector.Coding{{System: "urn:synthetic:lab", Code: "GLU", Display: "Glucose"}},
		"value_quantity": "180", "unit": "mg/dl", "effective": "2026-11-07T08:00:00+01:00"}
	for k, v := range fields {
		f[k] = v
	}
	return connector.RawRecord{Kind: "observation", SourceID: id, SourceVersion: version, Fields: f}
}

type obsRow struct {
	loinc, comparator, valueText, unit, status *string
	valueNum, valueSI                          *float64
	effAt                                      *time.Time
	effDate                                    *time.Time
	standardised                               bool
	superseded                                 *uuid.UUID
	mapping, normaliser                        string
}

func (f *fixture) obs(t *testing.T, sourceID string) []obsRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT loinc, comparator, value_text, unit_ucum, status, value_num::float8,
		value_si::float8, effective_at, effective_date, standardised, superseded_by, mapping_version, normaliser_version
		FROM emr.observations WHERE hospital_id = $1 AND source_id = $2 ORDER BY created_at`, f.hospital, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (obsRow, error) {
		var o obsRow
		return o, r.Scan(&o.loinc, &o.comparator, &o.valueText, &o.unit, &o.status, &o.valueNum, &o.valueSI, &o.effAt,
			&o.effDate, &o.standardised, &o.superseded, &o.mapping, &o.normaliser)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) rawStatus(t *testing.T, id uuid.UUID) (string, string, bool) {
	t.Helper()
	var status string
	var code *string
	var hasPayload bool
	if err := f.pool.QueryRow(context.Background(), `SELECT status, reject_code, payload IS NOT NULL FROM emr.raw_records WHERE id = $1`, id).
		Scan(&status, &code, &hasPayload); err != nil {
		t.Fatal(err)
	}
	if code == nil {
		return status, "", hasPayload
	}
	return status, *code, hasPayload
}

// The whole path for one observation: mapped by the connection's map,
// glucose converted by the table's factor, the offset applied, provenance
// recorded, announced on records.v1.<hid>.created by id only.
func TestObservationNormalised(t *testing.T) {
	f := newFixture(t)
	ids := f.put(t, f.hospital, f.conn, patientRec("p-1", "mrn 0001", "1970-01-01", "female"), obsRec("o-1", "1", "p-1", nil))
	f.normalise(t, ids)
	o := f.obs(t, "o-1")
	if len(o) != 1 || o[0].loinc == nil || *o[0].loinc != "99901-1" || o[0].valueSI == nil || math.Abs(*o[0].valueSI-180*0.0555) > 1e-9 ||
		*o[0].unit != "mg/dL" || !o[0].standardised || !o[0].effAt.Equal(time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)) ||
		o[0].mapping != f.n.Maps.Version() || o[0].normaliser != Version {
		t.Fatalf("observation %+v", o)
	}
	var mrn, name string
	if err := f.pool.QueryRow(context.Background(), `SELECT mrn, display_name FROM emr.patients WHERE hospital_id = $1`, f.hospital).Scan(&mrn, &name); err != nil ||
		mrn != "MRN0001" || name != "Synthetic Patient p-1" {
		t.Fatalf("patient %q %q %v", mrn, name, err)
	}
	created := f.pub.on(".created")
	if len(created) != 1 || created[0].subject != "records.v1."+f.hospital.String()+".created" {
		t.Fatalf("created %v", created)
	}
	var c emrpb.Created
	if err := proto.Unmarshal(created[0].data, &c); err != nil || len(c.RecordIds) != 1 {
		t.Fatalf("created body %v %v", &c, err)
	}
	for _, id := range ids {
		if s, _, _ := f.rawStatus(t, id); s != "normalised" {
			t.Errorf("raw %v: %s", id, s)
		}
	}
	// Redelivered: nothing new, the same id announced again (de-duplicated
	// by Nats-Msg-Id).
	f.normalise(t, ids)
	if o := f.obs(t, "o-1"); len(o) != 1 {
		t.Fatalf("redelivery added rows: %d", len(o))
	}
}

// Review Focus 1 and 3 through the normaliser: a date-only time is a date;
// a comparator stays text; a time without an offset, one in the future and
// a half-readable number reject the record (unreadable_data), nothing
// stored.
func TestNormaliserRefusesWhatItCantRead(t *testing.T) {
	f := newFixture(t)
	ids := f.put(t, f.hospital, f.conn,
		obsRec("o-date", "1", "p-1", map[string]any{"effective": "2026-11-07"}),
		obsRec("o-cmp", "1", "p-1", map[string]any{"value_quantity": "<0.5"}),
		obsRec("o-nooffset", "1", "p-1", map[string]any{"effective": "2026-11-07T08:00:00"}),
		obsRec("o-future", "1", "p-1", map[string]any{"effective": "2026-11-07T12:11:00Z"}),
		obsRec("o-soon", "1", "p-1", map[string]any{"effective": "2026-11-07T12:09:00Z"}),
		obsRec("o-comma", "1", "p-1", map[string]any{"value_quantity": "5,5"}),
		obsRec("o-unit", "1", "p-1", map[string]any{"value_quantity": "6.5 mmol/l"}),
		obsRec("o-scope", "1", "p-1", map[string]any{"category": "social-history"}),
		obsRec("o-nopatient", "1", "p-1", map[string]any{"patient": nil}),
	)
	f.normalise(t, ids)
	d := f.obs(t, "o-date")
	if len(d) != 1 || d[0].effAt != nil || d[0].effDate == nil || d[0].effDate.Format("2006-01-02") != "2026-11-07" {
		t.Fatalf("date-only %+v", d)
	}
	c := f.obs(t, "o-cmp")
	if len(c) != 1 || c[0].valueNum != nil || c[0].valueSI != nil || *c[0].comparator != "<" || *c[0].valueText != "<0.5" {
		t.Fatalf("comparator %+v", c)
	}
	if len(f.obs(t, "o-soon")) != 1 {
		t.Error("9 minutes ahead refused")
	}
	for src, code := range map[string]string{"o-nooffset": "unreadable_data", "o-future": "unreadable_data",
		"o-comma": "unreadable_data", "o-unit": "unreadable_data", "o-scope": "out_of_scope", "o-nopatient": "no_patient"} {
		if len(f.obs(t, src)) != 0 {
			t.Errorf("%s stored", src)
		}
		var id uuid.UUID
		if err := f.pool.QueryRow(context.Background(), `SELECT id FROM emr.raw_records WHERE source_id = $1`, src).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if s, got, _ := f.rawStatus(t, id); s != "rejected" || got != code {
			t.Errorf("%s: %s %s, want rejected %s", src, s, got, code)
		}
	}
}

// §9.1: an unknown code is kept unstandardised and counted, never guessed:
// a display equal to a known analyte's doesn't map, and neither does a
// deprecated LOINC; an unknown unit leaves no value_si.
func TestUnmappedStoredFlagged(t *testing.T) {
	f := newFixture(t)
	ids := f.put(t, f.hospital, f.conn,
		obsRec("o-unk", "1", "p-1", map[string]any{"codings": []connector.Coding{{System: "urn:synthetic:lab", Code: "XYZ", Display: "Glucose [synthetic]"}}}),
		obsRec("o-unk2", "1", "p-1", map[string]any{"codings": []connector.Coding{{System: "urn:synthetic:lab", Code: "XYZ", Display: "Glucose [synthetic]"}}}),
		obsRec("o-dep", "1", "p-1", map[string]any{"codings": []connector.Coding{{System: codes.LOINCSystem, Code: "99911-0"}}}),
		obsRec("o-unit", "1", "p-1", map[string]any{"unit": "milligrams per decilitre"}),
		obsRec("o-loinc", "1", "p-1", map[string]any{"codings": []connector.Coding{{System: codes.LOINCSystem, Code: "99902-9"}}, "value_quantity": "18", "unit": "mg/dL"}),
	)
	f.normalise(t, ids)
	for _, src := range []string{"o-unk", "o-dep"} {
		o := f.obs(t, src)
		if len(o) != 1 || o[0].loinc != nil || o[0].standardised || o[0].valueSI != nil {
			t.Errorf("%s: %+v", src, o)
		}
	}
	if o := f.obs(t, "o-unit"); len(o) != 1 || o[0].unit != nil || o[0].valueSI != nil || o[0].standardised {
		t.Errorf("unknown unit: %+v", o)
	}
	// A9: potassium in mg/dL has no reviewed conversion: no value_si.
	if o := f.obs(t, "o-loinc"); len(o) != 1 || *o[0].loinc != "99902-9" || o[0].valueSI != nil || o[0].standardised {
		t.Errorf("no conversion: %+v", o)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count FROM emr.unmapped_codes WHERE connection_id = $1 AND local_code = 'XYZ'`, f.conn).Scan(&count); err != nil || count != 2 {
		t.Errorf("unmapped XYZ count %d %v", count, err)
	}
	var units int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM emr.unmapped_codes WHERE connection_id = $1 AND kind = 'unit'`, f.conn).Scan(&units); err != nil || units != 1 {
		t.Errorf("unmapped units %d %v", units, err)
	}
}

// §8.2: a new source version is a new row; the old one is superseded,
// never changed; a late older version is stored already superseded;
// entered-in-error marks the current row.
func TestAmendedIsNewVersion(t *testing.T) {
	f := newFixture(t)
	at := func(h int) time.Time { return time.Date(2026, 11, 7, h, 0, 0, 0, time.UTC) }
	r1 := obsRec("o-1", "1", "p-1", map[string]any{"value_quantity": "100"})
	r1.SourceUpdatedAt = at(8)
	f.normalise(t, f.put(t, f.hospital, f.conn, r1))
	r3 := obsRec("o-1", "3", "p-1", map[string]any{"value_quantity": "300", "status": "amended"})
	r3.SourceUpdatedAt = at(10)
	f.normalise(t, f.put(t, f.hospital, f.conn, r3))
	r2 := obsRec("o-1", "2", "p-1", map[string]any{"value_quantity": "200", "status": "amended"})
	r2.SourceUpdatedAt = at(9)
	f.normalise(t, f.put(t, f.hospital, f.conn, r2))
	o := f.obs(t, "o-1")
	if len(o) != 3 {
		t.Fatalf("rows %d, want 3", len(o))
	}
	current := 0
	for _, r := range o {
		if r.superseded == nil {
			current++
			if *r.valueNum != 300 {
				t.Errorf("current value %v, want the newest (300)", *r.valueNum)
			}
		}
	}
	if current != 1 || o[0].superseded == nil || *o[0].valueNum != 100 {
		t.Fatalf("versions %+v", o)
	}
	r4 := obsRec("o-1", "4", "p-1", map[string]any{"status": "entered-in-error"})
	r4.SourceUpdatedAt = at(11)
	f.normalise(t, f.put(t, f.hospital, f.conn, r4))
	o = f.obs(t, "o-1")
	if len(o) != 3 {
		t.Fatalf("entered-in-error added a row: %d", len(o))
	}
	for _, r := range o {
		if r.superseded == nil && *r.status != "entered-in-error" {
			t.Errorf("current status %s", *r.status)
		}
	}
}

// §10.6: once excluded, a patient's new records are dropped (payload
// cleared), its Patient resource doesn't refill its name, and the link
// stays.
func TestExcludedPatientDropped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.normalise(t, f.put(t, f.hospital, f.conn, patientRec("p-1", "MRN-1", "1970-01-01", "male"), obsRec("o-1", "1", "p-1", nil)))
	var pid uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT patient_id FROM emr.patient_links WHERE source_patient_id = 'p-1'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SET LOCAL ROLE mhai_gateway`, `SELECT set_config('mhai.hospital_id', '` + f.hospital.String() + `', true)`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT * FROM emr_exclude_patient($1, 'objection')`, pid); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	ids := f.put(t, f.hospital, f.conn, obsRec("o-2", "1", "p-1", nil), patientRec("p-1", "MRN-1", "1970-01-01", "male"))
	patientV2 := patientRec("p-1", "MRN-1", "1970-01-01", "male")
	patientV2.SourceVersion = "2"
	ids = append(ids, f.put(t, f.hospital, f.conn, patientV2)...)
	f.normalise(t, ids)
	if len(f.obs(t, "o-2")) != 0 || len(f.obs(t, "o-1")) != 0 {
		t.Fatal("an excluded patient's observation is held")
	}
	for _, id := range ids {
		if s, code, payload := f.rawStatus(t, id); s != "rejected" || code != "excluded" || payload {
			t.Errorf("raw %v: %s %s payload %v", id, s, code, payload)
		}
	}
	var name *string
	var links int
	if err := f.pool.QueryRow(ctx, `SELECT display_name, (SELECT count(*) FROM emr.patient_links WHERE patient_id = $1)
		FROM emr.patients WHERE id = $1`, pid).Scan(&name, &links); err != nil || name != nil || links != 1 {
		t.Fatalf("excluded patient name %v links %d %v", name, links, err)
	}
}

// A13, message part: hospital A's subject naming B's raw ids normalises
// nothing (RLS hides them) and is acked, counted.
func TestFetchedForAnotherHospitalRefused(t *testing.T) {
	f := newFixture(t)
	b, cb := uuid.New(), uuid.New()
	f.addHospital(t, b, cb)
	idsB := f.put(t, b, cb, obsRec("o-b", "1", "p-b", nil))
	data, _ := proto.Marshal(&emrpb.Fetched{RawIds: []string{idsB[0].String()}})
	m := &fakeMsg{subject: "emr.v1." + f.hospital.String() + ".fetched", data: data, deliveries: 1}
	f.n.HandleMsg(context.Background(), m)
	if !m.acked || m.naked || m.termed {
		t.Fatalf("settled %+v", m)
	}
	if f.n.metrics().Refused.Load() != 1 {
		t.Errorf("refused %d", f.n.metrics().Refused.Load())
	}
	if s, _, _ := f.rawStatus(t, idsB[0]); s != "pending" {
		t.Errorf("B's record %s", s)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM emr.observations`).Scan(&n); err != nil || n != 0 {
		t.Errorf("observations %d %v", n, err)
	}
	for _, subj := range []string{"emr.v1.not-a-uuid.fetched", "emr.v1." + f.hospital.String() + ".finding", "emr.v1." + strings.ToUpper(f.hospital.String()) + ".fetched"} {
		m := &fakeMsg{subject: subj, data: data, deliveries: 1}
		f.n.HandleMsg(context.Background(), m)
		if !m.acked {
			t.Errorf("%s not acked", subj)
		}
	}
}

// §15: a raw record whose announcement was refused is announced by the
// sweep once it is two minutes old, and only once.
func TestSweepRepublishes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := sink.New(f.pool, f.hospital, f.conn, "stub", 1, mrnNS, func(context.Context, uuid.UUID, []uuid.UUID) error {
		return context.DeadlineExceeded
	})
	if err := s.Put(ctx, obsRec("o-1", "1", "p-1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err == nil {
		t.Fatal("refused publish not reported")
	}
	f.now = time.Now()
	if err := f.n.Sweep(ctx); err != nil || len(f.pub.on(".fetched")) != 0 {
		t.Fatalf("swept a fresh row: %v %d", err, len(f.pub.on(".fetched")))
	}
	f.now = time.Now().Add(3 * time.Minute)
	if err := f.n.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	fetched := f.pub.on(".fetched")
	if len(fetched) != 1 || fetched[0].subject != "emr.v1."+f.hospital.String()+".fetched" {
		t.Fatalf("fetched %v", fetched)
	}
	if err := f.n.Sweep(ctx); err != nil || len(f.pub.on(".fetched")) != 1 {
		t.Fatalf("swept twice: %v %d", err, len(f.pub.on(".fetched")))
	}
}

// A11 through the normaliser: two connections, equal MRN, different birth
// dates: two patients and one proposal (mrn_only), with a signal; equal
// MRN, birth date and sex: mrn_birth_sex. Names never matter.
func TestEqualMRNAcrossConnectionsProposes(t *testing.T) {
	f := newFixture(t)
	c2 := uuid.New()
	f.addConn(t, f.hospital, c2, "Second record")
	f.normalise(t, f.put(t, f.hospital, f.conn, patientRec("p-1", "MRN-9", "1970-01-01", "female")))
	f.normalise(t, f.put(t, f.hospital, c2, patientRec("x-1", " mrn-9", "1971-02-02", "female")))
	var patients, matches int
	var reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM emr.patients WHERE hospital_id = $1),
		(SELECT count(*) FROM emr.possible_matches WHERE hospital_id = $1), (SELECT max(reason) FROM emr.possible_matches WHERE hospital_id = $1)`,
		f.hospital).Scan(&patients, &matches, &reason); err != nil {
		t.Fatal(err)
	}
	if patients != 2 || matches != 1 || reason != "mrn_only" {
		t.Fatalf("patients %d matches %d reason %s", patients, matches, reason)
	}
	sig := f.pub.on(".signal")
	if len(sig) != 1 {
		t.Fatalf("signals %d", len(sig))
	}
	var s emrpb.Signal
	if err := proto.Unmarshal(sig[0].data, &s); err != nil || s.Kind != "possible_match" {
		t.Fatalf("signal %v %v", &s, err)
	}
	// The same patient again: no new proposal, no new signal.
	p := patientRec("x-1", " mrn-9", "1971-02-02", "female")
	p.SourceVersion = "2"
	f.normalise(t, f.put(t, f.hospital, c2, p))
	if len(f.pub.on(".signal")) != 1 {
		t.Error("a re-proposal signalled again")
	}
}

// A31/A12, emr side: a source's replaced-by within the connection makes a
// proposal and a signal and moves nothing; one pointing at another source
// is counted and ignored.
func TestReplacedByProposesOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.normalise(t, f.put(t, f.hospital, f.conn, patientRec("p-new", "MRN-1", "1970-01-01", "male"), obsRec("o-old", "1", "p-old", nil)))
	old := patientRec("p-old", "MRN-2", "1970-01-01", "male")
	old.Fields["link_replaced_by"] = "p-new"
	f.normalise(t, f.put(t, f.hospital, f.conn, old))
	var matches int
	var reason string
	if err := f.pool.QueryRow(ctx, `SELECT count(*), max(reason) FROM emr.possible_matches WHERE hospital_id = $1`, f.hospital).Scan(&matches, &reason); err != nil ||
		matches != 1 || reason != "source_merge" {
		t.Fatalf("matches %d %s %v", matches, reason, err)
	}
	var merged, moved int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.patients WHERE hospital_id = $1 AND status <> 'active'),
		(SELECT count(*) FROM emr.observations o JOIN emr.patient_links l ON l.patient_id = o.patient_id AND l.hospital_id = o.hospital_id
		 WHERE o.hospital_id = $1 AND l.source_patient_id = 'p-new')`, f.hospital).Scan(&merged, &moved); err != nil || merged != 0 || moved != 0 {
		t.Fatalf("merged %d moved %d %v", merged, moved, err)
	}
	if sig := f.pub.on(".signal"); len(sig) != 1 {
		t.Fatalf("signals %d", len(sig))
	} else {
		var s emrpb.Signal
		if err := proto.Unmarshal(sig[0].data, &s); err != nil || s.Kind != "source_merge" {
			t.Fatalf("signal %v", &s)
		}
	}
}

func TestReplacedByOtherConnectionIgnored(t *testing.T) {
	f := newFixture(t)
	p := patientRec("p-1", "MRN-1", "1970-01-01", "male")
	p.Fields["link_replaced_by_external"] = true
	f.normalise(t, f.put(t, f.hospital, f.conn, p))
	var matches int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM emr.possible_matches`).Scan(&matches); err != nil || matches != 0 {
		t.Fatalf("matches %d %v", matches, err)
	}
	if f.n.metrics().ExternalLinks.Load() != 1 || len(f.pub.on(".signal")) != 0 {
		t.Errorf("external links %d signals %d", f.n.metrics().ExternalLinks.Load(), len(f.pub.on(".signal")))
	}
}
