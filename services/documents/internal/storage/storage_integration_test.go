//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3/s3test"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/storage"
)

// DS3 against a real object store: an original sealed for hospital A is
// copied, byte for byte, to hospital B's prefix, to another document and to
// a page; with its wrapped key, none of the copies opens. The original
// itself reads back, and the readiness check passes on the deployed
// lifecycle.
func TestSealedObjectsOutOfPlaceDoNotOpen(t *testing.T) {
	st := s3test.Start(t, true)
	ctx := context.Background()
	ring := filepath.Join(t.TempDir(), "keyring.json")
	if err := kek.InitFile(ring); err != nil {
		t.Fatal(err)
	}
	s := &storage.Store{Objects: st.Client, Keys: kek.NewFile(ring, kek.Create)}
	if err := (&storage.Readiness{Objects: st.Client}).Check(ctx); err != nil {
		t.Fatalf("readiness: %v", err)
	}
	hospA, hospB, doc := uuid.New(), uuid.New(), uuid.New()
	plain := make([]byte, envelope.SegmentSize+4096)
	_, _ = rand.Read(plain)
	src := envelope.Ref{Hospital: hospA, Document: doc, Kind: envelope.Original}
	sealed, err := s.Put(ctx, src, bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// Hospital B has a key of its own.
	if _, err := s.Put(ctx, envelope.Ref{Hospital: hospB, Document: uuid.New(), Kind: envelope.Original},
		bytes.NewReader([]byte("synthetic"))); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(ctx, src, sealed.WrappedDEK, sealed.KEKVersion)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("read back: %v", err)
	}
	raw, err := st.Client.Get(ctx, src.Key())
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := io.ReadAll(raw)
	_ = raw.Close()
	for name, dst := range map[string]envelope.Ref{
		"hospital B":     {Hospital: hospB, Document: doc, Kind: envelope.Original},
		"other document": {Hospital: hospA, Document: uuid.New(), Kind: envelope.Original},
		"as a page":      {Hospital: hospA, Document: doc, Kind: envelope.Page, N: 1},
	} {
		if err := st.Client.Put(ctx, dst.Key(), ct, "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		rc, err := s.Open(ctx, dst, sealed.WrappedDEK, sealed.KEKVersion)
		if err == nil {
			_, err = io.ReadAll(rc)
			_ = rc.Close()
		}
		if !errors.Is(err, envelope.ErrDecrypt) {
			t.Errorf("%s: %v, want ErrDecrypt", name, err)
		}
	}
}
