// Package scan is the documents service's scan (documents design §5): on
// documents.v1.<hid>.uploaded (the documents_scan durable on DOCS) or from
// the sweeper, it claims the row (uploaded -> scanning, a 10-minute
// lease), then:
//
//  1. reads the quarantine object's size: over the purpose's limit is
//     too_large, without reading it (B1);
//  2. reads it (bounded), checks the magic bytes against the declared type
//     and the purpose (unsupported_type; B4, DS7) and, for an archive
//     document, the SHA-256 against the listing's (checksum_mismatch, DS19);
//  3. scans the bytes as received with clamd, after checking its
//     signatures are at most 48 hours old (malware; DS6);
//  4. renders them in docsandbox (too_many_pages, too_many_pixels,
//     password_protected, unreadable; B1-B3, DS8, DS9);
//  5. re-checks the output outside the sandbox (unreadable; B5);
//  6. renews the lease, seals and stores the original and each page under
//     the hospital's key, and writes the object rows, pages and ready in one
//     transaction;
//  7. deletes the quarantine object and announces
//     documents.v1.<hid>.scanned (ids and a code).
//
// A refusal is quarantined with its reason; nothing of the file is stored
// outside quarantine, and the quarantine object is deleted at once.
//
// Failing closed (§5.3, D8): when clamd, its signatures, the sandbox, the
// hospital's key or the store isn't there, the lease is released, the row
// stays scanning and the scan reports ErrBlocked (the message is retried
// with a back-off, the sweeper retries, and documents_scan_blocked is
// logged). A document is never ready without a clean scan; after 6 hours
// the claim ends it failed scan_unavailable.
//
// Nothing about a file reaches a log, a metric or a message: ids, reason
// codes and counts only (§11).
package scan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
)

// The outcomes a scan ends in.
const (
	OutcomeReady       = "ready"
	OutcomeQuarantined = "quarantined"
	OutcomeFailed      = "failed"
	// OutcomeSkipped: nothing to do (no such row in the hospital, or it is
	// not waiting for a scan, or another scan holds its lease).
	OutcomeSkipped = "skipped"
)

// Claim outcomes (documents_scan_claim).
const (
	ClaimClaimed      = "claimed"
	ClaimFailed       = "failed"
	ClaimNotClaimable = "not_claimable"
)

// ErrBlocked: the scan can't run now (scanner, signatures, sandbox, key or
// store unavailable); the row stays scanning, unclaimed, for a retry.
var ErrBlocked = errors.New("scan: blocked")

// Claim is a claimed row.
type Claim struct {
	Outcome  string
	Purpose  string
	MimeType string
	// SHA256 is the archive listing's (nil for a browser upload).
	SHA256 []byte
}

// Object is one sealed object for documents_scan_ready.
type Object struct {
	Kind   envelope.Kind
	N      int
	Sealed envelope.Sealed
}

// Rows are the documents_scan_* definers, under the hospital.
type Rows interface {
	Claim(ctx context.Context, hospital, document, lease uuid.UUID) (Claim, error)
	Renew(ctx context.Context, hospital, document, lease uuid.UUID) (bool, error)
	Release(ctx context.Context, hospital, document, lease uuid.UUID) (bool, error)
	Ready(ctx context.Context, hospital, document, lease uuid.UUID, sum [32]byte, pages int, objects []Object) (bool, error)
	Quarantine(ctx context.Context, hospital, document, lease uuid.UUID, reason string, sum *[32]byte) (bool, error)
}

