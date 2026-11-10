package main

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// The mark-N.png are Briard's mark at N px, each size its own drawing tuned to its pixel grid: the
// dog's head in navy with a thin white rim, so it reads on a light taskbar and a dark one. They
// are generated with the other brand images; regenerate them, never edit them.
//
//go:embed mark-*.png
var marks embed.FS

// sizes are the marks', smallest first: the notification icon's at 100, 125, 150, 200, 250 and
// 300 % scaling.
var sizes = []int{16, 20, 24, 32, 40, 48}

// art is the icon for a state at px px: the mark alone while Briard serves; the mark with a dot in
// its corner, amber when something needs the person and blue while Briard is busy; the mark
// greyed while it is stopped. The dot sits in a clear ring cut out of the mark, so it reads on a
// light taskbar and a dark one alike. Straight (not premultiplied) alpha, as Windows reads an
// icon's.
func art(s state, px int) *image.NRGBA {
	img := mark(px)
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

// mark is the mark at px px: its own drawing at a size it has; at one between, the next larger
// scaled down; above the largest, the largest, for Windows to scale up.
func mark(px int) *image.NRGBA {
	z := sizes[len(sizes)-1]
	for _, n := range sizes {
		if n >= px {
			z = n
			break
		}
	}
	b, _ := marks.ReadFile(fmt.Sprintf("mark-%d.png", z))
	m, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return image.NewNRGBA(image.Rect(0, 0, px, px)) // a clear icon rather than none
	}
	img := image.NewNRGBA(image.Rect(0, 0, z, z))
	draw.Draw(img, img.Bounds(), m, m.Bounds().Min, draw.Src)
	if z > px {
		img = shrink(img, px)
	}
	return img
}

// shrink scales a square image down to n px: each pixel the mean of the area it covers, its
// colour weighed by alpha so the clear ground's colour never bleeds into the edge.
func shrink(src *image.NRGBA, n int) *image.NRGBA {
	m := src.Bounds().Dx()
	k := float64(m) / float64(n)
	dst := image.NewNRGBA(image.Rect(0, 0, n, n))
	over := func(i int, lo, hi float64) float64 { return math.Min(float64(i+1), hi) - math.Max(float64(i), lo) }
	for y := 0; y < n; y++ {
		y0, y1 := float64(y)*k, float64(y+1)*k
		for x := 0; x < n; x++ {
			x0, x1 := float64(x)*k, float64(x+1)*k
			var r, g, b, a float64
			for sy := int(y0); sy < m && float64(sy) < y1; sy++ {
				for sx := int(x0); sx < m && float64(sx) < x1; sx++ {
					p := src.Pix[src.PixOffset(sx, sy):]
					w := over(sy, y0, y1) * over(sx, x0, x1) * float64(p[3])
					r, g, b, a = r+w*float64(p[0]), g+w*float64(p[1]), b+w*float64(p[2]), a+w
				}
			}
			if a == 0 {
				continue
			}
			p := dst.Pix[dst.PixOffset(x, y):]
			p[0], p[1], p[2], p[3] = uint8(r/a+0.5), uint8(g/a+0.5), uint8(b/a+0.5), uint8(a/(k*k)+0.5)
		}
	}
	return dst
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
