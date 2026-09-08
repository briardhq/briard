package guestagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"briard.io/agent/guestfirmware"
)

// The HOST end of the push protocol: the streaming client, and the capability check that tells
// a dressable guest from one that predates the protocol. The guest end's own proofs live with
// the firmware ([B.139], agent/guestfirmware/bin_test.go).

// A binary larger than the frame cap streams in chunks and lands as <name>.next, verified
// against the digest the LAST chunk carries, executable.
func TestBinStageStreamsAndVerifies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIARD_BIN_DIR", dir)
	t.Setenv("BRIARD_BIN_RUN", t.TempDir())
	want := make([]byte, 9<<20+123) // three chunks, the last one partial
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	g := dial(t, &fakeExec{})
	if err := g.BinStage(context.Background(), "briard-guest-agent", bytes.NewReader(want)); err != nil {
		t.Fatalf("BinStage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "briard-guest-agent.next"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("staged bytes differ (%d vs %d)", len(got), len(want))
	}
	if fi, _ := os.Stat(filepath.Join(dir, "briard-guest-agent.next")); fi.Mode().Perm()&0o111 == 0 {
		t.Error("the staged binary is not executable")
	}
	if _, err := os.Stat(filepath.Join(dir, "briard-guest-agent.part")); !os.IsNotExist(err) {
		t.Error("the .part file was left behind")
	}
}

// The handshake advertises all three verbs, so a host can tell a dressable guest from a
// firmware that predates the protocol.
func TestHandshakeAdvertisesBinPush(t *testing.T) {
	g := dial(t, &fakeExec{})
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.SupportsBinPush() {
		t.Error("bin.stage/bin.test/bin.activate are not advertised")
	}
}

// The firmware's own capability list is a STRICT SUBSET of the dressed agent's, and the push
// verbs are in it: a host that meets a firmware must still be able to dress it ([B.139]).
func TestFirmwareCapabilitiesAreASubsetOfTheAgents(t *testing.T) {
	full := map[string]bool{}
	for _, v := range guestCapabilities {
		full[v] = true
	}
	for _, v := range guestfirmware.Capabilities {
		if !full[v] {
			t.Errorf("the firmware advertises %q, which the dressed agent does not serve", v)
		}
	}
}
