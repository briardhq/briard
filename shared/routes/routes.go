// Package routes is the front door's routing table: the file the guest's converge WRITES and the
// reverse proxy READS, mapping a hostname a household types to the service that answers it.
//
// IT IS A DERIVED PROJECTION, NOT THE CATALOG. The signed manifest is stored verbatim on the
// replicated volume, because the service identity is the sha256 of those exact bytes — enriching
// it would remint the identity on every node, so the catalog document physically cannot be the
// thing we add fields to. This is the separate document derived from it, and its inputs come from
// three places that no single one of them could supply:
//
//   - the MANIFEST: the service's name, its port and its health path.
//   - the RENDERER: Address. A service's address is a rendering fact, not a manifest fact — the
//     manifest names a port, and what host answers on it is decided by how the pod was wired.
//   - NODE IDENTITY: Hosts, composed from the flock's minted name, which cannot exist in a
//     published catalog because it is per-household and chosen at install.
//
// So it lives in /run: node-local, tmpfs, re-derived by every converge rather than replicated,
// exactly like the quadlet units it is written beside.
//
// A HOSTNAME APPEARS HERE AND NOWHERE ELSE. These entries are what the mDNS publisher claims on
// the LAN and what the proxy matches on, so a name that is published but not routed — or routed
// but not published — is not expressible. That is why the names are composed once, here
// (HostName), rather than independently by the door and by the publisher.
package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Path is where converge writes the table, the proxy reads it, and the mDNS publisher reads it.
// One file: the publisher parses it with jq rather than being handed a pre-flattened projection,
// because a second file is a second thing that can be stale.
const Path = "/run/briard/routes.json"

// Table is the whole front door's routing, as one file. Ordered by service name, because converge
// renders in that order and a table two nodes disagree about the ORDER of is a diff nobody wants
// to read.
type Table struct {
	Services []Service `json:"services"`
}

// Service is one runtime-installed service as the node needs to see it.
type Service struct {
	// Name is the catalog slug, the same one the manifest and the units carry.
	Name string `json:"name"`
	// Hosts are the names this service answers to — today the single mDNS label HostName
	// composes, later the per-home `*.casa` name too ([V3b.14]).
	//
	// MATERIALISED RATHER THAN DERIVED, though both forms are computable from (flock, slug). The
	// reason is the mDNS publisher: it is a shell script, and if it composed names itself the
	// naming rule would live in two languages — one more each time a name form is added. Stored,
	// the publisher stays permanently dumb: it claims whatever this list says, and every rule for
	// building a name stays in HostName.
	//
	// EMPTY IS A REAL STATE: a node whose flock has no minted name publishes nothing rather than
	// a guess (net.mdnsname), so it routes nothing either. The service is still installed and
	// still reachable on any port it publishes; it simply has no name yet.
	Hosts []string `json:"hosts,omitempty"`
	// Address is the host this service answers on — the guest's own loopback while the pod shares
	// the guest's network namespace, the pod's address once it does not ([B.48](a)). A bare host,
	// with no port: a service is ONE pod and a pod is ONE network namespace, so one address serves
	// every port it listens on, and Health and Routes below carry only the port.
	//
	// It comes from the renderer, which is the only thing that knows how the pod was wired, and it
	// never appears in the catalog: a published, signed, node-independent document cannot carry a
	// fact that differs per node and per render.
	Address string `json:"address,omitempty"`
	// Health is the endpoint that answers "is this service serving", as a host-less URL resolved
	// against Address (see Resolve).
	//
	// It is NOT a route, and that is the point: the probe is performed by the guest, straight to
	// the service, and never traverses the proxy. A service the door may not front is still
	// probed here every cycle — mosquitto's management API is exactly that, bound inside the pod
	// and relayed to nobody.
	Health string `json:"health,omitempty"`
	// Routes is what the DOOR does about this service, and an empty list means nothing — the
	// service is named and probed, and no request is ever forwarded to it.
	//
	// ABSENCE RATHER THAN A FLAG, deliberately. A boolean saying "do not front this" leaves the
	// door holding an address it is trusted not to use, one inverted condition away from relaying
	// a pod-internal endpoint to the LAN. With no route there is nothing to forward and nothing to
	// get wrong — the same reason shared/manifest grants capability by omission.
	Routes []Route `json:"routes,omitempty"`
	// Ports are what this service publishes to the household, reachable at the node's service
	// address on the same number. NOT the door's business -- it forwards HTTP and these exist for
	// the services that do not speak it -- but the node's, because the page and `briard service
	// install` have to be able to say where a service actually is.
	//
	// It stayed out of this table until a manifest could declare exposure, and the reason is worth
	// keeping: under host networking every port a container listens on is reachable at the service
	// address, and the manifest names only the primary's -- so any list written then would have
	// been partial, and the page would have printed it as though it were the whole truth.
	Ports []int `json:"ports,omitempty"`
	// Announce are the mDNS SERVICE records this service wants on the household LAN -- what a
	// device looking for a broker browses for, as opposed to Hosts, which are names something has
	// to know already in order to ask.
	//
	// PRODUCT KNOWLEDGE, never the manifest's: which service type a thing is found under is the
	// kind of per-service fact a curated catalog exists to let us encode (agent/services), and a
	// publisher declaring its own type would be claiming a well-known name on a household's LAN.
	// The catalog names a port; what protocol answers there is ours to say.
	//
	// MATERIALISED, for the same reason Hosts is: the publisher is a shell script, so every rule
	// for composing a name lives in this package (InstanceName) and the script stays permanently
	// dumb. Each record is pointed at the service's OWN name -- the first of Hosts -- which is why
	// Validate refuses an announcement on a service that has none. An SRV record needs a target,
	// and the only other candidate is the guest's own hostname, which is node-scoped and would
	// send every device on the LAN to a machine that just stopped being the Primary.
	Announce []Announcement `json:"announce,omitempty"`
}

