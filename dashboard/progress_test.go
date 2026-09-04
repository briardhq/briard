package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// LIVE PULL PROGRESS ([V3b.31j]): the total from the host's pull record, finished layers from
// podman's store (only those created since the pull started), the layer in flight from the
// unit's private tmp -- and no bar at all without a record.
func TestPullProgressAddsFinishedLayersAndTheOneInFlight(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	must(t, os.Remove(filepath.Join(r.dir, "routes.json")))
	port := newFakePort()
	r.app.port = port
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "overlay-layers"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	pullDir := t.TempDir()
	r.app.pulls = pullPaths{records: pullDir, layers: filepath.Join(root, "overlay-layers", "layers.json"), tmp: filepath.Join(root, "tmp")}
	// The record the host writes: the pull started a minute ago, 600 MB to download.
	started := time.Now().Add(-time.Minute).UTC()
	rec, _ := json.Marshal(dashboard.Pull{Service: "home-assistant", Size: 600e6, InstalledSize: 2500e6, Started: started})
	must(t, os.WriteFile(filepath.Join(pullDir, "pull-home-assistant.json"), rec, 0o600))
	// podman's store: two layers from an OLD pull (before the start), two finished by this one.
	layers := []map[string]any{
		{"id": "a", "compressed-size": 100e6, "created": started.Add(-time.Hour).Format(time.RFC3339Nano)},
		{"id": "b", "compressed-size": 50e6, "created": started.Add(-time.Hour).Format(time.RFC3339Nano)},
		{"id": "c", "compressed-size": 120e6, "created": started.Add(10 * time.Second).Format(time.RFC3339Nano)},
		{"id": "d", "compressed-size": 80e6, "created": started.Add(20 * time.Second).Format(time.RFC3339Nano)},
	}
	raw, _ := json.Marshal(layers)
	must(t, os.WriteFile(r.app.pulls.layers, raw, 0o644))
	// The layer in flight: 40 MB under the pull unit's private tmp, plus a stranger's tmp that
	// must not count.
	inflight := filepath.Join(root, "tmp", "systemd-private-boot1-briard-home-assistant-app-image.service-XyZ", "tmp")
	must(t, os.MkdirAll(inflight, 0o755))
	must(t, os.WriteFile(filepath.Join(inflight, "storage123"), make([]byte, 40e6), 0o600))
	other := filepath.Join(root, "tmp", "systemd-private-boot1-briard-mosquitto-broker-image.service-Abc", "tmp")
	must(t, os.MkdirAll(other, 0o755))
	must(t, os.WriteFile(filepath.Join(other, "storage9"), make([]byte, 5e6), 0o600))

	r.do("POST", "/install/home-assistant", c, nil)
	<-port.waiting
	body := r.page(c)
	// 120 + 80 finished + 40 in flight = 240 of 600 -> 40 %.
	if !strings.Contains(body, "240 MB of 600 MB downloaded (40%)") || !strings.Contains(body, `<progress max="100" value="40">`) {
		t.Errorf("the Installing card does not show the pull's progress: %s", body)
	}
	// The in-flight layer completes: tmp empties, the store gains it, the number does not fall.
	must(t, os.Remove(filepath.Join(inflight, "storage123")))
	layers = append(layers, map[string]any{"id": "e", "compressed-size": 40e6, "created": started.Add(30 * time.Second).Format(time.RFC3339Nano)})
	raw, _ = json.Marshal(layers)
	must(t, os.WriteFile(r.app.pulls.layers, raw, 0o644))
	if body = r.page(c); !strings.Contains(body, "(40%)") {
		t.Errorf("progress fell when a layer moved from tmp to the store: %s", body)
	}
	// Never 100 from bytes alone: the last percent is the routes table.
	layers = append(layers, map[string]any{"id": "f", "compressed-size": 400e6, "created": started.Add(40 * time.Second).Format(time.RFC3339Nano)})
	raw, _ = json.Marshal(layers)
	must(t, os.WriteFile(r.app.pulls.layers, raw, 0o644))
	if body = r.page(c); !strings.Contains(body, "(99%)") {
		t.Errorf("bytes alone reached 100%%: %s", body)
	}
	// No record (an older host): the card says installing, with no number and no bar.
	must(t, os.Remove(filepath.Join(pullDir, "pull-home-assistant.json")))
	body = r.page(c)
	if !strings.Contains(body, "Installing") || strings.Contains(body, "<progress") || strings.Contains(body, "downloaded") {
		t.Errorf("without a pull record the card still draws a bar: %s", body)
	}
	port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "x"}
}

func TestGB(t *testing.T) {
	for n, want := range map[int64]string{621628919: "622 MB", 2486168064: "2.49 GB", 9981691: "10 MB", 4096: "4 kB"} {
		if got := gb(n); got != want {
			t.Errorf("gb(%d) = %q, want %q", n, got, want)
		}
	}
}
