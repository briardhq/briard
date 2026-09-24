// Package services is the per-service registry: which catalogued services the product knows
// something about beyond their manifest, and what it knows.
//
// WHY A REGISTRY. A manifest describes what a service IS in terms any node can execute without
// judgement, and its schema withholds host binds so that a catalog entry cannot reach the host.
// Some services still need something the schema cannot say -- Home Assistant needs a control
// channel minted into its container, mosquitto needs a config file in front of an image that
// takes no configuration from the environment. That knowledge belongs in the PRODUCT, keyed on
// the catalog name: a curated catalog is exactly the licence to hold it, and the same call
// [V3b.29] made for readiness assessors (agent/host/readiness.go), for the same reason.
//
// THE DEFAULT IS ALWAYS NOTHING. An unknown name gets no binds, no preparation and the plain
// HTTP reach line -- precisely what every service got before any of this existed. Adding an
// entry here is how a service gets more; nothing here can take anything away.
//
// It is deliberately a switch and not a plugin system. Two entries is not a framework, the set
// is small by design, and a table keyed on strings that anyone can extend is the shape that
// turns a curated catalog into an expression language.
package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

// Executor is the guest-side command runner the preparation steps need. Its method set matches
// what each service package declares for itself, so the value passes straight through.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
}

// THE NODE-SIDE LAYOUT ([B.143]): one directory per service, one socket for the agent.
//
// ⚠️ THE INBOUND CHANNEL STARTED AS A SOCKET PER SERVICE, and the swap is worth recording because
// the first version was a reasonable-looking mistake. Per-service sockets made a caller's identity
// a property of the transport -- structurally unforgeable, which is attractive when the caller is
// a workload. But it also made the listener set a function of the SERVICE LIST, and the only thing
// that knows that list is converge, which runs in two different processes (the host's verb and
// drbd-reactor's one-shot). That forced systemd to own the binds, which forced template units, a
// converge step to start instances, socket activation, fd inheritance and an idle-exit handler --
// and left the logic in a short-lived process while everything it reasons about lives in the
// long-running agent. A great deal of machinery downstream of one decision.
//
// A token buys the same property for a fraction of it: minted per service at converge, written
// where only that service's container can read it, resolved by the agent at request time -- which
// also means the agent needs no advance knowledge of the service list at all.
//
// It lives HERE rather than in agent/hass because it is not one service's knowledge, and not in
// agent/guestagent because that package imports this one and the renderer needs the paths.
//
// ONE DIRECTORY PER SERVICE, NAMED FOR IT, mounted read-only as /briard inside that service's
// container and holding everything the product hands that service. The token is simply one of
// the things in it, which is why it costs no mount of its own — the same way Home Assistant's
// control token, its planted integration and its s6 wrapper already ride that directory.
//
// The convention was already half here: agent/mosquitto's dir is /run/briard/mosquitto, and only
// agent/hass's was named for its Go package rather than its service. Regularising it is what
// makes "the DIRECTORY name is the mapping" true, which is what lets the agent resolve a caller
// by reading /run/briard rather than keeping a table.
const (
	defaultRunDir = "/run/briard"
	// ServiceMount is where a service's own directory appears inside its container.
	ServiceMount = "/briard"
	// InboundTokenName is that directory's token, so InboundTokenMount is simply inside it.
	InboundTokenName  = "inbound.token"
	InboundTokenMount = ServiceMount + "/" + InboundTokenName
	// InboundMount is the agent's socket inside the container.
	//
	// ⚠️ A SIBLING OF /briard, NEVER A CHILD OF IT, and the difference is whether the container
	// starts at all. The socket must be READ-WRITE (connect(2) needs write permission) while
	// /briard is read-only, and a bind whose destination sits inside another bind's destination
	// makes the runtime create that destination under an already-mounted parent: EROFS here, the
	// container never starts, and podman has left a root-owned directory where a socket belongs
	// — which poisons the path for every later attempt, the exact failure Prepare's own comment
	// describes. It WAS written as /briard/inbound.sock; TestNoBindNestsInsideAnother is what
	// keeps the class out rather than this comment.
	InboundMount = "/briard-agent.sock"
)

