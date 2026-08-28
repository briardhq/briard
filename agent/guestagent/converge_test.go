package guestagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A manifest the fixture catalog could have signed. Digest-pinned, which is what makes an
// exists-or-pull warm safe (converge.go argues why).
const dummyManifest = `{"name":"dummy","version":"1","containers":[{"name":"app",` +
	`"image":"localhost/briard-dummy@sha256:1111111111111111111111111111111111111111111111111111111111111111",` +
	`"mount":"/data","primary":true,"port":8080,"healthPath":"/healthz"}]}`

const otherManifest = `{"name":"other","version":"1","containers":[{"name":"app",` +
	`"image":"localhost/briard-other@sha256:2222222222222222222222222222222222222222222222222222222222222222",` +
	`"primary":true,"port":9090,"healthPath":"/healthz"}]}`

// convergeExec is a fakeExec taught the two directory listings converge reads and the podman
// image store it asks about. Everything else falls through to fakeExec's defaults.
type convergeExec struct {
	fakeExec
	services  []string // what `ls -1 <manifestDir>` returns; nil = the directory does not exist
	quadlet   []string // what `ls -1 <quadletDir>` returns
	haveImage bool     // whether `podman image exists <ref>` succeeds
	failStart map[string]bool
}

func (c *convergeExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	c.fakeExec.runs = append(c.fakeExec.runs, append([]string{name}, args...))
	switch {
	case name == "ls" && len(args) == 2 && args[1] == manifestDir:
		if c.services == nil {
			return nil, errors.New("exit status 2") // no such directory
		}
		return []byte(strings.Join(c.services, "\n") + "\n"), nil
	case name == "ls" && len(args) == 2 && args[1] == quadletDir:
		return []byte(strings.Join(c.quadlet, "\n") + "\n"), nil
	case name == "podman" && len(args) == 3 && args[0] == "image" && args[1] == "exists":
		if c.haveImage {
			return nil, nil
		}
		return nil, errors.New("exit status 125")
	case name == "systemctl" && len(args) == 2 && args[0] == "start" && c.failStart[args[1]]:
		return []byte("Job failed"), errors.New("exit status 1")
	}
	return nil, nil
}

// ran reports whether the fake saw this exact command line.
func (c *convergeExec) ran(argv ...string) bool {
	for _, r := range c.fakeExec.runs {
		if len(r) == len(argv) {
			same := true
			for i := range r {
				if r[i] != argv[i] {
					same = false
					break
				}
			}
			if same {
				return true
			}
		}
	}
	return false
}

func dummyNode(t *testing.T) *convergeExec {
	t.Helper()
	x := &convergeExec{services: []string{"dummy.json"}, haveImage: true}
	x.fakeExec.files = map[string]string{manifestDir + "/dummy.json": dummyManifest}
	return x
}

// TestConvergeRendersFromTheVolume is the item's whole point ([V3b.3](f)): a node that was never
// told about a service -- never rendered, never cached, never chained -- still runs it, because
// the volume said so. The measured failure was a survivor that promoted and served nothing.
func TestConvergeRendersFromTheVolume(t *testing.T) {
	x := dummyNode(t)
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	for _, f := range []string{"briard-dummy.pod", "briard-dummy-app.container", "briard-dummy-app.image"} {
		if _, ok := x.fakeExec.files[quadletDir+"/"+f]; !ok {
			t.Fatalf("converge did not write %s; wrote %v", f, keys(x.fakeExec.files))
		}
	}
	if !x.ran("systemctl", "daemon-reload") {
		t.Fatal("converge did not reload systemd, so quadlet never generated the units")
	}
	// The pod before its members: the quadlet spike proved starting the pod does not start them.
	if !x.ran("systemctl", "start", "briard-dummy-pod.service") ||
		!x.ran("systemctl", "start", "briard-dummy-app.service") {
		t.Fatalf("converge did not start the service units; ran %v", x.fakeExec.runs)
	}
}

