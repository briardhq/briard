package quadlet

import (
	"strings"
	"testing"

	"briard.io/shared/manifest"
)

const digestA = "ghcr.io/home-assistant/home-assistant@sha256:1111111111111111111111111111111111111111111111111111111111111111"
const digestB = "docker.io/library/redis@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func ha() manifest.Manifest {
	return manifest.Manifest{
		Name:    "home-assistant",
		Version: "2026.7.1",
		Containers: []manifest.Container{{
			Name: "ha", Image: digestA, Mount: "/config",
			Primary: true, Port: 8123, HealthPath: "/manifest.json",
			Env: map[string]string{"TZ": "Europe/Athens"},
		}},
	}
}

func mustRender(t *testing.T, m manifest.Manifest) Rendered {
	t.Helper()
	r, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return r
}

func TestRenderShape(t *testing.T) {
	r := mustRender(t, ha())
	for _, f := range []string{"briard-home-assistant.pod", "briard-home-assistant-ha.container", "briard-home-assistant-ha.image"} {
		if _, ok := r.Files[f]; !ok {
			t.Fatalf("missing %s; got:\n%s", f, r)
		}
	}
	c := r.Files["briard-home-assistant-ha.container"]
	for _, want := range []string{
		"Image=" + digestA,
		"Pod=briard-home-assistant.pod",
		"Pull=never",
		"Volume=/var/lib/briard/home-assistant/ha:/config",
		"Environment=TZ=Europe/Athens",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("container unit missing %q:\n%s", want, c)
		}
	}
}

// TestPromotionNeverPulls is the failover-critical trap. A .container that names its image as
// `Image=foo.image` gains an AUTOMATIC dependency on the pull unit, which turns promotion into a
// multi-GB cold load on exactly the path warm-standby exists to protect. The
// container must name the digest and set Pull=never, so a cold node fails fast instead of
// fetching — the same refusal briard-converge already makes.
func TestPromotionNeverPulls(t *testing.T) {
	r := mustRender(t, ha())
	c := r.Files["briard-home-assistant-ha.container"]
	if strings.Contains(c, ".image") {
		t.Fatalf("container references a .image unit — promotion would become a pull:\n%s", c)
	}
	if !strings.Contains(c, "Pull=never") {
		t.Fatalf("container does not set Pull=never — a cold node would pull at promotion:\n%s", c)
	}
}

// TestContainerSupervisesItself: the service units are NOT promoter chain members ([V3b.3](f)),
// so drbd-reactor neither restarts nor watches them. A container that dies must be brought back
// by its own unit or it stays dead with nothing noticing — the recovery the promoter used to
// provide, in the only place left to put it.
//
// `always`, not `on-failure`: a service container has no legitimate exit, so exiting 0 is exactly
// as dead as exiting 1. Asserting the exact value rather than mere presence is the point — the
// weaker policy is the plausible-looking wrong answer here, and it is also the one quadlet
// generates for the pod, so it could arrive by imitation.
func TestContainerSupervisesItself(t *testing.T) {
	r := mustRender(t, ha())
	c := r.Files["briard-home-assistant-ha.container"]
	if !strings.Contains(c, "\n[Service]\n") {
		t.Fatalf("container unit has no [Service] section — quadlet has nothing to pass through:\n%s", c)
	}
	if !strings.Contains(c, "Restart=always") {
		t.Fatalf("container unit does not set Restart=always — a crashed service would stay dead:\n%s", c)
	}
	// Past systemd's default start-rate limit (5 per 10s), so a crash-loop keeps retrying
	// instead of latching to `failed` on a transient cause.
	if !strings.Contains(c, "RestartSec=5") {
		t.Fatalf("container unit does not space its restarts — a crash-loop would latch to failed:\n%s", c)
	}
}

