package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	img.Set(1, 1, color.Gray{Y: 9})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// chunks lists a PNG's chunk types.
func chunks(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for i := 8; i+8 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		out = append(out, string(b[i+4:i+8]))
		i += 12 + n
	}
	return out
}

func writeOut(t *testing.T, dir string, pages [][2]int, files map[string][]byte) {
	t.Helper()
	var m struct {
		Pages []map[string]int `json:"pages"`
	}
	for i, wh := range pages {
		m.Pages = append(m.Pages, map[string]int{"n": i + 1, "width": wh[0], "height": wh[1]})
		if files == nil || files["page-"+strconv.Itoa(i+1)+".png"] == nil {
			if err := os.WriteFile(filepath.Join(dir, "page-"+strconv.Itoa(i+1)+".png"), pngOf(t, wh[0], wh[1]), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeSandbox answers on a Unix socket; render calls act(out dir) and
// replies with its outcome.
func fakeSandbox(t *testing.T, ping string, act func(job, out string) string) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	jobs := filepath.Join(dir, "jobs")
	if err := os.Mkdir(jobs, 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				_ = json.Unmarshal(line, &req)
				out := ping
				if req["op"] == "render" {
					job, _ := req["job"].(string)
					out = act(job, filepath.Join(jobs, job, "out"))
				}
				_, _ = c.Write([]byte(`{"outcome":"` + out + "\"}\n"))
			}(c)
		}
	}()
	return &Client{Socket: sock, JobsDir: jobs}
}

func TestRenderReturnsPagesEncodedAgain(t *testing.T) {
	var input []byte
	c := fakeSandbox(t, "ok", func(job, out string) string {
		input, _ = os.ReadFile(filepath.Join(filepath.Dir(out), "in"))
		// B5: a page with a trailing payload and an extra chunk-like tail.
		p := append(pngOf(t, 40, 30), []byte("<script>synthetic</script>")...)
		writeOut(t, out, [][2]int{{40, 30}, {20, 10}}, map[string][]byte{"page-1.png": p})
		return "ok"
	})
	pages, reason, err := c.Render(context.Background(), []byte("%PDF-1.7 synthetic"), "application/pdf", 50)
	if err != nil || reason != "" || len(pages) != 2 {
		t.Fatalf("render: %d %q %v", len(pages), reason, err)
	}
	if string(input) != "%PDF-1.7 synthetic" {
		t.Fatalf("the sandbox read %q", input)
	}
	for i, p := range pages {
		if got := chunks(t, p); len(got) != 3 || got[0] != "IHDR" || got[1] != "IDAT" || got[2] != "IEND" {
			t.Fatalf("page %d chunks %v", i+1, got)
		}
		if bytes.Contains(p, []byte("script")) {
			t.Fatalf("page %d kept the trailing payload", i+1)
		}
	}
	entries, _ := os.ReadDir(c.JobsDir)
	if len(entries) != 0 {
		t.Fatalf("job directories left: %d", len(entries))
	}
}

func TestRenderRefusalsAndUnavailable(t *testing.T) {
	for _, out := range []string{"too_many_pages", "too_many_pixels", "password_protected", "unreadable", "unsupported_type"} {
		c := fakeSandbox(t, "ok", func(string, string) string { return out })
		if _, reason, err := c.Render(context.Background(), []byte("x"), "image/png", 50); err != nil || reason != out {
			t.Errorf("%s: %q %v", out, reason, err)
		}
	}
	for _, out := range []string{"unavailable", "bad_request", "something"} {
		c := fakeSandbox(t, "ok", func(string, string) string { return out })
		if _, _, err := c.Render(context.Background(), []byte("x"), "image/png", 50); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", out, err)
		}
	}
	c := &Client{Socket: "/nonexistent/s.sock", JobsDir: t.TempDir()}
	if _, _, err := c.Render(context.Background(), []byte("x"), "image/png", 50); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no socket: %v", err)
	}
	if _, _, err := (*Client)(nil).Render(context.Background(), nil, "", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := c.Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ready without a socket: %v", err)
	}
	down := fakeSandbox(t, "unavailable", nil)
	if err := down.Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ready with a broken jail: %v", err)
	}
	up := fakeSandbox(t, "ok", nil)
	if err := up.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

