// Package routes is the front door's routing table: the file the guest's converge WRITES and the
// reverse proxy READS, mapping a hostname a household types to the service that answers it.
//
// WHY A FILE, AND WHY THIS ONE WRITES IT ([B.48]). The proxy could have read the volume's
// `.services/*.json` itself and built the table from the manifests — it runs in the same guest and
// the volume is mounted by the time it starts. That is wrong for one reason that decides
// everything else: A SERVICE'S ADDRESS IS A RENDERING FACT, NOT A MANIFEST FACT. The manifest
// names a port; where that port can be reached depends on how the renderer wired the pod, which is
// `127.0.0.1:<port>` under today's host networking and a pod address once a service may ask for a
// private network ([B.48](a)). Teaching the door to answer that question would weld it to podman
// and duplicate the renderer. Converge already holds both halves — the manifest and the rendering
// — and already runs at exactly the moments the table can change: every promotion, every install,
// every guest reboot.
//
// THE TABLE IS DERIVED, so it lives in /run: node-local, tmpfs, re-derived rather than replicated,
// exactly like the quadlet units it is written beside. What replicates is the manifest, which is
// the identity; a rendered address is this node's own answer to it.
//
// A HOSTNAME APPEARS HERE AND NOWHERE ELSE. The same entries are what the mDNS publisher claims
// on the LAN and what the proxy matches on, so a name that is published but not routed — or routed
// but not published — is not expressible. That is the whole reason the names are composed once,
// here (HostName), rather than independently by the door and by the publisher's shell.
package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Path is where converge writes the table and the proxy reads it. Under /run/briard, beside
// services.units and mdns.env, all of them the same kind of fact: what this node is doing now.
const Path = "/run/briard/routes.json"

// Table is the whole front door's routing, as one file. Ordered by service name, because converge
// renders in that order and a table two nodes disagree about the ORDER of is a diff nobody wants
// to read.
type Table struct {
	Services []Service `json:"services"`
}

