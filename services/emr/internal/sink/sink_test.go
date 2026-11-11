package sink

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

// Synthetic only.
const mrnNS = "urn:synthetic:mrn"

// §14.1: address, telecom, an identifier outside the MRN namespace,
// contact, practitioner, narrative text and an unlisted extension are
// never kept; a listed field of the wrong shape is dropped too.
func TestSinkDropsUnlistedFields(t *testing.T) {
	in := map[string]any{
		"given":        "Synthetic",
		"family":       "Patient-One",
		"birth_date":   "1970-01-01",
		"sex":          "female",
		"identifier":   []connector.Coding{{System: "urn:synthetic:nin", Code: "NIN-SYNTHETIC-1"}, {System: mrnNS, Code: "MRN-0001"}},
		"address":      "1 Synthetic Street",
		"telecom":      "+234 000 000 0000",
		"contact":      "Next Of Kin",
		"practitioner": "Dr Synthetic",
		"text":         "<div>narrative</div>",
		"extension":    map[string]any{"url": "urn:synthetic:ext", "value": "x"},
		"active":       "yes", // listed, wrong shape
	}
	out, dropped := keep("patient", in, mrnNS)
	if dropped != 7 {
		t.Errorf("dropped %d, want 7", dropped)
	}
	for k := range out {
		switch k {
		case "given", "family", "birth_date", "sex", "mrn":
		default:
			t.Errorf("kept %q", k)
		}
	}
	if out["mrn"] != "MRN-0001" {
		t.Errorf("mrn %v, want the MRN namespace's value only", out["mrn"])
	}
	// No namespace configured: no identifier at all.
	if out, _ := keep("patient", in, ""); out["mrn"] != nil {
		t.Errorf("mrn kept with no namespace: %v", out["mrn"])
	}
	// Kinds have their own lists: an observation keeps no name.
	if out, _ := keep("observation", map[string]any{"given": "Synthetic", "status": "final", "note": "free text"}, mrnNS); len(out) != 1 {
		t.Errorf("observation kept %v", out)
	}
	// A code with a space, an id with a control character, a time with
	// letters: dropped, not cleaned into something else.
	out, dropped = keep("observation", map[string]any{"status": "final; drop", "patient": "p\x001", "effective": "yesterday"}, mrnNS)
	if len(out) != 0 || dropped != 3 {
		t.Errorf("odd shapes kept %v (dropped %d)", out, dropped)
	}
}

// A8: display text loses markup, control and format characters, and is
// capped.
func TestCleanText(t *testing.T) {
	for in, want := range map[string]string{
		"<script>alert(1)</script>Potassium": "alert(1) Potassium",
		"Gluc\u202eose":                      "Glucose",
		"Sodium\x00\x1b[31m":                 "Sodium[31m",
		"  Haemo\tglobin \n ":                "Haemo globin",
		"<0.5 and >2":                        "<0.5 and >2",
		"Cafe\u0301":                         "Caf\u00e9",
		"zero\u200bwidth":                    "zerowidth",
		"bad \xff utf8":                      "bad utf8",
		"<img src=x onerror=alert(1)>":       "",
	} {
		if got := CleanText(in, 120); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("ab", 5000)
	if got := CleanText(long, 60); utf8.RuneCountInString(got) != 60 {
		t.Errorf("10,000 characters capped to %d", utf8.RuneCountInString(got))
	}
}

// A7: a cell a spreadsheet would run is neutralised before storage.
func TestCSVFormulaNeutralised(t *testing.T) {
	for in, want := range map[string]string{
		"=HYPERLINK(\"http://x\")": "'=HYPERLINK(\"http://x\")",
		"+1":                       "'+1",
		"-2+3":                     "'-2+3",
		"@SUM(A1)":                 "'@SUM(A1)",
		"\tx":                      "'\tx",
		"\rx":                      "'\rx",
		"Potassium":                "Potassium",
		"":                         "",
	} {
		if got := CSVCell(in); got != want {
			t.Errorf("CSVCell(%q) = %q, want %q", in, got, want)
		}
	}
	out, _ := keep("observation", map[string]any{"code_text": "=cmd|'/c calc'!A1", "value_quantity": "-3"}, mrnNS)
	if out["code_text"] != "'=cmd|'/c calc'!A1" {
		t.Errorf("display text stored as %q", out["code_text"])
	}
	if out["value_quantity"] != "-3" {
		t.Errorf("a value is not display text: %q", out["value_quantity"])
	}
}

// Nested and split tags can't rebuild markup after a pass: no '<' followed
// by a letter, '/', '!' or '?' survives, while comparators do.
func TestCleanTextNestedAndSplitTags(t *testing.T) {
	for _, in := range []string{
		"<scr<x>ipt>alert(1)</script>",
		"<<x>script>alert(1)<</x>/script>",
		"<scr<b>ipt>alert(1)</scr<i>ipt>",
		"<<script>script>alert(1)",
		"<<<b>b>b>img src=x onerror=alert(1)>",
		"<i<b>mg src=x onerror=alert(1)>",
		"<!<x>-- x -->",
		"</<x>a>",
		"<\x00script>alert(1)",
		"<\u200bscript>alert(1)",
	} {
		got := CleanText(in, 200)
		for i := 0; i+1 < len(got); i++ {
			if c := got[i+1]; got[i] == '<' && (c == '/' || c == '!' || c == '?' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')) {
				t.Errorf("CleanText(%q) = %q: markup can open", in, got)
				break
			}
		}
	}
	for in, want := range map[string]string{"<0.5": "<0.5", ">100": ">100", "<5.0 and >2": "<5.0 and >2", "a < b": "a < b"} {
		if got := CleanText(in, 120); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}
