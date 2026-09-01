// Package mosquitto is the product-side knowledge Briard holds about one catalogued service: the
// MQTT broker a Home Assistant household needs anyway ([V3b.4]).
//
// WHY THE PRODUCT HOLDS A SERVICE'S CONFIG AT ALL, and why this is not a manifest field. The
// manifest is a document a publisher writes, and its schema withholds host binds ON PURPOSE, so
// a catalog entry cannot reach the host. But mosquitto cannot be usefully run as its image ships
// it -- persistence is off by default and the image takes no configuration from the environment
// -- so SOMETHING has to put a config file in front of it. Doing that from the product, keyed on
// the catalog name, is the same call [V3b.29] made for the readiness registry and rests on the
// same fact: the catalog is curated precisely so Briard can encode how each service is operated.
// agent/hass is the first instance of this shape; this is the second.
//
// WHAT IT DOES NOT DO, which is most of it. The broker needs no control channel, no credential
// and nothing read out of its image: Prepare writes one file, and the service is otherwise the
// plain shape the manifest describes. It gets the liveness floor and no readiness assessor
// (agent/host/readiness.go), because "the broker is answering" is the whole of what a broker
// being ready means -- a differential gate would have nothing to differ.
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

// MQTTPort is the port clients connect to -- the manifest's `port` is the broker's HTTP
// management endpoint (the only thing the liveness floor can probe), so the port a HOUSEHOLD
// needs is product knowledge like the rest of this package. It is what the install verb tells
// the user to reach.
const MQTTPort = 1883

// Protocol is what a client must SPEAK to the published port, and it is product knowledge for the
// same reason MQTTPort and ServiceType are: a manifest names ports and never protocols, so without
// it the install verb can only say "point clients at <name>:1883" -- an address with no
// instruction attached to it. [B.48a] generalised that sentence off mosquitto and the word went
// with it; tier 4 caught the loss.
const Protocol = "MQTT"

// ServiceType is the mDNS service type a broker is found under, and it is product knowledge for
// the same reason MQTTPort is: the manifest names the management port, and what protocol answers
// where is ours to say.
//
// MEASURED: the broker cannot advertise itself. There is no mDNS, avahi or bonjour code anywhere
// in eclipse-mosquitto -- the daemon has no notion of announcing its own existence -- so a broker
// sitting on a household LAN is invisible to the Tasmota- and ESPHome-class devices that look
// `_mqtt._tcp` up precisely in order to find one. The announcement therefore has to come from
// outside the container, which for us costs nothing: the guest already runs avahi ([V3b.19]) and
// already publishes the service's name from the routing table.
//
// `_mqtt._tcp` is the registered type for plain MQTT (`_secure-mqtt._tcp` is the TLS one, which
// this broker does not speak).
const ServiceType = "_mqtt._tcp"

// HealthPort, HealthPath and DataMount are the three facts THIS package's config and the
// PUBLISHED manifest must agree on: the config opens the management listener, and the manifest
// tells every node to probe it; the config writes the persistence file into the directory the
// manifest binds. Disagreement is silent and total -- a health gate that can never pass, or a
// broker whose retained state lands outside the replicated volume.
//
// The node still reads the MANIFEST at runtime, never these: they exist so the two documents can
// be checked against each other (mosquitto_test.go, catalog/catalog_test.go) rather than
// discovered to disagree on a household's node.
const (
	HealthPort = 9883
	HealthPath = "/api/v1/listeners"
	DataMount  = "/mosquitto/data"
)

//go:embed mosquitto.conf
var confSource string

// Executor is the guest-side command runner. Structurally identical to agent/hass's, and
// deliberately its own: a package that needs two verbs should not import another service's
// interface to name them.
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

// Prepare materialises the config on this node. Called by converge before the container starts,
// on every node that promotes -- the installer, the survivor that was never told about this
// service, and every guest reboot.
//
// A FAILURE HERE COSTS THIS SERVICE AND NOT THE NODE: converge skips a service it could not
// prepare and starts the rest (agent/guestagent/converge.go). Skipping is also what keeps podman
// from creating the missing bind source as a root-owned DIRECTORY over the container's config
// path, which no later Prepare could fix.
// bindToken is the placeholder mosquitto.conf carries for the management listener's address, and
// substituting it is why Prepare takes the manifest rather than just the node.
//
// THE SAME LITERAL MEANS OPPOSITE THINGS IN THE TWO MODES, which is the whole reason this is not a
// constant in the file. Under a PRIVATE pod, 0.0.0.0 is that pod's namespace -- the guest reaches
// the management API across the pod network, and the LAN cannot, because only `ports` are
// published and this is not one of them. Under HOST networking the pod IS the guest, so 0.0.0.0
// would be every interface the household can see.
//
// It also removes an ordering hazard rather than merely expressing a preference: a config flipped
// to 0.0.0.0 one commit before the network mode would publish the management API to the LAN for
// exactly that long. Derived from the manifest, the two cannot be out of step in either direction.
const bindToken = "@BIND@"

func bindAddress(m manifest.Manifest) string {
	if m.HostNetwork() {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	if m.Name != Name {
		return nil
	}
	if _, err := x.Run(ctx, "mkdir", "-p", Dir); err != nil {
		return fmt.Errorf("mosquitto: %s: %w", Dir, err)
	}
	// The token is the contract between this function and the embedded file; if the file stops
	// carrying it, the broker would silently listen wherever the literal left behind says.
	//
	// CHECKED BEFORE THE REPLACE, because that is the half that actually catches it: a Replace of a
	// token that is not there is a silent NO-OP, so a check only afterwards catches a SECOND
	// placeholder and never a missing one -- which is precisely the case this paragraph describes.
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
