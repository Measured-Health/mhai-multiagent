package hl7bridge

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
)

var at = time.Date(2026, 11, 27, 10, 15, 0, 0, time.UTC)

func oru(ctrl string) []byte {
	return []byte(hl7test.ORU(ctrl, "SYN-ORD-1", at, hl7test.Potassium(at)))
}

func TestFramerReadsFrames(t *testing.T) {
	a, b := oru("C1"), oru("C2")
	stream := append([]byte("junk\r\n"), Frame(a)...)
	stream = append(stream, Frame(b)...)
	// One byte at a time: frames split across reads still read whole.
	fr := NewFramer(iotest.OneByteReader(bytes.NewReader(stream)), hl7v2.MaxMessage)
	starts, ends := 0, 0
	fr.OnStart, fr.OnEnd = func() { starts++ }, func() { ends++ }
	got1, err := fr.Next()
	if err != nil || !bytes.Equal(got1, a) {
		t.Fatalf("first: %v", err)
	}
	got2, err := fr.Next()
	if err != nil || !bytes.Equal(got2, b) {
		t.Fatalf("second: %v", err)
	}
	if _, err := fr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
	if fr.Discarded != 6 || starts != 2 || ends != 2 {
		t.Fatalf("discarded %d, starts %d, ends %d", fr.Discarded, starts, ends)
	}
}

func TestFramerRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		in   []byte
		want error
	}{
		"start inside":   {append([]byte{startBlock}, append(oru("C1"), Frame(oru("C2"))...)...), ErrFraming},
		"end without CR": {append(append([]byte{startBlock}, oru("C1")...), endBlock, 'x'), ErrFraming},
		"cut short":      {append([]byte{startBlock}, oru("C1")...), io.ErrUnexpectedEOF},
		"over the cap":   {Frame(bytes.Repeat([]byte("A"), 101)), ErrFrameTooLarge},
		"end, then EOF":  {append(append([]byte{startBlock}, oru("C1")...), endBlock), io.ErrUnexpectedEOF},
	} {
		fr := NewFramer(bytes.NewReader(c.in), map[bool]int{true: 100, false: hl7v2.MaxMessage}[name == "over the cap"])
		got, err := fr.Next()
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(got) > hl7v2.MaxMessage {
			t.Errorf("%s: %d bytes kept", name, len(got))
		}
	}
}

func TestAck(t *testing.T) {
	m, err := hl7v2.ParseMessage(oru("SYN-CTRL-9"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := hl7v2.ParseMessage(Ack(m, AckAccept, "", "MHAI-1", at))
	if err != nil {
		t.Fatalf("the ACK doesn't parse: %v", err)
	}
	msa := &a.Segs[1]
	if typ, _ := a.Type(); typ != "ACK" || msa.ID != "MSA" {
		t.Fatalf("ack %q", a.Text)
	}
	if v, _ := a.V(msa, 1, 1); v != "AA" {
		t.Fatal(v)
	}
	if v, _ := a.V(msa, 2, 1); v != "SYN-CTRL-9" {
		t.Fatal(v)
	}
	if v, _ := a.V(a.MSH(), 3, 1); v != "MHAI" { // receiving app becomes sending
		t.Fatal(v)
	}
	if v, _ := a.V(a.MSH(), 5, 1); v != "SYNTH-LAB" {
		t.Fatal(v)
	}
	e := string(Ack(m, AckError, errSequence, "MHAI-2", at))
	if !strings.Contains(e, "MSA|AE|SYN-CTRL-9") || !strings.Contains(e, "ERR|||100^^HL70357|E") ||
		strings.Contains(e, hl7test.Family) {
		t.Fatalf("error ack %q", e)
	}
	// A control id with delimiters and controls is escaped and cleaned.
	bad := strings.Replace(string(oru("X")), "|X|", `|A\F\B`+"\x01"+`|`, 1)
	h, ok := hl7v2.ParseHeader([]byte(bad))
	if !ok {
		t.Fatal("header")
	}
	if out, err := hl7v2.ParseMessage(Ack(h, AckReject, errRequired, "MHAI-3", at)); err != nil {
		t.Fatalf("escaped ack: %v", err)
	} else if v, _ := out.V(&out.Segs[1], 2, 1); v != "A|B" {
		t.Fatalf("echo %q", v)
	}
}

// The framer never panics, never yields more than the cap, and every frame
// it yields holds no MLLP block byte.
func FuzzMLLPReader(f *testing.F) {
	f.Add(Frame(oru("C1")))
	f.Add(append(Frame(oru("C1")), Frame(oru("C2"))...))
	f.Add([]byte{startBlock, 'M', endBlock})
	f.Add([]byte{startBlock, startBlock, endBlock, carriage})
	f.Add([]byte("junk"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fr := NewFramer(bytes.NewReader(data), 4096)
		for i := 0; i < 1000; i++ {
			frame, err := fr.Next()
			if len(frame) > 4096 {
				t.Fatalf("frame of %d bytes", len(frame))
			}
			if err != nil {
				return
			}
			if bytes.IndexByte(frame, startBlock) >= 0 || bytes.IndexByte(frame, endBlock) >= 0 {
				t.Fatal("a block byte inside a frame")
			}
		}
	})
}