// Service is one runtime-installed service as the front door needs to see it.
//
// WHAT IS DELIBERATELY NOT HERE: the manifest identity (the door does not care WHICH version it
// forwards to — that is the cloud's question, and api.ServiceStatus carries it), and the ports a
// service publishes for clients that are not HTTP. The second is a real gap and it is left open
// on purpose: under host networking every port a container listens on is already reachable at the
// VIP, and the manifest names only the primary's — so a port list today would be a partial one,
// and the landing page would print it as if it were the whole truth. It becomes fillable when a
// service can declare its own exposure ([B.48](a)), and not before.
type Service struct {
	// Name is the catalog slug, the same one the manifest and the units carry.
	Name string `json:"name"`
	// Hosts are the names this service answers to, lowercase and without a port — today the
	// single mDNS label HostName composes, later the per-home `*.casa` name ([V3b.14]).
	//
	// EMPTY IS A REAL STATE, not a bug: a node whose flock has no minted name publishes nothing
	// rather than a guess (net.mdnsname), so it routes nothing either. The service is still
	// installed, still running and still reachable on its port; it simply has no name yet.
	Hosts []string `json:"hosts,omitempty"`
	// Address is where this service ANSWERS, as a URL ("http://127.0.0.1:8123"), and HealthPath
	// the path on it that says whether it is serving. Both come from the renderer, which is the
	// only thing that knows how the pod was wired.
	//
	// NAMED FOR THE SERVICE, NOT FOR THE DOOR, because it is not the door's fact. The health probe
	// GETs this unconditionally — from inside the guest, never through the proxy — and the front
	// door additionally uses it as a proxy target when Fronted below allows. mosquitto is what
	// makes the distinction concrete: its address is a loopback-bound management API, probed every
	// cycle and relayed to nobody. Calling this "backend" implied the probe was proxy-adjacent,
	// which it has never been.
	Address    string `json:"address,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
	// Fronted is whether the door may PROXY to Address. False is a real and deliberate state, not
	// a missing value: mosquitto's manifest port is its management API, bound to the guest's
	// loopback on purpose, and the front door runs inside the guest — so routing to it by name
	// would republish a loopback-only endpoint on the LAN. Such a service keeps its name (which
	// resolves to the VIP, where its real protocol is listening) and the door says it does not
	// answer HTTP rather than 404ing, because the name IS ours.
	//
	// It costs such a service NOTHING but the relay: the address above is still real and still
	// probed, so "not fronted" and "not monitored" are unrelated — which is the whole reason this
	// is a second field rather than an empty Address.
	//
	// Written explicitly rather than by omission: this file is derived, not a published document,
	// and a security-relevant default is worth being able to read in a `cat`. The product decides
	// it today (agent/services.Fronted); a manifest will, once it can state its own exposure
	// ([B.48](a)) — where the second axis lands too, since a private-network service may want the
	// door, a published port, or both.
	Fronted bool `json:"fronted"`
}

// HostName is the single place a service's LAN name is composed: `briard-<flock>-<service>.local`.
//
// ONE LABEL, and that is measured rather than stylistic (V3.19d): `mdns4_minimal`, the resolver in
// Debian/Ubuntu's nsswitch, handles exactly ONE label before `.local`. So the flock-scoped
// `briard-picked-hornet-home-assistant.local` resolves on a stock client and the prettier
// `home-assistant.briard-picked-hornet.local` would publish fine and resolve nowhere — the same
// trap V3.19d found for the flock name itself, one level down.
//
// FLOCK-SCOPED rather than bare `home-assistant.local`, which is the name every Home Assistant
// tutorial uses and the one thing a household might already own. Measured 2026-08-23: two avahi
// publishers of the same `-a` record BOTH report Established, with no conflict, no rename and no
// log — the later silently shadows the earlier — and the household most likely to already own
// that name is the one migrating from an HAOS box, which is the likeliest way anyone arrives
// here. Claiming a name we can only lose silently is worse than a longer one nobody contests.
//
// An empty flock name yields no name at all, deliberately: `briard--home-assistant.local` is worse
// than silence, and the same rule already governs the flock's own publisher.
func HostName(flock, service string) string {
	if flock == "" || service == "" {
		return ""
	}
	return "briard-" + flock + "-" + service + ".local"
}

// Parse decodes a table. Unknown fields are refused for the same reason shared/manifest refuses
// them: the writer and the reader ship together, so a field this build does not understand means
// the two have drifted, and routing traffic on a table you only partly understand is worse than
// keeping the last one you did.
func Parse(raw []byte) (Table, error) {
	var t Table
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Table{}, fmt.Errorf("routes: %w", err)
	}
	return t, nil
}

// Marshal renders the table for the file. Indented, because a human debugging why their service
// is not reachable will `cat` this before they do anything else.
func (t Table) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("routes: %w", err)
	}
	return append(b, '\n'), nil
}

// Lookup finds the service a request's Host header names. The header is normalised first, because
// what arrives is whatever the client typed: any case, a trailing dot from a fully-qualified
// resolver, and a `:port` whenever the port is not the scheme's default.
//
// It reports a match for a service the door may not front, which is NOT the same as no match: the
// door then knows the name is ours and the service is real, and can say so instead of
// serving a 404 that reads as "you typed it wrong".
func (t Table) Lookup(host string) (Service, bool) {
	h := Normalise(host)
	if h == "" {
		return Service{}, false
	}
	for _, s := range t.Services {
		for _, n := range s.Hosts {
			if n == h {
				return s, true
			}
		}
	}
	return Service{}, false
}

// Get finds a service by name — what the per-service health probe resolves through, so that the
// address it probes is the one the renderer actually wired rather than one the caller assembled.
func (t Table) Get(name string) (Service, bool) {
	for _, s := range t.Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// Normalise reduces a Host header to the form Hosts are stored in: lowercase, no trailing dot, no
// port. IPv6 literals arrive bracketed (`[::1]:80`), so the port is split off the LAST colon and
// only when there is no unmatched bracket — a naive SplitN on ":" would truncate every v6 address
// to "[".
func Normalise(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(h, "]"); i >= 0 { // bracketed IPv6, with or without a port
		h = h[:i+1]
	} else if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[:i], ":") {
		h = h[:i] // a single colon is a port; several mean a bare v6 literal, which has none
	}
	return strings.TrimSuffix(h, ".")
}

// HostsPath is the same table's hostnames, one per line, for the mDNS publisher.
//
// A SECOND FILE, DELIBERATELY, and the reason is that its reader is a shell script: the publisher
// is a systemd unit bound to the VIP (a record lives exactly as long as the process holding it,
// which is the property that makes withdrawal on demote impossible to get wrong), and the guest
// image ships no JSON parser for it to use. The alternatives were worse in the way that matters —
// adding jq to parse the door's table, or hand-rolling a second JSON reader in shell, either of
// which is a second chance for the published names and the routed names to disagree. Written by
// the same call, from the same composed list, so they cannot.
const HostsPath = "/run/briard/routes.hosts"

// Hosts is every name in the table, in service order — the publisher's whole input.
func (t Table) Hosts() []string {
	var out []string
	for _, s := range t.Services {
		out = append(out, s.Hosts...)
	}
	return out
}
