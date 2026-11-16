// Package s3test starts a throwaway MinIO for the documents service's
// integration tests: the image the model store's compose file pins, a
// generated test-only credential, a bucket with versioning on and the
// deployed lifecycle (deploy/documents/bucket-lifecycle.xml). Only test
// code imports it.
package s3test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
)

// Image is docker-compose.models.yml's MinIO, pinned by digest.
const Image = "bitnamilegacy/minio@sha256:451fe6858cb770cc9d0e77ba811ce287420f781c7c1b806a386f6896471a349c"

// Bucket is the test bucket.
const Bucket = "mhai-documents-test"

// LifecyclePath is the deployed lifecycle configuration.
func LifecyclePath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "deploy", "documents", "bucket-lifecycle.xml")
}

// Store is a running MinIO.
type Store struct {
	Endpoint string
	Config   s3.Config
	Client   *s3.Client
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Start starts MinIO and makes the bucket. lifecycle false leaves the
// bucket without a lifecycle configuration.
func Start(t testing.TB, lifecycle bool) Store {
	t.Helper()
	ctx := context.Background()
	user, pass := "testonly"+randomHex(t, 4), "test-only-"+randomHex(t, 12)
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			Entrypoint:   []string{"minio"},
			Cmd:          []string{"server", "/tmp/data", "--address", ":9000"},
			Env:          map[string]string{"MINIO_ROOT_USER": user, "MINIO_ROOT_PASSWORD": pass},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + host + ":" + port.Port()
	cfg := s3.Config{Endpoint: endpoint, Bucket: Bucket, Region: "us-east-1",
		Credentials: s3.Credentials{AccessKeyID: user, Secret: pass}}
	cl, err := s3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// /minio/health/ready can answer a moment before the S3 API takes
	// writes; a 503 then means "not yet", so retry it briefly.
	if err := retryUnavailable(func() error { return cl.CreateBucket(ctx) }); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := cl.PutVersioning(ctx); err != nil {
		t.Fatalf("versioning: %v", err)
	}
	if lifecycle {
		raw, err := os.ReadFile(LifecyclePath())
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.PutLifecycle(ctx, raw); err != nil {
			t.Fatalf("lifecycle: %v", err)
		}
	}
	return Store{Endpoint: endpoint, Config: cfg, Client: cl}
}

// retryUnavailable runs f until it succeeds or fails with anything but a
// 503 from the store, for up to 30 seconds.
func retryUnavailable(f func() error) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := f()
		var se *s3.StatusError
		if err == nil || !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable || time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
