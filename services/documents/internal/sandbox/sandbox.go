// Package sandbox asks docsandbox to render a file and checks what comes
// back (documents design §5 steps 4 and 5). The file is written to a fresh
// per-job directory on the jobs tmpfs both containers mount; docsandbox
// renders it in a fresh nsjail process (services/docsandbox) and writes PNG
// pages and a manifest into the job's out directory. Everything there is
// untrusted: each page is opened without following a link, bounded in
// size, decoded again here with the same limits, its size compared with
// the manifest's, and encoded again, so only pixels leave (B2, B5).
//
// The protocol (one JSON line each way on the Unix socket, at most 4 KiB):
//
//	{"op":"ping"}                                             -> {"outcome":"ok"|"unavailable"}
//	{"op":"render","job":"<uuid>","mime":"...","max_pages":n} -> {"outcome":"<code>"}
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// The limits the output is checked against (§4.1, §5 step 4): the
// renderer scales every page to at most MaxSide on its longest side, so a
// page is at most MaxSide x MaxSide; the document at most MaxDocPixels.
const (
	MaxSide        = 4000
	MaxDocPixels   = 200_000_000
	maxManifest    = 64 << 10
	maxPageBytes   = 64 << 20
	maxLine        = 4096
	defaultTimeout = 150 * time.Second // the jail's 90 s wall time, plus the queue for a slot
	readyTTL       = 30 * time.Second
)

// The refusals a render can end in (documents.reason).
var refusals = map[string]bool{
	"too_many_pages": true, "too_many_pixels": true, "password_protected": true, "unreadable": true,
	"unsupported_type": true,
}

var (
	// ErrUnavailable: docsandbox didn't answer, or couldn't run a jail.
	// The document waits; it is never refused for it.
	ErrUnavailable = errors.New("sandbox: unavailable")
	// ErrOutput: what the sandbox wrote doesn't hold up (unreadable).
	ErrOutput = errors.New("sandbox: the output does not check")
)

// Client renders through docsandbox.
type Client struct {
	// Socket is docsandbox's Unix socket; JobsDir the shared tmpfs.
	Socket, JobsDir string
	// Timeout bounds one render (150 s when zero).
	Timeout time.Duration

	mu         sync.Mutex
	readyUntil time.Time
	readyErr   error
}

func (c *Client) call(ctx context.Context, req any, timeout time.Duration) (string, error) {
	if c == nil || c.Socket == "" {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return "", ErrUnavailable
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", ErrUnavailable
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", ErrUnavailable
	}
	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxLine), maxLine).ReadBytes('\n')
	if err != nil {
		return "", ErrUnavailable
	}
	var rep struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(line, &rep) != nil {
		return "", ErrUnavailable
	}
	return rep.Outcome, nil
}

// Ready reports nil when docsandbox answers and its jail runs (cached for
// 30 seconds).
func (c *Client) Ready(ctx context.Context) error {
	if c == nil || c.Socket == "" || c.JobsDir == "" {
		return ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.readyUntil) {
		return c.readyErr
	}
	out, err := c.call(ctx, map[string]string{"op": "ping"}, 10*time.Second)
	if err == nil && out != "ok" {
		err = ErrUnavailable
	}
	c.readyErr, c.readyUntil = err, time.Now().Add(readyTTL)
	return err
}

// Render renders data (declared mime, at most maxPages) and returns each
// page as a PNG encoded here, or a refusal reason, or an error
// (ErrUnavailable: try later). The job directory is removed either way.
func (c *Client) Render(ctx context.Context, data []byte, mime string, maxPages int) ([][]byte, string, error) {
	if c == nil || c.JobsDir == "" {
		return nil, "", ErrUnavailable
	}
	job := uuid.NewString()
	dir := filepath.Join(c.JobsDir, job)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, "", ErrUnavailable
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeInput(filepath.Join(dir, "in"), data); err != nil {
		return nil, "", ErrUnavailable
	}
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o700); err != nil {
		return nil, "", ErrUnavailable
	}
	t := c.Timeout
	if t == 0 {
		t = defaultTimeout
	}
	out, err := c.call(ctx, map[string]any{"op": "render", "job": job, "mime": mime, "max_pages": maxPages}, t)
	switch {
	case err != nil:
		return nil, "", err
	case out == "ok":
		pages, err := Recheck(filepath.Join(dir, "out"), maxPages)
		if err != nil {
			return nil, "unreadable", nil
		}
		return pages, "", nil
	case refusals[out]:
		return nil, out, nil
	}
	return nil, "", ErrUnavailable
}

func writeInput(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readFile reads a file the sandbox wrote: a regular file (never through a
// link, never a FIFO or a device) of at most max bytes.
func readFile(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrOutput
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, ErrOutput
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, ErrOutput
	}
	return b, nil
}

type manifest struct {
	Pages []struct {
		N      int `json:"n"`
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"pages"`
}

// Recheck checks the sandbox's output in dir (§5 step 5) and returns each
// page encoded again: the manifest lists pages 1..n, 1 <= n <= maxPages;
// each page-<i>.png decodes as a PNG of at most MaxSide on each side, of
// exactly the manifest's size, and the pages hold at most MaxDocPixels
// together. Anything else is ErrOutput (unreadable).
func Recheck(dir string, maxPages int) ([][]byte, error) {
	raw, err := readFile(filepath.Join(dir, "manifest.json"), maxManifest)
	if err != nil {
		return nil, err
	}
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil || len(m.Pages) < 1 || len(m.Pages) > maxPages {
		return nil, ErrOutput
	}
	var pixels int64
	for i, p := range m.Pages {
		if p.N != i+1 || p.Width < 1 || p.Height < 1 || p.Width > MaxSide || p.Height > MaxSide {
			return nil, ErrOutput
		}
		pixels += int64(p.Width) * int64(p.Height)
	}
	if pixels > MaxDocPixels {
		return nil, ErrOutput
	}
	out := make([][]byte, 0, len(m.Pages))
	for _, p := range m.Pages {
		b, err := readFile(filepath.Join(dir, "page-"+strconv.Itoa(p.N)+".png"), maxPageBytes)
		if err != nil {
			return nil, err
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil || cfg.Width != p.Width || cfg.Height != p.Height {
			return nil, ErrOutput
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds() != image.Rect(0, 0, p.Width, p.Height) {
			return nil, ErrOutput
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, ErrOutput
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}
