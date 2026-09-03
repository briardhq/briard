package sdnotify

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"briard.io/internal/testsock"
)

// With no $NOTIFY_SOCKET, Ready/Notify are no-ops (dev runs, tests, the lab fleet).
func TestNoSocketIsNoOp(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Ready(); err != nil {
		t.Errorf("Ready() with no NOTIFY_SOCKET = %v, want nil no-op", err)
	}
	if err := Notify("STATUS=x"); err != nil {
		t.Errorf("Notify() with no NOTIFY_SOCKET = %v, want nil", err)
	}
}

// With a real datagram socket, Ready() delivers exactly "READY=1" — the message that gates the
// self-update commit.
func TestReadyDeliversReadyDatagram(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "notify.sock")
	laddr := &net.UnixAddr{Name: sock, Net: "unixgram"}
	ln, err := net.ListenUnixgram("unixgram", laddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	t.Setenv("NOTIFY_SOCKET", sock)

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := ln.Read(buf)
		got <- string(buf[:n])
	}()

	if err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "READY=1" {
			t.Errorf("datagram = %q, want READY=1", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram received")
	}
}

func TestNotifyUnreachableSocketErrors(t *testing.T) {
	// A set-but-dead socket path surfaces an error (the caller treats it as non-fatal).
	t.Setenv("NOTIFY_SOCKET", filepath.Join(testsock.Dir(t), "does-not-exist.sock"))
	if err := Ready(); err == nil {
		t.Error("Ready() to a dead socket = nil, want an error")
	}
}

// Adopt takes the socket OUT of the environment while keeping it usable from this process: the
// agent goes on signalling, and the children it execs inherit nothing ([V3b.21e] — `systemctl`
// reports its own EXIT_STATUS=0 to any notify socket it finds, and systemd logs the refusal on
// every start).
//
// The assertion that must be able to fail is os.Environ(): that slice IS what exec.Cmd hands a
// child when Cmd.Env is nil, so checking it checks the mechanism rather than a proxy for it.
// Revert Adopt's Unsetenv and this test goes red on that check and on the real child below.
func TestAdoptKeepsTheSocketFromChildrenAndKeepsSignalling(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "notify.sock")
	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	t.Setenv("NOTIFY_SOCKET", sock)
	t.Cleanup(func() { adopted.Store(nil) }) // package state outlives t.Setenv's restore

	// Non-vacuity: the variable has to be visible BEFORE Adopt, or "gone after" proves nothing.
	if os.Getenv("NOTIFY_SOCKET") != sock {
		t.Fatalf("precondition: NOTIFY_SOCKET = %q, want %q", os.Getenv("NOTIFY_SOCKET"), sock)
	}
	Adopt()

	if got := os.Getenv("NOTIFY_SOCKET"); got != "" {
		t.Errorf("after Adopt, NOTIFY_SOCKET = %q, want it gone", got)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "NOTIFY_SOCKET=") {
			t.Errorf("after Adopt, os.Environ() still carries %q — every child would inherit it", kv)
		}
	}

	// The other half: the agent must still be able to signal, or this "fix" is an outage.
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := ln.Read(buf)
		got <- string(buf[:n])
	}()
	if err := Watchdog(); err != nil {
		t.Fatalf("Watchdog after Adopt: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "WATCHDOG=1" {
			t.Errorf("datagram = %q, want WATCHDOG=1", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram after Adopt — the agent stopped signalling, which is worse than the noise")
	}

	// And prove it against a real exec'd child, not only against os.Environ().
	if runtime.GOOS == "linux" {
		out, err := exec.Command("/bin/sh", "-c", `printf %s "${NOTIFY_SOCKET-}"`).Output()
		if err != nil {
			t.Fatalf("child: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("an exec'd child saw NOTIFY_SOCKET=%q, want empty", out)
		}
	}
}

// Without Adopt, the environment is still the source — a dev run, the lab fleet, and every test
// above depend on that path staying live.
func TestWithoutAdoptTheEnvironmentIsStillUsed(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "notify.sock")
	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	t.Setenv("NOTIFY_SOCKET", sock)

	if adopted.Load() != nil {
		t.Fatal("a previous test left the socket adopted; this one would not test the env path")
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := ln.Read(buf)
		got <- string(buf[:n])
	}()
	if err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "READY=1" {
			t.Errorf("datagram = %q, want READY=1", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram received")
	}
}
