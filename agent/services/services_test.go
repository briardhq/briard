package services

import (
	"context"
	"os"
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
	// THE PROTOCOL, not just the port -- the assertion this test was named for and did not make.
	// [B.48a] generalised the sentence off mosquitto and the word "MQTT" went with it while every
	// check here still passed, because a port and a name both survive a protocol going missing.
	// Tier 4 caught it against the live channel: a ten-minute VM run standing in for a string
	// compare.
	if !strings.Contains(got, mosquitto.Protocol) {
		t.Errorf("the reach line does not say what a client must speak to the port: %q", got)
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

// TestOnlyTheBrokerAnnouncesItself — the registry's third exposure decision, keyed on the catalog
// name like Fronted and Reach.
//
// It is the inverse of Fronted's question rather than the same one: Fronted asks what the DOOR may
// serve, this asks what the household's other DEVICES may find. mosquitto is false for one and
// true for the other, which is the whole point -- a broker's clients are appliances that browse,
// and its image advertises nothing on its own (agent/mosquitto).
func TestOnlyTheBrokerAnnouncesItself(t *testing.T) {
	mq := svc(mosquitto.Name, mosquitto.HealthPort)
	mq.Network = manifest.NetworkPrivate
	mq.Ports = []int{mosquitto.MQTTPort}
	got := Announce(mq, "brave-elf")
	if len(got) != 1 {
		t.Fatalf("the broker announced %d record(s), want exactly one: %+v", len(got), got)
	}
	if got[0].Type != "_mqtt._tcp" {
		t.Errorf("the broker is announced under %q, which no device browses for", got[0].Type)
	}
	// THE MQTT PORT, NEVER THE MANIFEST'S. The manifest names the management endpoint the health
	// floor probes, bound inside the pod -- announcing it would send every device on the LAN to a
	// port that refuses them, for a broker that is working perfectly.
	if got[0].Port != mosquitto.MQTTPort {
		t.Errorf("the announcement carries port %d, want MQTT's %d", got[0].Port, mosquitto.MQTTPort)
	}
	if got[0].Name != "briard-brave-elf-mosquitto" {
		t.Errorf("the instance label %q is not the flock-scoped name", got[0].Name)
	}
	// The default is silence: an ordinary HTTP service is reached by the name the door serves it
	// under, and browsing types it into a category it may not belong in.
	for _, m := range []manifest.Manifest{svc(hass.Name, 8123), svc("something-else", 8080)} {
		if a := Announce(m, "brave-elf"); len(a) != 0 {
			t.Errorf("%s announced %+v; the default must be nothing", m.Name, a)
		}
	}
	// No flock name means no name to point an SRV record at, so nothing is announced -- the same
	// rule that leaves the service's own A record unpublished.
	if a := Announce(mq, ""); len(a) != 0 {
		t.Errorf("a node with no published name announced %+v", a)
	}
}

// TestInboundBindIsReadWriteAndOnlyForHomeAssistant: connect(2) on a unix socket needs write
// permission on the socket file, so a read-only bind would make the channel unreachable from
// inside the container rather than merely read-only -- a failure that looks like "the agent is
// not listening" and is not ([B.143]).
//
// And it is OPT-IN: every container that gets this socket widens what a compromised service can
// reach, so a service with no in-container restart boundary gets nothing.
func TestInboundBindIsReadWriteAndOnlyForHomeAssistant(t *testing.T) {
	ha := manifest.Manifest{Name: "home-assistant", Containers: []manifest.Container{
		{Name: "app", Primary: true}, {Name: "sidecar"},
	}}
	var primary, secondary []string
	for _, c := range ha.Containers {
		if c.Primary {
			primary = Volumes(ha, c)
		} else {
			secondary = Volumes(ha, c)
		}
	}
	var bind string
	for _, v := range primary {
		if strings.Contains(v, InboundMount) {
			bind = v
		}
	}
	if bind == "" {
		t.Fatalf("Home Assistant's primary container has no inbound socket: %v", primary)
	}
	if !strings.HasSuffix(bind, ":rw") {
		t.Errorf("bind = %q, want :rw -- connect(2) needs write permission on the socket", bind)
	}
	if !strings.HasPrefix(bind, InboundSocket()+":") {
		t.Errorf("bind = %q, want the one shared socket", bind)
	}
	// AND THE TOKEN, read-only: it is how the agent knows who called, and the container has no
	// business changing it.
	var tok string
	for _, v := range primary {
		if strings.Contains(v, InboundTokenMount) {
			tok = v
		}
	}
	if tok != InboundTokenPath("home-assistant")+":"+InboundTokenMount+":ro" {
		t.Errorf("token bind = %q, want this service own token, read-only", tok)
	}
	for _, v := range secondary {
		if strings.Contains(v, InboundMount) {
			t.Errorf("a non-primary container got the inbound socket: %q", v)
		}
	}
	// The broker has no in-container restart boundary, so it gets no channel. This is the
	// registry's default-is-nothing rule applied to the sharpest thing it hands out.
	mq := manifest.Manifest{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true}}}
	for _, v := range Volumes(mq, mq.Containers[0]) {
		if strings.Contains(v, InboundMount) {
			t.Errorf("mosquitto was given an inbound socket: %q", v)
		}
	}
	if WantsInboundAny(mq) {
		t.Error("WantsInboundAny says mosquitto wants one")
	}
	if !WantsInboundAny(ha) {
		t.Error("WantsInboundAny says Home Assistant does not want one")
	}
}

