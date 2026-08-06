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

// TestNoAutoUpdate: podman must never change image identity behind our back — it would break
// announce-before-act and the health gate. Our upgrade path owns image identity.
func TestNoAutoUpdate(t *testing.T) {
	r := mustRender(t, ha())
	if strings.Contains(r.String(), "AutoUpdate") {
		t.Fatalf("AutoUpdate appears in the rendered set:\n%s", r)
	}
}

// TestOnlyImageUnitsAutoStart: the promoter decides what runs, so nothing but the boot-time
// pre-warm may carry [Install]. A pod or container with [Install] would start itself on a
// SECONDARY node — two nodes writing the same replicated volume is the one outcome the whole
// single-primary design exists to prevent.
func TestOnlyImageUnitsAutoStart(t *testing.T) {
	r := mustRender(t, ha())
	for name, body := range r.Files {
		hasInstall := strings.Contains(body, "[Install]")
		isImage := strings.HasSuffix(name, ".image")
		if hasInstall != isImage {
			t.Fatalf("%s: [Install]=%v but .image=%v\n%s", name, hasInstall, isImage, body)
		}
	}
	if !strings.Contains(r.Files["briard-home-assistant-ha.image"], "WantedBy=multi-user.target") {
		t.Fatal("the pre-warm unit is not boot-time — it must warm every node, not just the primary")
	}
}

// TestChainOrder: data -> pod -> members -> VIP. Naming the members explicitly is required —
// the quadlet spike proved that starting the pod service does not start its containers.
func TestChainOrder(t *testing.T) {
	m := ha()
	m.Containers = append(m.Containers, manifest.Container{Name: "cache", Image: digestB, Mount: "/data"})
	got := Chain(mustRender(t, m))
	want := []string{
		"briard-data.service",
		"briard-home-assistant-pod.service",
		"briard-home-assistant-ha.service",
		"briard-home-assistant-cache.service",
		"briard-vip.service",
	}
	if len(got) != len(want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestStatelessContainerGetsNoVolume: an empty Mount means the container keeps nothing, so it
// must not be handed a directory it will never write.
func TestStatelessContainerGetsNoVolume(t *testing.T) {
	m := ha()
	m.Containers[0].Mount = ""
	r := mustRender(t, m)
	if strings.Contains(r.Files["briard-home-assistant-ha.container"], "Volume=") {
		t.Fatalf("stateless container got a Volume:\n%s", r)
	}
	if dirs := Subdirs(m); len(dirs) != 0 {
		t.Fatalf("Subdirs = %v, want none for a stateless service", dirs)
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
