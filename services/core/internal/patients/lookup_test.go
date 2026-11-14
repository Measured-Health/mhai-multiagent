package patients_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/patients"
)

// An MRN is compared as the EMR index stores it (services/emr
// index.NormaliseMRN): every space removed, upper case, nothing else (no
// leading zeros dropped, no punctuation removed). 1 to 64 characters, no
// control characters. Every MRN here is synthetic.
func TestNormaliseMRN(t *testing.T) {
	for in, want := range map[string]string{
		"SYN-LKP-7Q4X-M2":       "SYN-LKP-7Q4X-M2",
		"  syn-lkp-7q4x-m2 \t":  "SYN-LKP-7Q4X-M2",
		"SYN LKP 7Q4X M2":       "SYNLKP7Q4XM2",
		"0004417-SYN":           "0004417-SYN",
		"syn/lkp.7q4x":          "SYN/LKP.7Q4X",
		strings.Repeat("Q", 64): strings.Repeat("Q", 64),
		"synthetic-ä":           "SYNTHETIC-Ä",
	} {
		got, ok := patients.NormaliseMRN(in)
		if !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "   ", "\t\n", strings.Repeat("Q", 65), "SYN\x00LKP", "SYN\x07LKP", "SYN\u200bLKP",
		strings.Repeat(" ", 300) + "Q"} {
		if got, ok := patients.NormaliseMRN(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// The route: POST only (the MRN never goes in a URL), clinicians only, a
// two-step session, a write (auditors refused), and no idempotency store
// (no hash of the MRN, no reply kept).
func TestLookupRouteRule(t *testing.T) {
	m := authz.NewMux(nil)
	patients.Register(m, patients.Deps{})
	table := m.Table()
	if len(table) != 1 {
		t.Fatalf("%d routes, want 1", len(table))
	}
	rr := table[0]
	if rr.Method != http.MethodPost || rr.Pattern != httpx.BasePath+"/patients/lookup" {
		t.Fatalf("%s %s", rr.Method, rr.Pattern)
	}
	r := rr.Rule
	if !slices.Equal(r.AnyRole, []string{authz.RoleClinician}) || !r.NeedsMFA || !r.Write || !r.NoIdempotency ||
		r.StepUp || r.Platform || r.Public || r.SelfService || r.Credentials {
		t.Fatalf("%+v", r)
	}
}
