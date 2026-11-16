package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
)

// Synthetic ids only.
var (
	hospA = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	hospB = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	docA  = uuid.MustParse("55555555-5555-4555-8555-555555555555")
)

// memObjects is an in-memory store that checks keys as the client does.
type memObjects struct {
	mu        sync.Mutex
	m         map[string][]byte
	lifecycle []byte
	lcErr     error
	lcCalls   int
	putErr    error
}

func (o *memObjects) Put(_ context.Context, key string, body []byte, _ string) error {
	if !s3.AllowedKey(key) {
		return s3.ErrKey
	}
	if o.putErr != nil {
		return o.putErr
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = map[string][]byte{}
	}
	o.m[key] = bytes.Clone(body)
	return nil
}

func (o *memObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.m[key]
	if !ok {
		return nil, s3.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (o *memObjects) Delete(_ context.Context, key string) error {
	if !s3.AllowedKey(key) {
		return s3.ErrKey
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, key)
	return nil
}

func (o *memObjects) Lifecycle(context.Context) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lcCalls++
	return o.lifecycle, o.lcErr
}

func newStore(t *testing.T) (*Store, *memObjects) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := kek.InitFile(path); err != nil {
		t.Fatal(err)
	}
	o := &memObjects{}
	return &Store{Objects: o, Keys: kek.NewFile(path, kek.Create)}, o
}

func TestPutOpenDelete(t *testing.T) {
	s, o := newStore(t)
	ctx := context.Background()
	plain := make([]byte, 3*envelope.SegmentSize/2)
	_, _ = rand.Read(plain)
	ref := envelope.Ref{Hospital: hospA, Document: docA, Kind: envelope.Original}
	sealed, err := s.Put(ctx, ref, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	stored := o.m[ref.Key()]
	if len(stored) <= len(plain) || bytes.Contains(stored, plain[:64]) {
		t.Fatal("the store holds plaintext")
	}
	rc, err := s.Open(ctx, ref, sealed.WrappedDEK, sealed.KEKVersion)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("read back: %v", err)
	}
	// DS3 through the store: the object copied under hospital B's key.
	moved := envelope.Ref{Hospital: hospB, Document: docA, Kind: envelope.Original}
	o.m[moved.Key()] = stored
	if rc, err := s.Open(ctx, moved, sealed.WrappedDEK, sealed.KEKVersion); err == nil {
		_, err = io.ReadAll(rc)
		if !errors.Is(err, envelope.ErrDecrypt) {
			t.Fatalf("moved object read: %v", err)
		}
	} else if !errors.Is(err, envelope.ErrDecrypt) {
		t.Fatalf("moved object: %v", err)
	}
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, ref, sealed.WrappedDEK, sealed.KEKVersion); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := s.Open(ctx, envelope.Ref{}, nil, 1); !errors.Is(err, envelope.ErrRef) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, envelope.Ref{}); !errors.Is(err, envelope.ErrRef) {
		t.Fatal(err)
	}
}

func TestPutRefusesTooLargeAndStoreErrors(t *testing.T) {
	s, o := newStore(t)
	ctx := context.Background()
	ref := envelope.Ref{Hospital: hospA, Document: docA, Kind: envelope.Page, N: 1}
	big := io.LimitReader(zeros{}, MaxObjectBytes+1)
	if _, err := s.Put(ctx, ref, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if len(o.m) != 0 {
		t.Fatal("a too-large object was stored")
	}
	o.putErr = &s3.StatusError{Code: 503}
	if _, err := s.Put(ctx, ref, strings.NewReader("x")); err == nil {
		t.Fatal("a failed put returned a sealed object")
	}
	if _, err := s.Put(ctx, envelope.Ref{}, strings.NewReader("x")); !errors.Is(err, envelope.ErrRef) {
		t.Fatal(err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestDeleteQuarantine(t *testing.T) {
	s, o := newStore(t)
	ctx := context.Background()
	key := envelope.QuarantineKey(hospA, docA)
	o.m = map[string][]byte{key: []byte("upload")}
	if err := s.DeleteQuarantine(ctx, hospA, docA); err != nil || len(o.m) != 0 {
		t.Fatalf("delete quarantine: %v", err)
	}
	if err := s.DeleteQuarantine(ctx, uuid.Nil, docA); !errors.Is(err, envelope.ErrRef) {
		t.Fatal(err)
	}
}

func TestReadinessChecksAndCachesTheLifecycle(t *testing.T) {
	deployed, err := os.ReadFile("../../../../deploy/documents/bucket-lifecycle.xml")
	if err != nil {
		t.Fatal(err)
	}
	o := &memObjects{lcErr: s3.ErrNoLifecycle}
	clock := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	r := &Readiness{Objects: o, Now: func() time.Time { return clock }}
	ctx := context.Background()
	if err := r.Check(ctx); !errors.Is(err, s3.ErrNoLifecycle) {
		t.Fatalf("no lifecycle: %v", err)
	}
	o.lcErr, o.lifecycle = nil, deployed
	if err := r.Check(ctx); !errors.Is(err, s3.ErrNoLifecycle) || o.lcCalls != 1 {
		t.Fatalf("cached: %v after %d calls", err, o.lcCalls)
	}
	clock = clock.Add(time.Minute)
	if err := r.Check(ctx); err != nil || o.lcCalls != 2 {
		t.Fatalf("after the TTL: %v after %d calls", err, o.lcCalls)
	}
	o.lifecycle = []byte("<LifecycleConfiguration/>")
	clock = clock.Add(time.Minute)
	if err := r.Check(ctx); !errors.Is(err, s3.ErrNoQuarantineRule) {
		t.Fatalf("rule removed: %v", err)
	}
	var none *Readiness
	if err := none.Check(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if err := (&Readiness{}).Check(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	// The real clock works too.
	if err := (&Readiness{Objects: &memObjects{lifecycle: deployed}}).Check(ctx); err != nil {
		t.Fatal(err)
	}
}
