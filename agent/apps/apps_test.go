package apps

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/files"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/mosquitto"
	"briard.io/shared/manifest"
)

const image = "example.com/x@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func svc(name string, port int) manifest.Manifest {
	return manifest.Manifest{
		Name: name, Version: "1",
		Containers: []manifest.Container{{
			Name: "app", Image: image, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/data"}},
			Primary: true, Port: port, HealthPath: "/healthz",
		}},
	}
}

type noopExec struct{ runs int }

func (n *noopExec) Run(context.Context, string, ...string) ([]byte, error) { n.runs++; return nil, nil }
func (n *noopExec) WriteFile(string, []byte) error                         { return nil }
func (n *noopExec) ReadFile(string) ([]byte, error)                        { return nil, nil }

// TestAnUnknownAppGetsNothing — the registry's default, and the property that keeps adding an
// entry from being able to change what every other app gets. An app the product has never
// heard of must render, prepare and read exactly as it did before this package existed.
func TestAnUnknownAppGetsNothing(t *testing.T) {
	m := svc("something-else", 8080)
	if v := Volumes(m, m.Containers[0]); v != nil {
		t.Errorf("an unknown app was handed binds: %v", v)
	}
	x := &noopExec{}
	if err := Prepare(context.Background(), x, m); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if x.runs != 0 {
		t.Errorf("an unknown app ran %d command(s) on the node", x.runs)
	}
	// The app's OWN name on the reverse proxy's :80, not the flock name and a port: the
	// port in this sentence was the shape of a node that could only be reached around the reverse
	// proxy.
	if got := Reach(m, "home"); got != "reach it at http://briard-something-else.local/" {
		t.Errorf("Reach = %q", got)
	}
}

// TestRestoreMarkersAreHomeAssistantsAlone: the registry's default is nothing, and a
// marker list that leaked to another app would have the restore path unlinking a file inside
// somebody else's data on the strength of a name Home Assistant chose.
//
// RELATIVE TO THE DATA ROOT, with the config directory in the path: the guest resolves them against a
// live subvolume and the restore against a staged copy, so an absolute path here would make that
// verb able to unlink anywhere on the volume.
func TestRestoreMarkersAreHomeAssistantsAlone(t *testing.T) {
	ha := svc(hass.Name, 8123)
	ha.Containers[0].Mounts[0].Path = "/config"
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
		t.Errorf("RestoreMarkers = %v, want only the config directory's", got)
	}
}

