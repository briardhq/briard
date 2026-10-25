package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// mark.png is Briard's mark at the icon's size, 32 px, made from the Home Assistant integration's
// brand icon (agent/hass/component/brand/icon@2x.png) -- the shield the site's favicon is too.
//
//go:embed mark.png
var markPNG []byte

// art is the icon for a state: the mark alone while Briard serves; the mark with a dot in its
// corner, amber when something needs the person and blue while Briard is busy; the mark greyed
// while it is stopped. The dot sits in a clear ring cut out of the mark, so it reads on a light
// taskbar and a dark one alike. Straight (not premultiplied) alpha, as Windows reads an icon's.
func art(s state) *image.NRGBA {
	m, err := png.Decode(bytes.NewReader(markPNG))
	if err != nil {
		return image.NewNRGBA(image.Rect(0, 0, 32, 32)) // a clear icon rather than none
	}
	img := image.NewNRGBA(m.Bounds())
	draw.Draw(img, img.Bounds(), m, m.Bounds().Min, draw.Src)
	switch s {
	case stopped:
		for i := 0; i < len(img.Pix); i += 4 {
			p := img.Pix[i : i+4]
			g := uint8((299*int(p[0]) + 587*int(p[1]) + 114*int(p[2])) / 1000)
			p[0], p[1], p[2], p[3] = g, g, g, p[3]/2
		}
	case degraded:
		badge(img, color.NRGBA{0xe8, 0x9a, 0x1f, 0xff})
	case working:
		badge(img, color.NRGBA{0x3a, 0x7b, 0xd5, 0xff})
	}
	return img
}

// badge draws the dot in the bottom-right corner, its edge smoothed by alpha, inside its ring.
func badge(img *image.NRGBA, c color.NRGBA) {
	b := img.Bounds()
	n := float64(b.Dx())
	cx, cy, r, ring := n*0.75, n*0.75, n*0.22, n*0.29
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			p := img.PixOffset(x, y)
			if cut := clamp(d - ring); cut < 1 { // inside the ring: clear the mark
				img.Pix[p+3] = uint8(float64(img.Pix[p+3]) * cut)
			}
			if a := clamp(r - d); a > 0 { // the dot over it
				img.Pix[p], img.Pix[p+1], img.Pix[p+2] = c.R, c.G, c.B
				img.Pix[p+3] = uint8(math.Max(float64(img.Pix[p+3]), a*255))
			}
		}
	}
}

func clamp(v float64) float64 { return math.Max(0, math.Min(1, v+0.5)) }
