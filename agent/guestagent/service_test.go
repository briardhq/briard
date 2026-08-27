package guestagent

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

func ran(f *fakeExec, name string, args ...string) bool {
	for _, r := range f.runs {
		if r[0] != name || len(r) != len(args)+1 {
			continue
		}
		match := true
		for i, a := range args {
			if r[i+1] != a {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestServiceRenderWritesAndReloads(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	files := map[string]string{
		"briard-ha.pod":          "[Pod]\nPodName=briard-ha\n",
		"briard-ha-ha.container": "[Container]\nImage=x\n",
	}
	if err := g.ServiceRender(context.Background(), files, nil); err != nil {
		t.Fatalf("ServiceRender: %v", err)
	}
	for n, body := range files {
		if got := f.files[quadletDir+"/"+n]; got != body {
			t.Fatalf("%s written as %q, want %q", n, got, body)
		}
	}
	// The generator only runs at daemon-reload: without it the files are inert text and
	// nothing the promoter names would exist.
	if !ran(f, "systemctl", "daemon-reload") {
		t.Fatalf("no daemon-reload after rendering; runs=%v", f.runs)
	}
}

// TestServiceRenderClearsStale: swapping which service occupies the slot must not leave the
// previous one's units behind for the promoter to trip over.
func TestServiceRenderClearsStale(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	err := g.ServiceRender(context.Background(),
		map[string]string{"briard-new.container": "x"},
		[]string{"briard-old.container", "briard-old.pod"})
	if err != nil {
		t.Fatalf("ServiceRender: %v", err)
	}
	for _, stale := range []string{"briard-old.container", "briard-old.pod"} {
		if !ran(f, "rm", "-f", quadletDir+"/"+stale) {
			t.Fatalf("stale unit %s not removed; runs=%v", stale, f.runs)
		}
	}
}

// TestServiceRenderRefusesEscape is defence in depth behind the manifest's own slug check. The
// guest writes as root into a directory systemd reads, so a host-side regression that produced
// a "../" must not become an arbitrary file write.
func TestServiceRenderRefusesEscape(t *testing.T) {
	for _, bad := range []string{"../evil.container", "a/b.container", "", `x\y`, "..", "foo/../../etc/passwd"} {
		t.Run(bad, func(t *testing.T) {
			f := &fakeExec{}
			g := dial(t, f)
			err := g.ServiceRender(context.Background(), map[string]string{bad: "x"}, nil)
			if err == nil {
				t.Fatalf("ServiceRender accepted filename %q", bad)
			}
			if len(f.files) != 0 {
				t.Fatalf("wrote %v despite refusing", f.files)
			}
		})
	}
}

// TestServiceRenderRefusesEmpty: rendering nothing is a caller bug, not a no-op install.
func TestServiceRenderRefusesEmpty(t *testing.T) {
	g := dial(t, &fakeExec{})
	if err := g.ServiceRender(context.Background(), nil, nil); err == nil {
		t.Fatal("ServiceRender accepted an empty file set")
	}
}

// TestServiceProvisionCreatesAndRecords: subvolume, flat subdirs, manifest, and the sync that
// makes the identity replicate before a failover can depend on it.
func TestServiceProvisionCreatesAndRecords(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		// No existing subvolume -> `btrfs subvolume show` fails, so provision creates one.
		if name == "btrfs" && len(args) > 1 && args[1] == "show" {
			return nil, errors.New("not a subvolume")
		}
		return nil, nil
	}}
	g := dial(t, f)
	err := g.ServiceProvision(context.Background(), "home-assistant", "/var/lib/briard/home-assistant", []string{"ha", "cache"}, `{"name":"home-assistant"}`)
	if err != nil {
		t.Fatalf("ServiceProvision: %v", err)
	}
	if !ran(f, "btrfs", "subvolume", "create", "/var/lib/briard/home-assistant") {
		t.Fatalf("no subvolume create; runs=%v", f.runs)
	}
	for _, d := range []string{"ha", "cache"} {
		if !ran(f, "mkdir", "-p", "/var/lib/briard/home-assistant/"+d) {
			t.Fatalf("subdir %s not created; runs=%v", d, f.runs)
		}
	}
	if got := f.files[manifestPath("home-assistant")]; !strings.Contains(got, "home-assistant") {
		t.Fatalf("manifest recorded as %q", got)
	}
	// Protocol C acks a device write only once the peer holds it; without the flush a crash in
	// the writeback window loses the identity and a survivor promotes not knowing what to run.
	if !ran(f, "sync", "-f", manifestPath("home-assistant")) {
		t.Fatalf("manifest not flushed to the DRBD backing; runs=%v", f.runs)
	}
}

// TestServiceProvisionKeepsExistingData is the one that protects the user's state: a re-install
// onto an existing service must REUSE the subvolume. Re-creating it would silently delete
// everything the service had written.
func TestServiceProvisionKeepsExistingData(t *testing.T) {
	f := &fakeExec{output: []byte("Name: \t\thome-assistant\n")} // `subvolume show` succeeds
	g := dial(t, f)
	if err := g.ServiceProvision(context.Background(), "home-assistant", "/var/lib/briard/home-assistant", []string{"ha"}, `{}`); err != nil {
		t.Fatalf("ServiceProvision: %v", err)
	}
	if ran(f, "btrfs", "subvolume", "create", "/var/lib/briard/home-assistant") {
		t.Fatalf("re-created an existing subvolume — the service's data would be gone; runs=%v", f.runs)
	}
}

func TestServiceProvisionRefusesIncomplete(t *testing.T) {
	g := dial(t, &fakeExec{})
	if err := g.ServiceProvision(context.Background(), "x", "", nil, `{}`); err == nil {
		t.Fatal("accepted an empty data dir")
	}
	if err := g.ServiceProvision(context.Background(), "x", "/var/lib/briard/x", nil, ""); err == nil {
		t.Fatal("accepted an empty manifest")
	}
}

// TestServiceInstalledAbsentIsEmptyNotError: the shipped zero-service node has no manifest, and
// that is a legitimate state. Reading it as an error would make every empty node look broken.
func TestServiceInstalledAbsentIsEmptyNotError(t *testing.T) {
	f := &fakeExec{err: errors.New("cat: no such file")}
	g := dial(t, f)
	got, err := g.ServiceInstalled(context.Background(), "home-assistant")
	if err != nil {
		t.Fatalf("ServiceInstalled on an empty node errored: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestServiceInstalledReadsBack(t *testing.T) {
	f := &fakeExec{output: []byte(`{"name":"home-assistant"}`)}
	g := dial(t, f)
	got, err := g.ServiceInstalled(context.Background(), "home-assistant")
	if err != nil || !strings.Contains(got, "home-assistant") {
		t.Fatalf("got %q, %v", got, err)
	}
}

// The pre-eviction flush ([B.100a]). What matters: it syncs the MOUNTED volume and answers
// "skipped" -- success, not error -- on a node that has it unmounted, because a Secondary
// asked to make its dirty data small is already done.
func TestFsSyncFlushesTheMountedVolume(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "stat" {
			return []byte(dataMountRoot + "\n"), nil // the path IS its own mount point
		}
		return nil, nil
	}}
	g := dial(t, f)
	detail, err := g.FsSync(context.Background())
	if err != nil {
		t.Fatalf("FsSync: %v", err)
	}
	if detail != "synced" {
		t.Fatalf("detail = %q, want synced", detail)
	}
	if !ran(f, "sync", "-f", dataMountRoot) {
		t.Fatalf("no sync -f %s; runs=%v", dataMountRoot, f.runs)
	}
}

func TestFsSyncSkipsAnUnmountedVolume(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "stat" {
			return []byte("/\n"), nil // the dir sits on the root fs: nothing of ours mounted
		}
		return nil, nil
	}}
	g := dial(t, f)
	detail, err := g.FsSync(context.Background())
	if err != nil {
		t.Fatalf("FsSync on an unmounted volume must succeed, got: %v", err)
	}
	if !strings.HasPrefix(detail, "skipped") {
		t.Fatalf("detail = %q, want skipped", detail)
	}
	if ran(f, "sync", "-f", dataMountRoot) {
		t.Fatal("synced a volume that is not mounted -- that flushes the ROOT fs for nothing")
	}
}