// TestNoAutoUpdate: podman must never change image identity behind our back — it would break
// announce-before-act and the health gate. Our upgrade path owns image identity.
func TestNoAutoUpdate(t *testing.T) {
	r := mustRender(t, ha())
	if strings.Contains(r.String(), "AutoUpdate") {
		t.Fatalf("AutoUpdate appears in the rendered set:\n%s", r)
	}
}

// TestNothingAutoStarts: NO rendered unit may carry [Install]. A pod or container with one would
// start itself on a SECONDARY node — two nodes writing the same replicated volume is the one
// outcome the whole single-primary design exists to prevent. An .image unit with one is the
// regression the 2026-08-29 nightly caught: starting it is an unconditional `podman image pull`,
// so systemd warming it behind our back
// pulls an image that is already local, fails on a node with no registry, and makes the next
// `switch-to-configuration switch` exit 4 — which the agent reads as a failed OS upgrade.
//
// This test was the inverse until 2026-08-29 (it REQUIRED [Install] on .image), which is why it
// held while the nightly's os-reboot and os-upgrade both rolled back. Warming is a caller's job,
// guarded by `podman image exists`; see ServiceWarm/warmImage.
func TestNothingAutoStarts(t *testing.T) {
	r := mustRender(t, ha())
	for name, body := range r.Files {
		if strings.Contains(body, "[Install]") {
			t.Fatalf("%s carries [Install] — systemd, not a guarded caller, would start it:\n%s", name, body)
		}
	}
	// The pre-warm unit must still EXIST and name its image: it is what ServiceWarm starts when
	// the guard says the image is genuinely absent.
	img := r.Files["briard-home-assistant-ha.image"]
	if !strings.Contains(img, "[Image]") || !strings.Contains(img, "Image=") {
		t.Fatalf("the pre-warm unit no longer names an image — nothing could warm a cold node:\n%s", img)
	}
}

