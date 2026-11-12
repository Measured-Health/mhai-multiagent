// Package cells is Analysis's raw reader: one context's raw values, read
// through mhai_analysis_cells (migration 00024) into disclose.Raw, for one
// guard computation. Only the engine (internal/analysis) imports it; the
// routes never do (I2, pinned by a test), so nothing that writes a
// response can reach a raw count.
package cells

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// ErrShape: the function's result isn't the shape this reader knows (a
// migration and a build out of step). The engine fails closed.
var ErrShape = errors.New("cells: unexpected result")

// FiltersJSON is the filters as mhai_analysis_cells takes them: the set
// dimensions by name.
func FiltersJSON(f disclose.Filters) string {
	m := map[string]string{}
	for d, v := range f {
		if v != "" {
			m[disclose.DimNames[d]] = v
		}
	}
	b, _ := json.Marshal(m) // a map of strings always encodes
	return string(b)
}

type kn struct {
	K string `json:"k"`
	N int64  `json:"n"`
}

type result struct {
	First       bool  `json:"first"`
	Patients    int64 `json:"patients"`
	Cases       int64 `json:"cases"`
	New         int64 `json:"new"`
	ByCondition []struct {
		C     string `json:"c"`
		Cases int64  `json:"cases"`
		New   int64  `json:"new"`
	} `json:"by_condition"`
	Blocks []struct {
		Start string `json:"start"`
		Cases int64  `json:"cases"`
		New   int64  `json:"new"`
	} `json:"blocks"`
	ByAge       []kn `json:"by_age"`
	BySex       []kn `json:"by_sex"`
	BySetting   []kn `json:"by_setting"`
	BySpecialty []kn `json:"by_specialty"`
	ByLocation  []struct {
		K       string `json:"k"`
		Setting string `json:"setting"`
		N       int64  `json:"n"`
	} `json:"by_location"`
	Sources []string `json:"sources"`
	Pairs   []struct {
		A         string `json:"a"`
		B         string `json:"b"`
		APatients int64  `json:"a_patients"`
		Patients  int64  `json:"patients"`
	} `json:"pairs"`
	Findings []struct {
		Name     string `json:"name"`
		Flag     string `json:"flag"`
		Patients int64  `json:"patients"`
		Results  int64  `json:"results"`
	} `json:"findings"`
	Alerts struct {
		Total, Critical, Urgent, Info, Dismissed int64
		Reasons                                  []struct {
			Reason string `json:"reason"`
			Count  int64  `json:"count"`
		} `json:"reasons"`
		Ack         []int64 `json:"ack"`
		CriticalAck []int64 `json:"critical_ack"`
	} `json:"alerts"`
	Quality []struct {
		C                                    string `json:"c"`
		N                                    int64  `json:"n"`
		High, Caution, Review, Failed, Rated int64
		Agreed, Corrections, Approved        int64
		Rejected, Waiting                    int64
	} `json:"quality"`
}

// Read is one context's raw values: period p (its id in the database)
// under f. It runs in the caller's request transaction (authz.Tx), which
// the function scopes to the request's hospital.
func Read(ctx context.Context, tx pgx.Tx, id uuid.UUID, p calendar.Period, f disclose.Filters) (disclose.Raw, error) {
	var raw disclose.Raw
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT mhai_analysis_cells($1, $2::jsonb)::text`, id, FiltersJSON(f)).Scan(&body); err != nil {
		return raw, err
	}
	return decode(body, p, f)
}

func decode(body []byte, p calendar.Period, f disclose.Filters) (disclose.Raw, error) {
	var raw disclose.Raw
	var r result
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return raw, ErrShape
	}
	raw.Context = disclose.Context{Period: p, Filters: f, First: r.First}
	raw.Patients = r.Patients
	raw.Total = disclose.CNE[int64]{Cases: r.Cases, New: r.New, Existing: r.Cases - r.New}
	for _, c := range r.ByCondition {
		raw.ByCondition = append(raw.ByCondition, disclose.ConditionRow[int64]{Condition: c.C,
			CNE: disclose.CNE[int64]{Cases: c.Cases, New: c.New, Existing: c.Cases - c.New}})
	}
	// The series: the period's children (a week's is itself), every one
	// listed, each the sum of its blocks.
	kids := calendar.Children(p)
	if p.Kind == calendar.Week {
		kids = []calendar.Period{p}
	}
	for _, k := range kids {
		pt := disclose.Point[int64]{Start: k.From.Format(time.DateOnly)}
		for _, b := range r.Blocks {
			day, err := time.Parse(time.DateOnly, b.Start)
			if err != nil {
				return raw, ErrShape
			}
			if !day.Before(k.From) && day.Before(k.To) {
				pt.Cases += b.Cases
				pt.New += b.New
			}
		}
		raw.Series = append(raw.Series, pt)
	}
	parts := func(in []kn) []disclose.Part[int64] {
		var out []disclose.Part[int64]
		for _, x := range in {
			out = append(out, disclose.Part[int64]{Key: x.K, Patients: x.N})
		}
		return out
	}
	raw.ByAge, raw.BySex, raw.BySetting, raw.BySpecialty = parts(r.ByAge), parts(r.BySex), parts(r.BySetting), parts(r.BySpecialty)
	for _, x := range r.ByLocation {
		raw.ByLocation = append(raw.ByLocation, disclose.Part[int64]{Key: x.K, Setting: x.Setting, Patients: x.N})
	}
	raw.Sources = r.Sources
	for _, x := range r.Pairs {
		raw.Pairs = append(raw.Pairs, disclose.Pair[int64, struct{}]{A: x.A, B: x.B, APatients: x.APatients, Patients: x.Patients})
	}
	for _, x := range r.Findings {
		raw.Findings = append(raw.Findings, disclose.FindingRow[int64]{Name: x.Name, Flag: x.Flag, Patients: x.Patients, Results: x.Results})
	}
	a := &raw.Alerts
	a.Total, a.Critical, a.Urgent, a.Info, a.Dismissed = r.Alerts.Total, r.Alerts.Critical, r.Alerts.Urgent, r.Alerts.Info, r.Alerts.Dismissed
	for _, x := range r.Alerts.Reasons {
		a.DismissReasons = append(a.DismissReasons, disclose.ReasonRow[int64]{Reason: x.Reason, Count: x.Count})
	}
	a.MedianAck, a.CriticalMedianAck = r.Alerts.Ack, r.Alerts.CriticalAck
	for _, x := range r.Quality {
		raw.Quality = append(raw.Quality, disclose.QualityRow[int64, struct{}]{Condition: x.C, Consultations: x.N,
			High: x.High, Caution: x.Caution, Review: x.Review, Failed: x.Failed, Rated: x.Rated, Agreed: x.Agreed,
			Corrections: disclose.Corrections[int64]{Total: x.Corrections, Approved: x.Approved, Rejected: x.Rejected, Waiting: x.Waiting}})
	}
	return raw, nil
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
