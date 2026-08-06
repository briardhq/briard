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