// Announcement is one mDNS service record: the label a browser shows, the type it browses for,
// and the port the SRV record carries. The target host is deliberately absent -- it is the
// service's own Hosts[0], so there is one name per service rather than two that can disagree.
type Announcement struct {
	// Name is the service-instance label, composed by InstanceName.
	Name string `json:"name"`
	// Type is the RFC 6763 service type: `_mqtt._tcp` and its kin.
	Type string `json:"type"`
	// Port is what the SRV record advertises -- the port a household connects to, which is not
	// always the manifest's, since that one names what the health floor probes.
	Port int `json:"port"`
}

// Route is one way in to a service: which listener the request arrives on, and where it goes.
type Route struct {
	// Listen is the listener this route is served on. ListenName is the shared :80/:443 front
	// door, matched against the service's own Hosts; a dedicated listener is spelled
	// "tls:<port>" and is not implemented yet (Validate refuses it, loudly — a route the door
	// silently ignored would be a service unreachable with nothing saying why).
	Listen string `json:"listen"`
	// To is where the door forwards, as a host-less URL resolved against the service's Address.
	// The scheme says how to speak to it: "http" is a proxied request, upgrades included.
	//
	// THE MISSING HOST IS A SECURITY PROPERTY, not a spelling convenience. Because the only
	// address the door can ever splice in is the service's own, no table — however it was written
	// — can point the front door at another machine. Validate refuses a host here.
	To string `json:"to"`
}

// ListenName is the shared front door: :80 and :443, matched on the request's Host header against
// the service's Hosts. The two ports are one listener here because the door treats them
// identically; separating them is a new value, not a new shape.
const ListenName = "name"

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
// that name is the one migrating from an HAOS box. Claiming a name we can only lose silently is
// worse than a longer one nobody contests.
//
// An empty flock name yields no name at all, deliberately: `briard--home-assistant.local` is worse
// than silence, and the same rule already governs the flock's own publisher.
func HostName(flock, service string) string {
	if flock == "" || service == "" {
		return ""
	}
	return "briard-" + flock + "-" + service + ".local"
}

// InstanceName is the mDNS SERVICE-INSTANCE label an announcement carries: the same
// `briard-<flock>-<service>` HostName builds, without the domain. It is what a household sees in
// a device's broker picker, which is why it is the flock-scoped name and not the bare slug -- two
// briard flocks in one house must be told apart by the person choosing between them.
//
// DERIVED FROM HostName ON PURPOSE, rather than assembled a second time: the instance and the SRV
// target it points at are then the same name by construction, and a future name form ([V3b.14])
// changes one function.
func InstanceName(flock, service string) string {
	return strings.TrimSuffix(HostName(flock, service), ".local")
}

// Parse decodes and validates a table. Unknown fields are refused for the same reason
// shared/manifest refuses them: the writer and the reader ship together, so a field this build
// does not understand means the two have drifted, and routing traffic on a table you only partly
// understand is worse than keeping the last one you did.
func Parse(raw []byte) (Table, error) {
	var t Table
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Table{}, fmt.Errorf("routes: %w", err)
	}
	if err := t.Validate(); err != nil {
		return Table{}, err
	}
	return t, nil
}

