// Package mosquitto is the product-side knowledge Briard holds about one catalogued app: the
// MQTT broker a Home Assistant household needs anyway.
//
// Why the product holds an app's config at all, and not as a manifest field: the manifest is a
// document a publisher writes, and its schema withholds host binds on purpose, so a catalog entry
// cannot reach the host. But mosquitto cannot usefully run as its image ships it (persistence is
// off by default and the image takes no configuration from the environment), so something has to
// put a config file in front of it. Doing that from the product, keyed on the catalog name, is
// the readiness registry's call and rests on the same fact: the catalog is curated precisely so
// Briard can encode how each app is operated.
//
// What it does not do is most of it: the broker needs no control channel, no credential and
// nothing read out of its image. Prepare writes one file; the app is otherwise the plain shape the
// manifest describes. Its readiness assessor (agent/host/readiness.go) writes a token into the
// broker's retained state and reads it back (probe.go).
package mosquitto

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"briard.io/shared/manifest"
)

// Name is the broker's catalog slug. Everything in this package is keyed on it, so a manifest
// published under any other name gets none of this.
const Name = "mosquitto"

// Dir is where the rendered config lives on the guest. /run for the same reason agent/hass uses
// it: tmpfs, so every converge re-derives it and no node can drift onto a stale copy.
const Dir = "/run/briard/mosquitto"

// confPath is the rendered file; containerConf is where the image's own command line expects to
// find it (`mosquitto -c /mosquitto/config/mosquitto.conf`, measured on the pinned image).
// Shadow-mounting that exact path is what replaces the baked config without rebuilding anything.
const (
	confPath      = Dir + "/mosquitto.conf"
	containerConf = "/mosquitto/config/mosquitto.conf"
)

// MQTTPort is the port clients connect to: the manifest's `port` is the broker's HTTP management
// endpoint (the only thing the liveness floor can probe), so the port a household needs is product
// knowledge like the rest of this package. It is what the install verb tells the user to reach.
const MQTTPort = 1883

// Protocol is what a client must speak to the published port, product knowledge for the same
// reason as MQTTPort and ServiceType: a manifest names ports, never protocols, so without it the
// install verb can only say "point clients at <name>:1883", an address with no instruction.
const Protocol = "MQTT"

// ServiceType is the mDNS service type a broker is found under, product knowledge for the same
// reason as MQTTPort: the manifest names the management port, and what protocol answers where is
// ours to say. Measured: the broker cannot advertise itself (eclipse-mosquitto has no mDNS, avahi
// or bonjour code anywhere, no notion of announcing its own existence), so on a household LAN it is
// invisible to the Tasmota- and ESPHome-class devices that look `_mqtt._tcp` up precisely to find
// one. The announcement has to come from outside the container, which costs us nothing: the
// reverse proxy's mDNS responder already answers for the app's name from the routing table
// (agent/apps.Announce). `_mqtt._tcp` is the registered type for plain MQTT (`_secure-mqtt._tcp`
// is the TLS one, which this broker does not speak).
const ServiceType = "_mqtt._tcp"

// HealthPort, HealthPath and DataMount are the three facts this package's config and the published
// manifest must agree on: the config opens the management listener the manifest tells every node
// to probe, and writes the persistence file into the directory the manifest binds. Disagreement is
// silent and total: a health gate that can never pass, or a broker whose retained state lands
// outside the replicated volume. The node still reads the manifest at runtime, never these: they
// exist so the two documents are checked against each other (mosquitto_test.go,
// catalog/catalog_test.go) rather than discovered to disagree on a household's node.
const (
	HealthPort = 9883
	HealthPath = "/api/v1/listeners"
	DataMount  = "/mosquitto/data"
)

//go:embed mosquitto.conf
var confSource string

// Executor is the guest-side command runner. Structurally identical to agent/hass's and
// agent/apps', and deliberately its own: an app package should not import another app's
// interface to name its verbs.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
}

// Volumes is the bind the broker's container needs beyond its own data: the rendered config,
// read-only, over the image's baked one.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	if m.Name != Name || !c.Primary {
		return nil
	}
	return []string{confPath + ":" + containerConf + ":ro"}
}

// bindToken is the placeholder mosquitto.conf carries for the management listener's address;
// substituting it is why Prepare takes the manifest rather than just the node. The same literal
// means opposite things in the two modes, the whole reason it is not a constant in the file: under
// a private pod, 0.0.0.0 is that pod's namespace (the guest reaches the management API across the
// pod network; the LAN cannot, since only `ports` are published and this is not one), while under
// host networking the pod is the guest, so 0.0.0.0 would be every interface the household can see.
// Derived from the manifest, the bind and the network mode can never be changed separately, so the
// management API is never on the LAN even for a moment.
const bindToken = "@BIND@"

func bindAddress(m manifest.Manifest) string {
	if m.HostNetwork() {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// Prepare materialises the config on this node. Called by converge before the container starts,
// on every node that promotes: the installer, the survivor never told about this app, and every
// guest reboot. A failure here costs this app, not the node: converge skips an app it could not
// prepare and starts the rest (agent/guestagent/converge.go). Skipping also keeps podman from
// creating the missing bind source as a root-owned directory over the container's config path,
// which no later Prepare could fix.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	if m.Name != Name {
		return nil
	}
	if _, err := x.Run(ctx, "mkdir", "-p", Dir); err != nil {
		return fmt.Errorf("mosquitto: %s: %w", Dir, err)
	}
	// The token is the contract between this function and the embedded file; if the file stops
	// carrying it, the broker would silently listen wherever the literal left behind says. Checked
	// before the replace, because a Replace of an absent token is a silent no-op: the check
	// afterwards catches only a second placeholder, never a missing one.
	if !strings.Contains(confSource, bindToken) {
		return fmt.Errorf("mosquitto: the config template no longer carries %s", bindToken)
	}
	conf := strings.Replace(confSource, bindToken, bindAddress(m), 1)
	if strings.Contains(conf, bindToken) {
		return fmt.Errorf("mosquitto: the config template carries %s more than once", bindToken)
	}
	// 0644, not 0600: the broker runs as its own unprivileged user inside the container (uid
	// 1883 on the pinned image) and has to read this file.
	return write(ctx, x, confPath, conf, "0644")
}

// write installs a file atomically-enough for /run: write beside, chmod, rename over. The rename
// is what keeps a container that starts mid-Prepare from binding a half-written config.
func write(ctx context.Context, x Executor, path, content, mode string) error {
	tmp := path + ".new"
	if err := x.WriteFile(tmp, []byte(content)); err != nil {
		return fmt.Errorf("mosquitto: write %s: %w", tmp, err)
	}
	if out, err := x.Run(ctx, "chmod", mode, tmp); err != nil {
		return fmt.Errorf("mosquitto: %s permissions: %w: %s", tmp, err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "mv", "-f", tmp, path); err != nil {
		return fmt.Errorf("mosquitto: install %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
