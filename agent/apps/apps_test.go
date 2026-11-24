package apps

import (
	"context"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/files"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/mosquitto"
	"briard.io/agent/navidrome"
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
func (n *noopExec) Stat(p string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}
func (n *noopExec) ListDir(p string) ([]string, error) {
	return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
}

// TestAnUnknownAppGetsNothing — the registry's default, which keeps a new entry from changing
// what every other app gets: an unknown app renders, prepares and reads as if this package did
// not exist.
func TestAnUnknownAppGetsNothing(t *testing.T) {
	m := svc("something-else", 8080)
	if v := Volumes(m, m.Containers[0]); v != nil {
		t.Errorf("an unknown app was handed binds: %v", v)
	}
	x := &noopExec{}
	if err := Prepare(context.Background(), x, m, ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if x.runs != 0 {
		t.Errorf("an unknown app ran %d command(s) on the node", x.runs)
	}
	// The app's own name on the reverse proxy's :80, not the flock name and a port.
	if got := Reach(m, "home"); got != "reach it at http://briard-something-else.local/" {
		t.Errorf("Reach = %q", got)
	}
}

// TestRestoreMarkersAreHomeAssistantsAlone: the default is nothing; a marker list leaked to
// another app would have the restore path unlinking a file in its data on the strength of a name
// Home Assistant chose. Markers are relative to the data root, config directory included: the
// guest resolves them against a live subvolume and the restore against a staged copy, so an
// absolute path would let that verb unlink anywhere on the volume.
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

// TestOnlyHomeAssistantGetsRawSockets: its primary container gets NET_RAW and NET_ADMIN and nothing
// more, and no other app or container inherits a capability from the name the registry keys on.
func TestOnlyHomeAssistantGetsRawSockets(t *testing.T) {
	ha := svc(hass.Name, 8123)
	if cp := Capabilities(ha, ha.Containers[0]); cp != "NET_RAW NET_ADMIN" {
		t.Errorf("Capabilities(home-assistant) = %q, want NET_RAW NET_ADMIN", cp)
	}
	if cp := Capabilities(ha, manifest.Container{Name: "sidecar", Image: image}); cp != "" {
		t.Errorf("a non-primary container of Home Assistant gets %q", cp)
	}
	for _, m := range []manifest.Manifest{svc(files.Name, 80), svc(mealie.Name, 9000), svc(mosquitto.Name, mosquitto.HealthPort), svc("something-else", 8080)} {
		if cp := Capabilities(m, m.Containers[0]); cp != "" {
			t.Errorf("Capabilities(%s) = %q, want none", m.Name, cp)
		}
	}
}

// TestReachNamesMQTTForTheBroker, because the manifest's port is the broker's management endpoint,
// bound inside the guest, off the LAN: printing it would hand the household a dead link for a
// working app, the failure the reach line exists to prevent.
func TestReachNamesMQTTForTheBroker(t *testing.T) {
	// The port comes from the manifest, not a constant here: what a household reaches is what the
	// catalog publishes, so a change there moves this sentence with it.
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
	// The protocol, not just the port -- the assertion this test is named for. A port and a name
	// both survive a protocol going missing, so without it a sentence generalised off mosquitto
	// can drop "MQTT" while every other check passes.
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

// TestOnlyTheBrokerIsNotFronted — the reverse proxy's exposure decision, keyed on the catalog
// name, and a security property: mosquitto's manifest port is its management API, which
// mosquitto.conf deliberately binds where only the guest reaches it (the pod's own namespace, or
// 127.0.0.1 under host networking). The reverse proxy runs in that same guest, so fronting it
// would republish a guest-only endpoint on the LAN through a mechanism that never mentions the
// bind.
func TestOnlyTheBrokerIsNotFronted(t *testing.T) {
	if Fronted(svc(mosquitto.Name, mosquitto.HealthPort)) {
		t.Error("the broker is fronted; its loopback-bound management API would reach the LAN")
	}
	// The default is the reverse proxy: an ordinary app's primary port is what a household opens.
	for _, m := range []manifest.Manifest{svc(hass.Name, 8123), svc("something-else", 8080)} {
		if !Fronted(m) {
			t.Errorf("%s is not fronted; the default must be that an app is reachable by name", m.Name)
		}
	}
}

// TestOnlyTheBrokerAnnouncesItself — the registry's third exposure decision, keyed on the catalog
// name like Fronted and Reach, and the inverse of Fronted's question: Fronted asks what the
// reverse proxy may serve, this asks what the household's other devices may find. mosquitto is
// false for one and true for the other, which is the point: a broker's clients are appliances
// that browse, and its image advertises nothing on its own (agent/mosquitto).
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
	// The MQTT port, never the manifest's: that is the management endpoint the health floor probes,
	// bound inside the pod, and announcing it would send every device on the LAN to a port that
	// refuses them, for a working broker.
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
// permission on the socket file, so a read-only bind makes the channel unreachable, not merely
// read-only -- a failure that looks like "the agent is not listening" and is not. The bind is
// opt-in: every container that gets it widens what a compromised app can reach, so an app with
// no in-container restart boundary gets nothing.
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
	// The token has no bind of its own, which is the point of the per-app directory: it arrives in
	// it, read-only, with everything else briard hands this app. A bind would be a third mount
	// buying nothing.
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
	// The token's node path is inside the mounted directory, so the container sees it where
	// InboundTokenMount says.
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
	// The broker has no in-container restart boundary, so no channel: the default-is-nothing rule
	// applied to the sharpest thing the registry hands out.
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

// TestPrepareMintsAFreshInboundToken: the agent resolves a caller by the token, so it must exist
// before the container presenting it starts, and Prepare is the one step that runs on every
// converge -- exactly the moments its mount is remade. Fresh every converge, where the HA control
// token beside it is kept for the boot; both live on tmpfs /run, so neither outlives its boot and
// a token read out of a backup or snapshot is worth nothing.
func TestPrepareMintsAFreshInboundToken(t *testing.T) {
	ha := manifest.Manifest{Name: "home-assistant", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/config"}}}}}
	f := &tokenExec{files: map[string]string{}}
	// Prepare's per-app work fails on this stub, deliberately unasserted: the mint runs ahead of
	// the switch on the app name, so a failed preparation leaves nothing half-identified. Converge
	// never starts that container either, so the unused token simply rotates next time.
	_ = Prepare(context.Background(), f, ha, "")
	first := f.files[InboundTokenPath("home-assistant")]
	if len(first) < 32 {
		t.Fatalf("token = %q, want a credential rather than an identifier", first)
	}
	if !f.ran("chmod", "0600", InboundTokenPath("home-assistant")) {
		t.Errorf("the token was left world-readable on /run: %v", f.runs)
	}
	_ = Prepare(context.Background(), f, ha, "")
	if f.files[InboundTokenPath("home-assistant")] == first {
		t.Error("the token did not rotate on the second converge")
	}
	// An app that gets no channel gets no token: the credential exists only where it is used.
	mq := manifest.Manifest{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true}}}
	_ = Prepare(context.Background(), f, mq, "")
	if _, ok := f.files[InboundTokenPath("mosquitto")]; ok {
		t.Error("mosquitto was minted an inbound token it cannot use")
	}
}

// tokenExec records writes and commands; hass.Prepare's own steps fail harmlessly on it, which is
// fine: the test is about what happens before the switch on the app name.
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
func (f *tokenExec) Stat(p string) (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}
func (f *tokenExec) ListDir(p string) ([]string, error) {
	return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
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

// TestNoBindNestsInsideAnother: a bind nested in another bind's destination makes the runtime
// create it under an already-mounted parent. Under a read-only parent (/briard, for Home
// Assistant) that is EROFS and the container never starts, and podman's root-owned directory where
// a file belongs poisons the path for every later attempt (the failure Prepare's comment
// describes). /briard/inbound.* for the socket and token would sit in exactly that mount; asserting
// the property rather than two paths keeps the next mount out too -- the temptation to group
// things under /briard will recur.
func TestNoBindNestsInsideAnother(t *testing.T) {
	for _, m := range []manifest.Manifest{
		{Name: "home-assistant", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/config"}}}}},
		{Name: "mosquitto", Containers: []manifest.Container{{Name: "broker", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "broker", Path: "/data"}}}}},
		{Name: "files", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierCache, Dir: "state", Path: "/home/filebrowser/data"}}}}},
		{Name: "navidrome", Containers: []manifest.Container{{Name: "app", Primary: true, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "data", Path: "/data"}, {Tier: manifest.TierFiles, Dir: "music", Path: "/music"}, {Tier: manifest.TierCache, Dir: "cache", Path: "/cache"}}}}},
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
			// The container's own mounts are destinations too; only they may nest, in each other,
			// because the renderer orders them parent first.
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

