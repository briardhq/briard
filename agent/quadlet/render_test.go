package quadlet

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"briard.io/shared/manifest"
)

const digestA = "ghcr.io/home-assistant/home-assistant@sha256:1111111111111111111111111111111111111111111111111111111111111111"
const digestB = "docker.io/library/redis@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func ha() manifest.Manifest {
	return manifest.Manifest{
		Name:    "home-assistant",
		Version: "2026.7.1",
		Containers: []manifest.Container{{
			Name: "ha", Image: digestA, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "ha", Path: "/config"}},
			Primary: true, Port: 8123, HealthPath: "/manifest.json",
			Env: map[string]string{"TZ": "Europe/Athens"},
		}},
	}
}

func mustRender(t *testing.T, m manifest.Manifest) Rendered {
	t.Helper()
	r, err := Render(m, "127.0.0.1")
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
		"Volume=/var/lib/briard/data/home-assistant/ha:/config",
		"Environment=TZ=Europe/Athens",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("container unit missing %q:\n%s", want, c)
		}
	}
}

// TestPromotionNeverPulls is the failover-critical trap: a .container naming its image as
// `Image=foo.image` gains an automatic dependency on the pull unit, turning promotion into a
// multi-GB cold load on exactly the path warm-standby exists to protect. The container names the
// digest and sets Pull=never, so starting it never fetches: a missing image is converge's to
// pull, by digest, before it starts the units (AppWarm/warmImage).
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

