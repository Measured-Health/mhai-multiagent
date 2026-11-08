package reqtoken

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The stp claim (EMR PR 2 review): core asserts, in the signed token, that
// it checked a fresh step-up code for this request. It is optional, true
// when present, and only an emr.v1.ctl subject may carry it, so no token
// for a worker ever does.
func TestStepUpClaim(t *testing.T) {
	s := newSigner(t)
	body := []byte("synthetic emr control body")
	want := Expect{Aud: "emr", Subject: "emr.v1.ctl.put_credentials", Kind: KindReq}

	c := emrClaims("put_credentials")
	c.StepUp = true
	tok, out, err := s.Mint(c, body, 5*time.Second)
	if err != nil || !out.StepUp {
		t.Fatalf("mint with step-up: %v (stp %v)", err, out.StepUp)
	}
	got, err := VerifyAt(tok, s.PublicKeys(), want, body, testNow)
	if err != nil || !got.StepUp {
		t.Fatalf("verify: %v (stp %v)", err, got.StepUp)
	}
	if p := decodeSeg(t, strings.Split(tok, ".")[1]); p["stp"] != true {
		t.Fatalf("payload stp = %v, want true", p["stp"])
	}

	// No step-up: the claim is absent, and reads false.
	plain := mint(t, s, emrClaims("put_credentials"), body, 5*time.Second)
	if p := decodeSeg(t, strings.Split(plain, ".")[1]); p["stp"] != nil {
		t.Fatalf("stp present without step-up: %v", p["stp"])
	}
	if got, err := VerifyAt(plain, s.PublicKeys(), want, body, testNow); err != nil || got.StepUp {
		t.Fatalf("verify without stp: %v (stp %v)", err, got.StepUp)
	}

	// stp false is not a canonical token.
	f := resign(t, s, plain, func(_, p map[string]any) { p["stp"] = false })
	if _, err := VerifyAt(f, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrMalformed) {
		t.Fatalf("stp false: %v, want ErrMalformed", err)
	}
	f = resign(t, s, plain, func(_, p map[string]any) { p["stp"] = "yes" })
	if _, err := VerifyAt(f, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrMalformed) {
		t.Fatalf("stp a string: %v, want ErrMalformed", err)
	}

	// Core never signs stp for a worker...
	inf := reqClaims()
	inf.StepUp = true
	if _, _, err := s.Mint(inf, nil, 5*time.Second); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted stp on an inference subject: %v", err)
	}
	job := jobClaims()
	job.StepUp = true
	if _, _, err := s.Mint(job, nil, time.Minute); !errors.Is(err, ErrMintClaims) {
		t.Fatalf("minted stp on a job: %v", err)
	}
	// ...and a verifier refuses one that carries it.
	forged := resign(t, s, mint(t, s, reqClaims(), nil, 5*time.Second), func(_, p map[string]any) { p["stp"] = true })
	if _, err := VerifyAt(forged, s.PublicKeys(), reqExpect(), nil, testNow); !errors.Is(err, ErrClaims) {
		t.Fatalf("stp on inference.v1.echo.ping: %v, want ErrClaims", err)
	}
}