// TestConvergeToNothingIsNotAnError: the shipped zero-service node has no manifest directory at
// all. It must promote, not refuse -- refusing would take the VIP down on every node nobody has
// given a workload to, which is every node at install time.
func TestConvergeToNothingIsNotAnError(t *testing.T) {
	x := &convergeExec{} // services nil => `ls` fails => no such directory
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("a zero-service node must converge successfully to nothing, got: %v", err)
	}
	if x.ran("systemctl", "daemon-reload") == false && len(x.fakeExec.files) > 1 {
		t.Fatal("nothing installed, yet converge wrote service units")
	}
}

// TestConvergeRefusesAnUnusableManifest is the other side of the line above, and the difference
// is deliberate: ABSENT means nothing is installed (a state we ship), while a manifest that is
// present and will not parse means this node cannot honour what the volume says it runs.
// Promoting anyway is how a household silently loses a service -- the exact failure (f) exists
// to end -- so converge fails and the promotion fails with it.
func TestConvergeRefusesAnUnusableManifest(t *testing.T) {
	x := &convergeExec{services: []string{"dummy.json"}, haveImage: true}
	x.fakeExec.files = map[string]string{manifestDir + "/dummy.json": "{not json"}
	err := Converge(context.Background(), x)
	if err == nil {
		t.Fatal("converge accepted a manifest that does not parse — the node would promote and serve nothing")
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("unhelpful error for a corrupt manifest: %v", err)
	}
}

// TestConvergeDoesNotPullAPresentImage is [V3.17]'s doctrine on the failover path: running and
// failing over never need the network. Starting an .image unit IS a registry pull (measured with
// podman's own generator), so a resident image must not have its unit started.
func TestConvergeDoesNotPullAPresentImage(t *testing.T) {
	x := dummyNode(t)
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if x.ran("systemctl", "start", "briard-dummy-app-image.service") {
		t.Fatal("converge started the .image unit for an image already in local storage — that is a registry pull on the promotion path")
	}
}

// TestConvergePullsAMissingImage is the other half, and the pair is what makes the test bite:
// skip-always passes the test above, pull-always passes this one, and only exists-or-pull passes
// both. Reaching this branch means the design was not upheld (install warms, prewarm warms every
// standby) -- but a short wait beats a node that will not come up, and the manifest's digest pins
// the bytes so a pull returns exactly those or fails.
func TestConvergePullsAMissingImage(t *testing.T) {
	x := dummyNode(t)
	x.haveImage = false
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if !x.ran("systemctl", "start", "briard-dummy-app-image.service") {
		t.Fatalf("converge did not fetch an absent image; ran %v", x.fakeExec.runs)
	}
}

// TestConvergeFailsWhenAnAbsentImageCannotBeFetched: the bound on the branch above. If the pull
// cannot happen (no WAN), converge must FAIL rather than start a partial chain -- which takes the
// VIP down and surfaces honestly, instead of promoting a node that cannot serve.
func TestConvergeFailsWhenAnAbsentImageCannotBeFetched(t *testing.T) {
	x := dummyNode(t)
	x.haveImage = false
	x.failStart = map[string]bool{"briard-dummy-app-image.service": true}
	if err := Converge(context.Background(), x); err == nil {
		t.Fatal("converge promoted with an image it could neither find nor fetch")
	}
	if x.ran("systemctl", "start", "briard-dummy-app.service") {
		t.Fatal("converge started the container after the image fetch failed — a partial chain")
	}
}

// TestAServiceThatWillNotStartDoesNotFailConverge is the failure rule ([V3b.3](f)): converge's
// OWN failure demotes, a SERVICE's failure alerts and promotes. A code fault is deterministic, so
// the peer running the identical closure hits it identically and the failover only flaps; and one
// broken service must not take the other N-1 down with it.
func TestAServiceThatWillNotStartDoesNotFailConverge(t *testing.T) {
	x := &convergeExec{services: []string{"dummy.json", "other.json"}, haveImage: true}
	x.fakeExec.files = map[string]string{
		manifestDir + "/dummy.json": dummyManifest,
		manifestDir + "/other.json": otherManifest,
	}
	x.failStart = map[string]bool{"briard-dummy-app.service": true}
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("a crashing service demoted the node: %v", err)
	}
	// ...and the OTHER service still ran. This is the "N-1" half, and without it the test would
	// pass on an implementation that gave up at the first failure.
	if !x.ran("systemctl", "start", "briard-other-app.service") {
		t.Fatalf("one broken service stopped the others from starting; ran %v", x.fakeExec.runs)
	}
}

