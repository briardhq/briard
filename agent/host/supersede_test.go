package host

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/guestagent"
	"briard.io/shared/manifest"
)

// volumeFake is the guest as dropSuperseded sees it: the manifests on the volume, and what it
// was asked to remove.
type volumeFake struct {
	volume  map[string]string
	readErr string // the service whose manifest cannot be read
	removed []string
}

func (v *volumeFake) ServiceList(context.Context) ([]string, error) {
	var names []string
	for n := range v.volume {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}
func (v *volumeFake) ServiceInstalled(_ context.Context, name string) (string, error) {
	if name == v.readErr {
		return "", errors.New("read failed")
	}
	return v.volume[name], nil
}
func (v *volumeFake) SupportsServiceList() bool { return true }
func (v *volumeFake) RemoveImage(_ context.Context, ref string) (bool, error) {
	v.removed = append(v.removed, ref)
	return false, nil
}
func (v *volumeFake) SupportsImageRemove() bool { return true }

func manifestWith(t *testing.T, name, version, digestChar string) (string, string) {
	t.Helper()
	m := testManifest()
	m.Name, m.Version = name, version
	m.Containers[0].Image = "ghcr.io/x/" + name + "@sha256:" + strings.Repeat(digestChar, 64)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manifest.Parse(raw); err != nil {
		t.Fatalf("test manifest does not parse: %v", err)
	}
	return string(raw), m.Containers[0].Image
}

// An upgrade that committed drops the image the old version ran on -- and only that one: the new
// version's image and every other service's stay.
func TestDropSupersededRemovesOnlyWhatNothingPins(t *testing.T) {
	was, oldRef := manifestWith(t, "home-assistant", "2026.6.0", "a")
	now, _ := manifestWith(t, "home-assistant", "2026.7.1", "b")
	other, _ := manifestWith(t, "mosquitto", "2.0", "c")
	v := &volumeFake{volume: map[string]string{"home-assistant": now, "mosquitto": other}}
	Config{}.dropSuperseded(context.Background(), v, was, t.Logf)
	if !slices.Equal(v.removed, []string{oldRef}) {
		t.Errorf("removed %v, want only the superseded %s", v.removed, oldRef)
	}
}

// An image another service still pins is not superseded, whoever moved off it.
func TestDropSupersededKeepsAnImageAnotherServicePins(t *testing.T) {
	was, _ := manifestWith(t, "home-assistant", "2026.6.0", "a")
	now, _ := manifestWith(t, "home-assistant", "2026.7.1", "b")
	twin, _ := manifestWith(t, "twin", "1", "a") // a different service on the same digest
	twin = strings.Replace(twin, "ghcr.io/x/twin@", "ghcr.io/x/home-assistant@", 1)
	v := &volumeFake{volume: map[string]string{"home-assistant": now, "twin": twin}}
	Config{}.dropSuperseded(context.Background(), v, was, t.Logf)
	if len(v.removed) != 0 {
		t.Errorf("removed %v, an image another service still runs on", v.removed)
	}
}

// THE GATE: a manifest on the volume that cannot be read means "could not tell", and that must
// never take the branch that deletes -- the service it belongs to may run on the very image.
func TestDropSupersededRemovesNothingWhenAVolumeReadFails(t *testing.T) {
	was, _ := manifestWith(t, "home-assistant", "2026.6.0", "a")
	now, _ := manifestWith(t, "home-assistant", "2026.7.1", "b")
	other, _ := manifestWith(t, "mosquitto", "2.0", "c")
	v := &volumeFake{volume: map[string]string{"home-assistant": now, "mosquitto": other}, readErr: "mosquitto"}
	Config{}.dropSuperseded(context.Background(), v, was, t.Logf)
	if len(v.removed) != 0 {
		t.Errorf("removed %v although a volume manifest could not be read", v.removed)
	}
}

// A fresh install has nothing superseded.
func TestDropSupersededFreshInstallRemovesNothing(t *testing.T) {
	now, _ := manifestWith(t, "home-assistant", "2026.7.1", "b")
	v := &volumeFake{volume: map[string]string{"home-assistant": now}}
	Config{}.dropSuperseded(context.Background(), v, "", t.Logf)
	if len(v.removed) != 0 {
		t.Errorf("a fresh install removed %v", v.removed)
	}
}

// THE REAL CLIENT IS AN INSTALLER, checked at compile time. host.go reaches it through a runtime
// type assertion (`r.(serviceInstaller)`), so a verb added to the interface with a signature the
// client does not have builds cleanly and then fails EVERY install on a real node with "guest client
// cannot install a service" -- nothing but this line, or a rig, would say so.
var _ serviceInstaller = (*guestagent.Client)(nil)
