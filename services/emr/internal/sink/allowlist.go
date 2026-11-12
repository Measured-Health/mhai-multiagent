package sink

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

// The allow-list (EMR design §14.1): per resource kind, the fields a raw
// record may keep and the shape each must have. A field not listed, or not
// of its shape, is dropped before storage and counted. Never on the list:
// addresses, telecom, other identifiers, contacts, practitioners, notes,
// narrative text, attachments, extensions.

type shape int

const (
	shapeText        shape = iota // display text: CleanText, then CSVCell
	shapeID                       // a source id: 1..256, printable, kept exactly
	shapeCode                     // a short code: letters, digits, '.', '_', '-'
	shapeVerbatim                 // a value or unit as the source wrote it, parsed strictly later
	shapeTime                     // a FHIR date or dateTime, parsed strictly later
	shapeBool                     // true or false
	shapeCodings                  // []connector.Coding, at most 10
	shapeIdentifiers              // []connector.Coding: only the MRN namespace's value is kept, as "mrn"
)

type spec struct {
	shape shape
	max   int
}

// Kinds are the resource kinds a raw record may be.
var Kinds = []string{"patient", "encounter", "location", "observation", "medication"}

var allow = map[string]map[string]spec{
	"patient": {
		"identifier": {shapeIdentifiers, 0},
		"given":      {shapeText, 60},
		"family":     {shapeText, 60},
		"birth_date": {shapeTime, 40},
		"sex":        {shapeCode, 16},
		"active":     {shapeBool, 0},
		// A merge in the source (FHIR Patient.link replaced-by): the
		// target's id on the same source, or a flag that it is elsewhere.
		"link_replaced_by":          {shapeID, 256},
		"link_replaced_by_external": {shapeBool, 0},
	},
	"encounter": {
		"patient":      {shapeID, 256},
		"class":        {shapeCode, 20},
		"status":       {shapeCode, 20},
		"period_start": {shapeTime, 40},
		"period_end":   {shapeTime, 40},
		"location":     {shapeID, 256},
		"bed_label":    {shapeText, 20},
	},
	"location": {
		"name": {shapeText, 60},
		"type": {shapeCode, 20},
	},
	"observation": {
		"patient":        {shapeID, 256},
		"encounter":      {shapeID, 256},
		"category":       {shapeCode, 20},
		"codings":        {shapeCodings, 0},
		"code_text":      {shapeText, 200},
		"value_quantity": {shapeVerbatim, 40},
		"value_text":     {shapeText, 120},
		"value_codings":  {shapeCodings, 0},
		"unit":           {shapeVerbatim, 40},
		"ref_low":        {shapeVerbatim, 40},
		"ref_high":       {shapeVerbatim, 40},
		"ref_text":       {shapeText, 120},
		"interpretation": {shapeCode, 4},
		"status":         {shapeCode, 20},
		"effective":      {shapeTime, 40},
		"issued":         {shapeTime, 40},
	},
	"medication": {
		"patient":      {shapeID, 256},
		"kind":         {shapeCode, 10},
		"codings":      {shapeCodings, 0},
		"med_text":     {shapeText, 200},
		"dose_value":   {shapeVerbatim, 40},
		"dose_unit":    {shapeVerbatim, 40},
		"route":        {shapeText, 40},
		"timing_text":  {shapeText, 60},
		"status":       {shapeCode, 20},
		"authored":     {shapeTime, 40},
		"period_start": {shapeTime, 40},
		"period_end":   {shapeTime, 40},
	},
}

var (
	codeShape = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	timeShape = regexp.MustCompile(`^[0-9T:.+Z-]+$`)
)

// printable: valid UTF-8, no control or format character.
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// keep returns the fields of kind that are on the list and of their shape,
// and how many were dropped. mrnSystem is the connection's MRN namespace:
// only an identifier in it is kept, as "mrn".
func keep(kind string, in map[string]any, mrnSystem string) (map[string]any, int) {
	list := allow[kind]
	out := map[string]any{}
	dropped := 0
	for k, v := range in {
		sp, ok := list[k]
		if !ok {
			dropped++
			continue
		}
		key, val, ok := fit(k, sp, v, mrnSystem)
		if !ok {
			dropped++
			continue
		}
		out[key] = val
	}
	return out, dropped
}

func fit(k string, sp spec, v any, mrnSystem string) (string, any, bool) {
	switch sp.shape {
	case shapeBool:
		b, ok := v.(bool)
		return k, b, ok
	case shapeCodings:
		cs, ok := v.([]connector.Coding)
		if !ok || len(cs) == 0 || len(cs) > 10 {
			return "", nil, false
		}
		out := make([]connector.Coding, 0, len(cs))
		for _, c := range cs {
			if len(c.System) > 200 || len(c.Code) > 64 || c.Code == "" || !printable(c.System) || !printable(c.Code) {
				return "", nil, false
			}
			out = append(out, connector.Coding{System: c.System, Code: c.Code, Display: CleanText(c.Display, 200)})
		}
		return k, out, true
	case shapeIdentifiers:
		cs, ok := v.([]connector.Coding)
		if !ok || mrnSystem == "" {
			return "", nil, false
		}
		for _, c := range cs {
			if c.System == mrnSystem {
				mrn := CleanText(c.Code, 64)
				return "mrn", mrn, mrn != ""
			}
		}
		return "", nil, false
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case json.Number:
		s = x.String()
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return "", nil, false
	}
	switch sp.shape {
	case shapeText:
		c := CSVCell(CleanText(s, sp.max))
		return k, c, c != ""
	case shapeID:
		return k, s, s != "" && len(s) <= sp.max && printable(s) && strings.TrimSpace(s) == s
	case shapeCode:
		return k, s, len(s) <= sp.max && codeShape.MatchString(s)
	case shapeTime:
		return k, s, len(s) <= sp.max && timeShape.MatchString(s)
	case shapeVerbatim:
		return k, s, s != "" && len(s) <= sp.max && printable(s)
	}
	return "", nil, false
}
