package synth

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/fhir"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// The generator: deterministic for a seed, obviously synthetic (SYN- MRNs,
// names from the marked list), and holding the cases the rules need.
func TestGenerate(t *testing.T) {
	a, b := Generate(1, 5), Generate(1, 5)
	if len(a) != 5 || !reflect.DeepEqual(names(a), names(b)) {
		t.Fatalf("not deterministic: %v %v", names(a), names(b))
	}
	if reflect.DeepEqual(names(a), names(Generate(2, 5))) {
		t.Fatal("seed ignored")
	}
	var critical, rise, amended bool
	for _, p := range a {
		if !strings.HasPrefix(p.MRN, "SYN-") || !slices.Contains(SyntheticGiven, p.Given) || !slices.Contains(SyntheticFamily, p.Family) {
			t.Errorf("not marked synthetic: %+v", p)
		}
		critical = critical || p.Has(CaseCriticalPotassium)
		rise = rise || p.Has(CaseCreatinineRise)
		amended = amended || p.Has(CaseAmended)
	}
	if !critical || !rise || !amended {
		t.Fatalf("cases: critical %v rise %v amended %v", critical, rise, amended)
	}
}

func names(ps []Patient) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Given+" "+p.Family+" "+p.MRN)
	}
	return out
}

// The synthetic server passes the FHIR connector's test and serves every
// record of the generated patients.
func TestSyntheticServerServesTheConnector(t *testing.T) {
	ps := Generate(1, 4)
	s := Server(ps)
	defer s.Close()
	c := fhir.New(func(connector.Config) *http.Client { return s.Client() })
	cfg := s.Config()
	creds := connector.NewCreds(map[string]seal.Secret{"private_key": seal.NewSecret(s.ClientKey)})
	if r := c.Test(context.Background(), cfg, creds); !r.OK {
		t.Fatalf("test: %+v", r)
	}
	sink := &memSink{}
	cur, err := c.Fetch(context.Background(), cfg, creds, nil, sink)
	if err != nil || !c.CaughtUp(cur) {
		t.Fatalf("fetch: %v %v", err, c.CaughtUp(cur))
	}
	want := 0
	for _, p := range ps {
		want += len(p.Observations) + len(p.Medications)
	}
	if got := sink.count("observation") + sink.count("medication"); got != want {
		t.Fatalf("%d records, want %d", got, want)
	}
	if got := sink.count("patient"); got != len(ps) {
		t.Fatalf("%d patients, want %d", got, len(ps))
	}
	// A wrong key is refused.
	other := Server(ps)
	defer other.Close()
	bad := connector.NewCreds(map[string]seal.Secret{"private_key": seal.NewSecret(other.ClientKey)})
	if r := c.Test(context.Background(), cfg, bad); r.Code != "sign_in_refused" {
		t.Fatalf("wrong key: %+v", r)
	}
}

type memSink struct{ recs []connector.RawRecord }

func (m *memSink) Put(_ context.Context, r connector.RawRecord) error {
	m.recs = append(m.recs, r)
	return nil
}

func (m *memSink) count(kind string) int {
	n := 0
	for _, r := range m.recs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}
