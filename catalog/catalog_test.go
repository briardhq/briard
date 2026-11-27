//go:build catalog

// Package catalog checks the app catalog (its own repository), laid here by `nix build
// .#catalog-test` from the pinned `catalog` flake input or one given with --override-input. It
// has no code: the test is the point. The build tag keeps a plain `go test ./...` from compiling
// it, so a checkout without the files reports nothing rather than an unearned pass.
//
// It parses every version file with the same function a node uses, so the schema check happens
// here rather than first on a household's node. It catches the failures cheap here and expensive
// live (an image not digest-pinned, a container name not a slug, a missing or duplicated primary,
// a health path that is not a path), each a manifest a node refuses only after the list verified
// and the fetch succeeded. And it holds the entries to what Briard's code for each app assumes,
// which nothing at runtime would notice drifting apart.
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
	"briard.io/agent/freshrss"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/mosquitto"
	"briard.io/agent/navidrome"
	"briard.io/shared/manifest"
)

// list is the list as the catalog holds it, main.json: each app's newest version there, which
// is what the tests run. latest.json beside it is the catalog's copy of the list nodes are
// served, checked where it is signed.
const list = "main.json"

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

// TestTheListNamesItsVersionFiles parses the list as a node does and checks its ties to the files
// beside it. An app is installable when it has a version file, so one with files and no line is
// one no page can show; an entry is marked available exactly when its directory is here. An
// available line names a file by version and by the sha256 of its bytes -- what a node checks
// before parsing it and compares with the identity it runs -- so a stale line is an install
// refused or an update offered for nothing. An entry with no files and no mark is the point of
// the list: an app a household may ask for.
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
// config agent/apps renders for it must agree across two languages, and nothing at runtime would
// notice them drift. A port changed on one side only is a health gate that can never pass; a
// mount changed on one side only puts the broker's retained state outside the replicated volume,
// where a failover silently loses it.
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
// mint) write into /config, HA's config directory in its image, so the replicated volume mounted
// anywhere else leaves the integration, the token and the household's configuration on the
// container's own disk, gone at the next start. And HA exists to find devices on the household's
// network: off host networking, its mDNS and SSDP discovery sees nothing.
//
// Its own backups are the one nested mount, in the unversioned tier: a second copy of its data,
// not to be snapshotted or backed up again. Nothing else under /config may be a mount: a restore
// from those backups removes every entry of /config but `backups`, and a mount point refuses its
// removal, leaving /config half cleared (homeassistant/backup_restore.py).
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
// its own, and its browser answers only inside its pod -- the reverse proxy is the one way in,
// and it signs.
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
	if m.HostNetwork() {
		t.Errorf("Files is on host networking: network %q", m.Network)
	}
	// The drive is the one thing Files publishes, and only a version that runs it: the same view
	// over SMB, on the drive's port, by a container the agent has to configure. Such a version
	// names the oldest briard that can, so an older one refuses it.
	drive := slices.ContainsFunc(m.Containers, func(c manifest.Container) bool { return c.Name == files.DriveContainer })
	switch {
	case !drive && len(m.Ports) != 0:
		t.Errorf("Files publishes %v and runs no drive: the reverse proxy is its one way in", m.Ports)
	case drive && !slices.Equal(m.Ports, []int{files.DrivePort}):
		t.Errorf("Files with the drive publishes %v, want the drive's port %d alone", m.Ports, files.DrivePort)
	case drive && m.MinBriard == "":
		t.Error("Files with the drive names no minBriard: a briard that predates the drive would start a Samba it cannot configure")
	}
}