// Quarantine is the object store's view of quarantine/ (s3.Client).
type Quarantine interface {
	Head(ctx context.Context, key string) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Store seals and stores objects (storage.Store).
type Store interface {
	Put(ctx context.Context, ref envelope.Ref, src io.Reader) (envelope.Sealed, error)
	Delete(ctx context.Context, ref envelope.Ref) error
}

// Virus is clamd (clamd.Client).
type Virus interface {
	Ready(ctx context.Context) error
	Scan(ctx context.Context, data []byte) (bool, error)
}

// Renderer is docsandbox (sandbox.Client): the pages, re-checked and
// encoded again, or a refusal reason.
type Renderer interface {
	Ready(ctx context.Context) error
	Render(ctx context.Context, data []byte, mime string, maxPages int) ([][]byte, string, error)
}

// Announcer publishes documents.v1.<hid>.scanned.
type Announcer interface {
	Scanned(ctx context.Context, hospital, document uuid.UUID, outcome, reason string, pages int) error
}

// Result is how a scan ended.
type Result struct {
	Outcome, Reason string
	Pages           int
}

// Metrics count outcomes; never anything about a file.
type Metrics struct {
	Ready, Quarantined, Failed, Skipped, Blocked, Errors atomic.Int64
}

// Scanner runs scans. Any dependency left nil is unavailable: the scan is
// blocked, never passed.
type Scanner struct {
	Rows       Rows
	Quarantine Quarantine
	Store      Store
	Virus      Virus
	Renderer   Renderer
	// HospitalKey reports whether the hospital's KEK is there (kek
	// Provider.Has).
	HospitalKey func(ctx context.Context, hospital uuid.UUID) error
	// StorageReady reports whether the store and its lifecycle are in force
	// (storage.Readiness.Check).
	StorageReady func(ctx context.Context) error
	Announce     Announcer
	Log          *slog.Logger
	Metrics      *Metrics

	blockedMu   sync.Mutex
	blockedSeen map[string]time.Time
}

func (s *Scanner) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Scanner) metrics() *Metrics {
	if s.Metrics == nil {
		s.Metrics = &Metrics{}
	}
	return s.Metrics
}

// blocked logs documents_scan_blocked (the alert, §5.3) with the reason
// code, at most once a minute per reason, and counts it.
func (s *Scanner) blocked(reason string) error {
	s.metrics().Blocked.Add(1)
	s.blockedMu.Lock()
	defer s.blockedMu.Unlock()
	if s.blockedSeen == nil {
		s.blockedSeen = map[string]time.Time{}
	}
	if t, ok := s.blockedSeen[reason]; !ok || time.Since(t) > time.Minute {
		s.blockedSeen[reason] = time.Now()
		s.log().Error("documents_scan_blocked: documents wait in scanning until the scan can run", "reason", reason)
	}
	return ErrBlocked
}

// preflight is what a scan needs before it reads anything: a reason code,
// or "".
func (s *Scanner) preflight(ctx context.Context, hospital uuid.UUID) string {
	switch {
	case s.Quarantine == nil || s.Store == nil || s.StorageReady == nil || s.StorageReady(ctx) != nil:
		return "storage"
	case s.HospitalKey == nil || s.HospitalKey(ctx, hospital) != nil:
		return "keyring"
	case s.Virus == nil || s.Virus.Ready(ctx) != nil:
		return "scanner"
	case s.Renderer == nil || s.Renderer.Ready(ctx) != nil:
		return "sandbox"
	}
	return ""
}

// Scan scans one document of hospital. It returns ErrBlocked (or a
// database error) when the document must be tried again later; otherwise
// the result, which has been written and announced.
func (s *Scanner) Scan(ctx context.Context, hospital, document uuid.UUID) (Result, error) {
	if hospital == uuid.Nil || document == uuid.Nil || s.Rows == nil {
		return Result{Outcome: OutcomeSkipped}, nil
	}
	lease := uuid.New()
	c, err := s.Rows.Claim(ctx, hospital, document, lease)
	if err != nil {
		s.metrics().Errors.Add(1)
		return Result{}, err
	}
	switch c.Outcome {
	case ClaimFailed:
		s.metrics().Failed.Add(1)
		s.log().Warn("documents: a scan never ran for 6 hours; the document failed", "document_id", document.String(),
			"reason", "scan_unavailable")
		r := Result{Outcome: OutcomeFailed, Reason: "scan_unavailable"}
		s.deleteQuarantine(ctx, hospital, document)
		s.announce(ctx, hospital, document, r)
		return r, nil
	case ClaimClaimed:
	default:
		s.metrics().Skipped.Add(1)
		return Result{Outcome: OutcomeSkipped}, nil
	}
	r, err := s.scanClaimed(ctx, hospital, document, lease, c)
	if err != nil {
		// Give the lease up so a retry can claim it at once; the row stays
		// scanning (never ready).
		if _, rerr := s.Rows.Release(context.WithoutCancel(ctx), hospital, document, lease); rerr != nil {
			s.metrics().Errors.Add(1)
		}
		return Result{}, err
	}
	return r, nil
}

