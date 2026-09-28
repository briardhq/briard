package casa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"
)

func TestNames(t *testing.T) {
	got := Names("amber-otter")
	want := []string{"amber-otter.briard.casa", "*.amber-otter.briard.casa"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Names = %v, want %v", got, want)
	}
}

// The canonical forms are three newline-joined lines -- the shape the Worker's JavaScript
// rebuilds. A change here must be a change there (cloud/dns/worker's golden catches it).
func TestCanonicalForms(t *testing.T) {
	if got := CanonicalA("amber-otter", "192.168.1.50", 1700000000); string(got) != "amber-otter\n192.168.1.50\n1700000000" {
		t.Fatalf("CanonicalA = %q", got)
	}
	if got := CanonicalCert("amber-otter", []byte{1, 2, 3}, 7); string(got) != "amber-otter\nAQID\n7" {
		t.Fatalf("CanonicalCert = %q", got)
	}
}

func TestFresh(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, c := range []struct {
		ts   int64
		want bool
	}{
		{1700000000, true}, {1700000000 - 300, true}, {1700000000 + 300, true},
		{1700000000 - 301, false}, {1700000000 + 301, false},
	} {
		if got := Fresh(c.ts, now); got != c.want {
			t.Errorf("Fresh(%d) = %v, want %v", c.ts, got, c.want)
		}
	}
}

// The wire shape the Worker reads: lowercase keys, byte fields as standard base64 -- so a Go
// []byte signature is the string JavaScript's atob decodes.
func TestARequestWire(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	req := ARequest{Name: "amber-otter", IP: "10.0.0.7", TS: 1700000000}
	req.Sig = ed25519.Sign(priv, CanonicalA(req.Name, req.IP, req.TS))
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"name", "ip", "ts", "sig"} {
		if _, ok := back[k]; !ok {
			t.Errorf("wire key %q missing in %s", k, b)
		}
	}
	var rt ARequest
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rt.Sig, req.Sig) || !ed25519.Verify(pub, CanonicalA(rt.Name, rt.IP, rt.TS), rt.Sig) {
		t.Fatal("signature did not survive the wire")
	}
}
