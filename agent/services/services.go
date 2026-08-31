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

// Volumes returns the host binds one container of one service needs beyond its own data.
//
// Called by the RENDERER (agent/quadlet), which stays a pure function of the manifest: the same
// manifest renders the same units on every node, and these binds are a property of the service's
// name rather than of anything a publisher wrote.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	switch m.Name {
	case hass.Name:
		return hass.Volumes(m, c)
	case mosquitto.Name:
		return mosquitto.Volumes(m, c)
	}
	return nil
}

// Prepare materialises whatever Volumes promised, on THIS node, before the container starts.
//
// Called by converge, which contains a failure here to the one service: the rendered unit names
// bind sources this writes, and podman creates a missing source as a root-owned directory, so a
// service that cannot be prepared must not be started at all.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	switch m.Name {
	case hass.Name:
		return hass.Prepare(ctx, x, m)
	case mosquitto.Name:
		return mosquitto.Prepare(ctx, x, m)
	}
	return nil
}

// Reach is the sentence `briard service install` ends on: where the household now finds the
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
	if m.Name == mosquitto.Name {
		if flock == "" {
			return fmt.Sprintf("it accepts MQTT on port %d", mosquitto.MQTTPort)
		}
		return fmt.Sprintf("point MQTT clients at briard-%s.local:%d", flock, mosquitto.MQTTPort)
	}
	host := routes.HostName(flock, m.Name)
	if host == "" {
		return fmt.Sprintf("it answers on port %d", m.Primary().Port)
	}
	return fmt.Sprintf("reach it at http://%s/", host)
}

// Fronted reports whether the front door may proxy to this service ([B.48]).
//
// TRUE BY DEFAULT, because a catalogued service's primary port is normally the HTTP front door a
// household is meant to open — Home Assistant's :8123 is the whole example.
//
// FALSE FOR mosquitto, and this is a security property rather than a nicety. Its manifest `port` is
// the broker's **management API**, which `mosquitto.conf` binds to `127.0.0.1` on purpose, with the
// reason written at the listener: "it is still not exposed, because nothing outside the guest has
// any business reading it". The front door runs INSIDE the guest, so routing to it by name would
// republish that loopback-only endpoint on the LAN at the VIP — undoing a deliberate bind through
// a mechanism that never mentions it. What a household actually reaches on the broker is MQTT on
// 1883, which no reverse proxy can serve anyway, and which host networking already exposes.
//
// SO THIS IS NOT THE SAME QUESTION AS HEALTH. A not-fronted service still has an address and is
// still probed there: mosquitto's management endpoint is exactly what the liveness floor GETs, and
// the guest can reach it because the guest is where the bind is. Fronted decides what the DOOR may
// forward, never what the node may ask.
//
// The keying is the same as everything else in this package — the catalog name, product-side, code
// we ship and review. A manifest cannot say it, which is the point: a publisher must not be able to
// hand itself the front door. That changes when a manifest can declare its own networking and
// exposure ([B.48](a)), and the default flips to whatever the schema says then.
func Fronted(m manifest.Manifest) bool {
	return m.Name != mosquitto.Name
}
