//go:build catalog

// Package catalog checks the published catalog, which lives in its own repository and is laid
// here by `nix build .#catalog-test` from the pinned `catalog` flake input (or another one given
// with --override-input). It has no code: the test is the point. The build tag keeps a plain
// `go test ./...` from compiling it, so a checkout without the files reports nothing rather than
// a pass it never earned.
//
// It parses every version file with the SAME function a node uses, so the schema check that
// would otherwise happen for the first time on a household's node happens here instead. The
// failures it is built to catch are the cheap ones that are expensive live: an image reference
// that is not digest-pinned, a container name that is not a slug, a missing or duplicated
// primary, a health path that is not a path — every one of them a manifest a node will refuse
// AFTER the list verified and the fetch succeeded. And it holds the entries to what Briard's own
// code for each app assumes, which nothing at runtime would notice drifting apart.
package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/apps"
	"briard.io/agent/files"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/mosquitto"
	"briard.io/shared/manifest"
)

// list is the list a node reads.
const list = "latest.json"

// versionFiles is every published version of every app: `<app>/<version>.json`.
func versionFiles(t *testing.T) []string {
	t.Helper()
	out, err := filepath.Glob("*/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no version files found; this test is vacuous without them")
	}
	return out
}

func TestEveryVersionFileParses(t *testing.T) {
	for _, path := range versionFiles(t) {
		t.Run(path, func(t *testing.T) {
			raw := read(t, path)
			m, id, err := manifest.Parse(raw)
			if err != nil {
				t.Fatalf("a node would refuse this entry: %v", err)
			}
			// The path is what a list line names (`<name>/<version>.json`), so a file whose
			// name or version differs is one no line can reach, or reaches as another app.
			if dir, ver := filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".json"); m.Name != dir || m.Version != ver {
				t.Errorf("served as %s %s but names itself %s %s", dir, ver, m.Name, m.Version)
			}
			// The identity is the hash of the bytes as published. Recording it in the log is how
			// a reviewer sees that an edit here is a new app version, not a cosmetic change.
			t.Logf("%s %s -> %s", m.Name, m.Version, id)
			// A trailing newline is a different identity for the same app (see the README).
			if strings.HasSuffix(string(raw), "\n") {
				t.Error("entry ends in a newline; the published bytes do not, and the bytes are the identity")
			}
		})
	}
}

// TestTheListNamesItsVersionFiles parses the list with the function a node uses and checks what
// ties it to the files beside it: an app is installable when it has a version file, so an app
// with files and no line is one no page can show, and an entry is marked available exactly
// when its directory is here. An available line names one of its files by version and by the
// sha256 of its bytes -- the check a node makes before it parses the file, and the identity it
// compares with the one it runs, so a stale line is an install refused, or an update offered for
// nothing. An entry with no files and no mark is the point of the list: an app a household may
// ask for.
func TestTheListNamesItsVersionFiles(t *testing.T) {
	idx, err := manifest.ParseIndex(read(t, list))
	if err != nil {
		t.Fatalf("a node would refuse the list: %v", err)
	}
	published := make(map[string]bool)
	for _, path := range versionFiles(t) {
		published[filepath.Dir(path)] = true
	}
	listed := make(map[string]bool, len(idx.Apps))
	for _, a := range idx.Apps {
		listed[a.Name] = true
		// Every entry carries its mark: the title's initial is a page's fallback for an icon
		// that could not be sourced, not a shape this file ships.
		if a.Icon == "" {
			t.Errorf("%s has no icon", a.Name)
		}
		if a.Available != published[a.Name] {
			t.Errorf("%s: available=%v in the list, version files beside it=%v", a.Name, a.Available, published[a.Name])
		}
		if a.Available {
			m, id, err := manifest.Parse(read(t, filepath.Join(a.Name, a.Version+".json")))
			switch {
			case err != nil:
				t.Errorf("%s %s: %v", a.Name, a.Version, err)
			case a.Identity != id || a.Version != m.Version:
				t.Errorf("%s: the list names %q %s, the file is %q %s", a.Name, a.Version, a.Identity, m.Version, id)
			}
		}
	}
	for name := range published {
		if !listed[name] {
			t.Errorf("%s has version files but is not in the list", name)
		}
	}
	t.Logf("%d apps listed, %d installable", len(idx.Apps), len(published))
}

// current is the version file the list names for an app: what a node installs now, and what
// Briard's code for the app has to agree with. Older versions agreed with the code of their day.
func current(t *testing.T, name string) []byte {
	t.Helper()
	idx, err := manifest.ParseIndex(read(t, list))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range idx.Apps {
		if a.Name == name && a.Available {
			return read(t, filepath.Join(name, a.Version+".json"))
		}
	}
	t.Fatalf("the list names no version of %s", name)
	return nil
}

// TestMosquittoEntryMatchesTheProductsConfig is the cross-document check: this entry and the
// config agent/apps renders for it are two files that must agree, written in two languages,
// and nothing at runtime would notice them drifting apart. A port changed here and not there is a
// health gate that can never pass; a mount changed here and not there puts the broker's retained
// state outside the replicated volume, where a failover silently loses it.
func TestMosquittoEntryMatchesTheProductsConfig(t *testing.T) {
	m, _, err := manifest.Parse(current(t, mosquitto.Name))
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
	if want := []manifest.Mount{{Tier: manifest.TierData, Dir: "data", Path: mosquitto.DataMount}}; !slices.Equal(p.Mounts, want) {
		t.Errorf("the broker mounts %v; the config persists into %q, in its data", p.Mounts, mosquitto.DataMount)
	}
}