// TestContainerSupervisesItself: the app units are not promoter chain members, so drbd-reactor
// neither restarts nor watches them; a container that dies must be brought back by its own unit
// or it stays dead with nothing noticing -- the recovery the promoter gives its chain members, in
// the only place left to put it. `always`, not `on-failure`: an app container has no
// legitimate exit, so exiting 0 is as dead as exiting 1. The exact value is asserted, not mere
// presence, because the weaker policy is the plausible wrong answer and the one quadlet generates
// for the pod, so it could arrive by imitation.
func TestContainerSupervisesItself(t *testing.T) {
	r := mustRender(t, ha())
	c := r.Files["briard-home-assistant-ha.container"]
	if !strings.Contains(c, "\n[Service]\n") {
		t.Fatalf("container unit has no [Service] section — quadlet has nothing to pass through:\n%s", c)
	}
	if !strings.Contains(c, "Restart=always") {
		t.Fatalf("container unit does not set Restart=always — a crashed app would stay dead:\n%s", c)
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

// TestNothingAutoStarts: no rendered unit may carry [Install]. A pod or container with one would
// start itself on a secondary node — two nodes writing the same replicated volume, the one
// outcome the single-primary design exists to prevent. An .image unit with one is no safer:
// starting it is an unconditional `podman image pull`, so systemd warming it behind our back
// pulls an image already local, fails on a node with no registry, and makes the next
// `switch-to-configuration switch` exit 4 — read by the agent as a failed OS upgrade. Warming is
// a caller's job, guarded by `podman image exists` (AppWarm/warmImage).
func TestNothingAutoStarts(t *testing.T) {
	r := mustRender(t, ha())
	for name, body := range r.Files {
		if strings.Contains(body, "[Install]") {
			t.Fatalf("%s carries [Install] — systemd, not a guarded caller, would start it:\n%s", name, body)
		}
	}
	// The pre-warm unit must still exist and name its image: it is what AppWarm restarts when
	// the guard says the image is genuinely absent.
	img := r.Files["briard-home-assistant-ha.image"]
	if !strings.Contains(img, "[Image]") || !strings.Contains(img, "Image=") {
		t.Fatalf("the pre-warm unit no longer names an image — nothing could warm a cold node:\n%s", img)
	}
}

// TestImagePullIsBounded: the .image unit must carry a TimeoutStartSec. Without one there is no
// bound at all, invisibly: quadlet generates Type=oneshot, for which systemd disables
// TimeoutStartSec= by default, so a missing line means "wait forever", not "the default". converge
// starts this unit on the promotion path, and nixosTest/cold-converge-pull.nix measured the cost:
// a node Primary with no VIP, no apps and nothing in `failed`, for as long as a registry cares to
// dribble bytes.
//
// The duration is asserted only as a floor, on purpose: the number is a judgement that may move,
// and pinning it would make this test a copy of the constant, not a check on it. What must not
// move is that it stays generous: an expiry throws away every byte fetched (image-pull-resume.nix
// measured 1.43x on the wire for one interrupted pull), so a bound short enough to feel
// responsive re-downloads until something gives up.
func TestImagePullIsBounded(t *testing.T) {
	r := mustRender(t, ha())
	img := r.Files["briard-home-assistant-ha.image"]
	want := "TimeoutStartSec=" + strconv.Itoa(int(ImagePullTimeout.Seconds()))
	if !strings.Contains(img, want) {
		t.Fatalf("the pre-warm unit is unbounded — a oneshot with no TimeoutStartSec waits forever:\n%s", img)
	}
	if ImagePullTimeout < 20*time.Minute {
		t.Fatalf("ImagePullTimeout is %s: too short to be safe. An expiry loses all progress, so a "+
			"bound near a slow household's real pull time never converges", ImagePullTimeout)
	}
	// Sized by the manifest: an entry that states its download gets the allowance plus the bytes
	// at the floor bitrate (Home Assistant's 622 MB ~22 minutes, a 10 MB broker the allowance plus
	// 15 seconds), never less than the allowance, so a small image is not given seconds for a pull
	// that is mostly registry round-trips.
	sized := ha()
	sized.Size, sized.InstalledSize = 621628919, 2486168064
	img = mustRender(t, sized).Files["briard-home-assistant-ha.image"]
	if got, want := PullTimeout(sized.Size), 5*time.Minute+994*time.Second; got != want {
		t.Fatalf("PullTimeout(622 MB) = %s, want %s", got, want)
	}
	if !strings.Contains(img, "TimeoutStartSec="+strconv.Itoa(int(PullTimeout(sized.Size).Seconds()))) {
		t.Fatalf("a sized entry did not get its own bound:\n%s", img)
	}
	if got := PullTimeout(9981691); got != 5*time.Minute+15*time.Second {
		t.Fatalf("PullTimeout(10 MB) = %s, want the allowance plus 15 s", got)
	}
	if PullAllowance < 5*time.Minute || PullBitrate > 10_000_000 {
		t.Fatalf("allowance %s / bitrate %d: a tighter bound expires layers on ordinary links", PullAllowance, PullBitrate)
	}
	// The bound and the sweep ship together, so they are asserted together. A pull downloads its
	// compressed layers into /var/tmp and deletes them only on completion, so a unit killed by a
	// timeout without a private tmp abandons its partial download at every expiry -- a slow ENOSPC
	// on the guest's OS disk. The timeout without this is worse than neither.
	if !strings.Contains(img, "PrivateTmp=true") {
		t.Fatalf("the pre-warm unit has a timeout but no private tmp — every expiry would abandon "+
			"its partial image in /var/tmp:\n%s", img)
	}
}

// TestUnitOrder: the pod, then its members, in the order converge starts them (the promoter
// chain is static and they are not in it). The members must be named explicitly: starting the
// pod's systemd service does not start its containers.
func TestUnitOrder(t *testing.T) {
	m := ha()
	m.Containers = append(m.Containers, manifest.Container{Name: "cache", Image: digestB, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "cache", Path: "/data"}}})
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

// TestStatelessContainerGetsNoVolume: no mounts means the container keeps nothing, so it must not
// be handed a directory it will never write. Deliberately not named home-assistant: the renderer
// knows that name and always gives it the control-channel binds
// (TestControlChannelIsKeyedOnTheAppName), so statelessness needs an app the product knows nothing
// about.
func TestStatelessContainerGetsNoVolume(t *testing.T) {
	m := ha()
	m.Name = "sample-app"
	m.Containers[0].Mounts = nil
	r := mustRender(t, m)
	if strings.Contains(r.Files["briard-sample-app-ha.container"], "Volume=") {
		t.Fatalf("stateless container got a Volume:\n%s", r)
	}
	if dirs := Subdirs(m); len(dirs) != 0 {
		t.Fatalf("Subdirs = %v, want none for a stateless app", dirs)
	}
}

// TestControlChannelIsKeyedOnTheAppName: Home Assistant's primary container gets the two binds
// its control channel needs (agent/hass) and its capabilities, and nothing else does. The schema
// refuses host binds so a catalog entry cannot reach the host, so these come from the product,
// keyed on the catalog name. The negative half is the one that matters: the same manifest under
// any other name must render without them, or "app-specific by design" is a comment, not a
// property.
func TestControlChannelIsKeyedOnTheAppName(t *testing.T) {
	binds := []string{
		"Volume=/run/briard/home-assistant:/briard:ro",
		"Volume=/run/briard/home-assistant/run:/etc/services.d/home-assistant/run:ro",
		"AddCapability=NET_RAW NET_ADMIN",
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
	if strings.Contains(got, "/run/briard/home-assistant") {
		t.Fatalf("an app that is not home-assistant got the control channel:\n%s", got)
	}
	if strings.Contains(got, "AddCapability=") {
		t.Fatalf("an app that is not home-assistant got a capability:\n%s", got)
	}

	// And only the primary container: the mint runs inside the one container that is Home
	// Assistant, not beside it.
	side := ha()
	side.Containers = append(side.Containers, manifest.Container{Name: "cache", Image: digestB})
	got = mustRender(t, side).Files["briard-home-assistant-cache.container"]
	if strings.Contains(got, "/run/briard/home-assistant") {
		t.Fatalf("a non-primary container got the control channel:\n%s", got)
	}
}

// TestMountsAreDirectoriesOfTheTiersAreas: each mount is a plain directory of the app's area in
// its tier, never its own subvolume (data.restore would break: `btrfs subvolume delete` refuses a
// subvolume containing nested ones), and Subdirs names each once, with its tier. A nested mount
// renders after its parent whatever the manifest order, or the parent's bind would land on top of
// it. Only a container that mounts the data takes the History's members at its start.
func TestMountsAreDirectoriesOfTheTiersAreas(t *testing.T) {
	m := ha()
	m.Containers[0].Mounts = []manifest.Mount{
		{Tier: manifest.TierUnversioned, Dir: "backups", Path: "/config/backups"},
		{Tier: manifest.TierData, Dir: "config", Path: "/config"},
		{Tier: manifest.TierFiles, Dir: "media", Path: "/media"},
	}
	m.Containers = append(m.Containers, manifest.Container{Name: "worker", Image: digestB, Mounts: []manifest.Mount{
		{Tier: manifest.TierCache, Dir: "thumbs", Path: "/cache"},
		{Tier: manifest.TierFiles, Dir: "media", Path: "/media"},
	}})
	want := []string{"cache/thumbs", "data/config", "files/media", "unversioned/backups"}
	if got := Subdirs(m); !slices.Equal(got, want) {
		t.Fatalf("Subdirs = %v, want %v", got, want)
	}
	r := mustRender(t, m)
	ha := r.Files["briard-home-assistant-ha.container"]
	parent := strings.Index(ha, "Volume=/var/lib/briard/data/home-assistant/config:/config\n")
	child := strings.Index(ha, "Volume=/var/lib/briard/unversioned/home-assistant/backups:/config/backups\n")
	if parent < 0 || child < 0 || child < parent {
		t.Fatalf("the nested mount is missing or renders before its parent:\n%s", ha)
	}
	if !strings.Contains(ha, "Volume=/var/lib/briard/files/home-assistant/media:/media\n") {
		t.Fatalf("the files mount is missing:\n%s", ha)
	}
	worker := r.Files["briard-home-assistant-worker.container"]
	if !strings.Contains(worker, "Volume=/var/lib/briard/cache/home-assistant/thumbs:/cache\n") ||
		!strings.Contains(worker, "Volume=/var/lib/briard/files/home-assistant/media:/media\n") {
		t.Fatalf("the worker's mounts are missing:\n%s", worker)
	}
	if !strings.Contains(ha, "--app-starting=") || strings.Contains(worker, "--app-starting=") {
		t.Fatalf("the start hook belongs to the container that mounts the data, and only to it")
	}
	if DataRoot("home-assistant") != "/var/lib/briard/data/home-assistant" {
		t.Fatalf("DataRoot = %q", DataRoot("home-assistant"))
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
// to something the catalog never wrote. Silent corruption of what the app was told.
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
	if _, err := Render(m, "127.0.0.1"); err == nil {
		t.Fatal("Render accepted a manifest carrying a unit-injection payload")
	}
}

// TestBrokerRendersItsConfigAndItsData: mosquitto renders as a plain unit plus the one bind the
// product holds for it — its config, read-only, over the image's baked one — and, as with the
// control channel, the same manifest under another name renders without it. It is here, not only
// in agent/apps, because this is where a bind reaches a unit file: a registry returning the right
// string and a renderer dropping it would pass every agent/apps test and leave the broker on the
// config it ships with — persistence off, management API on every interface.
func TestBrokerRendersItsConfigAndItsData(t *testing.T) {
	m := manifest.Manifest{
		Name:    "mosquitto",
		Version: "2.1.2",
		Containers: []manifest.Container{{
			Name: "broker", Image: digestB, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "broker", Path: "/mosquitto/data"}},
			Primary: true, Port: 9883, HealthPath: "/api/v1/listeners",
		}},
	}
	got := mustRender(t, m).Files["briard-mosquitto-broker.container"]
	for _, want := range []string{
		"Volume=/run/briard/mosquitto/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro",
		"Volume=/var/lib/briard/data/mosquitto/broker:/mosquitto/data",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("mosquitto is missing %q:\n%s", want, got)
		}
	}

	other := m
	other.Name = "sample-app"
	if got := mustRender(t, other).Files["briard-sample-app-broker.container"]; strings.Contains(got, "/run/briard/mosquitto") {
		t.Fatalf("an app that is not mosquitto got the broker's config:\n%s", got)
	}
}

