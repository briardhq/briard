package guestagent

import (
	"context"
	"strings"
	"testing"
)

// A service's memory comes from the unit's CGROUP, and specifically from anon -- not
// memory.current, which counts page cache that a database-writing service grows indefinitely, and
// not the main process, which for a container is podman's supervisor.
func TestParseCgroupAnonKB(t *testing.T) {
	v2 := "anon 268435456\nfile 1073741824\nkernel 12345\nslab 678\n"
	if got := parseCgroupAnonKB([]byte(v2)); got != 262144 {
		t.Errorf("parseCgroupAnonKB = %d, want 262144 (256 MB of anon)", got)
	}
	// A cgroup v1 memory.stat has no "anon" field (it says "rss"), and a v1 guest must read 0
	// rather than quietly reporting a different quantity under the same series name.
	v1 := "cache 1073741824\nrss 268435456\nrss_huge 0\nmapped_file 4096\n"
	if got := parseCgroupAnonKB([]byte(v1)); got != 0 {
		t.Errorf("v1 memory.stat = %d, want 0 (no anon field)", got)
	}
	for _, junk := range []string{"", "anon\n", "anon notanumber\n", "anon -5\n"} {
		if got := parseCgroupAnonKB([]byte(junk)); got != 0 {
			t.Errorf("parseCgroupAnonKB(%q) = %d, want 0", junk, got)
		}
	}
}

// Each memory parser reads 0 from junk rather than a wrong number: a guest with no zram device,
// or a kernel without PSI, must report "unread", never a plausible-looking value.
func TestMemoryParsersReadZeroFromJunk(t *testing.T) {
	for _, junk := range []string{"", "garbage\n", "MemTotal:\n", "1 2\n"} {
		if a, b, c := parseMeminfo([]byte(junk)); a|b|c != 0 {
			t.Errorf("parseMeminfo(%q) = %d %d %d, want zeros", junk, a, b, c)
		}
		if a, b := parseZramMMStat([]byte(junk)); a|b != 0 {
			t.Errorf("parseZramMMStat(%q) = %d %d, want zeros", junk, a, b)
		}
		if a, b := parseVmstatSwap([]byte(junk)); a|b != 0 {
			t.Errorf("parseVmstatSwap(%q) = %d %d, want zeros", junk, a, b)
		}
		if a, b := parsePSI60([]byte(junk)); a != 0 || b != 0 {
			t.Errorf("parsePSI60(%q) = %v %v, want zeros", junk, a, b)
		}
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
		case strings.HasPrefix(cmd, "systemctl show -p ControlGroup"):
			return []byte("/system.slice/briard-ha-app.service\n"), nil
		case cmd == "cat /sys/fs/cgroup/system.slice/briard-ha-app.service/memory.stat":
			// anon is what the service actually holds; file is page cache and must NOT count.
			return []byte("anon 90112000\nfile 4294967296\n"), nil
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
		case cmd == "cat /proc/meminfo":
			return []byte("MemTotal:        1000000 kB\nMemFree:          50000 kB\nMemAvailable:     300000 kB\nAnonPages:        600000 kB\n"), nil
		case cmd == "cat /sys/block/zram0/mm_stat":
			return []byte("  314572800  104857600  110100480        0  110100480     1234        0        0        0\n"), nil
		case cmd == "cat /proc/vmstat":
			return []byte("nr_free_pages 12500\npswpin 42\npswpout 76800\npswpin_extra 9\n"), nil
		case cmd == "cat /proc/pressure/memory":
			return []byte("some avg10=1.00 avg60=2.50 avg300=0.80 total=123\nfull avg10=0.10 avg60=0.40 avg300=0.05 total=45\n"), nil
		default:
			return nil, nil
		}
	}}
	g := dial(t, x)
	r, err := g.Resources(context.Background(), map[string]string{"ha": "podman-ha.service"}, "/var/lib/briard/data")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(r.Payloads) != 1 || r.Payloads[0].Name != "ha" {
		t.Fatalf("Payloads = %+v, want one entry NAMED for the service asked about", r.Payloads)
	}
	p := r.Payloads[0]
	if p.RSSKB != 88000 { // 90112000 bytes of anon; the 4 GB of page cache is excluded
		t.Errorf("RSSKB = %d, want 88000 (cgroup anon, not memory.current)", p.RSSKB)
	}
	if p.FDs != 5 {
		t.Errorf("FDs = %d, want 5", p.FDs)
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
	if p.Restarts != 3 {
		t.Errorf("Restarts = %d, want 3", p.Restarts)
	}
	if len(r.KernelErrors) != 2 { // both non-empty guest kernel lines reported (oracle scans them)
		t.Errorf("KernelErrors = %v, want 2 lines", r.KernelErrors)
	}
	if r.MemTotalKB != 1000000 || r.MemAvailableKB != 300000 || r.AnonKB != 600000 {
		t.Errorf("meminfo = total %d avail %d anon %d, want 1000000/300000/600000", r.MemTotalKB, r.MemAvailableKB, r.AnonKB)
	}
	// mm_stat is in BYTES: 300 MB parked, holding 105 MB of RAM (mem_used_total, field 2 --
	// not compr_data_size, field 1, which leaves out the allocator's own overhead).
	if r.ZramOrigKB != 307200 || r.ZramUsedKB != 107520 {
		t.Errorf("zram = orig %d used %d, want 307200/107520", r.ZramOrigKB, r.ZramUsedKB)
	}
	if r.PswpIn != 42 || r.PswpOut != 76800 { // exact names: pswpin_extra must not overwrite pswpin
		t.Errorf("swap = in %d out %d, want 42/76800", r.PswpIn, r.PswpOut)
	}
	if r.MemPSISome60 != 2.50 || r.MemPSIFull60 != 0.40 {
		t.Errorf("psi = some %v full %v, want avg60 2.50/0.40", r.MemPSISome60, r.MemPSIFull60)
	}
	// The verb fills appliance fields only; the host adds the agent footprint separately.
	if r.AgentRSSKB != 0 || r.AgentFDs != 0 {
		t.Errorf("verb must not set agent fields, got RSS=%d FDs=%d", r.AgentRSSKB, r.AgentFDs)
	}
}

