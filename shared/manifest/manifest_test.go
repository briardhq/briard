package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

const digestA = "ghcr.io/home-assistant/home-assistant@sha256:" + hex64
const digestB = "docker.io/library/redis@sha256:" + hex64b
const hex64 = "1111111111111111111111111111111111111111111111111111111111111111"
const hex64b = "2222222222222222222222222222222222222222222222222222222222222222"

func good() Manifest {
	return Manifest{
		Name:    "home-assistant",
		Version: "2026.7.1",
		Containers: []Container{{
			Name:       "ha",
			Image:      digestA,
			Mount:      "/config",
			Primary:    true,
			Port:       8123,
			HealthPath: "/manifest.json",
			Env:        map[string]string{"TZ": "Europe/Athens"},
		}},
	}
}

func raw(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestParseGood(t *testing.T) {
	m, id, err := Parse(raw(t, good()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "home-assistant" || m.Primary().Port != 8123 {
		t.Fatalf("parsed %+v, want the fixture back", m)
	}
	if !strings.HasPrefix(string(id), "sha256:") || len(id) != len("sha256:")+64 {
		t.Fatalf("identity = %q, want sha256:<64 hex>", id)
	}
}

// TestIdentityIsTheBytes: identity is the hash of the EXACT bytes given, not of a re-marshalling.
// Two documents that differ only in whitespace are different signed artifacts and must not
// collide, or a re-encode anywhere in the chain silently mints a new identity for the same
// service (or, worse, the same identity for two different ones).
func TestIdentityIsTheBytes(t *testing.T) {
	compact := raw(t, good())
	spaced := append(append([]byte(nil), compact...), '\n')
	_, id1, err := Parse(compact)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	_, id2, err := Parse(spaced)
	if err != nil {
		t.Fatalf("spaced: %v", err)
	}
	if id1 == id2 {
		t.Fatal("identity ignored a byte difference — it is not hashing the raw document")
	}
	_, again, err := Parse(compact)
	if err != nil || again != id1 {
		t.Fatalf("identity is not stable for identical bytes: %q vs %q (%v)", again, id1, err)
	}
}

// TestParseRejectsUnknownFields: a field this build does not understand may be one that grants a
// capability. Silently ignoring it lets a newer catalog widen what an older node runs, invisibly.
func TestParseRejectsUnknownFields(t *testing.T) {
	if _, _, err := Parse([]byte(`{"name":"x","version":"1","privileged":true,"containers":[]}`)); err == nil {
		t.Fatal("Parse accepted an unknown field")
	}
}

// TestValidateRejects is the security boundary, case by case. Each entry is a way a rotten or
// hostile catalog entry could reach past the schema; all of them must be refused.
func TestValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		bend func(*Manifest)
	}{
		{"empty service name", func(m *Manifest) { m.Name = "" }},
		{"service name with slash", func(m *Manifest) { m.Name = "evil/../../etc" }},
		{"service name with dots", func(m *Manifest) { m.Name = ".." }},
		{"uppercase service name", func(m *Manifest) { m.Name = "HomeAssistant" }},
		{"no containers", func(m *Manifest) { m.Containers = nil }},
		{"container name not a slug", func(m *Manifest) { m.Containers[0].Name = "../escape" }},
		{"tagged image", func(m *Manifest) { m.Containers[0].Image = "ghcr.io/foo/bar:latest" }},
		{"bare image", func(m *Manifest) { m.Containers[0].Image = "redis" }},
		{"short digest", func(m *Manifest) { m.Containers[0].Image = "r@sha256:abc" }},
		{"relative mount", func(m *Manifest) { m.Containers[0].Mount = "config" }},
		{"mount with dotdot", func(m *Manifest) { m.Containers[0].Mount = "/config/../../etc" }},
		{"unclean mount", func(m *Manifest) { m.Containers[0].Mount = "/config/" }},
		{"bad env key", func(m *Manifest) { m.Containers[0].Env = map[string]string{"a b": "c"} }},
		{"port zero", func(m *Manifest) { m.Containers[0].Port = 0 }},
		{"port too high", func(m *Manifest) { m.Containers[0].Port = 70000 }},
		{"health path relative", func(m *Manifest) { m.Containers[0].HealthPath = "healthz" }},
		{"health path dotdot", func(m *Manifest) { m.Containers[0].HealthPath = "/a/../../b" }},
		{"no primary", func(m *Manifest) { m.Containers[0].Primary = false }},
		{"two primaries", func(m *Manifest) {
			c := m.Containers[0]
			c.Name = "second"
			c.Image = digestB
			m.Containers = append(m.Containers, c)
		}},
		{"duplicate container names", func(m *Manifest) {
			c := m.Containers[0]
			c.Primary = false
			c.Image = digestB
			m.Containers = append(m.Containers, c)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := good()
			tc.bend(&m)
			if err := m.Validate(); err == nil {
				t.Fatalf("Validate accepted %s: %+v", tc.name, m)
			}
		})
	}
}

// TestValidateRejectsUnitInjection is the sharpest edge of the boundary. The renderer writes INI,
// so a newline smuggled through ANY rendered field would let a catalog entry append its own
// directives — "PodmanArgs=--privileged" — and grant by injection exactly the capability the
// schema withholds by omission. If this test ever goes green with the check removed, the
// "inexpressible" property is gone and only the wording survives.
func TestValidateRejectsUnitInjection(t *testing.T) {
	const inject = "x\nPodmanArgs=--privileged"
	for _, tc := range []struct {
		name string
		bend func(*Manifest)
	}{
		{"version", func(m *Manifest) { m.Version = inject }},
		{"env value", func(m *Manifest) { m.Containers[0].Env = map[string]string{"TZ": inject} }},
		{"health path", func(m *Manifest) { m.Containers[0].HealthPath = "/ok" + inject }},
		{"carriage return in env", func(m *Manifest) { m.Containers[0].Env = map[string]string{"TZ": "a\rb"} }},
		{"NUL in env", func(m *Manifest) { m.Containers[0].Env = map[string]string{"TZ": "a\x00b"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := good()
			tc.bend(&m)
			if err := m.Validate(); err == nil {
				t.Fatalf("Validate accepted a line break in %s — unit injection is possible", tc.name)
			}
		})
	}
}

// TestAcceptsRealWorldRegistryForms: the shapes a real catalog entry actually carries. The ported
// form is a REGRESSION TEST — the first version of the digest pattern had no `:` in it, so every
// registry on a non-default port was silently refused, and only the integration test running one
// found it.
func TestAcceptsRealWorldRegistryForms(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/home-assistant/home-assistant@sha256:" + hex64,
		"registry.example.com:5000/briard-fixture@sha256:" + hex64,
		"10.0.0.99:5000/briard-fixture@sha256:" + hex64,
		"docker.io/library/redis@sha256:" + hex64,
		"redis@sha256:" + hex64,
		"registry.example.com:5000/a/b/c@sha256:" + hex64,
	} {
		t.Run(image, func(t *testing.T) {
			m := good()
			m.Containers[0].Image = image
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate rejected a legitimate reference %q: %v", image, err)
			}
		})
	}
}

