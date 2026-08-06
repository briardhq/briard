package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/internal/testsock"
)

// fakeQMP serves one connection the way QEMU does -- greeting first, then a reply per
// command -- and records the commands it was asked to run. replies are sent verbatim in
// order, so a test can interleave events with the reply it is waiting for.
func fakeQMP(t *testing.T, replies map[string][]string) (path string, got *[]string) {
	t.Helper()
	dir := testsock.Dir(t)
	path = filepath.Join(dir, "qmp.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	commands := []string{}
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(`{"QMP":{"version":{"qemu":{"major":10}},"capabilities":[]}}` + "\n"))
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			var req struct {
				Execute string `json:"execute"`
			}
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				return
			}
			commands = append(commands, req.Execute)
			lines, ok := replies[req.Execute]
			if !ok {
				lines = []string{`{"return":{}}`}
			}
			for _, line := range lines {
				_, _ = c.Write([]byte(line + "\n"))
			}
		}
	}()
	return path, &commands
}

// The handshake is not optional politeness: QMP opens in capabilities-negotiation mode and
// refuses every other command until qmp_capabilities has run. So a command sent through this
// package must always be preceded by it, on every fresh connection.
func TestQMPNegotiatesBeforeCommanding(t *testing.T) {
	path, got := fakeQMP(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := qmpExecute(ctx, path, "system_powerdown", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"qmp_capabilities", "system_powerdown"}
	if len(*got) != 2 || (*got)[0] != want[0] || (*got)[1] != want[1] {
		t.Errorf("commands = %v, want %v", *got, want)
	}
}

// Events arrive asynchronously and can land between a command and its reply. Mistaking one
// for the reply would return success from a command that had not answered yet -- and the
// commands on this channel are the ones whose completion the caller acts on.
func TestQMPSkipsAsyncEvents(t *testing.T) {
	path, _ := fakeQMP(t, map[string][]string{
		"system_powerdown": {
			`{"event":"POWERDOWN","timestamp":{"seconds":1,"microseconds":0}}`,
			`{"event":"SHUTDOWN","data":{"guest":true},"timestamp":{"seconds":2,"microseconds":0}}`,
			`{"return":{}}`,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := qmpExecute(ctx, path, "system_powerdown", nil); err != nil {
		t.Fatalf("events should be skipped, not read as the reply: %v", err)
	}
}

// A QMP error is a structured object, not a transport failure -- so it must surface as an
// error rather than being read as an empty success.
func TestQMPSurfacesCommandError(t *testing.T) {
	path, _ := fakeQMP(t, map[string][]string{
		"system_reset": {`{"error":{"class":"GenericError","desc":"nope"}}`},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := qmpExecute(ctx, path, "system_reset", nil)
	if err == nil {
		t.Fatal("a QMP error reply must fail the call")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should carry QEMU's description: %v", err)
	}
}

// A guest launched without a monitor must say so, not silently do nothing. The caller's
// fallback is Stop -- a power cut -- and that has to be a decision, never a default reached
// by a no-op returning nil.
func TestShutdownWithoutMonitorFails(t *testing.T) {
	g := &Guest{unit: "briard-guest.service"}
	if err := g.Shutdown(context.Background(), time.Second); err == nil {
		t.Fatal("Shutdown without a QMP socket must fail")
	}
}

// QMP is unrestricted control of the VM and QEMU creates the socket 0755, so the directory
// is the containment. Launch applies it before starting qemu; here we assert the primitive,
// including that it TIGHTENS an existing loose directory rather than trusting it.
func TestSecureQMPDirIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(testsock.Dir(t), "run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureQMPDir(filepath.Join(dir, "qmp.sock")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("QMP dir mode = %o, want 700 (anyone who can connect owns the VM)", fi.Mode().Perm())
	}
	// No socket configured is not an error -- most launches want no monitor at all.
	if err := secureQMPDir(""); err != nil {
		t.Errorf("empty QMP socket should be a no-op: %v", err)
	}
}
