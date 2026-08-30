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
func Prepare(ctx context.Context, x Executor, m manifest.Manifest) error {
	if m.Name != Name {
		return nil
	}
	if _, err := x.Run(ctx, "mkdir", "-p", Dir); err != nil {
		return fmt.Errorf("mosquitto: %s: %w", Dir, err)
	}
	// 0644, not 0600: the broker runs as its own unprivileged user inside the container (uid
	// 1883 on the pinned image) and has to read this file.
	return write(ctx, x, confPath, confSource, "0644")
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
