package pages

import (
	"image"
	"image/color"
	"image/draw"
	"strings"
	"time"
	"unicode"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"golang.org/x/text/unicode/norm"
)

// scale draws src onto dst, resampled.
func scale(dst *image.RGBA, src image.Image) {
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
}

// stampText is the line drawn on every page: the viewer and the time, in
// UTC to the minute.
func stampText(name string, at time.Time) string {
	return printable(name) + "  " + at.UTC().Format("2006-01-02 15:04") + " UTC"
}

// printable is name in the stamp's font: accents dropped (NFD without its
// combining marks), anything else outside printable ASCII as '?'.
func printable(name string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(name) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// The stamp: the line in a solid band across the top and the foot, and
// repeated in translucent rows across the whole page, so cropping one copy
// out leaves others.
var (
	bandColor  = color.RGBA{255, 255, 255, 235}
	inkColor   = color.RGBA{20, 20, 20, 255}
	watermark  = color.NRGBA{200, 30, 30, 70}
	rowEvery   = 6 // rows of the tiled stamp per glyph height multiple
	glyphFace  = basicfont.Face7x13
	glyphH     = 13
	glyphAdv   = 7
	bandMargin = 4
)

// textMask is text drawn once at the font's own size: an alpha mask.
func textMask(text string) *image.Alpha {
	w := max(1, glyphAdv*len(text))
	m := image.NewAlpha(image.Rect(0, 0, w, glyphH))
	d := font.Drawer{Dst: m, Src: image.Opaque, Face: glyphFace, Dot: fixed.P(0, glyphFace.Ascent)}
	d.DrawString(text)
	return m
}

// enlarged is m scaled up by k (nearest neighbour: crisp glyphs).
func enlarged(m *image.Alpha, k int) *image.Alpha {
	b := m.Bounds()
	out := image.NewAlpha(image.Rect(0, 0, b.Dx()*k, b.Dy()*k))
	for y := 0; y < b.Dy()*k; y++ {
		for x := 0; x < b.Dx()*k; x++ {
			out.Pix[y*out.Stride+x] = m.Pix[(y/k)*m.Stride+x/k]
		}
	}
	return out
}

// stampOnto draws the stamp on page.
func stampOnto(page *image.RGBA, text string) {
	pb := page.Bounds()
	mask := textMask(text)
	// The bands: the line at a size that fits the page's width.
	k := max(1, min(3, (pb.Dx()-2*bandMargin)/max(1, mask.Bounds().Dx())))
	band := enlarged(mask, k)
	bh := band.Bounds().Dy() + 2*bandMargin
	for _, y := range []int{0, pb.Dy() - bh} {
		r := image.Rect(0, y, pb.Dx(), y+bh).Intersect(pb)
		draw.Draw(page, r, image.NewUniform(bandColor), image.Point{}, draw.Over)
		at := image.Rect(bandMargin, y+bandMargin, bandMargin+band.Bounds().Dx(), y+bandMargin+band.Bounds().Dy())
		draw.DrawMask(page, at.Intersect(pb), image.NewUniform(inkColor), image.Point{}, band, image.Point{}, draw.Over)
	}
	// The tiled rows, larger, each row shifted so the copies interleave.
	tk := max(2, k+1)
	tile := enlarged(mask, tk)
	tw, th := tile.Bounds().Dx(), tile.Bounds().Dy()
	step := th * rowEvery / 2
	for row, y := 0, bh+step/2; y < pb.Dy()-bh; row, y = row+1, y+step {
		x0 := -(row % 2) * tw / 2
		for x := x0; x < pb.Dx(); x += tw + tw/4 {
			at := image.Rect(x, y, x+tw, y+th)
			clip := at.Intersect(pb)
			if clip.Empty() {
				continue
			}
			draw.DrawMask(page, clip, image.NewUniform(watermark), image.Point{}, tile,
				clip.Min.Sub(at.Min), draw.Over)
		}
	}
}