func (s *Scanner) scanClaimed(ctx context.Context, hospital, document, lease uuid.UUID, c Claim) (Result, error) {
	limits, ok := PurposeLimits[c.Purpose]
	if !ok {
		return s.quarantine(ctx, hospital, document, lease, "unsupported_type", nil)
	}
	if reason := s.preflight(ctx, hospital); reason != "" {
		return Result{}, s.blocked(reason)
	}
	key := envelope.QuarantineKey(hospital, document)

	// 1. The size, before reading.
	size, err := s.Quarantine.Head(ctx, key)
	switch {
	case errors.Is(err, s3.ErrNotFound):
		// Nothing was posted (or it is gone): nothing to scan.
		return s.quarantine(ctx, hospital, document, lease, "unreadable", nil)
	case err != nil:
		return Result{}, s.blocked("storage")
	case size > limits.MaxBytes:
		return s.quarantine(ctx, hospital, document, lease, "too_large", nil)
	case size < 1:
		return s.quarantine(ctx, hospital, document, lease, "unreadable", nil)
	}

	// 2. The bytes (bounded however the store answers), type and checksum.
	data, err := s.read(ctx, key, limits.MaxBytes)
	switch {
	case errors.Is(err, errTooLarge):
		return s.quarantine(ctx, hospital, document, lease, "too_large", nil)
	case errors.Is(err, s3.ErrNotFound):
		return s.quarantine(ctx, hospital, document, lease, "unreadable", nil)
	case err != nil:
		return Result{}, s.blocked("storage")
	}
	sum := sha256.Sum256(data)
	if c.SHA256 != nil && !bytes.Equal(c.SHA256, sum[:]) {
		return s.quarantine(ctx, hospital, document, lease, "checksum_mismatch", nil)
	}
	if !TypeAllowed(c.Purpose, c.MimeType, data) {
		return s.quarantine(ctx, hospital, document, lease, "unsupported_type", &sum)
	}

	// 3. The malware scan, on the bytes as received, before any parser.
	if err := s.Virus.Ready(ctx); err != nil {
		return Result{}, s.blocked("scanner")
	}
	infected, err := s.Virus.Scan(ctx, data)
	if err != nil {
		return Result{}, s.blocked("scanner")
	}
	if infected {
		return s.quarantine(ctx, hospital, document, lease, "malware", &sum)
	}

	// 4-5. The sandbox, and the re-check of what it wrote.
	pages, reason, err := s.Renderer.Render(ctx, data, c.MimeType, limits.MaxPages)
	if err != nil {
		return Result{}, s.blocked("sandbox")
	}
	if reason != "" {
		return s.quarantine(ctx, hospital, document, lease, reason, &sum)
	}
	if len(pages) < 1 || len(pages) > limits.MaxPages {
		return s.quarantine(ctx, hospital, document, lease, "unreadable", &sum)
	}

	// 6. Store under the lease, then one transaction.
	ok, err = s.Rows.Renew(ctx, hospital, document, lease)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		s.metrics().Skipped.Add(1)
		return Result{Outcome: OutcomeSkipped}, nil
	}
	objects, err := s.store(ctx, hospital, document, data, pages)
	if err != nil {
		s.deleteObjects(ctx, hospital, document, objects)
		if errors.Is(err, errNoKey) {
			return Result{}, s.blocked("keyring")
		}
		return Result{}, s.blocked("storage")
	}
	ok, err = s.Rows.Ready(ctx, hospital, document, lease, sum, len(pages), objects)
	if err != nil || !ok {
		// Not written: the objects have no key anywhere; remove them.
		s.deleteObjects(ctx, hospital, document, objects)
		if err != nil {
			s.metrics().Errors.Add(1)
			return Result{}, err
		}
		s.metrics().Skipped.Add(1)
		return Result{Outcome: OutcomeSkipped}, nil
	}
	s.metrics().Ready.Add(1)
	r := Result{Outcome: OutcomeReady, Pages: len(pages)}
	s.deleteQuarantine(ctx, hospital, document)
	s.announce(ctx, hospital, document, r)
	return r, nil
}