// TestEachKnownAppGetsItsOwn: the dispatch is keyed on the catalog name, and the two entries
// must not bleed into each other.
func TestEachKnownAppGetsItsOwn(t *testing.T) {
	ha := svc(hass.Name, 8123)
	mq := svc(mosquitto.Name, mosquitto.HealthPort)

	haVols := Volumes(ha, ha.Containers[0])
	mqVols := Volumes(mq, mq.Containers[0])
	if len(haVols) == 0 || len(mqVols) == 0 {
		t.Fatalf("a known app was handed nothing: hass=%v mosquitto=%v", haVols, mqVols)
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

// Mealie is gated by header: its settings bound read-only where it reads them, the secret in them,
// and the secret's header named; it runs as its image's own user.
func TestMealieIsGatedByHeaderWithItsSettingsBound(t *testing.T) {
	m := svc(mealie.Name, 9000)
	if v := Volumes(m, m.Containers[0]); !slices.Contains(v, mealie.SettingsDir+":"+mealie.SettingsMount+":ro") {
		t.Errorf("Mealie lacks its settings: %v", v)
	}
	if g, h := Gate(m), GateHeader(m); g != mealie.KeyPath || h != mealie.SecretHeader {
		t.Errorf("Gate(mealie) = %q %q, want %q %q", g, h, mealie.KeyPath, mealie.SecretHeader)
	}
	if u := User(m, m.Containers[0]); u != "" {
		t.Errorf("User(mealie) = %q, want the image's own", u)
	}
}

// TestOnlyFilesSeesTheViewAsRootBehindTheGate: the one app that reads other apps' areas gets the
// view, root and the gate, and no other app gets any of the three -- a name the registry does not
// key on must not inherit the household's data.
func TestOnlyFilesSeesTheViewAsRootBehindTheGate(t *testing.T) {
	fl := svc(files.Name, 80)
	vols := Volumes(fl, fl.Containers[0])
	if !slices.Contains(vols, files.ViewDir+":"+files.ViewMount+":rslave") {
		t.Errorf("Files lacks the view with slave propagation: %v", vols)
	}
	if !slices.Contains(vols, AppDir(files.Name)+":"+AppMount+":ro") {
		t.Errorf("Files lacks its own directory, read-only: %v", vols)
	}
	if u := User(fl, fl.Containers[0]); u != "0" {
		t.Errorf("User(files) = %q, want 0", u)
	}
	if g := Gate(fl); g != files.KeyPath {
		t.Errorf("Gate(files) = %q, want %q", g, files.KeyPath)
	}
	for _, m := range []manifest.Manifest{svc(hass.Name, 8123), svc(mosquitto.Name, mosquitto.HealthPort), svc("something-else", 8080)} {
		for _, v := range Volumes(m, m.Containers[0]) {
			if strings.Contains(v, files.ViewDir) {
				t.Errorf("%s was handed the view: %v", m.Name, v)
			}
		}
		if u := User(m, m.Containers[0]); u != "" {
			t.Errorf("User(%s) = %q, want the image's own", m.Name, u)
		}
		if g, h := Gate(m), GateHeader(m); g != "" || h != "" {
			t.Errorf("Gate(%s) = %q %q, want none", m.Name, g, h)
		}
	}
	if h := GateHeader(fl); h != "" {
		t.Errorf("GateHeader(files) = %q, want the signed token", h)
	}
	// A second container of Files is not the browser, and gets none of the browser's binds.
	side := manifest.Container{Name: "sidecar", Image: image}
	if v := Volumes(fl, side); v != nil {
		t.Errorf("a non-primary container of Files was handed %v", v)
	}
	if u := User(fl, side); u != "" {
		t.Errorf("a non-primary container of Files runs as %q", u)
	}
}

// TestReachNamesMQTTForTheBroker, because the manifest's port is the broker's MANAGEMENT endpoint
// and it is bound to the guest's loopback. Printing it would hand the household a dead link for an
// app that is working perfectly — which is the failure the reach line exists to prevent.
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
	// A later change generalised the sentence off mosquitto and the word "MQTT" went with it while every
	// check here still passed, because a port and a name both survive a protocol going missing.
	// Tier 4 caught it against the live channel: a ten-minute VM run standing in for a string
	// compare.
	if !strings.Contains(got, mosquitto.Protocol) {
		t.Errorf("the reach line does not say what a client must speak to the port: %q", got)
	}
	if strings.Contains(got, "9883") {
		t.Errorf("the reach line names the pod-internal management port: %q", got)
	}
	if !strings.Contains(got, "briard-mosquitto.local") {
		t.Errorf("the reach line does not name the app: %q", got)
	}
	// No published name means no address to promise — the same rule the HTTP form follows.
	if bare := Reach(mq, ""); strings.Contains(bare, ".local") {
		t.Errorf("a node with no published name promised a name anyway: %q", bare)
	}
}

// TestOnlyTheBrokerIsNotFronted — the reverse proxy's exposure decision, keyed on the catalog name
// like everything else here. It is a security property: mosquitto's manifest port is its
// management API, which mosquitto.conf binds to 127.0.0.1 deliberately, and the reverse proxy runs
// inside that same guest — so fronting it would republish a loopback-only endpoint on the LAN
// through a mechanism that never mentions the bind.
func TestOnlyTheBrokerIsNotFronted(t *testing.T) {
	if Fronted(svc(mosquitto.Name, mosquitto.HealthPort)) {
		t.Error("the broker is fronted; its loopback-bound management API would reach the LAN")
	}
	// The default is the reverse proxy: an ordinary app's primary port IS what a household opens.
	for _, m := range []manifest.Manifest{svc(hass.Name, 8123), svc("something-else", 8080)} {
		if !Fronted(m) {
			t.Errorf("%s is not fronted; the default must be that an app is reachable by name", m.Name)
		}
	}
}

// TestOnlyTheBrokerAnnouncesItself — the registry's third exposure decision, keyed on the catalog
// name like Fronted and Reach.
//
// It is the inverse of Fronted's question rather than the same one: Fronted asks what the REVERSE
// PROXY may serve, this asks what the household's other DEVICES may find. mosquitto is false for
// one and true for the other, which is the whole point -- a broker's clients are appliances that
// browse, and its image advertises nothing on its own (agent/mosquitto).
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
	// The default is silence: an ordinary HTTP app is reached by the name the reverse proxy serves
	// it under, and browsing types it into a category it may not belong in.
	for _, m := range []manifest.Manifest{svc(hass.Name, 8123), svc("something-else", 8080)} {
		if a := Announce(m, "brave-elf"); len(a) != 0 {
			t.Errorf("%s announced %+v; the default must be nothing", m.Name, a)
		}
	}
	// No flock name means no name to point an SRV record at, so nothing is announced -- the same
	// rule that leaves the app's own A record unpublished.
	if a := Announce(mq, ""); len(a) != 0 {
		t.Errorf("a node with no published name announced %+v", a)
	}
}

