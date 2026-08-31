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

func TestEveryPublishedEntryParses(t *testing.T) {
	entries, err := filepath.Glob("*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no catalog entries found; this test is vacuous without them")
	}
	for _, path := range entries {
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
			// differs is one no `briard service install <name>` can ever reach.
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
	paths, err := filepath.Glob("*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no catalog entries found; this test is vacuous without them")
	}
	for _, path := range paths {
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

// THE BROKER IS PRIVATE AND PUBLISHES ONLY MQTT, which is the shape [B.48](a) exists to make
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