// TestStillRejectsMalformedRegistryForms: widening the pattern for ports must not have opened it
// to references that are not digest-pinned at all, which is the property doing the work.
func TestStillRejectsMalformedRegistryForms(t *testing.T) {
	for _, image := range []string{
		"registry.example.com:5000/foo:latest",                             // tagged, not pinned
		"registry.example.com:notaport/foo@sha256:" + hex64,                // bogus port
		"registry.example.com:5000/foo@sha512:" + hex64,                    // wrong algorithm
		"registry.example.com:5000/foo@sha256:abc",                         // truncated digest
		"registry.example.com:5000/../foo@sha256:" + hex64,                 // path traversal
		"registry.example.com:5000/foo@sha256:" + strings.Repeat("AB", 32), // non-lowercase hex
	} {
		t.Run(image, func(t *testing.T) {
			m := good()
			m.Containers[0].Image = image
			if err := m.Validate(); err == nil {
				t.Fatalf("Validate accepted %q", image)
			}
		})
	}
}

// TestStatelessContainerNeedsNoMount: an empty Mount is legitimate and means "keeps nothing",
// which must not be confused with a validation failure.
func TestStatelessContainerNeedsNoMount(t *testing.T) {
	m := good()
	m.Containers[0].Mount = ""
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a stateless container: %v", err)
	}
}

// TestMultiContainerPod: the two-container shape the quadlet spike proved.
func TestMultiContainerPod(t *testing.T) {
	m := good()
	m.Containers = append(m.Containers, Container{
		Name:  "cache",
		Image: digestB,
		Mount: "/data",
	})
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a two-container pod: %v", err)
	}
	if m.Primary().Name != "ha" {
		t.Fatalf("Primary() = %q, want ha", m.Primary().Name)
	}
}

// THE NETWORK FIELD IS A CLOSED ENUM AND ITS SILENCE MEANS PRIVATE ([B.48](a)). Both halves are
// load-bearing: silence yielding the LESS capable shape is property 2 working as stated, and
// refusing an unrecognised word is what stops a misspelled `"network":"hostt"` from installing,
// running, and finding no devices with nothing anywhere saying why.
func TestNetworkIsAClosedEnumDefaultingToPrivate(t *testing.T) {
	base := func(network string) Manifest {
		m := Manifest{Name: "svc", Version: "1", Network: network, Containers: []Container{{
			Name: "app", Image: digestA, Primary: true, Port: 8080, HealthPath: "/healthz",
		}}}
		return m
	}
	// Absent and explicit-private are the same answer, so no caller can handle them differently.
	for _, network := range []string{"", NetworkPrivate} {
		m := base(network)
		if err := m.Validate(); err != nil {
			t.Errorf("network %q rejected: %v", network, err)
		}
		if m.HostNetwork() {
			t.Errorf("network %q reads as host networking; silence must mean private", network)
		}
	}
	host := base(NetworkHost)
	if err := host.Validate(); err != nil {
		t.Errorf("network %q rejected: %v", NetworkHost, err)
	}
	if !host.HostNetwork() {
		t.Error("an explicit host manifest does not read as host networking")
	}
	for _, bad := range []string{"hostt", "HOST", "bridge", "none", "container:x"} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("network %q validated; an unrecognised mode must be refused, never defaulted", bad)
		}
	}
}

// A manifest written before the field existed still parses, and parses as private -- which is what
// makes "silence means private" a compatibility property as well as a security one.
func TestAManifestWithoutTheFieldParsesAsPrivate(t *testing.T) {
	raw := []byte(`{"name":"svc","version":"1","containers":[{"name":"app",` +
		`"image":"` + digestA + `","primary":true,"port":8080,"healthPath":"/healthz"}]}`)
	m, _, err := Parse(raw)
	if err != nil {
		t.Fatalf("a manifest with no network field did not parse: %v", err)
	}
	if m.HostNetwork() {
		t.Error("a manifest that says nothing about networking got host networking")
	}
}