// THE PER-SERVICE SPLIT ([V3b.3](b)): the volume must be able to say WHICH service a manifest
// belongs to. With one unnamed file, installing a second service recorded its identity over the
// first's -- so the first's prior became unreadable, and on the host side filesToRemove then
// deleted that service's rendered units as a renamed prior's orphans.
//
// Asserted on the PATHS, because that is where the property lives: two provisions must write two
// files, and each must be named for its own service.
func TestServiceProvisionRecordsPerService(t *testing.T) {
	f := &fakeExec{output: []byte("Name: \t\tx\n")} // an existing subvolume; this is about the manifest
	g := dial(t, f)
	for _, s := range []struct{ name, body string }{
		{"home-assistant", `{"name":"home-assistant"}`},
		{"mosquitto", `{"name":"mosquitto"}`},
	} {
		if err := g.ServiceProvision(context.Background(), s.name, "/var/lib/briard/"+s.name, nil, s.body); err != nil {
			t.Fatalf("ServiceProvision(%s): %v", s.name, err)
		}
	}
	for _, s := range []struct{ name, want string }{
		{"home-assistant", "home-assistant"},
		{"mosquitto", "mosquitto"},
	} {
		got, ok := f.files[manifestPath(s.name)]
		if !ok {
			t.Fatalf("no manifest recorded at %s; files=%v", manifestPath(s.name), slices.Sorted(maps.Keys(f.files)))
		}
		if !strings.Contains(got, s.want) {
			t.Errorf("%s recorded %q, want it to name %s -- one service's identity landed on another's file", s.name, got, s.want)
		}
	}
}

// A service name becomes a PATH ELEMENT on both verbs, so both re-check it here rather than
// trusting the host to have validated its own input.
func TestServiceVerbsRefuseAnEscapingName(t *testing.T) {
	g := dial(t, &fakeExec{})
	if err := g.ServiceProvision(context.Background(), "../../etc/x", "/var/lib/briard/x", nil, `{}`); err == nil {
		t.Error("service.provision accepted a name that escapes the manifest directory")
	}
	if _, err := g.ServiceInstalled(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("service.installed accepted a name that escapes the manifest directory")
	}
}
