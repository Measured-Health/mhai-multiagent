package platform_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

func TestNormaliseCountry(t *testing.T) {
	if platform.ISOCountryCount != 249 {
		t.Errorf("%d codes, want ISO 3166-1's 249", platform.ISOCountryCount)
	}
	for in, want := range map[string]string{"NG": "NG", " ng ": "NG", "gh": "GH", "KE": "KE", "GB": "GB", "US": "US", "ZA": "ZA"} {
		if got, ok := platform.NormaliseCountry(in); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "N", "NGA", "Nigeria", "N1", "12", "ZZ", "AA", "QM", "QZ", "XA", "XK", "UK", "EU", "UN", "N G", "ÑG"} {
		if got, ok := platform.NormaliseCountry(in); ok {
			t.Errorf("%q: accepted as %q", in, got)
		}
	}
}

func TestNormaliseEmailDomains(t *testing.T) {
	got, ok := platform.NormaliseEmailDomains([]string{" Riverside.Synthetic.Example", "riverside.synthetic.example", "a-b.synthetic.example"})
	if !ok || !slices.Equal(got, []string{"riverside.synthetic.example", "a-b.synthetic.example"}) {
		t.Errorf("got %v %v", got, ok)
	}
	if got, ok := platform.NormaliseEmailDomains([]string{}); !ok || got == nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, ok)
	}
	for _, bad := range [][]string{{""}, {"localhost"}, {"-a.synthetic.example"}, {"a-.synthetic.example"}, {"a b.example"},
		{"a.example,b.example"}, {"a..example"}, {"x@a.example"}, {strings.Repeat("a", 64) + ".example"},
		{strings.Repeat("a.", 127) + "example"}} {
		if got, ok := platform.NormaliseEmailDomains(bad); ok {
			t.Errorf("%q: accepted as %v", bad, got)
		}
	}
	many := []string{}
	for i := 0; i <= platform.MaxEmailDomains; i++ {
		many = append(many, string(rune('a'+i))+".synthetic.example")
	}
	if _, ok := platform.NormaliseEmailDomains(many); ok {
		t.Errorf("%d domains accepted", len(many))
	}
	if _, ok := platform.NormaliseEmailDomains(many[:platform.MaxEmailDomains]); !ok {
		t.Errorf("%d domains refused", platform.MaxEmailDomains)
	}
}
