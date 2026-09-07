package guestagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func binDirs(t *testing.T) (string, string) {
	t.Helper()
	dir, run := t.TempDir(), t.TempDir()
	t.Setenv("BRIARD_BIN_DIR", dir)
	t.Setenv("BRIARD_BIN_RUN", run)
	return dir, run
}

// A binary larger than the frame cap streams in chunks and lands as <name>.next, verified
// against the digest the LAST chunk carries, executable.
func TestBinStageStreamsAndVerifies(t *testing.T) {
	dir, _ := binDirs(t)
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

// The name table is closed, and a digest that does not match leaves no .next behind.
func TestBinStageRefusesStrangersAndBadDigests(t *testing.T) {
	dir, _ := binDirs(t)
	if err := stageChunk(BinChunk{Name: "evil", Seq: 0, Data: []byte("x"), Last: true, SHA256: "00"}); err == nil {
		t.Error("a name outside the table was accepted")
	}
	if err := stageChunk(BinChunk{Name: "briard-reverse-proxy", Seq: 0, Data: []byte("x"), Last: true, SHA256: "0000"}); err == nil {
		t.Error("a wrong digest was accepted")
	}
	if err := stageChunk(BinChunk{Name: "briard-reverse-proxy", Seq: 0, Data: []byte("x"), Last: true}); err == nil {
		t.Error("a last chunk with no digest was accepted")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("refused stages left files behind: %v", ents)
	}
}

// Activation arms a trial flag per binary, records the release for the commit, and restarts
// the units in the order given -- the guest agent's own last. Nothing is armed or restarted
// when any named binary is not staged.
func TestBinActivateArmsAndRestartsInOrder(t *testing.T) {
	dir, run := binDirs(t)
	fx := &fakeExec{}
	g := dial(t, fx)
	if err := g.BinActivate(context.Background(), "v3.20260907.abc1234", BinNames); err == nil {
		t.Fatal("activated with nothing staged")
	}
	if len(fx.runs) != 0 {
		t.Fatalf("restarted units for an unstaged set: %v", fx.runs)
	}
	for _, n := range BinNames {
		if err := os.WriteFile(filepath.Join(dir, n+".next"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.BinActivate(context.Background(), "v3.20260907.abc1234", BinNames); err != nil {
		t.Fatalf("BinActivate: %v", err)
	}
	want := [][]string{
		{"systemctl", "try-restart", "--no-block", "briard-reverse-proxy.service"},
		{"systemd-run", "--quiet", "--collect", "--on-active=1", "--timer-property=AccuracySec=100ms", "systemctl", "restart", "briard-guest-agent.service"},
	}
	if !reflect.DeepEqual(fx.runs, want) {
		t.Errorf("restarts = %v, want %v", fx.runs, want)
	}
	for _, n := range BinNames {
		if _, err := os.Stat(filepath.Join(run, n+".update")); err != nil {
			t.Errorf("%s was not armed: %v", n, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "RELEASE.next")); string(b) != "v3.20260907.abc1234\n" {
		t.Errorf("RELEASE.next = %q", b)
	}
	if err := g.BinActivate(context.Background(), "bad id/", BinNames); err == nil {
		t.Error("a release id with a slash was accepted")
	}
}

// The handshake advertises both verbs, so a host can tell a dressable guest from a firmware
// that predates the protocol.
func TestHandshakeAdvertisesBinPush(t *testing.T) {
	g := dial(t, &fakeExec{})
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.SupportsBinPush() {
		t.Error("bin.stage/bin.activate are not advertised")
	}
}
