// Package emrtypes is the table of EMR connector types and their declared
// fields (EMR design §4, §4.1), and the closed outcome codes. Core answers
// GET /platform/emr/connector-types and checks a connection's settings and
// credential keys from it; the emr service's connectors declare exactly
// these rows (a test there pins it). It is written once, here, because the
// emr module never imports core and core never imports the emr module.
package emrtypes

import (
	_ "embed"
	"regexp"
	"slices"
	"strings"
)

// Field kinds.
const (
	KindText   = "text"
	KindSecret = "secret"
)

// Field is one field a type declares: the form renders it as given. A
// secret is write-only.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Max      int    `json:"max"`
}

// TypeInfo is a kind of connection, as data. Labels name a standard or a
// method, never a product.
type TypeInfo struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Summary string  `json:"summary"`
	Version int     `json:"-"`
	Fields  []Field `json:"fields"`
	// built: a connector for this type exists in the emr service.
	built bool
	// development: offered only when APP_ENV is development.
	development bool
}

// Built reports whether the emr service has a connector for the type.
func (t TypeInfo) Built() bool { return t.built }

// DevelopmentOnly reports whether the type exists only in development.
func (t TypeInfo) DevelopmentOnly() bool { return t.development }

// Secrets are the type's secret field keys, in order.
func (t TypeInfo) Secrets() []string {
	var out []string
	for _, f := range t.Fields {
		if f.Kind == KindSecret {
			out = append(out, f.Key)
		}
	}
	return out
}

// Field returns the field with key.
func (t TypeInfo) Field(key string) (Field, bool) {
	for _, f := range t.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

var recordLink = Field{Key: "record_link", Label: "Record link", Kind: KindText, Max: 500}

// Types is the table (design §4.1). hl7v2_feed is not built and not here.
var Types = []TypeInfo{
	{ID: "fhir_r4", Label: "FHIR R4 server", Summary: "Reads results, vital signs and medications from a FHIR R4 server.",
		Version: 1, Fields: []Field{
			{Key: "client_id", Label: "Client id", Kind: KindText, Required: true, Max: 200},
			{Key: "token_url", Label: "Token address", Kind: KindText, Required: true, Max: 500},
			{Key: "private_key", Label: "Private key", Kind: KindSecret, Required: true, Max: 4000},
			{Key: "scope", Label: "Scope", Kind: KindText, Max: 200},
			recordLink,
		}},
	{ID: "document_archive", Label: "Scanned record archive", Summary: "Sends new scanned records for review.",
		Version: 1, Fields: []Field{
			{Key: "api_key", Label: "Access key", Kind: KindSecret, Required: true, Max: 4000},
		}},
	{ID: "file_export", Label: "Scheduled file export", Summary: "Reads a scheduled export file from a secure folder.",
		Version: 1, Fields: []Field{
			{Key: "folder", Label: "Folder", Kind: KindText, Required: true, Max: 200},
			{Key: "host_key_fingerprint", Label: "Host key fingerprint", Kind: KindText, Required: true, Max: 100},
			{Key: "format", Label: "File format", Kind: KindText, Required: true, Max: 20},
			{Key: "access_key", Label: "Access key", Kind: KindSecret, Required: true, Max: 4000},
		}},
	// stub: the development connector (plan Task 9). It calls nothing.
	{ID: "stub", Label: "Test connection", Summary: "A development connection that reads nothing.",
		Version: 1, built: true, development: true, Fields: []Field{
			{Key: "client_id", Label: "Client id", Kind: KindText, Required: true, Max: 200},
			{Key: "api_key", Label: "Access key", Kind: KindSecret, Required: true, Max: 4000},
			recordLink,
		}},
}

// Lookup returns the type with id.
func Lookup(id string) (TypeInfo, bool) {
	for _, t := range Types {
		if t.ID == id {
			return t, true
		}
	}
	return TypeInfo{}, false
}

// Built returns the ids of the types the emr service can run: built, and
// development-only ones only in development.
func Built(development bool) []string {
	var out []string
	for _, t := range Types {
		if t.built && (!t.development || development) {
			out = append(out, t.ID)
		}
	}
	return out
}

// Codes are the closed outcome codes of a test or a sync.
var Codes = []string{"ok", "unreachable", "sign_in_refused", "timed_out", "unreadable_data", "unmapped_codes",
	"too_many_requests"}

// IsCode reports whether c is one of Codes.
func IsCode(c string) bool { return slices.Contains(Codes, c) }

//go:embed vendors.txt
var vendorFile string

var vendors = func() []string {
	var out []string
	for _, l := range strings.Split(vendorFile, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, strings.ToLower(l))
		}
	}
	return out
}()

var (
	lowerUpper  = regexp.MustCompile(`([a-z])([A-Z])`)
	upperRun    = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	nonLetters  = regexp.MustCompile(`[^A-Za-z]+`)
	vendorWords = func() map[string]bool {
		m := map[string]bool{}
		for _, v := range vendors {
			m[v] = true
		}
		return m
	}()
)

// VendorIn returns the first product or vendor name in s, as a whole word
// in any case after splitting identifiers (EpicClient, epic_fhir and DHIS2
// read as words), or "" (mhai-web's findVendor).
func VendorIn(s string) string {
	s = lowerUpper.ReplaceAllString(s, "$1 $2")
	s = upperRun.ReplaceAllString(s, "$1 $2")
	for _, w := range strings.Fields(nonLetters.ReplaceAllString(s, " ")) {
		if w = strings.ToLower(w); vendorWords[w] {
			return w
		}
	}
	return ""
}