// TestPrepareMintsAFreshInboundToken: the token is what the agent resolves a caller by, so it has
// to exist before the container that will present it starts -- and Prepare is the one step that
// runs on every converge, which is exactly the set of moments the mount it lands in is remade.
//
// FRESH EVERY TIME, like the HA control token next to it: /run is tmpfs, so a value cannot
// outlive the boot that minted it and a token read out of a backup or a snapshot is worth
// nothing.
func TestPrepareMintsAFreshInboundToken(t *testing.T) {
	ha := manifest.Manifest{Name: "home-assistant", Containers: []manifest.Container{{Name: "app", Primary: true, Mount: "/config"}}}
	f := &tokenExec{files: map[string]string{}}
	// Prepare's own per-service work fails on this stub fixture, and that is deliberately not
	// what is asserted: the mint runs AHEAD of the switch on the service name, so a service whose
	// own preparation fails still leaves nothing half-identified behind. Its container never
	// starts either -- converge skips it -- so the unused token simply rotates next time.
	_ = Prepare(context.Background(), f, ha)
	first := f.files[InboundTokenPath("home-assistant")]
	if len(first) < 32 {
		t.Fatalf("token = %q, want a credential rather than an identifier", first)
	}
	if !f.ran("chmod", "0600", InboundTokenPath("home-assistant")) {
		t.Errorf("the token was left world-readable on /run: %v", f.runs)
	}
	_ = Prepare(context.Background(), f, ha)
	if f.files[InboundTokenPath("home-assistant")] == first {
		t.Error("the token did not rotate on the second converge")
	}
	// A service that gets no channel gets no token: the credential exists only where it is used.
	mq := manifest.Manifest{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true}}}
	_ = Prepare(context.Background(), f, mq)
	if _, ok := f.files[InboundTokenPath("mosquitto")]; ok {
		t.Error("mosquitto was minted an inbound token it cannot use")
	}
}

// tokenExec records writes and commands; hass.Prepare's own steps fail harmlessly on it, which is
// fine because this test is about what happens BEFORE the switch on the service name.
type tokenExec struct {
	files map[string]string
	runs  [][]string
}

func (f *tokenExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	return nil, nil
}
func (f *tokenExec) WriteFile(path string, data []byte) error {
	f.files[path] = string(data)
	return nil
}
func (f *tokenExec) ReadFile(path string) ([]byte, error) {
	if v, ok := f.files[path]; ok {
		return []byte(v), nil
	}
	return nil, os.ErrNotExist
}
func (f *tokenExec) ran(want ...string) bool {
	for _, r := range f.runs {
		if len(r) == len(want) {
			ok := true
			for i := range want {
				if r[i] != want[i] {
					ok = false
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}
