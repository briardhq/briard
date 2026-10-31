// Package catalog holds the published service manifests. It has no code: the test is the point.
//
// It parses every entry with the SAME function a node uses, so the schema check that would
// otherwise happen for the first time on a household's node happens here instead. The failures it
// is built to catch are the cheap ones that are expensive live: an image reference that is not
// digest-pinned, a container name that is not a slug, a missing or duplicated primary, a health
// path that is not a path — every one of them a manifest a node will refuse AFTER the signature
// verified and the fetch succeeded.
package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/mosquitto"
	"briard.io/shared/manifest"
)

// manifests is every published manifest here: the `.json` files other than the index, which
// is the listing of them and not one of them.
func manifests(t *testing.T) []string {
	t.Helper()
	entries, err := filepath.Glob("*.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, path := range entries {
		if path != manifest.IndexName {
			out = append(out, path)
		}
	}
	if len(out) == 0 {
		t.Fatal("no catalog entries found; this test is vacuous without them")
	}
	return out
}

func TestEveryPublishedEntryParses(t *testing.T) {
	for _, path := range manifests(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m, id, err := manifest.Parse(raw)
			if err != nil {
				t.Fatalf("a node would refuse this entry: %v", err)
			}
			// The filename is what the node requests (`<name>.json`), so a manifest whose name
			// differs is one no `briard app install <name>` can ever reach.
			if want := strings.TrimSuffix(path, ".json"); m.Name != want {
				t.Errorf("entry is served as %q but names itself %q", want, m.Name)
			}
			// The identity is the hash of the bytes as published. Recording it in the log is how
			// a reviewer sees that an edit here is a new service version, not a cosmetic change.
			t.Logf("%s %s -> %s", m.Name, m.Version, id)
			// A trailing newline is a different identity for the same service (see README).
			if strings.HasSuffix(string(raw), "\n") {
				t.Error("entry ends in a newline; the published bytes do not, and the bytes are the identity")
			}
		})
	}
}

// TestIndexListsEveryManifest parses the index with the function a node uses and checks the one
// relation between it and the manifests beside it: an app is installable when its manifest
// exists, so a manifest with no index entry is an installable app no page can show, and an
// entry is marked available exactly when its manifest is here: the index's word is the only
// one a node reads, so the two documents must agree where both are written. An entry with no
// manifest and no mark is the point of the index -- an app a household may ask for.
func TestIndexListsEveryManifest(t *testing.T) {
	idx, err := manifest.ParseIndex(read(t, manifest.IndexName))
	if err != nil {
		t.Fatalf("a node would refuse the index: %v", err)
	}
	published := make(map[string]bool)
	for _, path := range manifests(t) {
		published[strings.TrimSuffix(path, ".json")] = true
	}
	listed := make(map[string]bool, len(idx.Apps))
	for _, a := range idx.Apps {
		listed[a.Name] = true
		// Every published entry carries its mark: the title's initial is a page's fallback for an
		// icon that could not be sourced, not a shape this file ships.
		if a.Icon == "" {
			t.Errorf("%s has no icon", a.Name)
		}
		if a.Available != published[a.Name] {
			t.Errorf("%s: available=%v in the index, manifest beside it=%v", a.Name, a.Available, published[a.Name])
		}
	}
	for name := range published {
		if !listed[name] {
			t.Errorf("manifest %s.json is not listed in the index", name)
		}
	}
	t.Logf("%d apps listed, %d installable", len(idx.Apps), len(published))
}

// TestMosquittoEntryMatchesTheProductsConfig is the cross-document check: this entry and the
// config agent/services renders for it are two files that must agree, written in two languages,
// and nothing at runtime would notice them drifting apart. A port changed here and not there is a
// health gate that can never pass; a mount changed here and not there puts the broker's retained
// state outside the replicated volume, where a failover silently loses it.
func TestMosquittoEntryMatchesTheProductsConfig(t *testing.T) {
	raw, err := os.ReadFile("mosquitto.json")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := m.Primary()
	if p.Port != mosquitto.HealthPort {
		t.Errorf("nodes are told to probe port %d; the config opens %d", p.Port, mosquitto.HealthPort)
	}
	if p.HealthPath != mosquitto.HealthPath {
		t.Errorf("nodes are told to probe %q; the broker's API serves %q", p.HealthPath, mosquitto.HealthPath)
	}
	if p.Mount != mosquitto.DataMount {
		t.Errorf("the replicated volume is bound at %q; the config persists into %q", p.Mount, mosquitto.DataMount)
	}
}

// EVERY ENTRY STATES ITS NETWORKING EXPLICITLY, even though the schema lets silence mean private.
//
// The schema's default is a SAFETY property -- an unknown manifest gets the least capable shape --
// while this is a CURATION rule, and they are not in tension: in a document a human reviews before
// it reaches every household, the deployment shape should be visible rather than inferred from an
// absence. It also catches the failure that produced this test: the field was added, the renderer
// was taught to honour it, and both published entries kept saying nothing -- which would have moved
// Home Assistant onto a private network and taken the mDNS/SSDP discovery it exists for with it.
func TestEveryEntryDeclaresItsNetworking(t *testing.T) {
	for _, path := range manifests(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := manifest.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if m.Network == "" {
			t.Errorf("%s does not say what networking it wants; a published entry must state it, "+
				"even where silence would be valid", path)
		}
	}
}

// THE BROKER IS PRIVATE AND PUBLISHES ONLY MQTT, which is the shape the port-list schema exists to make
// expressible -- and the one entry where getting it wrong is a security regression rather than an
// outage. Its manifest `port` is the MANAGEMENT API, which must reach the guest and nothing else:
// private keeps it off the LAN, publishing 1883 and only 1883 is what the household actually
// reaches, and the front door is not involved because no reverse proxy can serve MQTT.
func TestTheBrokerIsPrivateAndPublishesOnlyMQTT(t *testing.T) {
	m, _, err := manifest.Parse(read(t, "mosquitto.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.HostNetwork() {
		t.Fatal("the broker asks for host networking; its management API would be on the household's LAN")
	}
	if len(m.Ports) != 1 || m.Ports[0] != mosquitto.MQTTPort {
		t.Errorf("published ports = %v, want exactly MQTT (%d)", m.Ports, mosquitto.MQTTPort)
	}
	for _, p := range m.Ports {
		if p == m.Primary().Port {
			t.Errorf("the broker publishes its management port %d to the household", p)
		}
	}
}

// read is the entry's exact bytes -- the thing the identity is taken over.
func read(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// EVERY ENTRY SAYS WHAT IT COSTS: the bytes a node downloads (`size`, the compressed
// layers) and the bytes the image store holds afterwards (`installedSize`, uncompressed). The
// first is a progress bar's denominator, the second the free-space gate's operand; a node can
// learn neither until the pull is over. Measured with skopeo (copy, then sum the manifest's
// layer sizes and the decompressed layers), 2026-09-04.
func TestEveryEntryDeclaresItsSizes(t *testing.T) {
	for _, path := range manifests(t) {
		m, _, err := manifest.Parse(read(t, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if m.Size <= 0 || m.InstalledSize <= 0 {
			t.Errorf("%s: size=%d installedSize=%d; every published entry declares both", path, m.Size, m.InstalledSize)
		}
		if m.InstalledSize < m.Size {
			t.Errorf("%s: installed %d < downloaded %d; swapped?", path, m.InstalledSize, m.Size)
		}
	}
}
