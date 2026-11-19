package pages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/envelope"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Synthetic ids and a synthetic viewer only.
var (
	hospital = uuid.MustParse("0a1b2c3d-0000-4000-8000-0000000000a1")
	document = uuid.MustParse("0a1b2c3d-0000-4000-8000-0000000000d1")
	at       = time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
)

const viewer = "Synthetic Viewer"

// page is a white PNG of w x h with a grey bar, as the scan stores pages.
func page(t *testing.T, w, h int, noise bool) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = 255
		if noise {
			img.Pix[i] = uint8(r.IntN(256))
		}
	}
	for y := h / 3; y < h/3+20 && y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: 90})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// §10.1, DS21, DS22: the served page is a JPEG of at most 900 KB and at
// most 1,600 px wide, in proportion, with the stamp drawn on it.
func TestStampScalesAndFits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		w, h  int
		noise bool
	}{{"a4 at 200 dpi", 1654, 2339, false}, {"narrow", 800, 1100, false}, {"noise, the largest side", 4000, 4000, true}} {
		t.Run(tc.name, func(t *testing.T) {
			out, w, h, err := Stamp(page(t, tc.w, tc.h, tc.noise), viewer, at)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > docops.MaxPageJPEG || w > MaxWidth || w < 1 || h < 1 {
				t.Fatalf("%d bytes, %d x %d", len(out), w, h)
			}
			if want := tc.h * w / tc.w; h < want-1 || h > want+1 {
				t.Errorf("height %d, want about %d", h, want)
			}
			img, err := jpeg.Decode(bytes.NewReader(out))
			if err != nil || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
				t.Fatalf("not the JPEG it says: %v", err)
			}
		})
	}
}

// The stamp differs by viewer and by time: a page shown to two people, or
// twice, is never the same picture.
func TestStampCarriesViewerAndTime(t *testing.T) {
	src := page(t, 1200, 1600, false)
	a, _, _, err := Stamp(src, viewer, at)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, _ := Stamp(src, "Another Synthetic Viewer", at)
	c, _, _, _ := Stamp(src, viewer, at.Add(time.Minute))
	if bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("the stamp doesn't depend on the viewer and the time")
	}
	// The top band is drawn: dark ink on the white page's first rows.
	img, _ := jpeg.Decode(bytes.NewReader(a))
	dark := 0
	for y := 0; y < 40; y++ {
		for x := 0; x < 600; x++ {
			if r, _, _, _ := img.At(x, y).RGBA(); r < 0x4000 {
				dark++
			}
		}
	}
	if dark < 200 {
		t.Fatalf("no stamp in the top band (%d dark pixels)", dark)
	}
}