// Validate enforces the two rules that are not style. A host in a URL would let the door be
// pointed off this node; an unrecognised listener would be a route silently not served.
func (t Table) Validate() error {
	for _, s := range t.Services {
		if s.Name == "" {
			return fmt.Errorf("routes: a service has no name")
		}
		if s.Health != "" {
			if err := checkSpec(s.Name, "health", s.Health); err != nil {
				return err
			}
		}
		for _, r := range s.Routes {
			if r.Listen != ListenName {
				// Deliberately not "ignore what we do not know": a route the door drops is a
				// service that is unreachable with nothing saying why.
				return fmt.Errorf("routes: service %q has a route on unknown listener %q", s.Name, r.Listen)
			}
			if err := checkSpec(s.Name, "route target", r.To); err != nil {
				return err
			}
		}
		if (s.Health != "" || len(s.Routes) > 0) && s.Address == "" {
			return fmt.Errorf("routes: service %q has endpoints but no address to resolve them against", s.Name)
		}
		for _, a := range s.Announce {
			if err := a.check(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// serviceType is an RFC 6763 service type: an underscore-prefixed application protocol label of
// at most 15 characters, then `._tcp` or `._udp`. Narrow because these strings become arguments
// to a publisher on a household's LAN, and because a type that avahi refuses would be a record
// that silently never appears.
var serviceType = regexp.MustCompile(`^_[a-z0-9]([a-z0-9-]{0,13}[a-z0-9])?\._(tcp|udp)$`)

// check is the announcement half of Validate, and every rule in it is load-bearing rather than
// tidy. The publisher reads these fields out of jq as tab-separated words, so a tab or a newline
// in a name would not be a bad label but a shifted line -- a port read as a type. Refusing them
// here is the same discipline shared/manifest applies to every string that reaches a unit file.
func (a Announcement) check(s Service) error {
	if a.Name == "" {
		return fmt.Errorf("routes: service %q announces %q with no instance name", s.Name, a.Type)
	}
	if strings.ContainsAny(a.Name, "\t\r\n") {
		return fmt.Errorf("routes: service %q announces under a name containing whitespace: %q", s.Name, a.Name)
	}
	if !serviceType.MatchString(a.Type) {
		return fmt.Errorf("routes: service %q announces an unusable service type %q", s.Name, a.Type)
	}
	if a.Port < 1 || a.Port > 65535 {
		return fmt.Errorf("routes: service %q announces %s on port %d", s.Name, a.Type, a.Port)
	}
	if len(s.Hosts) == 0 {
		// The record's SRV target is Hosts[0]; with no name there is nothing to point devices at,
		// and pointing them at the guest's own hostname would name a node rather than the flock.
		return fmt.Errorf("routes: service %q announces %s but has no name to point it at", s.Name, a.Type)
	}
	return nil
}

// checkSpec is the host-less-URL rule both Health and Route.To follow.
func checkSpec(service, what, spec string) error {
	u, err := url.Parse(spec)
	if err != nil {
		return fmt.Errorf("routes: service %q %s %q: %w", service, what, spec, err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("routes: service %q %s %q: scheme must be http", service, what, spec)
	}
	if u.Hostname() != "" {
		// The whole point of the missing host: with nothing but the service's own Address to
		// splice, no table can aim the door at another machine.
		return fmt.Errorf("routes: service %q %s %q names a host; it must be host-less", service, what, spec)
	}
	if u.Port() == "" {
		return fmt.Errorf("routes: service %q %s %q has no port", service, what, spec)
	}
	return nil
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

// Resolve turns one of this service's host-less specs into a real URL by splicing in its Address.
// The one operation every consumer performs, implemented once so the door and the health probe
// cannot disagree about where a service is.
func (s Service) Resolve(spec string) (*url.URL, error) {
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("routes: %s: %w", s.Name, err)
	}
	if s.Address == "" {
		return nil, fmt.Errorf("routes: %s has no address", s.Name)
	}
	if u.Hostname() != "" {
		// Validate already refused this; repeated here because Resolve is what actually hands an
		// address to a dialer, and a check at the point of use costs nothing.
		return nil, fmt.Errorf("routes: %s: %q names a host", s.Name, spec)
	}
	u.Host = net.JoinHostPort(s.Address, u.Port())
	return u, nil
}

// Route returns this service's route on the given listener.
func (s Service) Route(listen string) (Route, bool) {
	for _, r := range s.Routes {
		if r.Listen == listen {
			return r, true
		}
	}
	return Route{}, false
}

// Lookup finds the service a request's Host header names. The header is normalised first, because
// what arrives is whatever the client typed: any case, a trailing dot from a fully-qualified
// resolver, and a `:port` whenever the port is not the scheme's default.
//
// A match says only that the name is OURS. Whether anything is served under it is the service's
// Routes, and the difference matters: the door can then say "this is ours and does not answer
// HTTP" instead of a 404 that reads as "you typed it wrong".
func (t Table) Lookup(host string) (Service, bool) {
	h := Normalise(host)
	if h == "" {
		return Service{}, false
	}
	for _, s := range t.Services {
		for _, n := range s.Hosts {
			if Normalise(n) == h {
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
