package files

import (
	"encoding/binary"
	"math/bits"
)

// md4 is the MD4 digest of RFC 1320, here for one reader: Samba's password file holds the NT
// hash, which is MD4 over the password in UTF-16LE, and nothing else will be accepted there.
// It is a file format's encoding and protects nothing by itself: the password it encodes is
// 128 random bits. Go's standard library has no MD4.
func md4(msg []byte) [16]byte {
	// Padding: a 1 bit, zeros to 56 mod 64, then the length in bits, little-endian.
	n := len(msg)
	msg = append(msg[:n:n], 0x80)
	for len(msg)%64 != 56 {
		msg = append(msg, 0)
	}
	msg = binary.LittleEndian.AppendUint64(msg, uint64(n)*8)

	a, b, c, d := uint32(0x67452301), uint32(0xefcdab89), uint32(0x98badcfe), uint32(0x10325476)
	var x [16]uint32
	for ; len(msg) > 0; msg = msg[64:] {
		for i := range x {
			x[i] = binary.LittleEndian.Uint32(msg[4*i:])
		}
		aa, bb, cc, dd := a, b, c, d
		// Round 1: F(x,y,z) = xy v ~xz, the words in order, shifts 3 7 11 19.
		for i := 0; i < 16; i += 4 {
			a = bits.RotateLeft32(a+(b&c|^b&d)+x[i], 3)
			d = bits.RotateLeft32(d+(a&b|^a&c)+x[i+1], 7)
			c = bits.RotateLeft32(c+(d&a|^d&b)+x[i+2], 11)
			b = bits.RotateLeft32(b+(c&d|^c&a)+x[i+3], 19)
		}
		// Round 2: G(x,y,z) = xy v xz v yz, the words by column, shifts 3 5 9 13.
		const k2 = 0x5a827999
		for i := 0; i < 4; i++ {
			a = bits.RotateLeft32(a+(b&c|b&d|c&d)+x[i]+k2, 3)
			d = bits.RotateLeft32(d+(a&b|a&c|b&c)+x[i+4]+k2, 5)
			c = bits.RotateLeft32(c+(d&a|d&b|a&b)+x[i+8]+k2, 9)
			b = bits.RotateLeft32(b+(c&d|c&a|d&a)+x[i+12]+k2, 13)
		}
		// Round 3: H(x,y,z) = x ^ y ^ z, the words in bit-reversed order, shifts 3 9 11 15.
		const k3 = 0x6ed9eba1
		for _, i := range [4]int{0, 2, 1, 3} {
			a = bits.RotateLeft32(a+(b^c^d)+x[i]+k3, 3)
			d = bits.RotateLeft32(d+(a^b^c)+x[i+8]+k3, 9)
			c = bits.RotateLeft32(c+(d^a^b)+x[i+4]+k3, 11)
			b = bits.RotateLeft32(b+(c^d^a)+x[i+12]+k3, 15)
		}
		a, b, c, d = a+aa, b+bb, c+cc, d+dd
	}
	var out [16]byte
	binary.LittleEndian.PutUint32(out[0:], a)
	binary.LittleEndian.PutUint32(out[4:], b)
	binary.LittleEndian.PutUint32(out[8:], c)
	binary.LittleEndian.PutUint32(out[12:], d)
	return out
}
