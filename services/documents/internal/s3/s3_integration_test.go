//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3/s3test"
)

// post sends the policy's form with file as the browser would: every
// field, then the file last. extra fields are added before the file.
func post(t *testing.T, p s3.Policy, file []byte, override map[string]string) int {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range p.Fields {
		v := f.Value
		if o, ok := override[f.Name]; ok {
			v = o
		}
		if err := w.WriteField(f.Name, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(file)
	_ = w.Close()
	resp, err := http.Post(p.URL, w.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func ok(code int) bool { return code >= 200 && code < 300 }

// The client signs real requests: put, head, get, delete against MinIO,
// and the deployed lifecycle reads back as in force.
func TestClientAgainstMinIO(t *testing.T) {
	st := s3test.Start(t, true)
	ctx := context.Background()
	key := "originals/" + uuid.NewString() + "/" + uuid.NewString()
	if err := st.Client.Put(ctx, key, []byte("synthetic sealed bytes"), "application/octet-stream"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if n, err := st.Client.Head(ctx, key); err != nil || n != 22 {
		t.Fatalf("head: %d %v", n, err)
	}
	rc, err := st.Client.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "synthetic sealed bytes" {
		t.Fatal(string(b))
	}
	if err := st.Client.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Client.Get(ctx, key); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	lc, err := st.Client.Lifecycle(ctx)
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if err := s3.CheckLifecycle(lc); err != nil {
		t.Fatalf("the deployed lifecycle as MinIO returns it: %v", err)
	}
	// A wrong secret is refused by the store.
	bad := st.Config
	bad.Credentials.Secret = "test-only-wrong-secret-value"
	bc, err := s3.New(bad)
	if err != nil {
		t.Fatal(err)
	}
	var se *s3.StatusError
	if err := bc.Put(ctx, key, []byte("x"), "application/octet-stream"); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("wrong secret: %v", err)
	}
}

func TestNoLifecycleIsReported(t *testing.T) {
	st := s3test.Start(t, false)
	if _, err := st.Client.Lifecycle(context.Background()); !errors.Is(err, s3.ErrNoLifecycle) {
		t.Fatalf("lifecycle: %v", err)
	}
}

// DS5: a POST policy is good for its one key, its size range, its type and
// 10 minutes. Over the limit, an empty file, another key, another type, a
// changed policy, a changed signature or an expired policy: refused, and
// nothing is stored.
func TestPostPolicyAgainstMinIO(t *testing.T) {
	st := s3test.Start(t, true)
	ctx := context.Background()
	hid, doc := uuid.NewString(), uuid.NewString()
	key := "quarantine/" + hid + "/" + doc
	sign := func(c *s3.Client, max int64) s3.Policy {
		p, err := c.PostPolicy(s3.PolicyRequest{Key: key, ContentType: "application/pdf", MaxBytes: max}, "")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := sign(st.Client, 64)
	stored := func() bool {
		_, err := st.Client.Head(ctx, key)
		return err == nil
	}

	// The policy widened to a larger size, without a new signature.
	raw, err := base64.StdEncoding.DecodeString(p.Fields[5].Value)
	if err != nil || !bytes.Contains(raw, []byte(",64]")) {
		t.Fatalf("policy: %v", err)
	}
	widened := base64.StdEncoding.EncodeToString(bytes.Replace(raw, []byte(",64]"), []byte(",999999]"), 1))
	otherKey := "quarantine/" + hid + "/" + uuid.NewString()
	refusals := map[string]struct {
		file     []byte
		override map[string]string
	}{
		"over the limit":   {bytes.Repeat([]byte("A"), 65), nil},
		"empty":            {nil, nil},
		"another key":      {[]byte("%PDF-1.7 synthetic"), map[string]string{"key": otherKey}},
		"another hospital": {[]byte("%PDF-1.7 synthetic"), map[string]string{"key": "quarantine/" + uuid.NewString() + "/" + doc}},
		"an original":      {[]byte("%PDF-1.7 synthetic"), map[string]string{"key": "originals/" + hid + "/" + doc}},
		"another type":     {[]byte("<html>"), map[string]string{"Content-Type": "text/html"}},
		"widened policy":   {bytes.Repeat([]byte("A"), 65), map[string]string{"policy": widened}},
		"changed sig":      {[]byte("%PDF-1.7 synthetic"), map[string]string{"x-amz-signature": strings.Repeat("0", 64)}},
	}
	for name, tc := range refusals {
		if code := post(t, p, tc.file, tc.override); ok(code) {
			t.Errorf("%s: %d, want a refusal", name, code)
		}
	}
	if stored() {
		t.Fatal("a refused post stored the object")
	}
	if _, err := st.Client.Head(ctx, otherKey); err == nil {
		t.Fatal("another key was written")
	}

	// Expired: signed 11 minutes ago with the 10-minute lifetime.
	var pc *s3.Client
	past := st.Config
	past.Now = func() time.Time { return time.Now().Add(-11 * time.Minute) }
	pc, err = s3.New(past)
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, sign(pc, 64), []byte("%PDF-1.7 synthetic"), nil); ok(code) {
		t.Fatalf("expired: %d", code)
	}
	if stored() {
		t.Fatal("an expired policy stored the object")
	}

	// Within the limits: stored at exactly the policy's key.
	if code := post(t, p, []byte("%PDF-1.7 synthetic"), nil); !ok(code) {
		t.Fatalf("good post: %d", code)
	}
	if !stored() {
		t.Fatal("the good post stored nothing")
	}
}
