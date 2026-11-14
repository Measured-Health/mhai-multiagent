package alerts

import (
	"slices"

	"github.com/google/uuid"
)

// Category is one of the eight closed categories of design §11.3, with the
// hospital-facing label and example mhai-web shows.
type Category struct {
	ID, Label, Example string
}

// Categories are the eight, in display order.
var Categories = []Category{
	{"blood_chemistry", "Blood chemistry", "Potassium, sodium, calcium"},
	{"blood_count", "Blood count", "Haemoglobin, white cells, platelets"},
	{"kidney_function", "Kidney function", "Creatinine, urea"},
	{"blood_gases_lactate", "Blood gases and lactate", "pH, lactate"},
	{"heart_markers", "Heart markers", "Troponin"},
	{"blood_sugar", "Blood sugar", "Glucose"},
	{"vital_signs", "Vital signs", "Heart rate, blood pressure, oxygen saturation"},
	{"infection_markers", "Infection markers", "CRP, procalcitonin"},
}

// CategoryByID finds a category.
func CategoryByID(id string) (Category, bool) {
	i := slices.IndexFunc(Categories, func(c Category) bool { return c.ID == id })
	if i < 0 {
		return Category{}, false
	}
	return Categories[i], true
}

// Levels are the closed alert levels a category can be set to.
var Levels = []string{"off", "critical", "critical_urgent", "all"}

// DefaultLevel is a category's level until the hospital admin sets one
// (design §12.2, open question 5): critical and urgent, but critical only
// for vital signs.
func DefaultLevel(category string) string {
	if category == "vital_signs" {
		return "critical"
	}
	return "critical_urgent"
}

// Admits reports whether level lets an alert of severity through.
func Admits(level, severity string) bool {
	switch level {
	case "all":
		return severity == "critical" || severity == "urgent" || severity == "info"
	case "critical_urgent":
		return severity == "critical" || severity == "urgent"
	case "critical":
		return severity == "critical"
	}
	return false
}

// Settings are a hospital's alert settings as routing reads them.
// Recipients are the named clinicians who are still active clinicians of
// the hospital (the caller filters); Covered holds the units with a saved
// choice (a unit with none is covered); Levels the categories with a saved
// level (one with none has DefaultLevel).
type Settings struct {
	Queue      bool
	Recipients []uuid.UUID
	Covered    map[uuid.UUID]bool
	Levels     map[string]string
}

// The reasons Route gives for not raising an alert (alert.not_raised).
const (
	ReasonUnitNotCovered = "unit_not_covered"
	ReasonLevelOff       = "level_off"
)

// Route decides where an alert goes (design §12.1 steps 3, §12.2), for
// core's finding consumer (PR 8b): reason is non-empty when the settings
// filter it out (the unit isn't covered, or the category's level doesn't
// admit the severity). A patient with no recorded unit (nil) is covered:
// fail toward alerting. Otherwise it goes to the queue and the named
// clinicians as set, and never to nobody: with no active named clinician
// and the queue off it goes to the queue (A16). The caller fixes the
// result on the alert; it is never recomputed (Review Focus 4).
func Route(s Settings, unit *uuid.UUID, severity, category string) (queue bool, users []uuid.UUID, reason string) {
	if unit != nil {
		if covered, set := s.Covered[*unit]; set && !covered {
			return false, nil, ReasonUnitNotCovered
		}
	}
	level, set := s.Levels[category]
	if !set {
		level = DefaultLevel(category)
	}
	if !Admits(level, severity) {
		return false, nil, ReasonLevelOff
	}
	users = slices.Clone(s.Recipients)
	slices.SortFunc(users, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	users = slices.Compact(users)
	return s.Queue || len(users) == 0, users, ""
}