// A stopped service (MainPID=0) skips the /proc read rather than reading /proc/0, and the
// rest of the telemetry still comes back -- best-effort, never fatal.
func TestResourcesVerbSkipsAStoppedService(t *testing.T) {
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
	r, err := g.Resources(context.Background(), map[string]string{"ha": "podman-ha.service"}, "")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(r.Payloads) != 1 || r.Payloads[0].RSSKB != 0 || r.Payloads[0].FDs != 0 {
		t.Errorf("a stopped service should report zero footprint, got %+v", r.Payloads)
	}
	if r.Load1 != 0.10 {
		t.Errorf("Load1 = %v, want 0.10 (other metrics still gathered)", r.Load1)
	}
}

// journalctl talks in its own voice, and none of it is a log line. "-- No entries --" is what a
// HEALTHY node's poll returns, and counting it made kerr sit at a permanent 1 -- which hides the
// step up to a real kernel error, the one thing this signal exists to catch.
func TestSplitJournalCursorDropsJournalctlMarkers(t *testing.T) {
	t.Run("no entries yields no lines", func(t *testing.T) {
		lines, cursor := splitJournalCursor([]byte("-- No entries --\n"))
		if len(lines) != 0 {
			t.Errorf("healthy poll produced %d kernel error line(s): %q", len(lines), lines)
		}
		if cursor != "" {
			t.Errorf("no entries means no new cursor, got %q", cursor)
		}
	})
	t.Run("real entries survive alongside markers", func(t *testing.T) {
		out := []byte("-- Journal begins at Thu 2026-08-06 21:53:49 EEST. --\n" +
			"Aug 06 21:53:53 guest kernel: drbd: loading out-of-tree module taints kernel.\n" +
			"-- Reboot --\n" +
			"Aug 06 21:53:53 guest kernel: EXT4-fs error (device vda1): something real\n" +
			"-- cursor: s=abc;i=1;b=2\n")
		lines, cursor := splitJournalCursor(out)
		if len(lines) != 2 {
			t.Fatalf("want the 2 kernel lines, got %d: %q", len(lines), lines)
		}
		for _, l := range lines {
			if !strings.Contains(l, "kernel:") {
				t.Errorf("kept a non-kernel line: %q", l)
			}
		}
		if cursor != "s=abc;i=1;b=2" {
			t.Errorf("cursor = %q", cursor)
		}
	})
}

// N SERVICES, EACH MEASURED AND EACH NAMED. The alternative was summing them into
// the three scalars this replaced, and summing is what loses the signal: NRestarts exists to make
// a crash-loop loud, and one service flapping inside a total reads as a small climb. So the
// assertion is that a crash-looping service is attributable BY NAME while its quiet neighbour
// stays at zero -- a sum could not tell those apart.
func TestResourcesVerbMeasuresEachServiceSeparately(t *testing.T) {
	x := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.HasPrefix(cmd, "systemctl show -p NRestarts --value briard-mosquitto"):
			return []byte("7\n"), nil // the flapping one
		case strings.HasPrefix(cmd, "systemctl show -p NRestarts"):
			return []byte("0\n"), nil // the quiet one
		case strings.HasPrefix(cmd, "systemctl show -p MainPID"):
			return []byte("0\n"), nil
		default:
			return nil, nil
		}
	}}
	g := dial(t, x)
	r, err := g.Resources(context.Background(), map[string]string{
		"home-assistant": "briard-home-assistant-app.service",
		"mosquitto":      "briard-mosquitto-broker.service",
	}, "")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(r.Payloads) != 2 {
		t.Fatalf("Payloads = %+v, want one entry per service", r.Payloads)
	}
	// Sorted by name, so a run's forensics read the same order every cycle.
	if r.Payloads[0].Name != "home-assistant" || r.Payloads[1].Name != "mosquitto" {
		t.Errorf("order = %q,%q, want sorted by name", r.Payloads[0].Name, r.Payloads[1].Name)
	}
	if r.Payloads[0].Restarts != 0 {
		t.Errorf("home-assistant restarts = %d, want 0 -- its neighbour's flapping is not its own", r.Payloads[0].Restarts)
	}
	if r.Payloads[1].Restarts != 7 {
		t.Errorf("mosquitto restarts = %d, want 7 -- the crash-loop must be attributable by name", r.Payloads[1].Restarts)
	}
}

// A service whose unit is unnamed is SKIPPED, not measured as zero. An unmeasured service and an
// idle one must not read the same: a zero row would enter the oracle's series as a real sample and
// flatten a trend that nobody is actually watching.
func TestResourcesVerbSkipsAServiceWithNoUnit(t *testing.T) {
	g := dial(t, &fakeExec{})
	r, err := g.Resources(context.Background(), map[string]string{"ha": ""}, "")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if len(r.Payloads) != 0 {
		t.Errorf("Payloads = %+v, want none: an unmeasurable service must not report a zero row", r.Payloads)
	}
}
