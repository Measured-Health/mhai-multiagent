package password

import (
	"bufio"
	"compress/gzip"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	if err := Check("password1234", ""); !errors.Is(err, ErrBreached) {
		t.Fatalf("password1234: %v", err)
	}
	if err := Check("PassWord1234", ""); !errors.Is(err, ErrBreached) {
		t.Fatalf("case variant: %v", err)
	}
	if err := Check("tangerine-orbit-77-quill", ""); err != nil {
		t.Fatalf("good password rejected: %v", err)
	}
}

func TestShortRejected(t *testing.T) {
	if err := Check("abcdefghijk", ""); !errors.Is(err, ErrTooShort) { // 11
		t.Fatalf("11 chars: %v", err)
	}
	if err := Check("zq9-xv3-mw!k", ""); err != nil { // 12
		t.Fatalf("12 chars: %v", err)
	}
	if err := Check("ééééééééééé", ""); !errors.Is(err, ErrTooShort) { // 11 runes, 22 bytes
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
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		n++
		if utf8.RuneCountInString(sc.Text()) < MinLength {
			t.Fatalf("line %d is shorter than %d characters", n, MinLength)
		}
	}
	if sc.Err() != nil {
		t.Fatal(sc.Err())
	}
	if n < 100000 {
		t.Fatalf("breached list has %d lines, want at least 100000", n)
	}
	if err := Preload(); err != nil {
		t.Fatal(err)
	}
}

func TestPatternsRejected(t *testing.T) {
	for _, pw := range []string{"aaaaaaaaaaaa", "111111111111", "abcdefghijkl", "123456789012", "zyxwvutsrqpo",
		"987654321098"[:12], "qwertyuiopas", "ASDFGHJKLZXC", "poiuytrewqpo", "1234567890123456"} {
		if err := Check(pw, ""); !errors.Is(err, ErrPattern) && !errors.Is(err, ErrBreached) {
			t.Errorf("Check(%q) = %v, want a pattern or breached rejection", pw, err)
		}
	}
	// A pattern is only refused when it covers the whole password.
	if err := Check("abcdefghijkl-Tq7!", ""); err != nil {
		t.Errorf("partial run rejected: %v", err)
	}
}

func TestPatternRejectedEvenIfUnlisted(t *testing.T) {
	for _, pw := range []string{"bbbbbbbbbbbbbbbbbbbb", "mnopqrstuvwxyzab"[:14], "7777777777777"} {
		if err := Check(pw, ""); !errors.Is(err, ErrPattern) {
			t.Errorf("Check(%q) = %v, want ErrPattern", pw, err)
		}
	}
}

func TestEmailLocalPartRejected(t *testing.T) {
	email := "Grace.Nwosu@havana.example"
	if err := Check("my-GRACE.nwosu-secret-9", email); !errors.Is(err, ErrContainsEmail) {
		t.Fatalf("local part inside password: %v", err)
	}
	if err := Check("my-GRACE.nwosu-secret-9", ""); err != nil {
		t.Fatalf("no email given: %v", err)
	}
	// A local part under 4 characters is ignored.
	if err := Check("abc-tangerine-orbit", "abc@x.example"); err != nil {
		t.Fatalf("short local part: %v", err)
	}
	if err := Check("tangerine-orbit-77-quill", email); err != nil {
		t.Fatal(err)
	}
}

func TestUnicodeFormsHashAlike(t *testing.T) {
	nfc, nfd := "caf\u00e9-orbit-77-quill", "cafe\u0301-orbit-77-quill"
	h, err := Hash(nfc)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Verify(nfd, h); err != nil || !ok {
		t.Fatalf("NFD form did not verify: %v %v", ok, err)
	}
}

func TestVerifyRejectsWeakOrLenientHashes(t *testing.T) {
	salt, key := "AAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAA"
	for _, enc := range []string{
		"$argon2id$v=19$m=8,t=1,p=1$" + salt + "$" + key,         // below the cost floor
		"$argon2id$v=19$m=65536,t=1,p=1$" + salt + "$" + key,     // below the time floor
		"$argon2id$v=19junk$m=65536,t=2,p=1$" + salt + "$" + key, // trailing input
		"$argon2id$v=19$m=65536,t=2,p=1,x=9$" + salt + "$" + key,
		"$argon2id$v=19$m=+65536,t=2,p=1$" + salt + "$" + key,
	} {
		if ok, err := Verify("x", enc); ok || err == nil {
			t.Errorf("Verify(%q) = %v, %v", enc, ok, err)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	h, _ := Hash("some real password 1")
	if NeedsRehash(h) {
		t.Fatal("fresh hash flagged")
	}
	if !NeedsRehash("$argon2id$v=19$m=131072,t=2,p=1$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("different parameters not flagged")
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
	const runs = 7
	var realD, dummyD []time.Duration
	for i := 0; i < runs; i++ { // interleaved, so a load spike hits both
		s := time.Now()
		_, _ = Verify("another password 22", h)
		realD = append(realD, time.Since(s))
		s = time.Now()
		DummyVerify("another password 22")
		dummyD = append(dummyD, time.Since(s))
	}
	median := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	r, d := median(realD), median(dummyD)
	ratio := float64(d) / float64(r)
	if ratio < 0.7 || ratio > 1.3 {
		t.Fatalf("dummy %v vs real %v (ratio %.2f)", d, r, ratio)
	}
	if r < 50*time.Millisecond {
		t.Fatalf("verify took only %v: parameters too weak", r)
	}
}