// TestUnitOrder: the pod, then its members. Naming the members explicitly is required — the
// quadlet spike proved that starting the pod service does not start its containers.
//
// This used to assert a promoter start-list (data -> pod -> members -> VIP), which [V3b.3](f)
// retired along with the function that built it: the chain is static and these units are not
// members of it. What survives is the property that actually mattered — the order converge starts
// them in.
func TestUnitOrder(t *testing.T) {
	m := ha()
	m.Containers = append(m.Containers, manifest.Container{Name: "cache", Image: digestB, Mount: "/data"})
	got := mustRender(t, m).Units
	want := []string{
		"briard-home-assistant-pod.service",
		"briard-home-assistant-ha.service",
		"briard-home-assistant-cache.service",
	}
	if len(got) != len(want) {
		t.Fatalf("units = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("units[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestStatelessContainerGetsNoVolume: an empty Mount means the container keeps nothing, so it
// must not be handed a directory it will never write.
//
// Deliberately NOT the ha() fixture: `home-assistant` is a name the renderer knows, and it always
// carries the control-channel binds (see TestControlChannelIsKeyedOnTheServiceName). This
// assertion is about statelessness, so it needs a service the product holds no knowledge about.
func TestStatelessContainerGetsNoVolume(t *testing.T) {
	m := ha()
	m.Name = "sample-app"
	m.Containers[0].Mount = ""
	r := mustRender(t, m)
	if strings.Contains(r.Files["briard-sample-app-ha.container"], "Volume=") {
		t.Fatalf("stateless container got a Volume:\n%s", r)
	}
	if dirs := Subdirs(m); len(dirs) != 0 {
		t.Fatalf("Subdirs = %v, want none for a stateless service", dirs)
	}
}

// TestControlChannelIsKeyedOnTheServiceName: Home Assistant's primary container gets the two
// binds its control channel needs (agent/hass), and nothing else does.
//
// The manifest cannot ask for a host bind — the schema refuses them so a catalog entry cannot
// reach the host — so these come from the product, keyed on the catalog name. The negative half
// is the one that matters: an identical manifest under any other name must render an identical
// unit WITHOUT them, or "service-specific by design" would be a comment rather than a property.
func TestControlChannelIsKeyedOnTheServiceName(t *testing.T) {
	binds := []string{
		"Volume=/run/briard/hass:/briard:ro",
		"Volume=/run/briard/hass/run:/etc/services.d/home-assistant/run:ro",
	}
	got := mustRender(t, ha()).Files["briard-home-assistant-ha.container"]
	for _, b := range binds {
		if !strings.Contains(got, b) {
			t.Fatalf("home-assistant is missing %q:\n%s", b, got)
		}
	}

	other := ha()
	other.Name = "sample-app"
	got = mustRender(t, other).Files["briard-sample-app-ha.container"]
	if strings.Contains(got, "/run/briard/hass") {
		t.Fatalf("a service that is not home-assistant got the control channel:\n%s", got)
	}

	// And only the PRIMARY container: the mint runs inside the one container that is Home
	// Assistant, not beside it.
	side := ha()
	side.Containers = append(side.Containers, manifest.Container{Name: "cache", Image: digestB})
	got = mustRender(t, side).Files["briard-home-assistant-cache.container"]
	if strings.Contains(got, "/run/briard/hass") {
		t.Fatalf("a non-primary container got the control channel:\n%s", got)
	}
}

// TestSubdirsAreFlat: per-container storage is plain subdirectories of ONE subvolume. Nested
// subvolumes would break data.restore outright (`btrfs subvolume delete` refuses on a subvolume
// containing nested ones), so this shape is a requirement, not a layout preference.
func TestSubdirsAreFlat(t *testing.T) {
	m := ha()
	m.Containers = append(m.Containers, manifest.Container{Name: "cache", Image: digestB, Mount: "/data"})
	dirs := Subdirs(m)
	if len(dirs) != 2 || dirs[0] != "ha" || dirs[1] != "cache" {
		t.Fatalf("Subdirs = %v, want [ha cache]", dirs)
	}
	for _, d := range dirs {
		if strings.Contains(d, "/") {
			t.Fatalf("subdir %q is not flat", d)
		}
	}
	if DataPath("home-assistant", "ha") != "/var/lib/briard/home-assistant/ha" {
		t.Fatalf("DataPath = %q", DataPath("home-assistant", "ha"))
	}
}

// TestDeterministic: two renders of the same manifest must be byte-identical, because every node
// renders independently from the replicated manifest and a survivor's units must match the
// primary's. Map iteration order is the obvious way to lose this.
func TestDeterministic(t *testing.T) {
	m := ha()
	m.Containers[0].Env = map[string]string{"TZ": "Europe/Athens", "A": "1", "Z": "26", "M": "13"}
	first := mustRender(t, m).String()
	for i := 0; i < 20; i++ {
		if got := mustRender(t, m).String(); got != first {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
}

// TestSpecifiersEscaped: a literal '%' in a value is systemd specifier syntax and would expand
// to something the catalog never wrote. Silent corruption of what the service was told.
func TestSpecifiersEscaped(t *testing.T) {
	m := ha()
	m.Containers[0].Env = map[string]string{"FMT": "100%done %n"}
	r := mustRender(t, m)
	if !strings.Contains(r.Files["briard-home-assistant-ha.container"], "Environment=FMT=100%%done %%n") {
		t.Fatalf("percent not escaped:\n%s", r.Files["briard-home-assistant-ha.container"])
	}
}

// TestRenderRefusesInvalid: the belt to Parse's braces. Rendering an unvalidated manifest is how
// an injected line would reach a unit file.
func TestRenderRefusesInvalid(t *testing.T) {
	m := ha()
	m.Containers[0].Env = map[string]string{"X": "a\nPodmanArgs=--privileged"}
	if _, err := Render(m); err == nil {
		t.Fatal("Render accepted a manifest carrying a unit-injection payload")
	}
}
