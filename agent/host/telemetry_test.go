package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"briard.io/shared/telemetry"
)

// Telemetry off is the SHIPPED state (install.sh sets no TELEMETRY_PATH), so the nil writer is
// the ordinary path and not an edge case. A Config that never started one -- which is every
// caller of observe in these tests -- must be silently fine.
func TestWriteTelemetryOffIsANoOp(t *testing.T) {
	var cfg Config // no TelemetryPath, no writer
	cfg.writeTelemetry(&telemetry.NodeResources{AgentRSSKB: 1}, func(string, ...any) {
		t.Error("telemetry is off; nothing should have been logged")
	})
	if w := cfg.newTelemetryWriter(context.Background(), func(string, ...any) {}); w != nil {
		t.Errorf("newTelemetryWriter with no path = %v, want nil", w)
	}
}

// The sample reaches the file by atomic rename, and the .tmp sibling does not survive.
func TestWriteTelemetryPublishesTheSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.json")
	cfg := Config{TelemetryPath: path}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.telemetry = cfg.newTelemetryWriter(ctx, func(string, ...any) {})

	cfg.writeTelemetry(&telemetry.NodeResources{AgentRSSKB: 4242}, func(string, ...any) {})
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), "4242") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sample never appeared at %s (err %v)", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp sibling still present after the rename (err %v)", err)
	}
}

// THE ONE THAT CARRIES B.87. A TELEMETRY_PATH whose .tmp sibling is a reader-less FIFO blocks
// open(2) forever -- uninterruptibly, with no ctx to honour and no deadline that could reach it,
// which is precisely why the fix is a goroutine rather than a timeout. The caller must not
// notice: writeTelemetry hands off or drops, and returns either way.
//
// This is the mutation check for the whole change. Put the write back inline (or make the send
// blocking) and this test hangs on the FIRST call, which is exactly what it used to do to the
// observe loop -- and to the node's supervisor with it.
//
// The FIFO is a stand-in for the realistic trigger, a hung mount under TELEMETRY_PATH: an NFS or
// FUSE path gone unresponsive, which is not exotic on a box where someone pointed telemetry at
// shared storage. A full or read-only filesystem needs no test here -- WriteFile returns an
// error there and always did.
func TestWriteTelemetryDoesNotBlockOnAHungPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.json")
	if err := syscall.Mkfifo(path+".tmp", 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	cfg := Config{TelemetryPath: path}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // does NOT free the writer: it is parked in open(2), and that is the point
	cfg.telemetry = cfg.newTelemetryWriter(ctx, func(string, ...any) {})

	var mu sync.Mutex
	var logs []string
	logf := func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, f)
	}

	// Call as the observe loop would, on its cadence, and require the whole run to finish inside
	// a budget far below what one blocked open would cost (forever). Run it off the test
	// goroutine so a regression reports itself instead of hanging until the package timeout.
	done := make(chan int)
	go func() {
		// The first sample is consumed by the writer (which then blocks in open), the second
		// fills the depth-1 buffer, and every one after that must drop.
		for i := 0; i < 20; i++ {
			cfg.writeTelemetry(&telemetry.NodeResources{AgentRSSKB: int64(i)}, logf)
			time.Sleep(10 * time.Millisecond)
		}
		done <- 0
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writeTelemetry blocked on a hung TELEMETRY_PATH — the write is back on the caller's goroutine")
	}

	// Non-vacuous: prove the path really was hung rather than merely fast. Exactly one drop
	// notice, because it is edge-triggered and the writer never comes back.
	mu.Lock()
	defer mu.Unlock()
	drops := 0
	for _, l := range logs {
		if strings.Contains(l, "not keeping up") {
			drops++
		}
	}
	if drops != 1 {
		t.Errorf("drop notices = %d, want exactly 1 (edge-triggered); logs: %v", drops, logs)
	}
}

// The drop latch clears, so a path that recovers starts saying so again rather than staying
// silent for the life of the agent. Driven through the latch itself (no writer goroutine), since
// what is under test is the sender's bookkeeping and nothing else.
func TestWriteTelemetryDropLatchClearsOnRecovery(t *testing.T) {
	cfg := Config{TelemetryPath: "/dev/null", telemetry: &telemetryWriter{ch: make(chan *telemetry.NodeResources, 1)}}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	res := &telemetry.NodeResources{}

	cfg.writeTelemetry(res, logf) // buffered
	cfg.writeTelemetry(res, logf) // full -> drop, and say so
	cfg.writeTelemetry(res, logf) // still full -> drop silently
	<-cfg.telemetry.ch            // the writer catches up
	cfg.writeTelemetry(res, logf) // room again -> say so

	if len(logs) != 2 || !strings.Contains(logs[0], "not keeping up") || !strings.Contains(logs[1], "caught up") {
		t.Errorf("logs = %v, want exactly one drop notice then one recovery notice", logs)
	}
}

// The wedge fixture is off unless a test arms it, and an absent path is a silent no-op --
// the fail-safe direction, since a fixture that quietly fails to engage must make
// agent-watchdog.nix go red rather than green.
func TestWedgeFixtureIsOffByDefault(t *testing.T) {
	var cfg Config
	cfg.wedgeForTest(func(string, ...any) { t.Error("unarmed fixture logged") })
	cfg.WedgeFIFO = filepath.Join(t.TempDir(), "not-there")
	cfg.wedgeForTest(func(string, ...any) { t.Error("armed-but-absent fixture logged") })
}
