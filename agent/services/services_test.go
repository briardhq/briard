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
	// The service's OWN name on the front door's :80 ([B.48]), not the flock name and a port: the
	// port in this sentence was the shape of a node that could only be reached around its door.
	if got := Reach(m, "home"); got != "reach it at http://briard-home-something-else.local/" {
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
	// The port comes from the MANIFEST now, not from a constant in this package: what a household
	// reaches is what the catalog publishes, so a change there moves this sentence with it.
	mq := svc(mosquitto.Name, mosquitto.HealthPort)
	mq.Network = manifest.NetworkPrivate
	mq.Ports = []int{mosquitto.MQTTPort}
	got := Reach(mq, "home")
	if strings.Contains(got, "http://") {
		t.Errorf("the broker was advertised as a web address: %q", got)
	}
	if !strings.Contains(got, "1883") {
		t.Errorf("the reach line does not name the MQTT port: %q", got)
	}
	if strings.Contains(got, "9883") {
		t.Errorf("the reach line names the pod-internal management port: %q", got)
	}
	if !strings.Contains(got, "briard-home-mosquitto.local") {
		t.Errorf("the reach line does not name the service: %q", got)
	}
	// No published name means no address to promise — the same rule the HTTP form follows.
	if bare := Reach(mq, ""); strings.Contains(bare, ".local") {
		t.Errorf("a node with no published name promised a name anyway: %q", bare)
	}
}

// TestOnlyTheBrokerIsNotFronted — the front door's exposure decision, keyed on the catalog name
// like everything else here ([B.48]). It is a security property: mosquitto's manifest port is its
// management API, which mosquitto.conf binds to 127.0.0.1 deliberately, and the door runs inside
// that same guest — so fronting it would republish a loopback-only endpoint on the LAN through a
// mechanism that never mentions the bind.
func TestOnlyTheBrokerIsNotFronted(t *testing.T) {
	if Fronted(svc(mosquitto.Name, mosquitto.HealthPort)) {
		t.Error("the broker is fronted; its loopback-bound management API would reach the LAN")
	}
	// The default is the front door: an ordinary service's primary port IS what a household opens.
	for _, m := range []manifest.Manifest{svc(hass.Name, 8123), svc("something-else", 8080)} {
		if !Fronted(m) {
			t.Errorf("%s is not fronted; the default must be that a service is reachable by name", m.Name)
		}
	}
}