var (
	errTooLarge = errors.New("scan: over the purpose's limit")
	errNoKey    = errors.New("scan: no key for the hospital")
)

func (s *Scanner) read(ctx context.Context, key string, max int64) ([]byte, error) {
	body, err := s.Quarantine.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errTooLarge
	}
	return data, nil
}

func (s *Scanner) store(ctx context.Context, hospital, document uuid.UUID, original []byte, pages [][]byte) ([]Object, error) {
	objects := make([]Object, 0, len(pages)+1)
	put := func(kind envelope.Kind, n int, b []byte) error {
		ref := envelope.Ref{Hospital: hospital, Document: document, Kind: kind, N: n}
		sealed, err := s.Store.Put(ctx, ref, bytes.NewReader(b))
		if err != nil {
			if s.HospitalKey(ctx, hospital) != nil {
				return errNoKey
			}
			return err
		}
		objects = append(objects, Object{Kind: kind, N: n, Sealed: sealed})
		return nil
	}
	if err := put(envelope.Original, 0, original); err != nil {
		return objects, err
	}
	for i, p := range pages {
		if err := put(envelope.Page, i+1, p); err != nil {
			return objects, err
		}
	}
	return objects, nil
}

func (s *Scanner) deleteObjects(ctx context.Context, hospital, document uuid.UUID, objects []Object) {
	ctx = context.WithoutCancel(ctx)
	for _, o := range objects {
		if err := s.Store.Delete(ctx, envelope.Ref{Hospital: hospital, Document: document, Kind: o.Kind, N: o.N}); err != nil {
			// Unreadable anyway: its wrapped key was never stored.
			s.metrics().Errors.Add(1)
		}
	}
}

func (s *Scanner) quarantine(ctx context.Context, hospital, document, lease uuid.UUID, reason string, sum *[32]byte) (Result, error) {
	ok, err := s.Rows.Quarantine(ctx, hospital, document, lease, reason, sum)
	if err != nil {
		s.metrics().Errors.Add(1)
		return Result{}, err
	}
	if !ok {
		s.metrics().Skipped.Add(1)
		return Result{Outcome: OutcomeSkipped}, nil
	}
	s.metrics().Quarantined.Add(1)
	s.log().Info("documents: quarantined", "document_id", document.String(), "reason", reason)
	r := Result{Outcome: OutcomeQuarantined, Reason: reason}
	s.deleteQuarantine(ctx, hospital, document)
	s.announce(ctx, hospital, document, r)
	return r, nil
}

// deleteQuarantine removes the quarantine object as soon as the scan ends
// either way (§5.4); the bucket's lifecycle is the backstop.
func (s *Scanner) deleteQuarantine(ctx context.Context, hospital, document uuid.UUID) {
	if s.Quarantine == nil {
		return
	}
	if err := s.Quarantine.Delete(context.WithoutCancel(ctx), envelope.QuarantineKey(hospital, document)); err != nil {
		s.metrics().Errors.Add(1)
		s.log().Warn("documents: the quarantine object was not deleted; the lifecycle removes it within a day",
			"document_id", document.String())
	}
}

// announce publishes documents.v1.<hid>.scanned. The row is the record: a
// lost announcement is found by core from the row (PR 5).
func (s *Scanner) announce(ctx context.Context, hospital, document uuid.UUID, r Result) {
	if s.Announce == nil {
		return
	}
	if err := s.Announce.Scanned(context.WithoutCancel(ctx), hospital, document, r.Outcome, r.Reason, r.Pages); err != nil {
		s.metrics().Errors.Add(1)
		s.log().Warn("documents: the scan outcome was not announced", "document_id", document.String())
	}
}
