package main

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
)

// Each state's icon is the mark: as it is, dotted amber or blue in its corner, or greyed.
func TestArt(t *testing.T) {
	m, err := png.Decode(bytes.NewReader(markPNG))
	if err != nil || m.Bounds().Dx() != 32 {
		t.Fatalf("the embedded mark: %v %v", m.Bounds(), err)
	}
	mark := func(x, y int) color.NRGBA { return color.NRGBAModel.Convert(m.At(x, y)).(color.NRGBA) }
	at := func(s state, x, y int) color.NRGBA { return art(s).NRGBAAt(x, y) }
	for _, xy := range [][2]int{{16, 16}, {24, 24}, {8, 28}} {
		if got := at(serving, xy[0], xy[1]); got != mark(xy[0], xy[1]) {
			t.Errorf("serving at %v = %v, the mark has %v", xy, got, mark(xy[0], xy[1]))
		}
	}
	for s, want := range map[state]color.NRGBA{degraded: {0xe8, 0x9a, 0x1f, 0xff}, working: {0x3a, 0x7b, 0xd5, 0xff}} {
		if got := at(s, 24, 24); got != want {
			t.Errorf("state %d's dot = %v, want %v", s, got, want)
		}
		if got := at(s, 8, 8); got != mark(8, 8) {
			t.Errorf("state %d changed the mark away from its dot: %v", s, got)
		}
	}
	if p, o := at(stopped, 16, 16), mark(16, 16); p.R != p.G || p.G != p.B || p.A != o.A/2 {
		t.Errorf("stopped at the centre = %v, want grey at half of %d", p, o.A)
	}
}