// Mealie is reached only through the reverse proxy and told only by briard: private, nothing
// published, since the reverse proxy signs the user in; its data one area at /app/data; and none
// of the settings agent/mealie writes as files set in its manifest, because Mealie lets an
// environment variable win over its settings file, so an entry setting one (the password form,
// the sign-in, the address) would quietly overrule the product.
func TestMealieEntryMatchesWhatTheProductAssumes(t *testing.T) {
	m, _, err := manifest.Parse(current(t, mealie.Name))
	if err != nil {
		t.Fatal(err)
	}
	if m.HostNetwork() || len(m.Ports) != 0 {
		t.Errorf("Mealie is reachable other than through the reverse proxy: network %q, ports %v", m.Network, m.Ports)
	}
	p := m.Primary()
	if p.Name != mealie.Container {
		t.Errorf("Mealie's container is %q; the dashboard runs Mealie's admin tool in %q", p.Name, mealie.Container)
	}
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

// Navidrome's entry is what agent/navidrome configures: it reads the settings file written into
// its own directory, its library is the files-tier folder the registry names as the household's,
// and it is reached only through the reverse proxy, which it trusts by address -- so private, with
// nothing published. None of the settings briard writes is set in the entry, because Navidrome
// lets an environment variable win over its settings file. And it names the oldest briard that
// configures it: an older one would run it ungated, its first-admin form open to the LAN.
func TestNavidromeEntryMatchesWhatTheProductAssumes(t *testing.T) {
	m, _, err := manifest.Parse(current(t, navidrome.Name))
	if err != nil {
		t.Fatal(err)
	}
	if m.HostNetwork() || len(m.Ports) != 0 {
		t.Errorf("Navidrome is reachable other than through the reverse proxy: network %q, ports %v", m.Network, m.Ports)
	}
	if m.MinBriard == "" {
		t.Error("Navidrome names no minBriard: a briard that predates it would run it ungated")
	}
	p := m.Primary()
	if got, want := p.Env["ND_CONFIGFILE"], apps.AppMount+"/navidrome.toml"; got != want || navidrome.ConfigPath != apps.AppDir(navidrome.Name)+"/navidrome.toml" {
		t.Errorf("Navidrome reads its settings from %q; agent/navidrome writes them at %q", got, navidrome.ConfigPath)
	}
	if !slices.Contains(p.Mounts, manifest.Mount{Tier: manifest.TierFiles, Dir: navidrome.Music, Path: "/music"}) || p.Env["ND_MUSICFOLDER"] != "" {
		t.Errorf("Navidrome's library is not the files-tier %q at the image's /music: %v", navidrome.Music, p.Mounts)
	}
	for k := range p.Env {
		for _, owned := range []string{"ND_EXTAUTH", "ND_REVERSEPROXY", "ND_ENABLEINSIGHTSCOLLECTOR", "ND_ENABLESHARING"} {
			if strings.HasPrefix(k, owned) {
				t.Errorf("Navidrome's entry sets %s, which would overrule what briard tells it", k)
			}
		}
	}
}

// FreshRSS's entry is what agent/freshrss and the dashboard assume: installed by its image's own
// entrypoint in HTTP sign-in with the API on and `owner` as the default user, its state in the
// data tier where the dashboard reads and keeps it, and reached only through the reverse proxy,
// which it trusts by the address agent/freshrss's file names -- so private, with nothing
// published, and no trust, sign-in or OIDC of the entry's own. It names the oldest briard that
// writes that file: an older one would start it with the image's own trust and no hook, its pages
// refusing everyone and its installer's user nobody's.
func TestFreshRSSEntryMatchesWhatTheProductAssumes(t *testing.T) {
	m, _, err := manifest.Parse(current(t, freshrss.Name))
	if err != nil {
		t.Fatal(err)
	}
	if m.HostNetwork() || len(m.Ports) != 0 {
		t.Errorf("FreshRSS is reachable other than through the reverse proxy: network %q, ports %v", m.Network, m.Ports)
	}
	if m.MinBriard == "" {
		t.Error("FreshRSS names no minBriard: a briard that predates it would start it with no hook")
	}
	p := m.Primary()
	if p.Name != freshrss.Container {
		t.Errorf("FreshRSS's container is %q; the dashboard runs FreshRSS's tools in %q", p.Name, freshrss.Container)
	}
	if !slices.Contains(p.Mounts, manifest.Mount{Tier: manifest.TierData, Dir: "data", Path: freshrss.Data}) {
		t.Errorf("FreshRSS's state is not the data tier at %s: %v", freshrss.Data, p.Mounts)
	}
	install := " " + p.Env["FRESHRSS_INSTALL"] + " "
	for _, want := range []string{" --auth-type http_auth ", " --default-user owner ", " --api-enabled "} {
		if !strings.Contains(install, want) {
			t.Errorf("FreshRSS's install lacks %q: %q", strings.TrimSpace(want), p.Env["FRESHRSS_INSTALL"])
		}
	}
	for k := range p.Env {
		for _, owned := range []string{"TRUSTED_PROXY", "OIDC_", "LISTEN"} {
			if strings.HasPrefix(k, owned) {
				t.Errorf("FreshRSS's entry sets %s, which would overrule what briard tells it", k)
			}
		}
	}
}

// Every entry states its networking, though the schema lets silence mean private. The default is
// a safety property (an unknown manifest gets the least capable shape); this is a curation rule,
// and they do not conflict: in a document a human reviews before it reaches every household, the
// deployment shape should be visible, not inferred from an absence. It also catches an entry that
// means host networking and says nothing: silence moves Home Assistant onto a private network and
// takes the mDNS/SSDP discovery it exists for with it.
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

// The broker is private and publishes only MQTT: the shape the port-list schema exists to express,
// and the one entry where getting it wrong is a security regression, not an outage. Its manifest
// `port` is the management API, which must reach the guest and nothing else: private keeps it off
// the LAN, 1883 and only 1883 is what the household reaches, and the reverse proxy is not
// involved: none can serve MQTT.
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

// Every entry says what it costs: the bytes a node downloads (`size`, compressed layers), a
// progress bar's denominator, and the bytes the image store then holds (`installedSize`,
// uncompressed), the free-space gate's operand; a node can learn neither until the pull is over.
// Measured with skopeo (copy, then sum the manifest's layer sizes and the decompressed layers).
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
// by it; an image without one builds with nix's placeholder hash and fails every test that pulls
// it, found only once the tests read the catalog as published.
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
