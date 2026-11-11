package index

import "testing"

func TestNormaliseMRN(t *testing.T) {
	for in, want := range map[string]string{"mrn 0001": "MRN0001", " MRN-1 ": "MRN-1", "a\tb": "AB", "": ""} {
		if got := NormaliseMRN(in); got != want {
			t.Errorf("NormaliseMRN(%q) = %q, want %q", in, got, want)
		}
	}
}
