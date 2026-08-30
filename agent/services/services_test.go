package services

import (
	"context"
	"strings"
	"testing"

	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/shared/manifest"
)

const image = "example.com/x@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func svc(name string, port int) manifest.Manifest {
	return manifest.Manifest{
		Name: name, Version: "1",
		Containers: []manifest.Container{{
			Name: "app", Image: image, Mount: "/data",
			Primary: true, Port: port, HealthPath: "/healthz",
		}},
	}
}

type noopExec struct{ runs int }

func (n *noopExec) Run(context.Context, string, ...string) ([]byte, error) { n.runs++; return nil, nil }
func (n *noopExec) WriteFile(string, []byte) error                         { return nil }
func (n *noopExec) ReadFile(string) ([]byte, error)                        { return nil, nil }

// TestAnUnknownServiceGetsNothing — the registry's default, and the property that keeps adding an
// entry from being able to change what every other service gets. A service the product has never
// heard of must render, prepare and read exactly as it did before this package existed.
func TestAnUnknownServiceGetsNothing(t *testing.T) {
	m := svc("something-else", 8080)
	if v := Volumes(m, m.Containers[0]); v != nil {
		t.Errorf("an unknown service was handed binds: %v", v)
	}
	x := &noopExec{}
	if err := Prepare(context.Background(), x, m); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if x.runs != 0 {
		t.Errorf("an unknown service ran %d command(s) on the node", x.runs)
	}
	if got := Reach(m, "home"); got != "reach it at http://briard-home.local:8080/" {
		t.Errorf("Reach = %q", got)
	}
}

// TestEachKnownServiceGetsItsOwn: the dispatch is keyed on the catalog name, and the two entries
// must not bleed into each other.
func TestEachKnownServiceGetsItsOwn(t *testing.T) {
	ha := svc(hass.Name, 8123)
	mq := svc(mosquitto.Name, mosquitto.HealthPort)

	haVols := Volumes(ha, ha.Containers[0])
	mqVols := Volumes(mq, mq.Containers[0])
	if len(haVols) == 0 || len(mqVols) == 0 {
		t.Fatalf("a known service was handed nothing: hass=%v mosquitto=%v", haVols, mqVols)
	}
	for _, v := range haVols {
		if strings.Contains(v, mosquitto.Dir) {
			t.Errorf("Home Assistant was handed mosquitto's config: %v", haVols)
		}
	}
	for _, v := range mqVols {
		if strings.Contains(v, hass.Dir) {
			t.Errorf("mosquitto was handed Home Assistant's control channel: %v", mqVols)
		}
	}
}

// TestReachNamesMQTTForTheBroker, because the manifest's port is the broker's MANAGEMENT endpoint
// and it is bound to the guest's loopback. Printing it would hand the household a dead link for a
// service that is working perfectly — which is the failure the reach line exists to prevent.
func TestReachNamesMQTTForTheBroker(t *testing.T) {
	mq := svc(mosquitto.Name, mosquitto.HealthPort)
	got := Reach(mq, "home")
	if strings.Contains(got, "http://") {
		t.Errorf("the broker was advertised as a web address: %q", got)
	}
	if !strings.Contains(got, "1883") {
		t.Errorf("the reach line does not name the MQTT port: %q", got)
	}
	if strings.Contains(got, "9883") {
		t.Errorf("the reach line names the loopback-only management port: %q", got)
	}
	// No published name means no address to promise — the same rule the HTTP form follows.
	if bare := Reach(mq, ""); strings.Contains(bare, ".local") {
		t.Errorf("a node with no published name promised a name anyway: %q", bare)
	}
}
