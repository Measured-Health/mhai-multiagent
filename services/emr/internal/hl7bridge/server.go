package hl7bridge

// The MLLP receiver (design §4.1).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
)

// MLLP limits.
const (
	MaxConnections = 16
	MLLPIdle       = 5 * time.Minute
	FrameTimeout   = 2 * time.Minute
)

// Stored are the message types the spool keeps: those the hl7v2_file
// connector reads. Others are acknowledged and dropped (never on our disk).
var Stored = map[string]bool{"ORU^R01": true, "RDE^O11": true}

// Allowlist is a set of address ranges.
type Allowlist struct{ prefixes []netip.Prefix }

// ParseAllowlist reads comma-separated CIDRs (an address alone is /32 or
// /128). Empty is an error: there is no default.
func ParseAllowlist(s string) (*Allowlist, error) {
	a := &Allowlist{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			ad, err := netip.ParseAddr(p)
			if err != nil {
				return nil, errors.New("hl7bridge: an allow-list entry is not an address or range")
			}
			p = netip.PrefixFrom(ad, ad.BitLen()).String()
		}
		pf, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, errors.New("hl7bridge: an allow-list entry is not an address or range")
		}
		a.prefixes = append(a.prefixes, pf.Masked())
	}
	if len(a.prefixes) == 0 {
		return nil, errors.New("hl7bridge: the allow-list is empty")
	}
	return a, nil
}

// Allows reports whether ip is in a range.
func (a *Allowlist) Allows(ip netip.Addr) bool {
	if a == nil || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, p := range a.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(c net.Conn) netip.Addr {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

// Counters are counts only.
type Counters struct {
	Accepted, Duplicates, Dropped, Errors, Rejected, Refused, NoAck atomic.Int64
	LastMessage                                                     atomic.Int64 // unix seconds
}

// Receiver is the MLLP listener.
type Receiver struct {
	Spool   *Spool
	Allow   *Allowlist
	Log     *slog.Logger
	Counts  *Counters
	Now     func() time.Time
	conns   atomic.Int64
	ctrlSeq atomic.Int64
	prefix  string
	once    sync.Once
}

func (r *Receiver) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// ackID is a new control id for an acknowledgement.
func (r *Receiver) ackID() string {
	r.once.Do(func() {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		r.prefix = "MHAI" + strings.ToUpper(hex.EncodeToString(b))
	})
	return r.prefix + "-" + itoa(r.ctrlSeq.Add(1))
}

func itoa(n int64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	return string(b[i:])
}

// Serve accepts on ln until ctx ends.
func (r *Receiver) Serve(ctx context.Context, ln net.Listener) error {
	if r.Counts == nil {
		r.Counts = &Counters{}
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		ip := remoteIP(c)
		if !r.Allow.Allows(ip) {
			r.Counts.Refused.Add(1)
			r.log().Warn("mllp_refused", "reason", "address", "remote", ip.String())
			_ = c.Close()
			continue
		}
		if r.conns.Add(1) > MaxConnections {
			r.conns.Add(-1)
			r.Counts.Refused.Add(1)
			r.log().Warn("mllp_refused", "reason", "connections", "remote", ip.String())
			_ = c.Close()
			continue
		}
		go func() {
			defer r.conns.Add(-1)
			defer func() { _ = c.Close() }()
			r.serveConn(c)
		}()
	}
}

// deadlineReader sets a read deadline before each read: the idle time, or
// sooner when a frame started and must end by its deadline.
type deadlineReader struct {
	c     net.Conn
	idle  time.Duration
	frame *time.Time
}

func (d deadlineReader) Read(b []byte) (int, error) {
	dl := time.Now().Add(d.idle)
	if !d.frame.IsZero() && d.frame.Before(dl) {
		dl = *d.frame
	}
	_ = d.c.SetReadDeadline(dl)
	return d.c.Read(b)
}

// serveConn reads frames and answers each; a framing error, an over-cap
// frame or a spool failure ends the connection.
func (r *Receiver) serveConn(c net.Conn) {
	var frameBy time.Time
	fr := NewFramer(deadlineReader{c: c, idle: MLLPIdle, frame: &frameBy}, hl7v2.MaxMessage)
	fr.OnStart = func() { frameBy = time.Now().Add(FrameTimeout) }
	fr.OnEnd = func() { frameBy = time.Time{} }
	for {
		frame, err := fr.Next()
		if err != nil {
			if errors.Is(err, ErrFrameTooLarge) || errors.Is(err, ErrFraming) {
				r.Counts.Rejected.Add(1)
				r.log().Warn("mllp_rejected", "code", map[bool]string{true: "too_large", false: "framing"}[errors.Is(err, ErrFrameTooLarge)])
				if h, ok := hl7v2.ParseHeader(frame); ok {
					r.write(c, Ack(h, AckReject, errSequence, r.ackID(), r.now()))
				}
			}
			return
		}
		ack, keep := r.handle(frame)
		if !keep {
			r.Counts.NoAck.Add(1)
			r.log().Error("mllp_no_ack", "reason", "spool")
			return // the sender keeps the message and retries
		}
		if ack != nil && !r.write(c, ack) {
			return
		}
	}
}

func (r *Receiver) write(c net.Conn, ack []byte) bool {
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.Write(Frame(ack))
	return err == nil
}

// handle is one frame's answer. keep is false when the message couldn't be
// stored: no acknowledgement then, and the connection closes.
func (r *Receiver) handle(frame []byte) (ack []byte, keep bool) {
	m, err := hl7v2.ParseMessage(frame)
	if err != nil {
		h, ok := hl7v2.ParseHeader(frame)
		if !ok {
			r.Counts.Rejected.Add(1)
			r.log().Warn("mllp_rejected", "code", "no_header")
			return nil, true // nothing to answer with
		}
		code, errCode, reason := AckError, errSequence, "shape"
		var he *hl7v2.Error
		if errors.As(err, &he) {
			reason = he.Reason
			switch he.Reason {
			case hl7v2.ReasonHeader:
				code, errCode = AckReject, errRequired
				if v, _ := h.V(h.MSH(), 12, 1); v != "" && !strings.HasPrefix(v, "2.") {
					errCode = errVersion
				}
			case hl7v2.ReasonEncoding:
				code, errCode = AckReject, errDataType
			case hl7v2.ReasonMessageTooLarge, hl7v2.ReasonSegmentTooLong:
				code = AckReject
			}
		}
		r.Counts.Errors.Add(1)
		r.log().Warn("mllp_rejected", "code", reason)
		return Ack(h, code, errCode, r.ackID(), r.now()), true
	}
	typ, ev := m.Type()
	proc, _ := m.V(m.MSH(), 11, 1)
	if !Stored[typ+"^"+ev] || proc != "P" {
		r.Counts.Dropped.Add(1)
		r.log().Info("mllp_dropped", "type", typ+"^"+ev)
		return Ack(m, AckAccept, "", r.ackID(), r.now()), true
	}
	dup, err := r.Spool.Append(frame, KeyOf(m))
	if err != nil {
		return nil, false
	}
	r.Counts.LastMessage.Store(r.now().Unix())
	if dup {
		r.Counts.Duplicates.Add(1)
		r.log().Info("mllp_duplicate")
	} else {
		r.Counts.Accepted.Add(1)
	}
	return Ack(m, AckAccept, "", r.ackID(), r.now()), true
}
