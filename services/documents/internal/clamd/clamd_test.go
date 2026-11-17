package clamd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fake is a clamd that answers VERSION with version and INSTREAM by
// verdict(the streamed bytes).
type fake struct {
	version string
	verdict func([]byte) string
	got     chan []byte
}

func (f *fake) start(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return l.Addr().String()
}

func (f *fake) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	cmd, err := r.ReadString(0)
	if err != nil {
		return
	}
	switch cmd {
	case "zVERSION\x00":
		_, _ = c.Write([]byte(f.version + "\x00"))
	case "zINSTREAM\x00":
		var all bytes.Buffer
		for {
			var n uint32
			if binary.Read(r, binary.BigEndian, &n) != nil {
				return
			}
			if n == 0 {
				break
			}
			if _, err := io.CopyN(&all, r, int64(n)); err != nil {
				return
			}
		}
		if f.got != nil {
			f.got <- all.Bytes()
		}
		_, _ = c.Write([]byte(f.verdict(all.Bytes()) + "\x00"))
	}
}

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestParseVersion(t *testing.T) {
	for rep, want := range map[string]time.Time{
		"ClamAV 1.4.3/27820/Mon Nov 16 08:35:01 2026": time.Date(2026, 11, 16, 8, 35, 1, 0, time.UTC),
		"ClamAV 1.4.3/27820/Mon Nov  2 08:35:01 2026": time.Date(2026, 11, 2, 8, 35, 1, 0, time.UTC),
	} {
		got, err := ParseVersion(rep)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", rep, got, err)
		}
	}
	for _, rep := range []string{"ClamAV 1.4.3", "ClamAV 1.4.3/27820/yesterday", "something else/a/b", ""} {
		if _, err := ParseVersion(rep); !errors.Is(err, ErrStale) {
			t.Errorf("%q: %v", rep, err)
		}
	}
}

// §5.2, DS6: signatures 49 hours old, or a clamd that doesn't answer, are
// not ready.
func TestReadyNeedsFreshSignatures(t *testing.T) {
	fresh := &fake{version: "ClamAV 1.4.3/27820/Mon Nov 16 08:35:01 2026"}
	c := &Client{Addr: fresh.start(t), Now: func() time.Time { return now }}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	stale := &fake{version: "ClamAV 1.4.3/27820/" + now.Add(-49*time.Hour).Format("Mon Jan _2 15:04:05 2006")}
	c = &Client{Addr: stale.start(t), Now: func() time.Time { return now }}
	if err := c.Ready(context.Background()); !errors.Is(err, ErrStale) {
		t.Fatalf("49 hours old: %v", err)
	}
	c.MaxAge = 50 * time.Hour
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("within a raised limit: %v", err)
	}
	noDB := &fake{version: "ClamAV 1.4.3"}
	if err := (&Client{Addr: noDB.start(t)}).Ready(context.Background()); !errors.Is(err, ErrStale) {
		t.Fatalf("no database: %v", err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	if err := (&Client{Addr: addr, Timeout: time.Second}).Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped: %v", err)
	}
	if err := (*Client)(nil).Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil: %v", err)
	}
}

func TestScanStreamsAndReadsTheVerdict(t *testing.T) {
	f := &fake{got: make(chan []byte, 1), verdict: func(b []byte) string {
		switch {
		case bytes.Contains(b, []byte("SYNTHETIC-HIT")):
			return "stream: Synthetic.Test.Signature FOUND"
		case bytes.Contains(b, []byte("SYNTHETIC-LIMIT")):
			return "INSTREAM size limit exceeded. ERROR"
		case bytes.Contains(b, []byte("SYNTHETIC-ERR")):
			return "stream: Can't allocate memory ERROR"
		}
		return "stream: OK"
	}}
	c := &Client{Addr: f.start(t)}
	big := bytes.Repeat([]byte("a"), 3*chunk+17)
	infected, err := c.Scan(context.Background(), big)
	if err != nil || infected {
		t.Fatalf("clean: %v %v", infected, err)
	}
	if got := <-f.got; !bytes.Equal(got, big) {
		t.Fatalf("streamed %d bytes, want %d", len(got), len(big))
	}
	for _, c2 := range []struct {
		data     string
		infected bool
		err      error
	}{
		{"x SYNTHETIC-HIT x", true, nil},
		{"x SYNTHETIC-LIMIT x", true, nil},
		{"x SYNTHETIC-ERR x", false, ErrUnavailable},
	} {
		infected, err := c.Scan(context.Background(), []byte(c2.data))
		<-f.got
		if infected != c2.infected || !errors.Is(err, c2.err) {
			t.Errorf("%s: %v %v", c2.data, infected, err)
		}
	}
}

func TestVerdictKeepsNoSignatureName(t *testing.T) {
	for rep, want := range map[string]bool{
		"stream: OK": false,
		"stream: Heuristics.Limits.Exceeded.MaxFileSize FOUND": true,
	} {
		got, err := Verdict(rep)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", rep, got, err)
		}
	}
	if _, err := Verdict("stream: OK extra"); err == nil || strings.Contains(err.Error(), "extra") {
		t.Fatalf("an unknown reply: %v", err)
	}
}

// A clamd that hangs up, or answers nothing usable, is unavailable: never
// a clean verdict.
func TestScanFailsClosed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close() // hang up at once
		}
	}()
	c := &Client{Addr: l.Addr().String(), Timeout: 2 * time.Second}
	if infected, err := c.Scan(context.Background(), []byte("synthetic")); !errors.Is(err, ErrUnavailable) || infected {
		t.Fatalf("hang-up: %v %v", infected, err)
	}
	if _, err := c.SignatureTime(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("version hang-up: %v", err)
	}
	if infected, err := (&Client{}).Scan(context.Background(), nil); !errors.Is(err, ErrUnavailable) || infected {
		t.Fatalf("no address: %v %v", infected, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Client{Addr: l.Addr().String()}).Scan(ctx, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestDefaultsAndClock(t *testing.T) {
	f := &fake{version: "ClamAV 1.4.3/27820/" + time.Now().UTC().Add(-time.Hour).Format("Mon Jan _2 15:04:05 2006")}
	c := &Client{Addr: f.start(t)}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("an hour old with the real clock: %v", err)
	}
}
