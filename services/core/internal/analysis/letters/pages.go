package letters

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"sync"
)

// DemoPages is the fixture letter's page count (plan departure 35).
const DemoPages = 2

// maxPages is the most pages a letter may have (design §8.1).
const maxPages = 20

var (
	pagesOnce sync.Once
	pagesPNG  [DemoPages][]byte
)

// fixturePage is page n (1 to DemoPages) of the demo's sample letter: a
// flat PNG drawn in code, so no file is shipped. It shows grey bars for
// lines of text, a signature stroke, and a red diagonal band marking it
// as a sample; page n carries n dots at the foot. It is never stamped
// with the viewer's name (that is the documents service's, live).
func fixturePage(n int) []byte {
	pagesOnce.Do(func() {
		for i := range pagesPNG {
			pagesPNG[i] = drawPage(i + 1)
		}
	})
	if n < 1 || n > DemoPages {
		return nil
	}
	return pagesPNG[n-1]
}

func drawPage(n int) []byte {
	const w, h = 595, 842
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{
		color.White, color.Gray{Y: 0xb0}, color.Gray{Y: 0x30}, color.RGBA{R: 0xd0, G: 0x30, B: 0x30, A: 0xff},
	})
	fill := func(x0, y0, x1, y1 int, c uint8) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetColorIndex(x, y, c)
			}
		}
	}
	// A letterhead, lines of "text" and a signature.
	fill(60, 60, 300, 76, 2)
	for i := 0; i < 18; i++ {
		y := 140 + i*26
		end := w - 60 - (i%4)*40
		fill(60, y, end, y+8, 1)
	}
	if n == DemoPages {
		for x := 70; x < 230; x++ {
			y := 660 + (x%23)/3
			fill(x, y, x+1, y+3, 2)
		}
	}
	// The sample band, top left to bottom right.
	for y := 0; y < h; y++ {
		x := y * w / h
		fill(max(0, x-14), y, min(w, x+14), y+1, 3)
	}
	for i := 0; i < n; i++ {
		x := w/2 - 12*n + 24*i
		fill(x, h-50, x+10, h-40, 2)
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&b, img); err != nil {
		return nil
	}
	return b.Bytes()
}
