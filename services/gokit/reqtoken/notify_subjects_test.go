package reqtoken

import (
	"errors"
	"testing"
	"time"
)

func notifyClaims(op string) Claims {
	return Claims{Aud: "notifications", Sub: SvcCore, Hid: testHospital, Rid: testRid,
		Kind: KindReq, Subject: "notify.v1.ctl." + op, ReplyPrefix: ReplyCore, Via: ViaCore}
}

// ADR 0018 (notifications design §7; NT7): notify.v1.ctl.<op> is a request
// subject whose audience is always notifications. Only core calls it: no
// grant token and no step-up claim. Every other audience is refused on it,
// and aud notifications everywhere else.
func TestSubjectRulesNotifyCtl(t *testing.T) {
	s := newSigner(t)
	body := []byte("synthetic notifications control body")
	c := notifyClaims("health")
	tok, _, err := s.Mint(c, body, 5*time.Second)
	if err != nil {
		t.Fatalf("mint notify.v1.ctl.health: %v", err)
	}
	want := Expect{Aud: "notifications", Subject: "notify.v1.ctl.health", Kind: KindReq}
	got, err := VerifyAt(tok, s.PublicKeys(), want, body, testNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := CheckReply(got, "_INBOX_gateway.x", false); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got.Hid != testHospital || got.Sub != SvcCore {
		t.Fatalf("claims: %+v", got)
	}
	// Another audience on the notifications subject: never minted, never
	// verified.
	for _, aud := range []string{"emr", "documents", "pipeline", "echo"} {
		bad := c
		bad.Aud = aud
		if _, _, err := s.Mint(bad, body, 5*time.Second); !errors.Is(err, ErrMintClaims) {
			t.Errorf("minted aud %s on notify.v1.ctl.health: %v", aud, err)
		}
		forged := resign(t, s, tok, func(_, p map[string]any) { p["aud"] = aud })
		if _, err := VerifyAt(forged, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrClaims) {
			t.Errorf("aud %s verified by notifications: %v", aud, err)
		}
	}
	// aud notifications on another service's control subject is refused,
	// and a notifications token moved there doesn't verify.
	for _, subj := range []string{"documents.v1.ctl.health", "emr.v1.ctl.health"} {
		cross := c
		cross.Subject = subj
		if _, _, err := s.Mint(cross, nil, 5*time.Second); !errors.Is(err, ErrMintClaims) {
			t.Errorf("minted aud notifications on %s: %v", subj, err)
		}
		moved := resign(t, s, tok, func(_, p map[string]any) { p["nsub"] = subj })
		if _, err := VerifyAt(moved, s.PublicKeys(), Expect{Aud: "notifications", Subject: subj, Kind: KindReq},
			body, testNow); err == nil {
			t.Errorf("a notifications token verified on %s", subj)
		}
	}
	docs := resign(t, s, tok, func(_, p map[string]any) { p["aud"], p["nsub"] = "documents", "documents.v1.ctl.health" })
	if _, err := VerifyAt(docs, s.PublicKeys(), want, body, testNow); err == nil {
		t.Error("a documents token verified on notify.v1.ctl.health")
	}
	// Only core calls the notifications service: no grant token.
	grant := c
	grant.Via, grant.ReplyPrefix = ViaGrant, "_INBOX_bff.clinical."
	if _, _, err := s.Mint(grant, body, 2*time.Second); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted a grant for the notifications service: %v", err)
	}
	g := resign(t, s, tok, func(_, p map[string]any) { p["via"], p["rpl"] = "grant", "_INBOX_bff.clinical." })
	if _, err := VerifyAt(g, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("grant on notify.v1.ctl.health: %v, want ErrBinding", err)
	}
	// No step-up on it, at mint or at verify.
	stp := c
	stp.StepUp = true
	if _, _, err := s.Mint(stp, body, 5*time.Second); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted stp on notify.v1.ctl.health: %v", err)
	}
	if _, err := VerifyAt(resign(t, s, tok, func(_, p map[string]any) { p["stp"] = true }), s.PublicKeys(), want, body,
		testNow); err == nil {
		t.Fatal("stp verified on notify.v1.ctl.health")
	}
}

// Shapes the table refuses for notifications subjects: a due message is a
// JetStream event, never a request token's subject.
func TestSubjectRulesNotifyCtlShapes(t *testing.T) {
	s := newSigner(t)
	for _, subj := range []string{"notify.v1.ctl", "notify.v1.ctl.health.x", "notify.v1.ctl.Health", "notify.v1.ctl.",
		"notify.v2.ctl.health", "notify.v1.ctrl.health", "notify.v1." + testHospital + ".due", "notify.v1.platform.due"} {
		c := notifyClaims("health")
		c.Subject = subj
		if _, _, err := s.Mint(c, nil, 5*time.Second); !errors.Is(err, ErrMintClaims) {
			t.Errorf("minted %q: %v", subj, err)
		}
	}
	for _, tc := range []struct {
		kind    Kind
		subject string
		aud     string
		ok      bool
	}{
		{KindReq, "notify.v1.ctl.health", "notifications", true},
		{KindReq, "notify.v1.ctl.send_link", "notifications", true},
		{KindJob, "notify.v1.ctl.health", "", false},
		{KindReq, "notify.v1." + testHospital + ".due", "", false},
	} {
		aud, ok := AudienceFor(tc.kind, tc.subject)
		if aud != tc.aud || ok != tc.ok {
			t.Errorf("AudienceFor(%s, %q) = %q, %v; want %q, %v", tc.kind, tc.subject, aud, ok, tc.aud, tc.ok)
		}
	}
}