// TestConvergeRemovesOrphanUnits: converge is the whole truth about what this node runs, not an
// addition to it. A service uninstalled elsewhere, or renamed, leaves unit source behind, and an
// orphan .container is a unit a later converge could start against data that no longer exists.
func TestConvergeRemovesOrphanUnits(t *testing.T) {
	x := dummyNode(t)
	x.quadlet = []string{"briard-gone-app.container", "briard-gone.pod", "user-thing.container"}
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	for _, orphan := range []string{"briard-gone-app.container", "briard-gone.pod"} {
		if !x.ran("rm", "-f", quadletDir+"/"+orphan) {
			t.Fatalf("converge left the orphan %s behind; ran %v", orphan, x.fakeExec.runs)
		}
	}
	// A quadlet file someone else put there is theirs. Owning the whole directory would make
	// installing a service a way to delete a user's own unit.
	if x.ran("rm", "-f", quadletDir+"/user-thing.container") {
		t.Fatal("converge deleted a unit it did not render and does not own")
	}
}

// TestConvergeStopStopsWhatItStarted, in reverse. The service units are NOT chain members, so
// drbd-reactor stops nothing on demote -- without this the containers keep running on a volume
// that is about to be unmounted.
func TestConvergeStopStopsWhatItStarted(t *testing.T) {
	x := dummyNode(t)
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	before := len(x.fakeExec.runs)
	if err := ConvergeStop(context.Background(), x); err != nil {
		t.Fatalf("ConvergeStop: %v", err)
	}
	var stopped []string
	for _, r := range x.fakeExec.runs[before:] {
		if len(r) == 3 && r[0] == "systemctl" && r[1] == "stop" {
			stopped = append(stopped, r[2])
		}
	}
	want := []string{"briard-dummy-app.service", "briard-dummy-pod.service"}
	if fmt.Sprint(stopped) != fmt.Sprint(want) {
		t.Fatalf("stopped %v, want %v — containers must go before their pod, as the promoter unwound", stopped, want)
	}
}