// TestHomeAssistantEntryMatchesWhatTheProductAssumes is HA's half of the same check, against what
// agent/hass takes for granted rather than a config it renders. Its scripts (plant.py, the token
// mint) write into /config, Home Assistant's config directory in its image, so an entry mounting
// the replicated volume anywhere else leaves the integration, the token and the household's whole
// configuration on the container's own disk, gone at the next start. And HA exists to find the
// devices on the household's network: off host networking its mDNS and SSDP discovery sees nothing.
//
// ITS OWN BACKUPS ARE THE ONE NESTED MOUNT, in the unversioned tier: a second copy of its data,
// not to be snapshotted or backed up again. Nothing else under /config may be a mount -- a
// restore from one of those backups removes every entry of /config but `backups`, and a mount
// point refuses its removal, leaving /config half cleared (homeassistant/backup_restore.py).
func TestHomeAssistantEntryMatchesWhatTheProductAssumes(t *testing.T) {
	m, _, err := manifest.Parse(current(t, hass.Name))
	if err != nil {
		t.Fatal(err)
	}
	want := []manifest.Mount{
		{Tier: manifest.TierData, Dir: "config", Path: "/config"},
		{Tier: manifest.TierUnversioned, Dir: "backups", Path: "/config/backups"},
	}
	if p := m.Primary(); !slices.Equal(p.Mounts, want) {
		t.Errorf("Home Assistant mounts %v; agent/hass writes into /config, in its data, and only backups may sit apart", p.Mounts)
	}
	if !m.HostNetwork() {
		t.Error("Home Assistant is off host networking; its discovery would see nothing on the household's LAN")
	}
}

// Files' entry is what agent/files configures: it reads the config written into its own
// directory, keeps its state in its cache (its index and users are disposable), owns no data of
// its own, and answers only inside its pod -- the door is the one way in, and it signs.
func TestFilesEntryMatchesWhatTheProductAssumes(t *testing.T) {
	m, _, err := manifest.Parse(current(t, files.Name))
	if err != nil {
		t.Fatal(err)
	}
	p := m.Primary()
	if got, want := p.Env["FILEBROWSER_CONFIG"], apps.AppMount+"/config.yaml"; got != want {
		t.Errorf("Files reads its config from %q; agent/files writes it at %q", got, want)
	}
	for _, mt := range p.Mounts {
		if mt.Tier != manifest.TierCache {
			t.Errorf("Files mounts %v; its own state is disposable and belongs in the cache", mt)
		}
	}
	if m.HostNetwork() || len(m.Ports) != 0 {
		t.Errorf("Files is reachable other than through the door: network %q, ports %v", m.Network, m.Ports)
	}
}

// MEALIE IS REACHED ONLY THROUGH THE DOOR AND TOLD ONLY BY BRIARD. Private, with nothing published:
// the door is what signs the user in. Its data is one area at /app/data. And its manifest sets
// none of the settings agent/mealie writes as files: Mealie lets an environment variable win over
// its settings file, so an entry that set one -- the password form, the sign-in, the address --
// would quietly overrule the product.
func TestMealieEntryMatchesWhatTheProductAssumes(t *testing.T) {
	m, _, err := manifest.Parse(current(t, mealie.Name))
	if err != nil {
		t.Fatal(err)
	}
	if m.HostNetwork() || len(m.Ports) != 0 {
		t.Errorf("Mealie is reachable other than through the door: network %q, ports %v", m.Network, m.Ports)
	}
	p := m.Primary()
	if want := []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/app/data"}}; !slices.Equal(p.Mounts, want) {
		t.Errorf("Mealie mounts %v, want its data at /app/data", p.Mounts)
	}
	for k := range p.Env {
		for _, owned := range []string{"ALLOW_PASSWORD_LOGIN", "REVERSE_PROXY_AUTH", "BASE_URL", "OIDC_", "LDAP_", "ALLOW_SIGNUP"} {
			if strings.HasPrefix(k, owned) {
				t.Errorf("Mealie's entry sets %s, which would overrule what briard tells it", k)
			}
		}
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
	for _, path := range versionFiles(t) {
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
	m, _, err := manifest.Parse(current(t, mosquitto.Name))
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
	for _, path := range versionFiles(t) {
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

// imageHashes is the catalog's catalog-images.json, laid beside the version files: the hash nix
// checks each pulled image against, keyed on the image ref, one per arch.
const imageHashes = "catalog-images.json"

// TestEveryImageHasItsHash: every image a version file names has its amd64 hash. The tests pull
// an entry's image by it, and an image without one builds with nix's placeholder hash and fails
// every test that pulls it -- found only after the version is published, when the tests read the
// catalog as published.
func TestEveryImageHasItsHash(t *testing.T) {
	var hashes map[string]map[string]string
	if err := json.Unmarshal(read(t, imageHashes), &hashes); err != nil {
		t.Fatalf("%s: %v", imageHashes, err)
	}
	for _, path := range versionFiles(t) {
		m, _, err := manifest.Parse(read(t, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, c := range m.Containers {
			if h := hashes[c.Image]["amd64"]; !strings.HasPrefix(h, "sha256-") {
				t.Errorf("%s: %s has no amd64 hash in %s (got %q)", path, c.Image, imageHashes, h)
			}
		}
	}
}