// Files renders as root, with the view the agent keeps (slave propagation, so later binds reach
// it) and its own directory -- and the same manifest under any other name renders none of that.
func TestFilesRendersAsRootOverTheView(t *testing.T) {
	m := manifest.Manifest{
		Name:    "files",
		Version: "2.0.2",
		Containers: []manifest.Container{{
			Name: "app", Image: digestB, Mounts: []manifest.Mount{{Tier: manifest.TierCache, Dir: "state", Path: "/home/filebrowser/data"}},
			Primary: true, Port: 80, HealthPath: "/health",
		}},
	}
	got := mustRender(t, m).Files["briard-files-app.container"]
	for _, want := range []string{
		"Volume=/run/briard/files-view:/srv:rslave",
		"Volume=/run/briard/files:/briard:ro",
		"Volume=/var/lib/briard/cache/files/state:/home/filebrowser/data",
		"User=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Files is missing %q:\n%s", want, got)
		}
	}
	other := m
	other.Name = "sample-app"
	got = mustRender(t, other).Files["briard-sample-app-app.container"]
	for _, never := range []string{"files-view", "User="} {
		if strings.Contains(got, never) {
			t.Fatalf("an app that is not Files got %q:\n%s", never, got)
		}
	}
}

// The pod's networking is the one decision this package makes. These are the three shapes it can
// take, asserted on the rendered bytes because that is what podman reads.
func TestPodNetworkingFollowsTheManifest(t *testing.T) {
	svc := func(network string) manifest.Manifest {
		return manifest.Manifest{Name: "svc", Version: "1", Network: network, Containers: []manifest.Container{{
			Name: "app", Image: digestA, Primary: true, Port: 8080, HealthPath: "/healthz",
		}}}
	}
	// Host: the pod is the guest's namespace, and no address is written into it at all.
	r, err := Render(svc(manifest.NetworkHost), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	pod := r.Files["briard-svc.pod"]
	if !strings.Contains(pod, "Network=host") {
		t.Errorf("host-networked pod = %q, want Network=host", pod)
	}
	if strings.Contains(pod, "IP=") {
		t.Errorf("host-networked pod carries an address: %q", pod)
	}

	// Private with an address: the node's own choice, pinned, so a recreated container returns to
	// the same place and the routing table cannot go stale behind it.
	r, err = Render(svc(""), "10.12.7.2")
	if err != nil {
		t.Fatal(err)
	}
	pod = r.Files["briard-svc.pod"]
	if !strings.Contains(pod, "Network="+PodNetwork) || !strings.Contains(pod, "IP=10.12.7.2") {
		t.Errorf("private pod = %q, want the named network and the pinned address", pod)
	}
	if strings.Contains(pod, "Network=host") {
		t.Errorf("a manifest that said nothing about networking got the host's namespace: %q", pod)
	}
	if r.Address != "10.12.7.2" {
		t.Errorf("Address = %q, want what the caller allocated", r.Address)
	}

	// Private with no address: unpinned, for callers that need names and digests rather than
	// something to start. Still a private network -- an address-less render must never silently
	// become host networking, which is the one way this could grant a capability by accident.
	r, err = Render(svc(manifest.NetworkPrivate), "")
	if err != nil {
		t.Fatal(err)
	}
	pod = r.Files["briard-svc.pod"]
	if strings.Contains(pod, "Network=host") || strings.Contains(pod, "IP=") {
		t.Errorf("address-less private pod = %q, want the named network and no address", pod)
	}
}

// Images renders the pre-warm units and nothing that depends on an address -- which is what lets a
// node that is not serving, and so has allocated nothing, hold a private app's images.
func TestImagesNeedsNoAddress(t *testing.T) {
	m := manifest.Manifest{Name: "svc", Version: "1", Containers: []manifest.Container{{
		Name: "app", Image: digestA, Primary: true, Port: 8080, HealthPath: "/healthz",
	}}}
	r, err := Images(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ImageUnits) != 1 || r.ImageRefs["briard-svc-app-image.service"] != digestA {
		t.Errorf("Images = %+v, want the one pre-warm unit and its digestA", r)
	}
	for name := range r.Files {
		if !strings.HasSuffix(name, ".image") {
			t.Errorf("Images rendered %s; a prewarm must write nothing that needs an address", name)
		}
	}
}

// TestSnapshotMemberNamesTheSeries is the one place a member's path is asserted as a literal, as
// it must be: everywhere else derives the name from this function, so only here can the format
// itself be caught wrong. Three load-bearing properties: the trigger is in the name, so a human
// scanning `.snapshots` tells an update point from a clock one without opening a sidecar; the
// timestamp is UTC and fixed-width, so within one trigger lexical order is chronological (across
// triggers it is not: TestSnapshotMemberTimeIsTheOrder); and the name is one path element — a
// member is a btrfs subvolume, and a `/` would put it somewhere else entirely.
func TestSnapshotMemberNamesTheSeries(t *testing.T) {
	at := time.Date(2026, 9, 22, 10, 30, 5, 0, time.UTC)
	got := SnapshotMember("home-assistant", TriggerAppUpdateBefore, at)
	want := "/var/lib/briard/data/.snapshots/home-assistant-app-update-before-20260922T103005Z"
	if got != want {
		t.Errorf("SnapshotMember = %q, want %q", got, want)
	}
	if s := SnapshotSidecar(got); s != want+".json" {
		t.Errorf("SnapshotSidecar = %q, want it beside the member", s)
	}
	// A non-UTC clock must not move the name: two nodes in different zones taking a member at the
	// same instant have to agree on what it is called, or the ring forks per node.
	east := time.FixedZone("UTC+9", 9*3600)
	if other := SnapshotMember("home-assistant", TriggerAppUpdateBefore, at.In(east)); other != want {
		t.Errorf("a non-UTC clock renamed the member: %q, want %q", other, want)
	}
	if strings.Count(strings.TrimPrefix(got, "/var/lib/briard/data/.snapshots/"), "/") != 0 {
		t.Errorf("a member must be one path element: %q", got)
	}
}

// TestSnapshotMemberTimeIsTheOrder, and ⚠️ the name is not. Lexical order over member names is
// chronological only within one trigger, so a test comparing cases that share one trigger would
// "prove" the false rule: the trigger sits between the app and the stamp, so every `-clock-`
// member sorts before every `-start-` one whatever their times, and a reader taking the lexically
// last member as the newest would get an old start's age. Asserted is what readers may rely on:
// the stamp orders within a trigger, SnapshotMemberTime orders across them, and the two disagree.
func TestSnapshotMemberTimeIsTheOrder(t *testing.T) {
	base := time.Date(2026, 9, 22, 10, 30, 5, 0, time.UTC)
	earlier := SnapshotMember("ha", TriggerClock, base)
	later := SnapshotMember("ha", TriggerClock, base.Add(time.Second))
	if !(earlier < later) {
		t.Errorf("within one trigger the stamp must order: %q then %q", earlier, later)
	}
	yearOver := SnapshotMember("ha", TriggerClock, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if !(later < yearOver) {
		t.Errorf("the stamp breaks across a year: %q then %q", later, yearOver)
	}
	// The trap, asserted so nobody re-derives the false rule: an older start member sorts after
	// a newer clock member, purely because of where the trigger sits.
	oldStart := SnapshotMember("ha", TriggerStart, base.Add(-48*time.Hour))
	newClock := SnapshotMember("ha", TriggerClock, base)
	if !(newClock < oldStart) {
		t.Fatal("the name format changed; re-check every reader that orders members")
	}
	to, _ := SnapshotMemberTime(oldStart)
	tn, _ := SnapshotMemberTime(newClock)
	if !to.Before(tn) {
		t.Error("SnapshotMemberTime does not order what the name misorders")
	}
}

// TestDataContainerTakesARingMemberAtStart: the generic hook. Every catalogued app
// gets a member at container start, which is the only boundary visible from outside the
// container -- Home Assistant adds its own internal restarts through the inbound channel, and
// nothing else can.
func TestDataContainerTakesARingMemberAtStart(t *testing.T) {
	m := manifest.Manifest{
		Name: "home-assistant", Version: "1",
		Containers: []manifest.Container{
			{Name: "app", Image: digestA, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/config"}}, Primary: true, Port: 8123, HealthPath: "/x"},
			{Name: "sidecar", Image: digestA}, // no Mount: shares nothing of its own
		},
	}
	r, err := Render(m, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	app := r.Files["briard-home-assistant-app.container"]
	var line string
	for _, l := range strings.Split(app, "\n") {
		if strings.HasPrefix(l, "ExecStartPre=") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the data container takes no member at start:\n%s", app)
	}
	// ⚠️ The leading `-`: without it a snapshot that fails takes the household's app down
	// with it, and a ring missing a member is nothing next to an app that will not start.
	if !strings.HasPrefix(line, "ExecStartPre=-") {
		t.Errorf("%q can fail the app; systemd needs the `-` prefix", line)
	}
	// systemd requires an absolute path for the first word of an Exec line; a bare command name
	// makes the unit fail to load rather than fail to run.
	if !strings.Contains(line, "=-/") {
		t.Errorf("%q does not name an absolute path", line)
	}
	if !strings.HasSuffix(line, "--app-starting=home-assistant") {
		t.Errorf("%q does not name the app whose member it takes", line)
	}
	// An app's other containers share its one subvolume, so a hook on each of them would take
	// a member of the same bytes at every start.
	if side := r.Files["briard-home-assistant-sidecar.container"]; strings.Contains(side, "ExecStartPre=") {
		t.Errorf("a container with no data of its own takes a member:\n%s", side)
	}

	// And the stop half: a clean stop records that this app's data was flushed, so
	// the next start's member can say what its bytes are. Without it every start after a container
	// stop would read as crash-consistent -- which is what a promotion after a dead primary is,
	// and what an ordinary restart is not.
	var stop string
	for _, l := range strings.Split(app, "\n") {
		if strings.HasPrefix(l, "ExecStopPost=") {
			stop = l
		}
	}
	if stop == "" {
		t.Fatalf("the data container records nothing when it stops:\n%s", app)
	}
	// ExecStopPost, never ExecStop: it runs after the container is down and is the only place
	// systemd exposes $SERVICE_RESULT, which is the whole evidence for the claim.
	if !strings.HasPrefix(stop, "ExecStopPost=-/") || !strings.HasSuffix(stop, "--app-stopped=home-assistant") {
		t.Errorf("%q is not a `-`-prefixed absolute call naming this app", stop)
	}
	if side := r.Files["briard-home-assistant-sidecar.container"]; strings.Contains(side, "ExecStopPost=") {
		t.Errorf("a container with no data of its own claims its stop flushed something:\n%s", side)
	}
}

// TestThePodOutlivesItsContainers: with quadlet's default exit policy a crashed container
// takes its pod down, systemd stops the container unit as the pod's dependent, and that stop
// suppresses Restart=always -- a crashed Home Assistant would stay down. Every network shape gets
// it.
func TestThePodOutlivesItsContainers(t *testing.T) {
	for _, network := range []string{manifest.NetworkHost, ""} {
		m := manifest.Manifest{Name: "svc", Version: "1", Network: network, Containers: []manifest.Container{{
			Name: "app", Image: digestA, Primary: true, Port: 8080, HealthPath: "/healthz",
		}}}
		r, err := Render(m, "10.12.7.5")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Files["briard-svc.pod"], "\nExitPolicy=continue\n") {
			t.Errorf("network %q: the pod stops with its last container: %q", network, r.Files["briard-svc.pod"])
		}
	}
}

// An app that declares a minimum is capped at MemoryLimitFactor times it in RAM and at the
// minimum in swap, inside its own unit; one that declares nothing is not capped at all -- an
// unmeasured entry must not be guessed into a cage.
func TestContainerMemoryLimitFollowsTheDeclaredMinimum(t *testing.T) {
	m := ha()
	m.MinMemoryMB = 512
	c := mustRender(t, m).Files["briard-home-assistant-ha.container"]
	svc := c[strings.Index(c, "\n[Service]\n"):]
	for _, want := range []string{"\nMemoryMax=2048M\n", "\nMemorySwapMax=512M\n"} {
		if !strings.Contains(svc, want) {
			t.Errorf("[Service] lacks %q:\n%s", strings.TrimSpace(want), svc)
		}
	}
	c = mustRender(t, ha()).Files["briard-home-assistant-ha.container"]
	if strings.Contains(c, "MemoryMax=") || strings.Contains(c, "MemorySwapMax=") {
		t.Errorf("an app with no declared minimum was capped:\n%s", c)
	}
}
