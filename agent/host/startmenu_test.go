package host

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"

	"briard.io/agent/hass"
)

// The Start menu's icon is Briard's 256 px mark behind an .ico header Windows reads: one image,
// its side written as 0 (256), 32 bits, and a PNG at the offset the entry names that decodes to
// straight RGBA -- whatever colour type the source PNG has (the mark's is a palette).
func TestIconWrapsTheMarkAsAnRGBAImage(t *testing.T) {
	ico, err := icoOf(hass.Mark())
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Reserved, Type, Count uint16
		W, H, Colors, Res     uint8
		Planes, Bits          uint16
		Size, Offset          uint32
	}
	if err := binary.Read(bytes.NewReader(ico), binary.LittleEndian, &h); err != nil {
		t.Fatal(err)
	}
	if h.Reserved != 0 || h.Type != 1 || h.Count != 1 || h.W != 0 || h.H != 0 || h.Planes != 1 || h.Bits != 32 || h.Offset != 22 {
		t.Fatalf("header %+v: want one 256x256 32-bit image at offset 22", h)
	}
	if int(h.Offset)+int(h.Size) != len(ico) {
		t.Fatalf("the entry says %d bytes at %d; the file is %d", h.Size, h.Offset, len(ico))
	}
	m, err := png.Decode(bytes.NewReader(ico[h.Offset:]))
	if err != nil {
		t.Fatalf("the image is not a PNG: %v", err)
	}
	if _, ok := m.(*image.NRGBA); !ok || m.Bounds().Dx() != 256 {
		t.Errorf("the image is %T %v; want 256 px straight RGBA", m, m.Bounds())
	}
}

func TestIconRefusesAnImageOverAnIconsSide(t *testing.T) {
	var big bytes.Buffer
	_ = png.Encode(&big, image.NewNRGBA(image.Rect(0, 0, 512, 512)))
	if _, err := icoOf(big.Bytes()); err == nil {
		t.Error("a 512 px image was wrapped; an icon entry cannot say more than 256")
	}
}