// RunDir is overridable for tests exactly as agent/guestagent's unitDir is, and for the same
// reason: every path here is absolute on a real guest, so a test that could not move them could
// only ever assert a refusal.
func RunDir() string {
	if d := os.Getenv("BRIARD_RUN_DIR"); d != "" {
		return d
	}
	return defaultRunDir
}

// ServiceDir is one service's own directory on the node — what ServiceMount shows it.
func ServiceDir(service string) string { return RunDir() + "/" + service }

// InboundSocket is the one listener, owned by the long-running guest agent. Beside the service
// directories rather than inside any of them: it is the agent's, shared by every caller.
func InboundSocket() string { return RunDir() + "/agent.sock" }

// InboundTokenPath is where one service's token lives on the node.
func InboundTokenPath(service string) string { return ServiceDir(service) + "/" + InboundTokenName }

// WantsInbound reports whether a service's container gets the inbound channel.
//
// OPT-IN, PER SERVICE, and narrow on purpose: the socket is a channel from a workload to the
// agent, so every container that gets one widens what a compromised service can reach. Home
// Assistant has it because it restarts itself far more often than its container does and nothing
// outside can see those restarts. A service with no such boundary needs none, and the generic
// container-start hook already covers it.
func WantsInbound(m manifest.Manifest, c manifest.Container) bool {
	return m.Name == hass.Name && c.Primary
}

// Volumes returns the host binds one container of one service needs beyond its own data.
//
// Called by the RENDERER (agent/quadlet), which stays a pure function of the manifest: the same
// manifest renders the same units on every node, and these binds are a property of the service's
// name rather than of anything a publisher wrote.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	var out []string
	switch m.Name {
	case hass.Name:
		out = hass.Volumes(m, c)
	case mosquitto.Name:
		out = mosquitto.Volumes(m, c)
	}
	// THE SERVICE'S OWN DIRECTORY, read-only, and it is the product's general shape rather than
	// one service's arrangement -- which is why it lives here and not in agent/hass, where the
	// same mount used to be spelled out. Everything briard hands a service goes in it: Home
	// Assistant's control token, its planted integration, its s6 wrapper, and the inbound token
	// for any service that has one. A service needing none of that gets no directory and no
	// mount.
	if NeedsServiceDir(m, c) {
		out = append(out, ServiceDir(m.Name)+":"+ServiceMount+":ro")
	}
	if WantsInbound(m, c) {
		// ⚠️ THE SOCKET BIND IS READ-WRITE, and it has to be: connect(2) needs write permission
		// on the socket file, so a read-only bind makes the channel unreachable from inside the
		// container rather than merely read-only. Its token needs no bind at all -- it is inside
		// the directory above.
		//
		// THE FILE, NEVER ITS DIRECTORY. A directory mounted rw would let the container unlink
		// or replace the socket, and this socket is shared by every participating service -- so
		// that would be one workload taking the channel away from the others.
		out = append(out, InboundSocket()+":"+InboundMount+":rw")
	}
	return out
}

// Quiesce asks a service to hold still so a member taken while it RUNS is application-consistent,
// and returns the release that ends the window ([B.143]).
//
// THE DEFAULT IS NOTHING, as everywhere here: a service with no way to hold still returns a nil
// release and an error saying so, the caller takes its member anyway, and the member says
// crash-consistent. That is the honest answer for mosquitto, whose own durability is an autosave
// interval — there is nothing to ask it for.
//
// ⚠️ THE RELEASE REPORTS WHETHER THE LOCK HELD FOR THE WHOLE WINDOW, which is the caller's input
// for the member's class rather than a courtesy. Home Assistant breaks its own lock if its
// buffered backlog grows while held; a release that answers false means the service resumed
// writing under the snapshot.
//
// It takes the port because that is how a service is reached on the guest's loopback, and only the
// manifest knows it.
func Quiesce(ctx context.Context, x Executor, m manifest.Manifest, port int) (release func(context.Context) (bool, error), err error) {
	switch m.Name {
	case hass.Name:
		if err := hass.Hold(ctx, x, port); err != nil {
			return nil, err
		}
		return func(ctx context.Context) (bool, error) { return hass.Release(ctx, x, port) }, nil
	}
	return nil, fmt.Errorf("services: %s has no way to hold still", m.Name)
}

