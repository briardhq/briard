package guestagent

import (
	"context"
	"strings"
	"testing"
)

func TestParseVmRSSKB(t *testing.T) {
	status := "Name:\tqemu\nVmPeak:\t 900000 kB\nVmRSS:\t  123456 kB\nThreads:\t4\n"
	if got := parseVmRSSKB([]byte(status)); got != 123456 {
		t.Errorf("parseVmRSSKB = %d, want 123456", got)
	}
	if got := parseVmRSSKB([]byte("no rss here")); got != 0 {
		t.Errorf("parseVmRSSKB(absent) = %d, want 0", got)
	}
}

func TestParseLoad1(t *testing.T) {
	if got := parseLoad1([]byte("0.42 0.31 0.20 1/234 5678\n")); got != 0.42 {
		t.Errorf("parseLoad1 = %v, want 0.42", got)
	}
	if got := parseLoad1(nil); got != 0 {
		t.Errorf("parseLoad1(empty) = %v, want 0", got)
	}
}

func TestParseDfUsedKB(t *testing.T) {
	df := "Filesystem     1024-blocks   Used Available Capacity Mounted on\n" +
		"/dev/drbd1        10321208 654321   9666887       7% /var/lib/briard\n"
	if got := parseDfUsedKB([]byte(df)); got != 654321 {
		t.Errorf("parseDfUsedKB = %d, want 654321", got)
	}
	if got := parseDfUsedKB([]byte("header only\n")); got != 0 {
		t.Errorf("parseDfUsedKB(no data) = %d, want 0", got)
	}
}

func TestParseDuKB(t *testing.T) {
	if got := parseDuKB([]byte("204800\t/var/log/journal\n")); got != 204800 {
		t.Errorf("parseDuKB = %d, want 204800", got)
	}
}

func TestCountLines(t *testing.T) {
	if got := countLines([]byte("a\nb\n\nc\n")); got != 3 {
		t.Errorf("countLines = %d, want 3 (blank lines ignored)", got)
	}
	if got := countLines(nil); got != 0 {
		t.Errorf("countLines(empty) = %d, want 0", got)
	}
}

// The sys.resources verb round-trips: the host asks for a unit + data dir, the guest reads
// each source and returns the parsed appliance telemetry. The fake keys on the command so
// each probe gets a realistic canned output.
func TestResourcesVerbGathers(t *testing.T) {
	x := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.HasPrefix(cmd, "systemctl show -p MainPID"):
			return []byte("4242\n"), nil
		case cmd == "cat /proc/4242/status":
			return []byte("VmRSS:\t  88000 kB\n"), nil
		case cmd == "ls /proc/4242/fd":
			return []byte("0\n1\n2\n3\n4\n"), nil
		case cmd == "cat /proc/loadavg":
			return []byte("1.50 0.9 0.7 2/300 999\n"), nil
		case strings.HasPrefix(cmd, "df -kP"):
			return []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
				"/dev/drbd1 100 4200 60 40% /d\n"), nil
		case strings.HasPrefix(cmd, "btrfs subvolume list -s"):
			return []byte("ID 260 gen 9 top 5 path .snap/pre-1\nID 261 gen 9 top 5 path .snap/pre-2\n"), nil
		case strings.HasPrefix(cmd, "du -skx /var/log/journal"):
			return []byte("2048\t/var/log/journal\n"), nil
		case strings.HasPrefix(cmd, "du -skx /var/lib/containers"):
			return []byte("500000\t/var/lib/containers\n"), nil
		case strings.HasPrefix(cmd, "systemctl show -p NRestarts"):
			return []byte("3\n"), nil
		case strings.HasPrefix(cmd, "journalctl -k"):
			// --show-cursor appends a trailing "-- cursor: <c>" marker (stripped by the guest).
			return []byte("kernel: booting\nkernel: BTRFS error (device drbd1): bad tree block\n-- cursor: s=abc123\n"), nil
		default:
			return nil, nil
		}
	}}
	g := dial(t, x)
	r, err := g.Resources(context.Background(), "podman-ha.service", "/var/lib/briard/data")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if r.PayloadRSSKB != 88000 {
		t.Errorf("PayloadRSSKB = %d, want 88000", r.PayloadRSSKB)
	}
	if r.PayloadFDs != 5 {
		t.Errorf("PayloadFDs = %d, want 5", r.PayloadFDs)
	}
	if r.Load1 != 1.50 {
		t.Errorf("Load1 = %v, want 1.50", r.Load1)
	}
	if r.VolumeUsedKB != 4200 {
		t.Errorf("VolumeUsedKB = %d, want 4200", r.VolumeUsedKB)
	}
	if r.SnapshotCount != 2 {
		t.Errorf("SnapshotCount = %d, want 2", r.SnapshotCount)
	}
	if r.LogSizeKB != 2048 {
		t.Errorf("LogSizeKB = %d, want 2048", r.LogSizeKB)
	}
	if r.PodmanStoreKB != 500000 {
		t.Errorf("PodmanStoreKB = %d, want 500000", r.PodmanStoreKB)
	}
	if r.PayloadRestarts != 3 {
		t.Errorf("PayloadRestarts = %d, want 3", r.PayloadRestarts)
	}
	if len(r.KernelErrors) != 2 { // both non-empty guest kernel lines reported (oracle scans them)
		t.Errorf("KernelErrors = %v, want 2 lines", r.KernelErrors)
	}
	// The verb fills appliance fields only; the host adds the agent footprint separately.
	if r.AgentRSSKB != 0 || r.AgentFDs != 0 {
		t.Errorf("verb must not set agent fields, got RSS=%d FDs=%d", r.AgentRSSKB, r.AgentFDs)
	}
}

// A stopped payload (MainPID=0) skips the /proc read rather than reading /proc/0, and the
// rest of the telemetry still comes back -- best-effort, never fatal.
func TestResourcesVerbSkipsStoppedPayload(t *testing.T) {
	x := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.HasPrefix(cmd, "systemctl show -p MainPID"):
			return []byte("0\n"), nil
		case cmd == "cat /proc/loadavg":
			return []byte("0.10 0 0 1/1 1\n"), nil
		case strings.HasPrefix(cmd, "cat /proc/0/"):
			t.Fatalf("must not read /proc/0 for a stopped unit")
			return nil, nil
		default:
			return nil, nil
		}
	}}
	g := dial(t, x)
	r, err := g.Resources(context.Background(), "podman-ha.service", "")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if r.PayloadRSSKB != 0 || r.PayloadFDs != 0 {
		t.Errorf("stopped payload should report zero footprint, got RSS=%d FDs=%d", r.PayloadRSSKB, r.PayloadFDs)
	}
	if r.Load1 != 0.10 {
		t.Errorf("Load1 = %v, want 0.10 (other metrics still gathered)", r.Load1)
	}
}
