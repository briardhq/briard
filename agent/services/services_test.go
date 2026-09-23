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

// TestRestoreMarkersAreHomeAssistantsAlone ([B.143]): the registry's default is nothing, and a
// marker list that leaked to another service would have the restore path unlinking a file inside
// somebody else's data on the strength of a name Home Assistant chose.
//
// RELATIVE TO THE DATA ROOT, with the container in the path: the guest resolves them against a
// live subvolume and the restore against a staged copy, so an absolute path here would make that
// verb able to unlink anywhere on the volume.
func TestRestoreMarkersAreHomeAssistantsAlone(t *testing.T) {
	ha := svc(hass.Name, 8123)
	got := RestoreMarkers(ha)
	if len(got) != 1 || got[0] != "app/"+hass.RestoreMarker {
		t.Errorf("RestoreMarkers(home-assistant) = %v, want [app/%s]", got, hass.RestoreMarker)
	}
	for _, m := range []manifest.Manifest{svc(mosquitto.Name, 1883), svc("something-else", 8080)} {
		if v := RestoreMarkers(m); v != nil {
			t.Errorf("RestoreMarkers(%s) = %v, want nothing", m.Name, v)
		}
	}
	// A container that keeps no state writes nothing of its own, and a marker named for it would
	// point inside a directory the member does not have.
	ha.Containers = append(ha.Containers, manifest.Container{Name: "sidecar", Image: image})
	if got := RestoreMarkers(ha); len(got) != 1 {
		t.Errorf("RestoreMarkers = %v, want only the container that holds the data", got)
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
	// THE TOKEN HAS NO BIND OF ITS OWN, and that is the point of the per-service directory: it is
	// simply one of the things in it, arriving read-only with everything else briard hands this
	// service. A bind here would be a third mount buying nothing.
	for _, v := range primary {
		if strings.HasPrefix(v, InboundTokenPath("home-assistant")+":") {
			t.Errorf("the token was given its own bind (%q); it should ride the service directory", v)
		}
	}
	var dir string
	for _, v := range primary {
		if strings.HasSuffix(v, ":"+ServiceMount+":ro") {
			dir = v
		}
	}
	if dir != ServiceDir("home-assistant")+":"+ServiceMount+":ro" {
		t.Fatalf("service directory bind = %q, want this service's own directory, read-only", dir)
	}
	// And the two agree: the token's path on the node is inside the directory that is mounted, so
	// the container really does see it where InboundTokenMount says.
	if !strings.HasPrefix(InboundTokenPath("home-assistant"), ServiceDir("home-assistant")+"/") {
		t.Errorf("the token at %q is not inside the mounted directory %q", InboundTokenPath("home-assistant"), ServiceDir("home-assistant"))
	}
	if InboundTokenMount != ServiceMount+"/"+InboundTokenName {
		t.Errorf("InboundTokenMount = %q, want it inside %q", InboundTokenMount, ServiceMount)
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

// TestNoBindNestsInsideAnother: a bind whose destination sits inside another bind's destination
// makes the runtime create that destination under an already-mounted parent. When the parent is
// READ-ONLY -- which /briard is, for Home Assistant -- that is EROFS and the container never
// starts; and podman having created a root-owned directory where a file belongs poisons the path
// for every later attempt, which is the failure Prepare's own comment describes.
//
// Caught this the hard way: the inbound socket and token were first written as /briard/inbound.*,
// inside exactly that read-only mount. Asserting the PROPERTY rather than the two paths is what
// keeps the next mount out of the same hole -- the temptation to group things under /briard is
// obvious and will recur.
func TestNoBindNestsInsideAnother(t *testing.T) {
	for _, m := range []manifest.Manifest{
		{Name: "home-assistant", Containers: []manifest.Container{{Name: "app", Primary: true, Mount: "/config"}}},
		{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true, Mount: "/data"}}},
	} {
		for _, c := range m.Containers {
			var dests []string
			for _, v := range Volumes(m, c) {
				parts := strings.Split(v, ":")
				if len(parts) < 2 {
					t.Fatalf("%s: bind %q has no destination", m.Name, v)
				}
				dests = append(dests, parts[1])
			}
			// The container's own data bind counts too: it is a destination like any other.
			if c.Mount != "" {
				dests = append(dests, c.Mount)
			}
			for _, a := range dests {
				for _, b := range dests {
					if a != b && strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/") {
						t.Errorf("%s/%s: bind %q is nested inside %q; the runtime must create it under an already-mounted parent", m.Name, c.Name, b, a)
					}
				}
			}
		}
	}
}

// TestServiceDirMatchesTheRegistry: agent/hass declares its own directory as a const, because it
// cannot import this package (this one imports it). So two definitions have to agree, and the
// agreement is load-bearing in a way that is easy to miss: the agent resolves an inbound caller
// by reading the run directory and taking a DIRECTORY NAME as a service name. A directory named
// for the Go package rather than the service would resolve to a service that does not exist, and
// every call from that container would come back "unknown caller" with nothing pointing at why.
//
// The directory is named for the service, as agent/mosquitto's is ([B.143]).
func TestServiceDirMatchesTheRegistry(t *testing.T) {
	for _, c := range []struct{ name, dir string }{
		{hass.Name, hass.Dir},
		{mosquitto.Name, mosquitto.Dir},
	} {
		if want := defaultRunDir + "/" + c.name; c.dir != want {
			t.Errorf("%s's package declares %q, the registry derives %q", c.name, c.dir, want)
		}
	}
}