// §5 step 5: what the sandbox wrote is checked outside it; anything off
// is unreadable, and nothing is read through a link or from a FIFO.
func TestRecheckRefusesBadOutput(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(secret, pngOf(t, 10, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(dir string){
		"no manifest": func(string) {},
		"size differs from the manifest": func(d string) {
			writeOut(t, d, [][2]int{{10, 10}}, map[string][]byte{"page-1.png": pngOf(t, 11, 10)})
		},
		"too many pages": func(d string) { writeOut(t, d, [][2]int{{1, 1}, {1, 1}, {1, 1}}, nil) },
		"side over 4000": func(d string) { writeOut(t, d, [][2]int{{4001, 1}}, nil) },
		"missing page": func(d string) {
			writeOut(t, d, [][2]int{{1, 1}, {1, 1}}, nil)
			_ = os.Remove(filepath.Join(d, "page-2.png"))
		},
		"not a png": func(d string) {
			writeOut(t, d, [][2]int{{1, 1}}, map[string][]byte{"page-1.png": []byte("%PDF-1.7")})
		},
		"unknown manifest field": func(d string) {
			writeOut(t, d, [][2]int{{1, 1}}, nil)
			_ = os.WriteFile(filepath.Join(d, "manifest.json"), []byte(`{"pages":[{"n":1,"width":1,"height":1}],"x":1}`), 0o600)
		},
		"pages out of order": func(d string) {
			writeOut(t, d, [][2]int{{1, 1}}, nil)
			_ = os.WriteFile(filepath.Join(d, "manifest.json"), []byte(`{"pages":[{"n":2,"width":1,"height":1}]}`), 0o600)
		},
		"page is a link": func(d string) {
			writeOut(t, d, [][2]int{{10, 10}}, nil)
			_ = os.Remove(filepath.Join(d, "page-1.png"))
			if err := os.Symlink(secret, filepath.Join(d, "page-1.png")); err != nil {
				t.Fatal(err)
			}
		},
		"manifest is a link": func(d string) {
			other := filepath.Join(t.TempDir(), "m.json")
			_ = os.WriteFile(other, []byte(`{"pages":[{"n":1,"width":10,"height":10}]}`), 0o600)
			_ = os.WriteFile(filepath.Join(d, "page-1.png"), pngOf(t, 10, 10), 0o600)
			_ = os.Symlink(other, filepath.Join(d, "manifest.json"))
		},
		"page is a fifo": func(d string) {
			writeOut(t, d, [][2]int{{10, 10}}, nil)
			_ = os.Remove(filepath.Join(d, "page-1.png"))
			if err := syscall.Mkfifo(filepath.Join(d, "page-1.png"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		d := t.TempDir()
		setup(d)
		if pages, err := Recheck(d, 2); !errors.Is(err, ErrOutput) || pages != nil {
			t.Errorf("%s: %d pages, %v", name, len(pages), err)
		}
	}
}

func TestRecheckDocumentPixelBudget(t *testing.T) {
	d := t.TempDir()
	// 13 pages of 4000 x 4000 = 208 MP > 200 MP; checked from the manifest
	// before any page is decoded.
	m := `{"pages":[`
	for i := 1; i <= 13; i++ {
		if i > 1 {
			m += ","
		}
		m += `{"n":` + strconv.Itoa(i) + `,"width":4000,"height":4000}`
	}
	_ = os.WriteFile(filepath.Join(d, "manifest.json"), []byte(m+`]}`), 0o600)
	if _, err := Recheck(d, 50); !errors.Is(err, ErrOutput) {
		t.Fatalf("over budget: %v", err)
	}
}

// Whatever goes wrong around the call is unavailable (the document waits),
// and the job directory never stays behind.
func TestRenderUnavailablePaths(t *testing.T) {
	// The jobs directory isn't there.
	c := fakeSandbox(t, "ok", func(string, string) string { return "ok" })
	c.JobsDir = filepath.Join(t.TempDir(), "missing")
	if _, _, err := c.Render(context.Background(), []byte("x"), "image/png", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no jobs dir: %v", err)
	}
	// A reply that isn't JSON, or a hang-up.
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	replies := make(chan string, 2)
	replies <- "not json\n"
	replies <- ""
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			_, _ = conn.Write([]byte(<-replies))
			_ = conn.Close()
		}
	}()
	jobs := t.TempDir()
	bad := &Client{Socket: sock, JobsDir: jobs}
	for i := 0; i < 2; i++ {
		if _, _, err := bad.Render(context.Background(), []byte("x"), "image/png", 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if entries, _ := os.ReadDir(jobs); len(entries) != 0 {
		t.Fatalf("job directories left: %d", len(entries))
	}
	// An ok whose output doesn't check is unreadable, not unavailable.
	broken := fakeSandbox(t, "ok", func(_, out string) string {
		_ = os.WriteFile(filepath.Join(out, "manifest.json"), []byte("{}"), 0o600)
		return "ok"
	})
	if _, reason, err := broken.Render(context.Background(), []byte("x"), "image/png", 1); err != nil || reason != "unreadable" {
		t.Fatalf("bad output: %q %v", reason, err)
	}
	// Ready is cached: a second call within the TTL doesn't ask again.
	up := fakeSandbox(t, "ok", nil)
	if err := up.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.Socket = "/nonexistent/s.sock"
	if err := up.Ready(context.Background()); err != nil {
		t.Fatalf("not cached: %v", err)
	}
}