// RestoreMarkers names the files a service writes inside its own data to say "a restore of MY
// backups is in flight" — paths relative to that service's data root, one per container that
// keeps state ([B.143]). The default, as always here, is nothing.
//
// TWO CALLERS, ONE FACT, which is why it is one function. The guest's inbound take reads them to
// TITLE the pair a household's own restore produces (*before* and *after* restoring a backup),
// and the restore path SWEEPS them out of a member as it is materialised — so that putting such a
// member back does not replay the restore it was taken around.
//
// RELATIVE, NEVER ABSOLUTE: the caller owns the root. The guest resolves them against the live
// subvolume and the restore against a staged copy of it, and a verb carrying absolute paths would
// be a verb that names a path, which is the thing the inbound channel's trust rules forbid.
func RestoreMarkers(m manifest.Manifest) []string {
	if m.Name != hass.Name {
		return nil
	}
	var out []string
	for _, c := range m.Containers {
		// The container that holds the data is the one whose config directory HA writes into.
		// The others share the subvolume and write nothing of their own.
		if c.Mount != "" {
			out = append(out, c.Name+"/"+hass.RestoreMarker)
		}
	}
	return out
}

// Detect says what a household changed in a service between two ring members, as the one line its
// history shows ([B.167]), or "". prev and next are member paths, which hold the data subvolume's
// layout: one directory per container that keeps state. running says next was taken while the
// service ran.
//
// OPTIONAL PER SERVICE, and the default is nothing, as everywhere here: a service with no detector
// still has the day event, which is what catches what no detector sees. Only Home Assistant has
// one.
func Detect(ctx context.Context, x Executor, m manifest.Manifest, prev, next string, running bool) (string, error) {
	if m.Name != hass.Name {
		return "", nil
	}
	var out []string
	for _, c := range m.Containers {
		if c.Mount == "" {
			continue // shares the subvolume and writes nothing of its own
		}
		before, err := hass.Signals(ctx, x, prev+"/"+c.Name)
		if err != nil {
			return "", err
		}
		after, err := hass.Signals(ctx, x, next+"/"+c.Name)
		if err != nil {
			return "", err
		}
		out = append(out, hass.Detect(before, after, running)...)
	}
	return hass.Sentence(out), nil
}

// Prepare materialises whatever Volumes promised, on THIS node, before the container starts.
//
// Called by converge, which contains a failure here to the one service: the rendered unit names
// bind sources this writes, and podman creates a missing source as a root-owned directory, so a
// service that cannot be prepared must not be started at all.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	// THE INBOUND TOKEN, for any service that gets the channel ([B.143]). Minted here because
	// Prepare is the one step that runs on EVERY converge -- the installing primary, the survivor
	// promoting into a service it was never told about, and every guest reboot -- which is
	// exactly the set of moments the mount it lands in is (re)made.
	//
	// IT ROTATES PER CONVERGE, and that is the same custody the HA control token already has:
	// /run is tmpfs, so a value cannot outlive the boot that minted it, and a token read out of
	// a backup or a snapshot is worth nothing. Consumers know it from t=0 -- the container is
	// started after this -- so there is no return channel and nothing to refresh.
	if WantsInboundAny(m) {
		if err := mintInboundToken(ctx, x, m.Name); err != nil {
			return fmt.Errorf("services: mint %s's inbound token: %w", m.Name, err)
		}
	}
	switch m.Name {
	case hass.Name:
		// The broker's port travels from one service's package to the other's HERE, which is the
		// only place that sees both. Home Assistant needs to be told where the broker is
		// ([V3b.30](c)), and neither package may reach into the other: agent/hass would then
		// carry mosquitto knowledge, and a second copy of the port is a second thing to keep in
		// step with the catalog.
		return hass.Prepare(ctx, x, m, mosquitto.MQTTPort)
	case mosquitto.Name:
		return mosquitto.Prepare(ctx, x, m)
	}
	return nil
}

