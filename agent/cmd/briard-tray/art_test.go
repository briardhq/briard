package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"testing"
)

func embedded(t *testing.T, px int) *image.NRGBA {
	b, err := marks.ReadFile(fmt.Sprintf("mark-%d.png", px))
	if err != nil {
		t.Fatal(err)
	}
	m, err := png.Decode(bytes.NewReader(b))
	if err != nil || m.Bounds() != image.Rect(0, 0, px, px) {
		t.Fatalf("the embedded mark-%d: %v %v", px, m.Bounds(), err)
	}
	img := image.NewNRGBA(m.Bounds())
	draw.Draw(img, img.Bounds(), m, image.Point{}, draw.Src)
	return img
}

// Each state's icon, at every size the taskbar asks for, is that size's own mark: as it is, dotted
// amber or blue in its corner inside a ring the mark does not show in, or greyed.
func TestArt(t *testing.T) {
	dots := map[state]color.NRGBA{degraded: {0xe8, 0x9a, 0x1f, 0xff}, working: {0x3a, 0x7b, 0xd5, 0xff}}
	for _, px := range sizes {
		m := embedded(t, px)
		n := float64(px)
		for _, s := range []state{serving, degraded, stopped, working} {
			img := art(s, px)
			if img.Bounds() != m.Bounds() {
				t.Fatalf("%d px, state %d: %v", px, s, img.Bounds())
			}
			for y := 0; y < px; y++ {
				for x := 0; x < px; x++ {
					got, o := img.NRGBAAt(x, y), m.NRGBAAt(x, y)
					d := math.Hypot(float64(x)+0.5-n*0.75, float64(y)+0.5-n*0.75)
					switch dot, dotted := dots[s]; {
					case s == serving && got != o:
						t.Errorf("%d px serving at %d,%d = %v, the mark has %v", px, x, y, got, o)
					case s == stopped && (got.R != got.G || got.G != got.B || got.A != o.A/2):
						t.Errorf("%d px stopped at %d,%d = %v, want grey at half of %d", px, x, y, got, o.A)
					case dotted && d >= n*0.29+0.5 && got != o:
						t.Errorf("%d px state %d changed the mark outside its ring at %d,%d: %v", px, s, x, y, got)
					case dotted && d <= n*0.29-0.5 && got.A > 0 && (got.R != dot.R || got.G != dot.G || got.B != dot.B):
						t.Errorf("%d px state %d shows the mark inside its ring at %d,%d: %v", px, s, x, y, got)
					}
				}
			}
			if dot, dotted := dots[s]; dotted {
				if c := int(n * 0.75); img.NRGBAAt(c, c) != dot {
					t.Errorf("%d px state %d's dot = %v, want %v", px, s, img.NRGBAAt(c, c), dot)
				}
			}
		}
	}
}

// A size between the marks' is the next larger scaled down, the whole of it; one above them all is
// the largest, for Windows to scale.
func TestArtBetweenSizes(t *testing.T) {
	cover := func(img *image.NRGBA) (a float64) {
		for i := 3; i < len(img.Pix); i += 4 {
			a += float64(img.Pix[i])
		}
		return a / float64(img.Bounds().Dx()*img.Bounds().Dy())
	}
	for px, from := range map[int]int{28: 32, 36: 40, 18: 20} {
		got, src := art(serving, px), embedded(t, from)
		if got.Bounds().Dx() != px {
			t.Fatalf("%d px: %v", px, got.Bounds())
		}
		if g, w := cover(got), cover(src); math.Abs(g-w) > 0.02*w {
			t.Errorf("%d px covers %.1f of 255 a pixel, mark-%d %.1f: not the whole mark scaled", px, g, from, w)
		}
	}
	if got, want := art(serving, 64), embedded(t, 48); !bytes.Equal(got.Pix, want.Pix) {
		t.Errorf("64 px is not the 48 px mark as it is: %v", got.Bounds())
	}
}

// Scaling down weighs colour by alpha: a navy pixel among clear ones stays navy, a quarter opaque.
func TestShrink(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{0x1b, 0x28, 0x31, 0xff})
	src.SetNRGBA(1, 1, color.NRGBA{0xff, 0xff, 0xff, 0}) // a clear pixel's colour counts for nothing
	if got, want := shrink(src, 1).NRGBAAt(0, 0), (color.NRGBA{0x1b, 0x28, 0x31, 0x40}); got != want {
		t.Errorf("shrink = %v, want %v", got, want)
	}
}
