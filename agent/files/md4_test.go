package files

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The seven test vectors of RFC 1320, appendix A.5.
func TestMD4MatchesRFC1320(t *testing.T) {
	for msg, want := range map[string]string{
		"":                           "31d6cfe0d16ae931b73c59d7e0c089c0",
		"a":                          "bde52cb31de33e46245e05fbdbd6fb24",
		"abc":                        "a448017aaf21d8525fc10ae87aa6729d",
		"message digest":             "d9130a8164549fe818874806e1c7014b",
		"abcdefghijklmnopqrstuvwxyz": "d79e1c308aa5bbcdeea8ed63df412da9",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789":                   "043f8582f241db351ce627e153e7f0e4",
		"12345678901234567890123456789012345678901234567890123456789012345678901234567890": "e33b4ddc9c38f2199c3e7b164fcc0536",
	} {
		got := md4([]byte(msg))
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("md4(%q) = %x, want %s", msg, got, want)
		}
	}
}

// The caller's bytes are not written to: the padding goes on a copy, whatever capacity the
// slice came with.
func TestMD4LeavesItsInputAlone(t *testing.T) {
	buf := []byte(strings.Repeat("x", 8) + strings.Repeat("y", 100))
	md4(buf[:8])
	if string(buf[8:]) != strings.Repeat("y", 100) {
		t.Fatal("md4 wrote past the message it was given")
	}
}