// Reach is the sentence `briard app install` ends on: where the household now finds the
// thing it just installed.
//
// LEAD WITH THE NAME, the doctrine install.sh already prints under -- the name stays true if the
// address moves, and under DHCP the VIP is acquired in-guest, so a plausible-but-wrong address is
// the failure [V3.17] exists to end. No published name (a witness, or FLOCK_NAME unset) means no
// URL to promise: say the port and stop.
//
// THE NAME IS THE SERVICE'S OWN since [B.48]: `briard-<flock>-<service>.local`, on the front
// door's :80, rather than the flock name and the service's port. That is the whole user-facing
// payoff of the routing table -- a port in the address was the shape of a node that could only
// reach a service by going around its front door.
//
// The default assumes the manifest's port is an HTTP front door, which is true of every service
// whose front door IS its HTTP endpoint. mosquitto is the first entry where it is not: what it
// publishes for the household is MQTT on 1883, while the manifest's port is the management
// endpoint the liveness floor probes, bound to the guest's loopback. Telling a user to open that
// in a browser would hand them a dead link for a service that is working perfectly -- and no
// reverse proxy can front it either, which is why its name resolves but does not serve.
func Reach(m manifest.Manifest, flock string) string {
	host := routes.HostName(flock, m.Name)
	// A service the door does not front is reached at a PUBLISHED PORT, and saying which one is
	// the whole of its address. mosquitto is the case: the manifest's port is the management
	// endpoint the liveness floor probes, and what the household points clients at is MQTT.
	if !Fronted(m) {
		port := 0
		if len(m.Ports) > 0 {
			port = m.Ports[0]
		}
		if port == 0 {
			// Nothing published and nothing fronted: there is no address to promise, and inventing
			// one is the plausible-but-wrong answer [V3.17] exists to end.
			return "it publishes no address of its own yet"
		}
		// NAME THE PROTOCOL WHERE WE KNOW IT. A port on its own is an address a user cannot act on:
		// nothing in the manifest says what to speak to it, because the schema deliberately carries
		// ports and never protocols. The qualifier is therefore product knowledge, keyed on the
		// service name like everything else here, and it degrades to the bare sentence for a
		// service we have nothing to say about rather than guessing from the port number.
		qual := ""
		if p := protocol(m); p != "" {
			qual = p + " "
		}
		if host == "" {
			return fmt.Sprintf("it accepts %sconnections on port %d", qual, port)
		}
		return fmt.Sprintf("point %sclients at %s:%d", qual, host, port)
	}
	if host == "" {
		return fmt.Sprintf("it answers on port %d", m.Primary().Port)
	}
	return fmt.Sprintf("reach it at http://%s/", host)
}

// protocol is what a client speaks to a service's published port, or "" when we have nothing to
// say. Keyed on the service name, in the same place and for the same reason as Prepare's switch:
// this is the one file that may hold knowledge of both services, and a protocol is a fact about a
// service rather than about the manifest that describes it.
func protocol(m manifest.Manifest) string {
	if m.Name == mosquitto.Name {
		return mosquitto.Protocol
	}
	return ""
}

