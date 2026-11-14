package analysis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// T20: the reply must hold exactly the asked references, at most 200.
func TestT20ReplyMustMatch(t *testing.T) {
	asked := []string{"syn-ref-0001", "syn-ref-0002"}
	ok := map[string]Identity{"syn-ref-0001": NewIdentity("Zephyrine Quenby (synthetic)", "SYN-00000001"),
		"syn-ref-0002": NewIdentity("Quillon Starrow (synthetic)", "")}
	if err := CheckIdentities(asked, ok); err != nil {
		t.Fatalf("exact reply refused: %v", err)
	}
	extra := map[string]Identity{"syn-ref-0003": NewIdentity("Ondine Halloway (synthetic)", "SYN-3")}
	for k, v := range ok {
		extra[k] = v
	}
	wrong := map[string]Identity{"syn-ref-0001": ok["syn-ref-0001"], "syn-ref-9999": ok["syn-ref-0002"]}
	missing := map[string]Identity{"syn-ref-0001": ok["syn-ref-0001"]}
	noName := map[string]Identity{"syn-ref-0001": ok["syn-ref-0001"], "syn-ref-0002": NewIdentity("", "SYN-2")}
	for name, got := range map[string]map[string]Identity{"extra": extra, "wrong subject": wrong, "missing": missing, "no name": noName, "none": nil} {
		if err := CheckIdentities(asked, got); !errors.Is(err, ErrIdentityReply) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var many []string
	big := map[string]Identity{}
	for i := 0; i <= MaxIdentities; i++ {
		r := fmt.Sprintf("syn-ref-%04d", i)
		many = append(many, r)
		big[r] = NewIdentity("Calliope Fernquist (synthetic)", "")
	}
	if err := CheckIdentities(many, big); !errors.Is(err, ErrIdentityReply) {
		t.Errorf("201 references: %v", err)
	}
	if err := CheckIdentities([]string{"a", "a"}, map[string]Identity{"a": ok["syn-ref-0001"]}); !errors.Is(err, ErrIdentityReply) {
		t.Errorf("a repeated reference: %v", err)
	}
}

// T27: the demo fixture refuses to start with live identity configured.
func TestT27DemoFixtureRefusesLive(t *testing.T) {
	if d, err := NewDemoIdentity(true); err == nil || d != nil {
		t.Fatal("the demo fixture started with the EMR integration configured")
	}
	if d, err := NewDemoIdentity(false); err != nil || d == nil {
		t.Fatalf("demo: %v", err)
	}
	if _, err := (Unavailable{}).Resolve(context.Background(), IdentityRequest{Demo: true, Refs: []string{"x"}}); !errors.Is(err, ErrIdentityUnavailable) {
		t.Errorf("live: %v", err)
	}
}

// The fixture names only a demo letter's list, the same reference the
// same way, each marked synthetic.
func TestDemoIdentityOnlyForDemoLetters(t *testing.T) {
	d, _ := NewDemoIdentity(false)
	refs := []string{"syn-ref-a", "syn-ref-b"}
	if _, err := d.Resolve(context.Background(), IdentityRequest{Refs: refs}); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("a live letter's list: %v", err)
	}
	got, err := d.Resolve(context.Background(), IdentityRequest{Demo: true, Refs: refs})
	if err != nil || CheckIdentities(refs, got) != nil {
		t.Fatalf("demo: %v", err)
	}
	again, _ := d.Resolve(context.Background(), IdentityRequest{Demo: true, Refs: refs})
	for _, r := range refs {
		if !strings.HasSuffix(got[r].Name(), "(synthetic)") || !strings.HasPrefix(got[r].MRN(), "SYN-") || got[r] != again[r] {
			t.Errorf("%s: not a stable synthetic identity", r)
		}
	}
}

// An identity never prints, whatever the verb or the logger.
func TestIdentityNeverPrints(t *testing.T) {
	id := NewIdentity("Bartholo Vantreece (synthetic)", "SYN-77777777")
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("x", "identity", id, "map", map[string]Identity{"r": id})
	s := fmt.Sprintf("%v %+v %#v %s %x %q %d", id, id, id, id, id, id, id) + fmt.Sprint(id) + buf.String()
	for _, leak := range []string{"Bartholo", "Vantreece", "77777777"} {
		if strings.Contains(s, leak) {
			t.Fatalf("an identity printed: %s", s)
		}
	}
}
