//go:build sidecars

package clamd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is compose's clamav, pinned by digest. Its bundled signatures are
// as old as the image, so these tests raise MaxAge; the 48-hour rule is
// tested against a fake (clamd_test.go).
const Image = "clamav/clamav@sha256:4cc4350d3fded380caf1799e76c0d1c93f3fa40af99c0cb2862ec0c9b117718a"

// eicar is the industry test string, built here so no file in the repo
// carries it.
func eicar() []byte {
	return []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$` + "EICAR-STANDARD-ANTIVIRUS-TEST-FILE!" + `$H+H*`)
}

func confPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "deploy", "documents", "clamd.conf")
}

func startClamd(t *testing.T) *Client {
	t.Helper()
	if _, err := os.Stat(confPath()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			Env:          map[string]string{"CLAMAV_NO_FRESHCLAMD": "true", "CLAMAV_NO_MILTERD": "true"},
			ExposedPorts: []string{"3310/tcp"},
			Files:        []testcontainers.ContainerFile{{HostFilePath: confPath(), ContainerFilePath: "/etc/clamav/clamd.conf", FileMode: 0o644}},
			WaitingFor:   wait.ForListeningPort("3310/tcp").WithStartupTimeout(5 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start clamav: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "3310/tcp")
	if err != nil {
		t.Fatal(err)
	}
	cl := &Client{Addr: host + ":" + port.Port(), MaxAge: 10 * 365 * 24 * time.Hour}
	deadline := time.Now().Add(3 * time.Minute)
	for cl.Ready(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatal("clamd never became ready")
		}
		time.Sleep(2 * time.Second)
	}
	return cl
}

func pdfWith(payload []byte) []byte {
	obj := "1 0 obj\n<< /Type /Catalog /Pages 2 0 R /Names << /EmbeddedFiles << /Names [(t) 3 0 R] >> >> >>\nendobj\n" +
		"2 0 obj\n<< /Type /Pages /Kids [] /Count 0 >>\nendobj\n" +
		"3 0 obj\n<< /Type /Filespec /F (t) /EF << /F 4 0 R >> >>\nendobj\n" +
		"4 0 obj\n<< /Type /EmbeddedFile /Length " + strconv.Itoa(len(payload)) + " >>\nstream\n"
	return append(append([]byte("%PDF-1.7\n"+obj), payload...), []byte("\nendstream\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")...)
}

// DS6 against the real engine and the deployed clamd.conf: EICAR alone and
// inside a PDF (an embedded file clamd extracts) is malware; a plain file
// is clean. EICAR after a TIFF header is NOT found: ClamAV matches the
// test signature only on a whole file or one it extracts, and it doesn't
// extract TIFF contents (design §17 departure 34). The case is kept so a
// change in that answer shows up; the scan doesn't depend on it (the
// sandbox is the real control, §14.6).
func TestRealClamd(t *testing.T) {
	cl := startClamd(t)
	ctx := context.Background()
	if _, err := cl.SignatureTime(ctx); err != nil {
		t.Fatalf("version: %v", err)
	}
	for name, c := range map[string]struct {
		data     []byte
		infected bool
	}{
		"plain":         {[]byte("%PDF-1.7\nsynthetic clean document\n"), false},
		"eicar":         {eicar(), true},
		"eicar in pdf":  {pdfWith(eicar()), true},
		"eicar in tiff": {append([]byte("II*\x00\x08\x00\x00\x00"), eicar()...), false},
	} {
		infected, err := cl.Scan(ctx, c.data)
		if err != nil || infected != c.infected {
			t.Errorf("%s: infected %v, %v", name, infected, err)
		}
	}
}
