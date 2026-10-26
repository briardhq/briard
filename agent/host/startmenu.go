package host

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

// iconFile is the Start menu entry's icon, laid beside the programs in agent\ -- the one directory
// Users may read, which Explorer must to draw it. The programs carry no icon resource of their own.
const iconFile = "briard.ico"

// icoOf wraps a PNG as a one-image .ico: a 6-byte header, one 16-byte entry, the image. Windows
// reads a PNG-compressed image in an icon from Vista on; the image is re-encoded as 32-bit RGBA, the
// form an icon's PNG is documented in, whatever the source's colour type. Up to 256 px a side, which
// the entry writes as 0.
func icoOf(src []byte) ([]byte, error) {
	m, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("the mark: %w", err)
	}
	b := m.Bounds()
	if b.Dx() > 256 || b.Dy() > 256 {
		return nil, fmt.Errorf("the mark is %dx%d; an icon image is at most 256x256", b.Dx(), b.Dy())
	}
	img := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), m, b.Min, draw.Src)
	var p bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		return nil, err
	}
	side := func(n int) byte { return byte(n % 256) } // 256 is written as 0
	var out bytes.Buffer
	for _, v := range []any{
		uint16(0), uint16(1), uint16(1), // reserved, type icon, one image
		side(b.Dx()), side(b.Dy()), byte(0), byte(0), // width, height, no palette, reserved
		uint16(1), uint16(32), uint32(p.Len()), uint32(6 + 16), // planes, bits, size, offset
	} {
		_ = binary.Write(&out, binary.LittleEndian, v)
	}
	out.Write(p.Bytes())
	return out.Bytes(), nil
}