// TestConvergeStopOnANodeThatNeverConverged is the demote path on a node that never promoted, or
// that already stopped. It must be a silent success: a failed ExecStop leaves briard-services
// active, which blocks the unmount queued behind it.
func TestConvergeStopOnANodeThatNeverConverged(t *testing.T) {
	x := &convergeExec{}
	if err := ConvergeStop(context.Background(), x); err != nil {
		t.Fatalf("stopping a node that never converged must be a no-op, got: %v", err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// dummyManifestV2 is the same service at a new version: same name, therefore the same unit names,
// and a different image digest. This is the real upgrade shape (home-assistant 2026.8 -> 2026.9),
// and the one a start alone cannot deliver.
const dummyManifestV2 = `{"name":"dummy","version":"2","containers":[{"name":"app",` +
	`"image":"localhost/briard-dummy@sha256:3333333333333333333333333333333333333333333333333333333333333333",` +
	`"mount":"/data","primary":true,"port":8080,"healthPath":"/healthz"}]}`

// TestConvergeBouncesAServiceWhoseRenderingChanged is the upgrade case. systemd does not restart
// an already-active unit on daemon-reload, so a converge that only ever STARTS would leave the
// old container serving while every file on disk described the new one — green, and wrong. This
// is what the install path's quiesce did inside the maintenance bracket, and it moved here with
// the bracket's deletion.
func TestConvergeBouncesAServiceWhoseRenderingChanged(t *testing.T) {
	x := dummyNode(t)
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	// The upgrade: a new manifest lands on the volume and the node re-converges in place.
	x.fakeExec.files[manifestDir+"/dummy.json"] = dummyManifestV2
	x.quadlet = []string{"briard-dummy.pod", "briard-dummy-app.container", "briard-dummy-app.image"}
	before := len(x.fakeExec.runs)
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("re-Converge: %v", err)
	}
	var stopped []string
	for _, r := range x.fakeExec.runs[before:] {
		if len(r) == 3 && r[0] == "systemctl" && r[1] == "stop" {
			stopped = append(stopped, r[2])
		}
	}
	// The container before the pod: stopping the pod first makes podman kill its members out
	// from under their units, so each lands in `failed` rather than stopping cleanly.
	want := []string{"briard-dummy-app.service", "briard-dummy-pod.service"}
	if fmt.Sprint(stopped) != fmt.Sprint(want) {
		t.Fatalf("an upgraded service was not bounced: stopped %v, want %v", stopped, want)
	}
}

// TestConvergeLeavesAnUnchangedServiceAlone is the half that keeps the one above honest: bouncing
// unconditionally would also pass it. Converge runs on every promotion AND every install, so a
// service nobody touched must not be taken down because a DIFFERENT one was upgraded.
func TestConvergeLeavesAnUnchangedServiceAlone(t *testing.T) {
	x := &convergeExec{services: []string{"dummy.json", "other.json"}, haveImage: true}
	x.fakeExec.files = map[string]string{
		manifestDir + "/dummy.json": dummyManifest,
		manifestDir + "/other.json": otherManifest,
	}
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	x.fakeExec.files[manifestDir+"/dummy.json"] = dummyManifestV2 // only `dummy` is upgraded
	x.quadlet = []string{
		"briard-dummy.pod", "briard-dummy-app.container", "briard-dummy-app.image",
		"briard-other.pod", "briard-other-app.container", "briard-other-app.image",
	}
	before := len(x.fakeExec.runs)
	if err := Converge(context.Background(), x); err != nil {
		t.Fatalf("re-Converge: %v", err)
	}
	for _, r := range x.fakeExec.runs[before:] {
		if len(r) == 3 && r[0] == "systemctl" && r[1] == "stop" && strings.HasPrefix(r[2], "briard-other") {
			t.Fatalf("upgrading `dummy` took `other` down: stopped %s", r[2])
		}
	}
}

// TestServiceForgetRemovesTheManifestAndFlushes: reverting a FRESH install has to remove the
// service's identity from the volume, not just stop its units — under converge the volume is what
// every future promotion, on every node, renders from ([V3b.3](f)).
//
// The `sync -f` is the durable half and is asserted rather than assumed: the fact that has to
// survive a power cut here is the directory ENTRY's removal, the same reason provisionService
// syncs after writing one. Without it a crash inside the writeback window resurrects a service
// the install had already given up on.
func TestServiceForgetRemovesTheManifestAndFlushes(t *testing.T) {
	x := dummyNode(t)
	g := dial(t, x)
	if err := g.ServiceForget(context.Background(), "dummy"); err != nil {
		t.Fatalf("ServiceForget: %v", err)
	}
	if !x.ran("rm", "-f", manifestDir+"/dummy.json") {
		t.Fatalf("the manifest was not removed; ran %v", x.fakeExec.runs)
	}
	if !x.ran("sync", "-f", manifestDir) {
		t.Fatalf("the removal was not flushed to the replicated volume; ran %v", x.fakeExec.runs)
	}
}

// TestConvergeVerbsAreAdvertised: the host gates the whole install on the handshake
// (SupportsServiceConverge), so a verb the dispatch switch handles but the capability list omits
// is invisible — an install would refuse a guest that could in fact do the work.
//
// THE HANDSHAKE IS THE POINT, not scenery: Supports returns true when caps is nil (a guest too old
// to advertise anything), so asserting on an un-handshaken client passes whatever the capability
// list says. That is exactly how this test was wrong the first time it was written.
func TestConvergeVerbsAreAdvertised(t *testing.T) {
	g := dial(t, dummyNode(t))
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.SupportsServiceConverge() {
		t.Fatal("service.converge is handled but not advertised — every install would refuse this guest")
	}
	if !g.Supports(verbServiceForget) {
		t.Fatal("service.forget is handled but not advertised")
	}
}
