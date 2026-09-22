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
	"fmt"

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

// InboundDir holds the per-service inbound sockets ([B.143]). ONE SOCKET PER SERVICE, mounted
// into that service's container and no other: a caller's identity is then a property of the
// transport rather than a claim in its request, which is what makes the channel safe to expose to
// a workload at all (agent/guestagent/inbound.go's trust rules).
//
// It lives HERE rather than in agent/hass because it is not one service's knowledge: any service
// whose entrypoint can be wrapped gets the same channel on the same terms. It cannot live in
// agent/guestagent either -- that package imports this one, and the renderer needs the path.
const InboundDir = "/run/briard/inbound"

// InboundSocketPath is one service's socket on the node.
func InboundSocketPath(service string) string { return InboundDir + "/" + service + ".sock" }

// InboundMount is where that socket appears inside the container. A fixed name, so a client in
// the container needs to know nothing about which service it is speaking for -- which is the same
// property the trust rules depend on, seen from the other side.
const InboundMount = "/briard/inbound.sock"

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
	// ⚠️ READ-WRITE, and it has to be: connect(2) on a unix socket needs write permission on the
	// socket file, so a read-only bind makes the channel unreachable from inside the container
	// rather than merely read-only. It is also why this cannot be a file inside the service's
	// existing `:ro` directory bind and needs a mount of its own.
	if WantsInbound(m, c) {
		out = append(out, InboundSocketPath(m.Name)+":"+InboundMount+":rw")
	}
	return out
}

// Prepare materialises whatever Volumes promised, on THIS node, before the container starts.
//
// Called by converge, which contains a failure here to the one service: the rendered unit names
// bind sources this writes, and podman creates a missing source as a root-owned directory, so a
// service that cannot be prepared must not be started at all.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
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
