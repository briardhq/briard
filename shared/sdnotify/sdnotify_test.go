package sdnotify

import (
	"net"
	"path/filepath"
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
