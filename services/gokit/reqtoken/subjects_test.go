package reqtoken

import (
	"errors"
	"testing"
	"time"
)

func emrClaims(op string) Claims {
	return Claims{Aud: "emr", Sub: testUser, Hid: testHospital, Roles: []string{"platform_admin"}, Rid: testRid,
		Kind: KindReq, Subject: "emr.v1.ctl." + op, ReplyPrefix: ReplyCore, Via: ViaCore}
}

// A4 (EMR design §3.2, plan departure 4): emr.v1.ctl.<op> is a request
// subject whose audience is always emr. It mints and verifies for aud emr
// only; any other audience on it is refused at mint and at verify.
func TestSubjectRulesEMRCtl(t *testing.T) {
	s := newSigner(t)
	body := []byte("synthetic emr control body")
	c := emrClaims("test")
	tok, _, err := s.Mint(c, body, 5*time.Second)
	if err != nil {
		t.Fatalf("mint emr.v1.ctl.test: %v", err)
	}
	want := Expect{Aud: "emr", Subject: "emr.v1.ctl.test", Kind: KindReq}
	got, err := VerifyAt(tok, s.PublicKeys(), want, body, testNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := CheckReply(got, "_INBOX_gateway.x", false); err != nil {
		t.Fatalf("reply: %v", err)
	}

	// Another audience on the emr subject: core never signs it...
	bad := c
	bad.Aud = "pipeline"
	if _, _, err := s.Mint(bad, body, 5*time.Second); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted aud pipeline on emr.v1.ctl.test: %v", err)
	}
	// ...and a validly signed one is refused by its verifier.
	forged := resign(t, s, tok, func(_, p map[string]any) { p["aud"] = "pipeline" })
	if _, err := VerifyAt(forged, s.PublicKeys(), Expect{Aud: "pipeline", Subject: "emr.v1.ctl.test", Kind: KindReq},
		body, testNow); err == nil {
		t.Fatal("aud pipeline on emr.v1.ctl.test verified")
	}
	// The emr service doesn't accept a pipeline token either.
	if _, err := VerifyAt(forged, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrClaims) {
		t.Fatalf("aud pipeline verified by emr: %v", err)
	}
	// aud emr on an inference subject is refused both ways.
	cross := reqClaims()
	cross.Aud = "emr"
	if _, _, err := s.Mint(cross, nil, 5*time.Second); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted aud emr on an inference subject: %v", err)
	}
	inf := resign(t, s, mint(t, s, reqClaims(), nil, 5*time.Second), func(_, p map[string]any) { p["aud"] = "emr" })
	if _, err := VerifyAt(inf, s.PublicKeys(), Expect{Aud: "emr", Subject: "inference.v1.echo.ping", Kind: KindReq},
		nil, testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("aud emr on inference.v1.echo.ping: %v, want ErrBinding", err)
	}

	// Only core calls the emr service: no grant (BFF-direct) token.
	grant := c
	grant.Via, grant.ReplyPrefix = ViaGrant, "_INBOX_bff.platform."
	if _, _, err := s.Mint(grant, body, 2*time.Second); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted a grant for the emr service: %v", err)
	}
	g := resign(t, s, tok, func(_, p map[string]any) { p["via"], p["rpl"] = "grant", "_INBOX_bff.platform." })
	if _, err := VerifyAt(g, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("grant on emr.v1.ctl.test: %v, want ErrBinding", err)
	}
}

// Shapes the table refuses: too few or too many segments, an op that isn't
// a name, and the job kind on a control subject.
func TestSubjectRulesEMRCtlShapes(t *testing.T) {
	s := newSigner(t)
	for _, subj := range []string{"emr.v1.ctl", "emr.v1.ctl.test.x", "emr.v1.ctl.Test", "emr.v1.ctl.", "emr.v2.ctl.test",
		"emr.v1.ctrl.test", "emr.v1.ctl.te-st"} {
		c := emrClaims("test")
		c.Subject = subj
		if _, _, err := s.Mint(c, nil, 5*time.Second); !errors.Is(err, ErrMintClaims) {
			t.Errorf("minted %q: %v", subj, err)
		}
		tok := resign(t, s, mint(t, s, emrClaims("test"), nil, 5*time.Second), func(_, p map[string]any) { p["nsub"] = subj })
		if _, err := VerifyAt(tok, s.PublicKeys(), Expect{Aud: "emr", Subject: subj, Kind: KindReq}, nil, testNow); !errors.Is(err, ErrBinding) {
			t.Errorf("verified %q: %v, want ErrBinding", subj, err)
		}
	}
	job := emrClaims("test")
	job.Kind, job.ReplyPrefix = KindJob, ""
	if _, _, err := s.Mint(job, nil, time.Minute); !errors.Is(err, ErrMintClaims) {
		t.Errorf("minted a job on emr.v1.ctl.test: %v", err)
	}
}

// The table gives the audience each subject fixes; nothing outside it has
// one.
func TestAudienceFor(t *testing.T) {
	for _, tc := range []struct {
		kind    Kind
		subject string
		aud     string
		ok      bool
	}{
		{KindReq, "inference.v1.echo.ping", "echo", true},
		{KindReq, "inference.v1.pipeline.health", "pipeline", true},
		{KindReq, "emr.v1.ctl.health", "emr", true},
		{KindReq, "emr.v1.ctl.put_credentials", "emr", true},
		{KindJob, "jobs.v1." + testHospital + ".run_query", "", true},
		{KindJob, "jobs.v1.global.retention_purge", "", true},
		{KindReq, "jobs.v1.global.retention_purge", "", false},
		{KindJob, "emr.v1.ctl.test", "", false},
		{KindReq, "emr.v1." + testHospital + ".fetched", "", false},
		{KindReq, "records.v1." + testHospital + ".created", "", false},
		{KindReq, "inference.v1.Echo.ping", "", false},
		{KindReq, "inference.v1.echo", "", false},
		{KindJob, "jobs.v1.not-a-hospital.run_query", "", false},
		{"other", "inference.v1.echo.ping", "", false},
	} {
		aud, ok := AudienceFor(tc.kind, tc.subject)
		if aud != tc.aud || ok != tc.ok {
			t.Errorf("AudienceFor(%s, %q) = %q, %v; want %q, %v", tc.kind, tc.subject, aud, ok, tc.aud, tc.ok)
		}
	}
}

// Every inference and job case the package already had is unchanged: the
// shared vectors (TestRequestTokenVectorsUpToDate) cover the verdicts; here
// the minting side for each existing shape.
func TestSubjectRulesUnchangedForInferenceAndJobs(t *testing.T) {
	s := newSigner(t)
	for _, c := range []Claims{reqClaims(), jobClaims(),
		{Aud: "pipeline", Sub: SvcCore, Rid: testRid, Kind: KindJob, Subject: "jobs.v1.global.retention_purge", Via: ViaCore}} {
		ttl := 5 * time.Second
		if _, _, err := s.Mint(c, nil, ttl); err != nil {
			t.Errorf("mint %s: %v", c.Subject, err)
		}
	}
	other := jobClaims()
	other.Subject = "jobs.v1." + otherHosp + ".run_query"
	if _, _, err := s.Mint(other, nil, time.Minute); !errors.Is(err, ErrMintClaims) {
		t.Errorf("minted a job for another hospital's subject: %v", err)
	}
}
