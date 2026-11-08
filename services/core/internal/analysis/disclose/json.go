package disclose

import (
	"encoding/json"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// ViewJSON is one view of a body in the contract's shape (design §9),
// without what render adds: condition names, drill keys and updated_at. A
// condition is {"id"}, a location {"id", "setting"}. Keys are written in
// order (maps encode sorted), so a body encodes the same way every time.
func ViewJSON(b Body, v View) ([]byte, error) {
	p := b.Context.Period
	bucket := "week"
	if p.Kind == calendar.Quarter {
		bucket = "month"
	}
	o := map[string]any{
		"from":     p.From.Format(time.DateOnly),
		"to":       p.To.AddDate(0, 0, -1).Format(time.DateOnly),
		"bucket":   bucket,
		"period":   map[string]string{"level": string(p.Kind), "start": p.From.Format(time.DateOnly)},
		"patients": b.Patients,
	}
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	cond := func(id string) map[string]string { return map[string]string{"id": id} }
	switch v {
	case Prevalence:
		o["total"] = map[string]any{"cases": b.Total.Cases, "new": b.Total.New, "existing": b.Total.Existing}
		rows := []any{}
		for _, r := range b.ByCondition {
			rows = append(rows, map[string]any{"condition": cond(r.Condition), "cases": r.Cases, "new": r.New, "existing": r.Existing})
		}
		o["by_condition"] = rows
		series := []any{}
		for _, s := range b.Series {
			series = append(series, map[string]any{"start": s.Start, "cases": s.Cases, "new": s.New})
		}
		o["series"] = series
	case Affected:
		part := func(field, name string, rows []Part[smallgroup.Guarded], val func(Part[smallgroup.Guarded]) any) {
			out := []any{}
			for _, r := range rows {
				out = append(out, map[string]any{name: val(r), "patients": r.Patients})
			}
			o[field] = out
		}
		part("by_age", "band", b.ByAge, func(r Part[smallgroup.Guarded]) any { return null(r.Key) })
		part("by_sex", "sex", b.BySex, func(r Part[smallgroup.Guarded]) any { return r.Key })
		part("by_setting", "setting", b.BySetting, func(r Part[smallgroup.Guarded]) any { return null(r.Key) })
		part("by_location", "location", b.ByLocation, func(r Part[smallgroup.Guarded]) any {
			if r.Key == "" {
				return nil
			}
			return map[string]any{"id": r.Key, "setting": null(r.Setting)}
		})
		part("by_specialty", "specialty", b.BySpecialty, func(r Part[smallgroup.Guarded]) any { return null(r.Key) })
	case CoOccurring:
		rows := []any{}
		for _, r := range b.Pairs {
			rows = append(rows, map[string]any{"a": cond(r.A), "b": cond(r.B), "a_patients": r.APatients, "patients": r.Patients, "share_of_a": r.ShareOfA})
		}
		o["pairs"] = rows
	case Findings:
		rows := []any{}
		for _, r := range b.Findings {
			rows = append(rows, map[string]any{"name": null(r.Name), "flag": null(r.Flag), "patients": r.Patients, "results": r.Results})
		}
		o["findings"] = rows
		a := b.Alerts
		reasons := []any{}
		for _, r := range a.DismissReasons {
			reasons = append(reasons, map[string]any{"reason": r.Reason, "count": r.Count})
		}
		o["alerts"] = map[string]any{
			"total":                       a.Total,
			"by_severity":                 map[string]any{"critical": a.Critical, "urgent": a.Urgent, "info": a.Info},
			"median_ack_minutes":          a.MedianAck,
			"critical_median_ack_minutes": a.CriticalMedianAck,
			"dismissed":                   a.Dismissed,
			"dismiss_reasons":             reasons,
		}
	case Quality:
		rows := []any{}
		for _, r := range b.Quality {
			rows = append(rows, map[string]any{
				"condition":      cond(r.Condition),
				"consultations":  r.Consultations,
				"confidence":     map[string]any{"high": r.High, "caution": r.Caution, "review": r.Review, "failed": r.Failed},
				"agreement_rate": r.AgreementRate,
				"rated":          r.Rated,
				"corrections":    map[string]any{"total": r.Corrections.Total, "approved": r.Corrections.Approved, "rejected": r.Corrections.Rejected, "waiting": r.Corrections.Waiting},
			})
		}
		o["by_condition"] = rows
	default:
		return nil, ErrInvalid
	}
	return json.Marshal(o)
}