// Fronted reports whether the front door may serve this service under its own name ([B.48]).
//
// It decides whether converge emits a ROUTE for the service, not a flag the door then has to
// honour: a service this returns false for is named and probed, and the door is handed no way to
// reach it at all. Absence rather than a checked condition, for the same reason shared/manifest
// grants capability by omission.
//
// TRUE BY DEFAULT, because a catalogued service's primary port is normally the HTTP front door a
// household is meant to open — Home Assistant's :8123 is the whole example.
//
// FALSE FOR mosquitto, and this is a security property rather than a nicety. Its manifest `port`
// is the broker's **management API**, which `mosquitto.conf` binds inside the container on
// purpose, with the reason written at the listener: "it is still not exposed, because nothing
// outside the guest has any business reading it". The front door runs INSIDE that guest, so
// routing to it by name would republish that endpoint on the LAN — undoing a deliberate bind
// through a mechanism that never mentions it. What a household actually reaches on the broker is
// MQTT on 1883, which no reverse proxy can serve anyway.
//
// SO THIS IS NOT THE SAME QUESTION AS HEALTH. A not-fronted service still has an address and is
// still probed there every cycle: mosquitto's management endpoint is exactly what the liveness
// floor GETs, and the guest can reach it because the guest is where the bind is. This decides what
// the DOOR may serve, never what the node may ask.
//
// The keying is the same as everything else in this package — the catalog name, product-side, code
// we ship and review. A manifest cannot say it, which is the point: a publisher must not be able to
// hand itself the front door. That changes when a manifest can declare its own networking and
// exposure ([B.48](a)), and this becomes the fallback for entries that say nothing.
func Fronted(m manifest.Manifest) bool {
	return m.Name != mosquitto.Name
}

// Announce is what this service wants said about itself on the household LAN: the mDNS service
// records converge writes into the routing table for the guest's publisher to claim.
//
// THE DEFAULT IS SILENCE, like everything else here. A service that answers HTTP is already
// reachable by the name the door serves it under, and browsing types it into a category it may
// not belong in; the announcement exists for the services a household's OTHER DEVICES go looking
// for, which today is one.
//
// mosquitto is that one, and it is not a nicety: a broker is the rare service whose clients are
// appliances rather than people. Tasmota and ESPHome firmware browse `_mqtt._tcp` to find a
// broker, the image cannot advertise itself (agent/mosquitto), and a broker on a LAN announcing
// nothing is simply a service being impolite. The port is MQTT's, never the manifest's -- the
// manifest names the management endpoint the health floor probes, which is bound inside the pod
// and is nobody else's business ([B.48](a)).
//
// The instance label is the flock-scoped name, composed once in shared/routes, so what a device
// offers a household to pick from names the flock it belongs to.
func Announce(m manifest.Manifest, flock string) []routes.Announcement {
	name := routes.InstanceName(flock, m.Name)
	if name == "" {
		// No flock name yet means no name to point an SRV record at, and announcing a broker at a
		// host nothing resolves is worse than announcing nothing. The same rule already governs
		// the service's own A record.
		return nil
	}
	switch m.Name {
	case mosquitto.Name:
		return []routes.Announcement{{Name: name, Type: mosquitto.ServiceType, Port: mosquitto.MQTTPort}}
	}
	return nil
}

// WantsInboundAny reports whether any container of a service wants the inbound channel -- what
// converge asks before starting the socket unit, which is per SERVICE rather than per container.
func WantsInboundAny(m manifest.Manifest) bool {
	for _, c := range m.Containers {
		if WantsInbound(m, c) {
			return true
		}
	}
	return false
}

// mintInboundToken writes a fresh secret into the service own directory, 0600.
//
// 32 bytes of crypto/rand: this is a bearer credential for a channel that acts on a household's
// data, so it is sized like one rather than like an identifier. The directory is created here
// because Prepare is the earliest thing on every path that needs it -- and because that same
// directory is what the container sees at ServiceMount, so the token reaches it with no mount of
// its own.
func mintInboundToken(ctx context.Context, x Executor, service string) error {
	if _, err := x.Run(ctx, "mkdir", "-p", ServiceDir(service)); err != nil {
		return err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	if err := x.WriteFile(InboundTokenPath(service), []byte(hex.EncodeToString(buf))); err != nil {
		return err
	}
	// The token is a credential, and /run is world-readable by default. The bind mount is what
	// carries it into one container; the mode is what keeps it from every other reader on the
	// node.
	_, err := x.Run(ctx, "chmod", "0600", InboundTokenPath(service))
	return err
}

// NeedsServiceDir reports whether a container gets its service's own directory at ServiceMount.
// True for anything the product hands something to: Home Assistant's control channel, or the
// inbound token. A service needing neither gets no directory and no mount.
func NeedsServiceDir(m manifest.Manifest, c manifest.Container) bool {
	return (m.Name == hass.Name && c.Primary) || WantsInbound(m, c)
}