// TestInboundBindIsReadWriteAndOnlyForHomeAssistant: connect(2) on a unix socket needs write
// permission on the socket file, so a read-only bind would make the channel unreachable from
// inside the container rather than merely read-only -- a failure that looks like "the agent is
// not listening" and is not.
//
// And it is OPT-IN: every container that gets this socket widens what a compromised app can
// reach, so an app with no in-container restart boundary gets nothing.
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
	// THE TOKEN HAS NO BIND OF ITS OWN, and that is the point of the per-app directory: it is
	// simply one of the things in it, arriving read-only with everything else briard hands this
	// app. A bind here would be a third mount buying nothing.
	for _, v := range primary {
		if strings.HasPrefix(v, InboundTokenPath("home-assistant")+":") {
			t.Errorf("the token was given its own bind (%q); it should ride the app directory", v)
		}
	}
	var dir string
	for _, v := range primary {
		if strings.HasSuffix(v, ":"+AppMount+":ro") {
			dir = v
		}
	}
	if dir != AppDir("home-assistant")+":"+AppMount+":ro" {
		t.Fatalf("app directory bind = %q, want this app's own directory, read-only", dir)
	}
	// And the two agree: the token's path on the node is inside the directory that is mounted, so
	// the container really does see it where InboundTokenMount says.
	if !strings.HasPrefix(InboundTokenPath("home-assistant"), AppDir("home-assistant")+"/") {
		t.Errorf("the token at %q is not inside the mounted directory %q", InboundTokenPath("home-assistant"), AppDir("home-assistant"))
	}
	if InboundTokenMount != AppMount+"/"+InboundTokenName {
		t.Errorf("InboundTokenMount = %q, want it inside %q", InboundTokenMount, AppMount)
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
	ha := manifest.Manifest{Name: "home-assistant", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/config"}}}}}
	f := &tokenExec{files: map[string]string{}}
	// Prepare's own per-app work fails on this stub fixture, and that is deliberately not
	// what is asserted: the mint runs AHEAD of the switch on the app name, so an app whose
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
	// An app that gets no channel gets no token: the credential exists only where it is used.
	mq := manifest.Manifest{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true}}}
	_ = Prepare(context.Background(), f, mq)
	if _, ok := f.files[InboundTokenPath("mosquitto")]; ok {
		t.Error("mosquitto was minted an inbound token it cannot use")
	}
}

// tokenExec records writes and commands; hass.Prepare's own steps fail harmlessly on it, which is
// fine because this test is about what happens BEFORE the switch on the app name.
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
		{Name: "home-assistant", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/config"}}}}},
		{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "broker", Path: "/data"}}}}},
		{Name: "files", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierCache, Dir: "state", Path: "/home/filebrowser/data"}}}}},
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
			// The container's own mounts count too: they are destinations like any other. Only
			// two of them may nest, in each other, because the renderer orders them parent first.
			product := len(dests)
			for _, mt := range c.Mounts {
				dests = append(dests, mt.Path)
			}
			for i, a := range dests {
				for j, b := range dests {
					if i >= product && j >= product {
						continue
					}
					if a != b && strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/") {
						t.Errorf("%s/%s: bind %q is nested inside %q; the runtime must create it under an already-mounted parent", m.Name, c.Name, b, a)
					}
				}
			}
		}
	}
}

// TestAppDirMatchesTheRegistry: agent/hass declares its own directory as a const, because it
// cannot import this package (this one imports it). So two definitions have to agree, and the
// agreement is load-bearing in a way that is easy to miss: the agent resolves an inbound caller
// by reading the run directory and taking a DIRECTORY NAME as an app name. A directory named
// for the Go package rather than the app would resolve to an app that does not exist, and
// every call from that container would come back "unknown caller" with nothing pointing at why.
//
// The directory is named for the app, as agent/mosquitto's is.
func TestAppDirMatchesTheRegistry(t *testing.T) {
	for _, c := range []struct{ name, dir string }{
		{hass.Name, hass.Dir},
		{mosquitto.Name, mosquitto.Dir},
		{files.Name, files.Dir},
	} {
		if want := defaultRunDir + "/" + c.name; c.dir != want {
			t.Errorf("%s's package declares %q, the registry derives %q", c.name, c.dir, want)
		}
	}
}

// TestStartingIsSettledAsUnhealthy (owner, 2026-09-27): once HealthGate has passed, an app still
// starting is not working -- the gate reverts it and the history records it so -- while unknown
// stays unknown, and never reverts.
func TestStartingIsSettledAsUnhealthy(t *testing.T) {
	for h, want := range map[Health]Health{
		Starting: Unhealthy, Unhealthy: Unhealthy, Healthy: Healthy, HealthUnknown: HealthUnknown,
	} {
		if got := h.Settled(); got != want {
			t.Errorf("%q.Settled() = %q, want %q", h, got, want)
		}
	}
}
