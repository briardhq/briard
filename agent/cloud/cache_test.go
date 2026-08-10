package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"briard.io/shared/api"
	"briard.io/shared/model"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment.json")
	want := api.Assignment{Tenant: "default", Role: model.RoleAnchor}
	if err := SaveAssignment(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadAssignment(path)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
	// The atomic temp file must not linger.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file lingered: %v", err)
	}
}

// A failed save must leave the PREVIOUS cache intact. Blocking the temp path with a directory is
// the cheap way to make the write fail after the caller has committed to it.
//
// STATED PLAINLY, because the point of these tests is not to flatter the diff: this one does NOT
// cover the fsync [V3.26b] added -- it passes against the pre-fsync implementation too, which
// already staged through a temp file. Durability is not observable from userspace without cutting
// power mid-write, so the fsync is closed by reading the code and by [V3.9]'s rig if it is ever
// built, not here. What this locks down is the atomicity the fsync sits on top of, which nothing
// else asserted. (agent/host's sibling test IS a real mutation guard -- that one was a bare
// WriteFile.)
func TestSaveAssignmentFailureKeepsThePriorCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment.json")
	prior := api.Assignment{Tenant: "default", Role: model.RoleAnchor}
	if err := SaveAssignment(path, prior); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o755); err != nil { // the staging path is now unusable
		t.Fatal(err)
	}
	if err := SaveAssignment(path, api.Assignment{Tenant: "other", Role: model.RoleAnchor}); err == nil {
		t.Fatal("save onto a blocked temp path returned nil, want an error")
	}
	got, ok, err := LoadAssignment(path)
	if err != nil || !ok {
		t.Fatalf("prior cache unreadable after a failed save: ok=%v err=%v", ok, err)
	}
	if got != prior {
		t.Errorf("failed save damaged the cache: got %+v, want the prior %+v", got, prior)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	_, ok, err := LoadAssignment(filepath.Join(t.TempDir(), "absent.json"))
	if ok || err != nil {
		t.Fatalf("missing cache: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// errClient is a CloudClient whose Register always fails -- stands in for a cloud/WAN outage.
type errClient struct{}

func (errClient) Register(context.Context, api.NodeInfo) (api.Assignment, error) {
	return api.Assignment{}, errors.New("cloud unreachable")
}
func (errClient) Report(context.Context, api.ReportRequest) ([]api.Directive, error) {
	return nil, nil
}
func (errClient) ReportMetrics(context.Context, string, []api.MetricAggregate) error { return nil }

func nolog(string, ...any) {}

// A reachable cloud: Resolve registers and caches the Assignment.
func TestResolveRegistersAndCaches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment.json")
	client := &Stub{Assignment: api.Assignment{Tenant: "default", Role: model.RoleAnchor}}
	info := api.NodeInfo{NodeName: "n1", Role: model.RoleAnchor}

	got := Resolve(context.Background(), client, path, info, nolog)
	if got.Tenant != "default" {
		t.Errorf("resolved tenant = %q, want default", got.Tenant)
	}
	if cached, ok, _ := LoadAssignment(path); !ok || cached != got {
		t.Errorf("cache = %+v ok=%v, want the registered assignment", cached, ok)
	}
}

// Cloud down but a cache present: Resolve cold-boots from the cache (degrade-to-local).
func TestResolveColdBootsFromCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment.json")
	cached := api.Assignment{Tenant: "default", Role: model.RoleDiskless}
	if err := SaveAssignment(path, cached); err != nil {
		t.Fatal(err)
	}
	got := Resolve(context.Background(), errClient{}, path, api.NodeInfo{NodeName: "n1", Role: model.RoleDiskless}, nolog)
	if got != cached {
		t.Errorf("cold-boot = %+v, want cached %+v", got, cached)
	}
}

// First boot during an outage (cloud down, no cache): Resolve returns a local default so
// the node still boots, tenant unresolved.
func TestResolveDefaultWhenNoCloudNoCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment.json")
	got := Resolve(context.Background(), errClient{}, path, api.NodeInfo{NodeName: "n1", Role: model.RoleAnchor}, nolog)
	if got.Tenant != "" || got.Role != model.RoleAnchor {
		t.Errorf("default = %+v, want empty tenant + anchor role", got)
	}
}
