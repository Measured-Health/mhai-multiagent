package password

import (
	"compress/gzip"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=2,p=1$") {
		t.Fatalf("unexpected encoding: %.40s", h)
	}
	ok, err := Verify("correct horse battery staple", h)
	if err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	h2, _ := Hash("correct horse battery staple")
	if h == h2 {
		t.Fatal("two hashes of one password are identical: salt is not random")
	}
}

func TestVerifyWrongPassword(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Verify("correct horse battery stapl3", h)
	if err != nil || ok {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, enc := range []string{"", "plain", "$argon2id$v=19$m=1,t=1,p=1$AA$AA", "$argon2i$v=19$m=65536,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=4000000000,t=2,p=1$AAAA$AAAA"} {
		if ok, err := Verify("x", enc); ok || err == nil {
			t.Errorf("Verify(%q) = %v, %v; want false and an error", enc, ok, err)
		}
	}
}

func TestBreachedRejected(t *testing.T) {
	if err := Check("password1234"); !errors.Is(err, ErrBreached) {
		t.Fatalf("password1234: %v", err)
	}
	if err := Check("PassWord1234"); !errors.Is(err, ErrBreached) {
		t.Fatalf("case variant: %v", err)
	}
	if err := Check("tangerine-orbit-77-quill"); err != nil {
		t.Fatalf("good password rejected: %v", err)
	}
}

func TestShortRejected(t *testing.T) {
	if err := Check("abcdefghijk"); !errors.Is(err, ErrTooShort) { // 11
		t.Fatalf("11 chars: %v", err)
	}
	if err := Check("zq9-xv3-mw!k"); err != nil { // 12
		t.Fatalf("12 chars: %v", err)
	}
	if err := Check("ééééééééééé"); !errors.Is(err, ErrTooShort) { // 11 runes, 22 bytes
		t.Fatalf("length must count characters: %v", err)
	}
}

func TestBreachedListSize(t *testing.T) {
	f, err := listFS.Open("breached.txt.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	buf := make([]byte, 1<<16)
	for {
		m, err := zr.Read(buf)
		n += strings.Count(string(buf[:m]), "\n")
		if err != nil {
			break
		}
	}
	if n < 100000 {
		t.Fatalf("breached list has %d lines, want at least 100000", n)
	}
}

// The fixed hash behind DummyVerify must carry today's parameters, or an
// unknown login would be answered faster or slower than a real one.
func TestDummyHashMatchesParams(t *testing.T) {
	p, _, _, err := decode(dummyHash)
	if err != nil {
		t.Fatal(err)
	}
	if p != current {
		t.Fatalf("dummy hash params %+v differ from current %+v", p, current)
	}
}

func TestDummyVerifyTakesAboutAsLongAsVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	h, _ := Hash("some real password 1")
	med := func(f func()) time.Duration {
		var d []time.Duration
		for i := 0; i < 5; i++ {
			s := time.Now()
			f()
			d = append(d, time.Since(s))
		}
		for i := range d {
			for j := i + 1; j < len(d); j++ {
				if d[j] < d[i] {
					d[i], d[j] = d[j], d[i]
				}
			}
		}
		return d[2]
	}
	real := med(func() { _, _ = Verify("another password 22", h) })
	dummy := med(func() { DummyVerify("another password 22") })
	ratio := float64(dummy) / float64(real)
	if ratio < 0.7 || ratio > 1.3 {
		t.Fatalf("dummy %v vs real %v (ratio %.2f)", dummy, real, ratio)
	}
	if real < 50*time.Millisecond {
		t.Fatalf("verify took only %v: parameters too weak", real)
	}
}
