package fileexport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// rawServer is an SSH server whose sftp subsystem is handle(ch): the test
// writes the protocol itself, so it can misbehave.
func rawServer(t *testing.T, handle func(ch io.ReadWriter)) *ssh.Client {
	t.Helper()
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	hs, _ := ssh.NewSignerFromKey(hk)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(hs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		defer func() { _ = sc.Close() }()
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			ch, in, err := nc.Accept()
			if err != nil {
				return
			}
			go func() {
				for req := range in {
					_ = req.Reply(req.Type == "subsystem", nil)
					if req.Type == "subsystem" {
						go func() { handle(ch); _ = ch.Close() }()
					}
				}
			}()
		}
	}()
	cl, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "synthetic-user",
		HostKeyCallback: ssh.FixedHostKey(hs.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func readPacket(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(h[:4])-1)
	_, err := io.ReadFull(r, b)
	return h[4], b, err
}

func writePacket(w io.Writer, typ byte, parts ...[]byte) error {
	n := 1
	for _, p := range parts {
		n += len(p)
	}
	b := binary.BigEndian.AppendUint32(nil, uint32(n))
	b = append(b, typ)
	for _, p := range parts {
		b = append(b, p...)
	}
	_, err := w.Write(b)
	return err
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func str(s string) []byte { return append(u32(uint32(len(s))), s...) }

// endless answers every READDIR with another page of 500 regular files,
// forever, and counts the pages it served.
func endless(pages *atomic.Int64) func(io.ReadWriter) {
	return func(ch io.ReadWriter) {
		for {
			typ, b, err := readPacket(ch)
			if err != nil {
				return
			}
			switch typ {
			case 1: // INIT
				_ = writePacket(ch, 2, u32(3))
			case 11: // OPENDIR
				_ = writePacket(ch, 102, b[:4], str("h"))
			case 12: // READDIR
				n := pages.Add(1)
				var body []byte
				for i := 0; i < 500; i++ {
					name := fmt.Sprintf("results-20261114T%06dZ-%d.csv", n, i)
					body = append(body, str(name)...)
					body = append(body, str("-rw-r--r-- "+name)...)
					body = append(body, u32(0x4|0x1)...)
					body = append(body, make([]byte, 8)...)
					body = append(body, u32(0o100644)...)
				}
				_ = writePacket(ch, 104, b[:4], u32(500), body)
			case 4: // CLOSE
				_ = writePacket(ch, 101, b[:4], u32(0), str(""), str(""))
			}
		}
	}
}

// The listing is read a page at a time and counted as it goes: a server
// that lists without end is refused (unreadable_data) as soon as the count
// passes MaxListed, having served barely more pages than that needs.
func TestListingBoundedAsItGoes(t *testing.T) {
	var pages atomic.Int64
	cl := rawServer(t, endless(&pages))
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entries, err := listDir(c, cl, "/outbound/mhai")
	if !errors.Is(err, errListing) || entries != nil {
		t.Fatalf("endless listing: %d entries, %v", len(entries), err)
	}
	if got, most := pages.Load(), int64(MaxListed/500+2); got > most {
		t.Fatalf("served %d pages, want at most %d: the read didn't stop early", got, most)
	}
}

// A packet longer than the cap is refused before it is read into memory.
func TestListingPacketCapped(t *testing.T) {
	cl := rawServer(t, func(ch io.ReadWriter) {
		for {
			typ, b, err := readPacket(ch)
			if err != nil {
				return
			}
			switch typ {
			case 1:
				_ = writePacket(ch, 2, u32(3))
			case 11:
				_ = writePacket(ch, 102, b[:4], str("h"))
			case 12:
				// Claims 1 GiB; sends little.
				_, _ = ch.Write(append(u32(1<<30), 104))
				return
			}
		}
	})
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := listDir(c, cl, "/outbound/mhai"); !errors.Is(err, errListing) {
		t.Fatalf("oversized packet: %v", err)
	}
}

// A normal listing: names, kinds and sizes, until the server says EOF.
func TestListingReadsToEOF(t *testing.T) {
	cl := rawServer(t, func(ch io.ReadWriter) {
		served := false
		for {
			typ, b, err := readPacket(ch)
			if err != nil {
				return
			}
			switch typ {
			case 1:
				_ = writePacket(ch, 2, u32(3))
			case 11:
				_ = writePacket(ch, 102, b[:4], str("h"))
			case 12:
				if served {
					_ = writePacket(ch, 101, b[:4], u32(1), str("eof"), str(""))
					continue
				}
				served = true
				var body []byte
				for _, e := range []struct {
					name string
					mode uint32
				}{{".", 0o40755}, {"processed", 0o40755}, {"results-20261114T060000Z.csv", 0o100644}, {"link.csv", 0o120777}} {
					body = append(body, str(e.name)...)
					body = append(body, str("")...)
					body = append(body, u32(0x4|0x1)...)
					body = append(body, binary.BigEndian.AppendUint64(nil, 42)...)
					body = append(body, u32(e.mode)...)
				}
				_ = writePacket(ch, 104, b[:4], u32(4), body)
			case 4:
				_ = writePacket(ch, 101, b[:4], u32(0), str(""), str(""))
			}
		}
	})
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entries, err := listDir(c, cl, "/outbound/mhai")
	if err != nil || len(entries) != 3 {
		t.Fatalf("%+v %v", entries, err)
	}
	if e := entries[1]; e.name != "results-20261114T060000Z.csv" || !e.regular || e.dir || e.size != 42 {
		t.Errorf("file: %+v", e)
	}
	if !entries[0].dir || entries[2].regular || entries[2].dir {
		t.Errorf("dir and link: %+v", entries)
	}
}