func TestPrintableName(t *testing.T) {
	for in, want := range map[string]string{
		"Synthetic Viewer":   "Synthetic Viewer",
		"Adébáyọ̀ Ọlá":       "Adebayo Ola",
		"Zoë O’Neil":         "Zoe O?Neil",
		"合成":                 "??",
		"Tab\tName":          "Tab?Name",
		"  Spaces Kept  ":    "  Spaces Kept  ",
		"Ünïcödé Çharacters": "Unicode Characters",
	} {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
	if s := stampText(viewer, at); s != "Synthetic Viewer  2026-11-18 09:30 UTC" {
		t.Errorf("stampText: %q", s)
	}
}

// What isn't a stored page is refused, never decoded past its header.
func TestStampRefusesWhatIsntAPage(t *testing.T) {
	big := image.NewGray(image.Rect(0, 0, 4001, 10))
	var buf bytes.Buffer
	_ = png.Encode(&buf, big)
	for name, b := range map[string][]byte{"empty": nil, "not png": []byte("%PDF-1.7 synthetic"),
		"wider than stored": buf.Bytes(), "truncated": page(t, 100, 100, false)[:60]} {
		if _, _, _, err := Stamp(b, viewer, at); err == nil {
			t.Errorf("%s: stamped", name)
		}
	}
}

type fakeRows struct {
	row  Row
	err  error
	asks []uuid.UUID
}

func (f *fakeRows) PageRow(_ context.Context, h, d uuid.UUID, n int) (Row, error) {
	f.asks = append(f.asks, h)
	if d != document || n != 1 {
		return Row{}, ErrNotFound
	}
	return f.row, f.err
}

type fakeStore struct {
	body []byte
	err  error
	ref  envelope.Ref
}

func (f *fakeStore) Open(_ context.Context, ref envelope.Ref, _ []byte, _ int) (io.ReadCloser, error) {
	f.ref = ref
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func claims(hid string) reqtoken.Claims {
	return reqtoken.Claims{Aud: "documents", Sub: reqtoken.SvcCore, Hid: hid, Kind: reqtoken.KindReq,
		Subject: "documents.v1.ctl.page_image"}
}

func request(doc string, n uint32, name string, at int64) *docspb.PageImageRequest {
	return &docspb.PageImageRequest{DocumentId: doc, N: n, ViewerName: name, At: at}
}

func outcome(t *testing.T, m proto.Message, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return m.(*docspb.PageImageReply).GetOutcome()
}

func call(t *testing.T, h *Handler, req *docspb.PageImageRequest) string {
	t.Helper()
	rep, err := h.Handle(context.Background(), claims(hospital.String()), req)
	return outcome(t, rep, err)
}

func TestHandle(t *testing.T) {
	stored := page(t, 900, 1200, false)
	sum := sha256.Sum256(stored)
	ok := func() (*Handler, *fakeRows, *fakeStore) {
		rows := &fakeRows{row: Row{Status: "ready", Wrapped: make([]byte, 60), KEKVersion: 1, SHA256: sum[:]}}
		st := &fakeStore{body: stored}
		return &Handler{Rows: rows, Store: st, Ready: func(context.Context) error { return nil }}, rows, st
	}
	ctx := context.Background()

	h, rows, st := ok()
	rep, err := h.Handle(ctx, claims(hospital.String()), request(document.String(), 1, viewer, at.Unix()))
	if got := outcome(t, rep, err); got != OutcomeOK {
		t.Fatalf("ok: %s", got)
	}
	r := rep.(*docspb.PageImageReply)
	if len(r.GetJpeg()) == 0 || len(r.GetJpeg()) > docops.MaxPageJPEG || r.GetWidth() != 900 || r.GetHeight() != 1200 {
		t.Fatalf("reply: %d bytes %dx%d", len(r.GetJpeg()), r.GetWidth(), r.GetHeight())
	}
	// The hospital is the token's; the object is the page's own.
	if len(rows.asks) != 1 || rows.asks[0] != hospital ||
		st.ref != (envelope.Ref{Hospital: hospital, Document: document, Kind: envelope.Page, N: 1}) {
		t.Fatalf("asked %v for %+v", rows.asks, st.ref)
	}

	// A token without a hospital: an error (the responder refuses it first).
	if _, err := h.Handle(ctx, claims(""), request(document.String(), 1, viewer, at.Unix())); err == nil {
		t.Fatal("no hospital: answered")
	}
	// Malformed requests name nothing: not_found, nothing read.
	for name, req := range map[string]*docspb.PageImageRequest{
		"no id":           request("", 1, viewer, at.Unix()),
		"upper-case id":   request(strings.ToUpper(document.String()), 1, viewer, at.Unix()),
		"nil id":          request(uuid.Nil.String(), 1, viewer, at.Unix()),
		"page 0":          request(document.String(), 0, viewer, at.Unix()),
		"page 51":         request(document.String(), 51, viewer, at.Unix()),
		"no viewer":       request(document.String(), 1, "", at.Unix()),
		"control in name": request(document.String(), 1, "Synthetic\nViewer", at.Unix()),
		"long name":       request(document.String(), 1, strings.Repeat("x", MaxViewerName+1), at.Unix()),
		"bad utf-8":       request(document.String(), 1, "\xff", at.Unix()),
		"no time":         request(document.String(), 1, viewer, 0),
	} {
		h, rows, _ := ok()
		if got := call(t, h, req); got != OutcomeNotFound || len(rows.asks) != 0 {
			t.Errorf("%s: %s, read %d", name, got, len(rows.asks))
		}
	}
	// DS1: another document, page or hospital's row is not found.
	h, _, _ = ok()
	if got := call(t, h, request(uuid.NewString(), 1, viewer, at.Unix())); got != OutcomeNotFound {
		t.Errorf("another document: %s", got)
	}
	if got := call(t, h, request(document.String(), 2, viewer, at.Unix())); got != OutcomeNotFound {
		t.Errorf("another page: %s", got)
	}
	// Pages of a document that wasn't scanned clean, or has ended, aren't shown.
	for _, s := range []string{"uploading", "uploaded", "scanning", "quarantined", "failed", "cancelled", "rejected", "purged"} {
		h, rows, st := ok()
		rows.row.Status = s
		if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeNotFound || st.ref.N != 0 {
			t.Errorf("status %s: %s, opened %v", s, got, st.ref.N != 0)
		}
	}
	// Not ready, a row error, a failed open, a page that isn't what the
	// scan stored: unavailable.
	h, _, _ = ok()
	h.Ready = func(context.Context) error { return errors.New("synthetic: not ready") }
	if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeUnavailable {
		t.Errorf("not ready: %s", got)
	}
	h, _, _ = ok()
	h.Store = nil
	if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeUnavailable {
		t.Errorf("no store: %s", got)
	}
	h, rows, _ = ok()
	rows.err = errors.New("synthetic: database")
	if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeUnavailable {
		t.Errorf("row error: %s", got)
	}
	h, _, st = ok()
	st.err = envelope.ErrDecrypt
	if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeUnavailable {
		t.Errorf("decrypt: %s", got)
	}
	h, _, st = ok()
	st.body = page(t, 900, 1200, true)
	if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeUnavailable {
		t.Errorf("checksum: %s", got)
	}
	h, rows, _ = ok()
	rows.row.SHA256 = nil
	if got := call(t, h, request(document.String(), 1, viewer, at.Unix())); got != OutcomeUnavailable {
		t.Errorf("no checksum: %s", got)
	}
	if _, err := h.Handle(ctx, claims(hospital.String()), &docspb.HealthRequest{}); err == nil {
		t.Error("another message: answered")
	}
}