// TestAppDirMatchesTheRegistry: agent/hass declares its directory as a const because it cannot
// import this package (this one imports it), so two definitions must agree -- and it matters: the
// agent resolves an inbound caller by taking a directory name in the run directory as an app name.
// A directory named for the Go package rather than the app resolves to no app, and every call
// from that container comes back "unknown caller" with nothing pointing at why. Each directory is
// named for the app, as agent/mosquitto's is.
func TestAppDirMatchesTheRegistry(t *testing.T) {
	for _, c := range []struct{ name, dir string }{
		{hass.Name, hass.Dir},
		{mosquitto.Name, mosquitto.Dir},
		{files.Name, files.Dir},
		{navidrome.Name, navidrome.Dir},
	} {
		if want := defaultRunDir + "/" + c.name; c.dir != want {
			t.Errorf("%s's package declares %q, the registry derives %q", c.name, c.dir, want)
		}
	}
}

// TestStartingIsSettledAsUnhealthy: once HealthGate has passed, an app still
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

// Navidrome is gated by address alone, with no secret, its Subsonic API open; its library is the
// one files-tier folder the household writes; and it gets its own directory for its settings.
// No other app gets any of it.
func TestNavidromeIsGatedByAddressAndItsLibraryIsTheHouseholds(t *testing.T) {
	m := svc(navidrome.Name, 4533)
	if !GateByAddress(m) || Gate(m) != "" || GateHeader(m) != "" {
		t.Errorf("Navidrome's gate: by address %v, secret %q, header %q; want by address alone", GateByAddress(m), Gate(m), GateHeader(m))
	}
	if o := Open(m); !slices.Equal(o, []string{"/rest/"}) {
		t.Errorf("Open(navidrome) = %v", o)
	}
	if !HouseholdWrites(m, navidrome.Music) || HouseholdWrites(m, "data") {
		t.Error("Navidrome's household-written folder is not its library alone")
	}
	if v := Volumes(m, m.Containers[0]); !slices.Contains(v, AppDir(navidrome.Name)+":"+AppMount+":ro") {
		t.Errorf("Navidrome lacks its own directory: %v", v)
	}
	if u := User(m, m.Containers[0]); u != "" {
		t.Errorf("User(navidrome) = %q, want the image's own", u)
	}
	for _, o := range []manifest.Manifest{svc(files.Name, 80), svc(mealie.Name, 9000), svc(hass.Name, 8123), svc("something-else", 8080)} {
		if GateByAddress(o) || Open(o) != nil || HouseholdWrites(o, navidrome.Music) {
			t.Errorf("%s was given Navidrome's gate, open paths or library", o.Name)
		}
	}
}
