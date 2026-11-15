// Package storage stores and reads the documents service's sealed objects
// (documents design §6): each original and page is sealed by package
// envelope under a fresh data key wrapped by the hospital's KEK, and put
// at its id-only key (originals/<hid>/<doc>, pages/<hid>/<doc>/<n>). The
// caller keeps the wrapped key and its KEK version in document_objects
// (PR 4's scan writes them in the same transaction as the status).
//
// Readiness (§5.4, §6.2): the service takes no upload unless the bucket's
// lifecycle deletes quarantine/ within a day and expires non-current
// versions within 7 days, and the keyring is ready.
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
)

// MaxObjectBytes is the largest plaintext stored: the largest purpose's
// limit (§4.1, 25 MB). A page is far smaller.
const MaxObjectBytes = 25 << 20

// sealedType is every sealed object's content type: opaque bytes.
const sealedType = "application/octet-stream"

// ErrTooLarge: a plaintext over MaxObjectBytes; nothing is stored.
var ErrTooLarge = errors.New("storage: object too large")

// Objects is the object store (s3.Client).
type Objects interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Lifecycle(ctx context.Context) ([]byte, error)
}

// Store seals, stores and opens objects.
type Store struct {
	Objects Objects
	Keys    kek.Provider
}

// Put seals src (at most MaxObjectBytes) for ref and stores it. The
// ciphertext is held in memory to give the store its length (§12: a 25 MB
// original at most).
func (s *Store) Put(ctx context.Context, ref envelope.Ref, src io.Reader) (envelope.Sealed, error) {
	var buf bytes.Buffer
	lr := &io.LimitedReader{R: src, N: MaxObjectBytes + 1}
	sealed, err := envelope.Seal(ctx, s.Keys, ref, &buf, lr)
	if err != nil {
		return envelope.Sealed{}, err
	}
	if sealed.Size > MaxObjectBytes {
		return envelope.Sealed{}, ErrTooLarge
	}
	if err := s.Objects.Put(ctx, ref.Key(), buf.Bytes(), sealedType); err != nil {
		return envelope.Sealed{}, err
	}
	return sealed, nil
}

// Open reads the object for ref with its wrapped key; the caller closes
// the reader. A misplaced or changed object is envelope.ErrDecrypt.
func (s *Store) Open(ctx context.Context, ref envelope.Ref, wrapped []byte, kekVersion int) (io.ReadCloser, error) {
	if err := ref.Valid(); err != nil {
		return nil, err
	}
	body, err := s.Objects.Get(ctx, ref.Key())
	if err != nil {
		return nil, err
	}
	r, err := envelope.Open(ctx, s.Keys, ref, wrapped, kekVersion, body)
	if err != nil {
		_ = body.Close()
		return nil, err
	}
	return readCloser{r, body}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Delete removes ref's object. The caller deletes its row (and so its
// wrapped key) first: the object is unreadable from that moment (§6.3).
func (s *Store) Delete(ctx context.Context, ref envelope.Ref) error {
	if err := ref.Valid(); err != nil {
		return err
	}
	return s.Objects.Delete(ctx, ref.Key())
}

// DeleteQuarantine removes a document's quarantine object, as soon as its
// scan finishes either way (§5.4).
func (s *Store) DeleteQuarantine(ctx context.Context, hospital, document uuid.UUID) error {
	if hospital == uuid.Nil || document == uuid.Nil {
		return envelope.ErrRef
	}
	return s.Objects.Delete(ctx, envelope.QuarantineKey(hospital, document))
}

// Readiness checks the bucket's lifecycle and caches the answer, so a
// burst of requests asks the store once.
type Readiness struct {
	Objects Objects
	// TTL is how long an answer is kept (a minute when zero).
	TTL time.Duration
	// Now is nil for time.Now.
	Now func() time.Time

	mu    sync.Mutex
	until time.Time
	err   error
}

// ErrNotConfigured: no object store is configured.
var ErrNotConfigured = errors.New("storage: no object store configured")

// Check reports nil when the store answers with the lifecycle in force.
func (r *Readiness) Check(ctx context.Context) error {
	if r == nil || r.Objects == nil {
		return ErrNotConfigured
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	ttl := r.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if now().Before(r.until) {
		return r.err
	}
	raw, err := r.Objects.Lifecycle(ctx)
	if err == nil {
		err = s3.CheckLifecycle(raw)
	}
	r.err, r.until = err, now().Add(ttl)
	return err
}
