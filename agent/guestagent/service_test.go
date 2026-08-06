package guestagent

import (
	"context"
	"errors"
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
	err := g.ServiceProvision(context.Background(), "/var/lib/briard/home-assistant", []string{"ha", "cache"}, `{"name":"home-assistant"}`)
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
	if got := f.files[manifestPinPath]; !strings.Contains(got, "home-assistant") {
		t.Fatalf("manifest recorded as %q", got)
	}
	// Protocol C acks a device write only once the peer holds it; without the flush a crash in
	// the writeback window loses the identity and a survivor promotes not knowing what to run.
	if !ran(f, "sync", "-f", manifestPinPath) {
		t.Fatalf("manifest not flushed to the DRBD backing; runs=%v", f.runs)
	}
}

// TestServiceProvisionKeepsExistingData is the one that protects the user's state: a re-install
// onto an existing service must REUSE the subvolume. Re-creating it would silently delete
// everything the service had written.
func TestServiceProvisionKeepsExistingData(t *testing.T) {
	f := &fakeExec{output: []byte("Name: \t\thome-assistant\n")} // `subvolume show` succeeds
	g := dial(t, f)
	if err := g.ServiceProvision(context.Background(), "/var/lib/briard/home-assistant", []string{"ha"}, `{}`); err != nil {
		t.Fatalf("ServiceProvision: %v", err)
	}
	if ran(f, "btrfs", "subvolume", "create", "/var/lib/briard/home-assistant") {
		t.Fatalf("re-created an existing subvolume — the service's data would be gone; runs=%v", f.runs)
	}
}

func TestServiceProvisionRefusesIncomplete(t *testing.T) {
	g := dial(t, &fakeExec{})
	if err := g.ServiceProvision(context.Background(), "", nil, `{}`); err == nil {
		t.Fatal("accepted an empty data dir")
	}
	if err := g.ServiceProvision(context.Background(), "/var/lib/briard/x", nil, ""); err == nil {
		t.Fatal("accepted an empty manifest")
	}
}

// TestServiceManifestAbsentIsEmptyNotError: the shipped zero-service node has no manifest, and
// that is a legitimate state. Reading it as an error would make every empty node look broken.
func TestServiceManifestAbsentIsEmptyNotError(t *testing.T) {
	f := &fakeExec{err: errors.New("cat: no such file")}
	g := dial(t, f)
	got, err := g.ServiceManifest(context.Background())
	if err != nil {
		t.Fatalf("ServiceManifest on an empty node errored: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestServiceManifestReadsBack(t *testing.T) {
	f := &fakeExec{output: []byte(`{"name":"home-assistant"}`)}
	g := dial(t, f)
	got, err := g.ServiceManifest(context.Background())
	if err != nil || !strings.Contains(got, "home-assistant") {
		t.Fatalf("got %q, %v", got, err)
	}
}
