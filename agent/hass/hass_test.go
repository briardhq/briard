package hass

import (
	"context"
	"errors"
	"strings"
	"testing"

	"briard.io/shared/manifest"
)

const image = "ghcr.io/home-assistant/home-assistant@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func ha() manifest.Manifest {
	return manifest.Manifest{
		Name:    Name,
		Version: "2026.7.1",
		Containers: []manifest.Container{{
			Name: "app", Image: image, Mount: "/config",
			Primary: true, Port: 8123, HealthPath: "/manifest.json",
		}},
	}
}

// fake records every command and every write, and can be told to fail one command.
type fake struct {
	files map[string]string
	runs  [][]string
	fail  func(name string, args []string) error
	// extracted is what a `podman cp` out of the image yields. "" models the copy producing
	// nothing readable -- a directory, or a path that moved.
	extracted string
}

// withImage is a fake whose image hands over a plausible s6 run script.
func withImage() *fake {
	return &fake{extracted: "#!/usr/bin/with-contenv bashio\nexec python3 -m homeassistant\n"}
}

func (f *fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	if f.fail != nil {
		if err := f.fail(name, args); err != nil {
			return []byte("boom"), err
		}
	}
	// `mv` is modelled, because atomic-rename is the property under test: a caller that
	// forgets it would leave the content at the .new path and this fake would show it.
	if name == "mv" && len(args) == 3 && args[0] == "-f" {
		if v, ok := f.files[args[1]]; ok {
			delete(f.files, args[1])
			f.files[args[2]] = v
		}
	}
	// `podman cp` is modelled too, because what it produced is now checked: the extraction has
	// to come back as a readable, non-empty file or it is refused. extracted lets a test say
	// what the image handed over -- including nothing at all.
	if name == "podman" && len(args) == 3 && args[0] == "cp" {
		if f.extracted != "" {
			f.WriteFile(args[2], []byte(f.extracted))
		}
	}
	return nil, nil
}

func (f *fake) WriteFile(path string, data []byte) error {
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

func (f *fake) ReadFile(path string) ([]byte, error) {
	if v, ok := f.files[path]; ok {
		return []byte(v), nil
	}
	return nil, errors.New("no such file")
}

func (f *fake) ran(argv ...string) bool {
	for _, r := range f.runs {
		if len(r) != len(argv) {
			continue
		}
		same := true
		for i := range r {
			if r[i] != argv[i] {
				same = false
			}
		}
		if same {
			return true
		}
	}
	return false
}

// TestPrepareMaterialisesTheChannel: after one Prepare the node holds everything the container
// will bind — a token, the mint, our wrapper, and the image's own run extracted out of the
// digest the manifest pins.
func TestPrepareMaterialisesTheChannel(t *testing.T) {
	f := withImage()
	if err := Prepare(context.Background(), f, ha(), 1883); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, p := range []string{TokenPath, scriptPath, planterPath, wrapperPath, implPath} {
		if _, ok := f.files[p]; !ok {
			t.Fatalf("%s was not written (files: %v)", p, f.files)
		}
		// Every write lands through a rename, so nothing may be left at the staging name —
		// truncating a live bind-mount source in place is what this avoids.
		if _, ok := f.files[p+".new"]; ok {
			t.Fatalf("%s was left at its staging name; the rename did not happen", p)
		}
	}
	if !f.ran("podman", "cp", extractCtr+":"+s6Run, originalPath+".new") {
		t.Fatalf("the image's own run was not extracted; ran: %v", f.runs)
	}
	if !f.ran("podman", "create", "--name", extractCtr, image) {
		t.Fatalf("extraction did not stage the manifest's pinned image; ran: %v", f.runs)
	}
	// Created and removed, never started: nothing from the image executes here.
	if f.ran("podman", "start", extractCtr) {
		t.Fatalf("the extraction container was started")
	}
	if !f.ran("podman", "rm", "-f", extractCtr) {
		t.Fatalf("the extraction container was not removed; ran: %v", f.runs)
	}
	if !f.ran("chmod", "700", Dir) {
		t.Fatalf("%s was not made private; ran: %v", Dir, f.runs)
	}
	if !f.ran("chmod", "0600", TokenPath+".new") {
		t.Fatalf("the token was not written 0600; ran: %v", f.runs)
	}
	if !f.ran("chmod", "0755", wrapperPath+".new") {
		t.Fatalf("the wrapper was not made executable; ran: %v", f.runs)
	}
}

// TestPrepareIsOnlyForHomeAssistant: the knowledge in this package is keyed on the catalog name,
// so every other service must come out of Prepare untouched — no directory, no podman, nothing.
func TestPrepareIsOnlyForHomeAssistant(t *testing.T) {
	m := ha()
	m.Name = "sample-app"
	f := &fake{}
	if err := Prepare(context.Background(), f, m, 1883); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.runs) != 0 || len(f.files) != 0 {
		t.Fatalf("a service that is not %s was touched: runs=%v files=%v", Name, f.runs, f.files)
	}
	if v := Volumes(m, m.Containers[0]); v != nil {
		t.Fatalf("Volumes for %q = %v, want none", m.Name, v)
	}
}

// TestTokenIsEnsuredNotRotated: converge runs on every install and every promotion. A value that
// changed under a container already running would break every consumer until something restarted
// Home Assistant, so a token that is already there survives. Rotation comes from tmpfs — a guest
// reboot clears /run, and nothing is running then either.
func TestTokenIsEnsuredNotRotated(t *testing.T) {
	f := withImage()
	ctx := context.Background()
	if err := Prepare(ctx, f, ha(), 1883); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	first := f.files[TokenPath]
	if err := Prepare(ctx, f, ha(), 1883); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if f.files[TokenPath] != first {
		t.Fatalf("the token rotated under a running container: %q -> %q", first, f.files[TokenPath])
	}
}

// TestShortTokenIsReplaced: a truncated or empty file is not a token we wrote, and minting the
// empty string as a credential is the failure worth being explicit about. (The mint refuses it
// too — this is the node-side half of the same guard.)
func TestShortTokenIsReplaced(t *testing.T) {
	f := withImage()
	f.files = map[string]string{TokenPath: "\n"}
	if err := Prepare(context.Background(), f, ha(), 1883); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := strings.TrimSpace(f.files[TokenPath]); len(got) != 2*tokenBytes {
		t.Fatalf("token = %q (%d chars), want %d hex chars", got, len(got), 2*tokenBytes)
	}
}

// TestPrepareFailsLoudly: the alternative to an error here is a rendered unit naming a bind
// source that does not exist. Measured: the runtime CREATES the missing source as a root-owned
// directory and then refuses to start the container onto a file — which also poisons the path
// for the next Prepare, since a file has to go where a directory now sits. A converge that fails
// is strictly better than one that half-succeeds and cannot recover.
func TestPrepareFailsLoudly(t *testing.T) {
	f := &fake{fail: func(name string, args []string) error {
		if name == "podman" && len(args) > 0 && args[0] == "cp" {
			return errors.New("exit status 125")
		}
		return nil
	}}
	err := Prepare(context.Background(), f, ha(), 1883)
	if err == nil {
		t.Fatal("a failed extraction was swallowed")
	}
	if !strings.Contains(err.Error(), s6Run) {
		t.Fatalf("the error does not say what could not be extracted: %v", err)
	}
}

// TestVolumesAreTheTwoBinds: one read-only mount for the whole directory, plus the shadow over
// the image's own run script. Both sources are outside /config, so HA's restore wipe — which
// clears the config directory wholesale — never sees them.
func TestVolumesAreTheTwoBinds(t *testing.T) {
	got := Volumes(ha(), ha().Containers[0])
	want := []string{
		"/run/briard/hass:/briard:ro",
		"/run/briard/hass/run:/etc/services.d/home-assistant/run:ro",
	}
	if len(got) != len(want) {
		t.Fatalf("Volumes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Volumes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, v := range got {
		if strings.HasPrefix(v, "/config") || strings.Contains(v, ":/config") {
			t.Fatalf("%q puts a bind source or target under /config, which HA's restore wipes", v)
		}
	}
}

// TestWrapperHandsOverToTheImagesOwnScript: the wrapper must exec the EXTRACTED original, never a
// copy of Home Assistant's launch line written by us. Authoring one is the failure mode §6.4
// names — it drifts the day upstream moves the s6 furniture, and it drifts on every node at once.
func TestWrapperHandsOverToTheImagesOwnScript(t *testing.T) {
	if !strings.Contains(wrapperSource, "exec "+mountPoint+"/run.original") {
		t.Fatalf("the wrapper does not hand over to the extracted original:\n%s", wrapperSource)
	}
	if strings.Contains(wrapperSource, "python3 -m homeassistant") {
		t.Fatalf("the wrapper launches Home Assistant itself instead of relaying:\n%s", wrapperSource)
	}
	// The mint can never fail the service.
	if !strings.Contains(wrapperSource, "|| true") {
		t.Fatalf("the mint is not fenced; a token failure would stop HA from starting:\n%s", wrapperSource)
	}
	// The paths the wrapper uses are the ones the binds actually deliver.
	for _, p := range []string{mountPoint + "/ensure-token.py", mountPoint + "/token"} {
		if !strings.Contains(wrapperSource, p) {
			t.Fatalf("the wrapper does not use %s:\n%s", p, wrapperSource)
		}
	}
}

// TestMintAsksForAdminAndPrunes: the two properties of the mint that are not obvious from
// reading it run. Admin is what makes the token able to ACT — measured, reading config entries
// does not need it, but `/api/services/...` returns 401 without it — and a system user is only
// admin through that group. The prune is what revokes a token resurrected by a backup restore.
func TestMintAsksForAdminAndPrunes(t *testing.T) {
	for _, want := range []string{"GROUP_ID_ADMIN", "async_remove_refresh_token", "TOKEN_TYPE_SYSTEM"} {
		if !strings.Contains(scriptSource, want) {
			t.Fatalf("the mint does not use %s", want)
		}
	}
	// hass.async_stop() is a no-op on a HomeAssistant that was never started, so it flushes
	// nothing; the final-write event is what the delayed store save is waiting for. Measured
	// against 2026.7.1 — without it the auth store was simply never written.
	if !strings.Contains(scriptSource, "EVENT_HOMEASSISTANT_FINAL_WRITE") {
		t.Fatal("the mint does not flush the auth store; nothing would be persisted")
	}
	// THE BOOTSTRAP IS BORROWED, NOT COPIED, and this is the assertion that keeps it that way.
	// This package necessarily spans two Home Assistant releases -- an upgrade mints under the old
	// image to take the readiness baseline and under the new one afterwards -- and the setup HA
	// needs before its auth store will load CHANGED between them: 2026 added a
	// device_registry.async_setup that the store's own load now requires, and releases before it
	// have no such function. A hand-written copy therefore raises AttributeError on one end of the
	// range and RuntimeError on the other, which is exactly how this file first failed. Running
	// HA's own `scripts/auth.run_command` and passing our mint as its callback makes the
	// version-sensitive part someone else's to maintain, and leaves this file with no version
	// branch at all.
	if !strings.Contains(scriptSource, "ha_auth.run_command(") {
		t.Fatal("the mint no longer borrows HA's own bootstrap; a copy of it drifts per release")
	}
	// Checked on the IMPORTS, not on prose: the comment above necessarily names the symbols the
	// copy used, so a substring search over the whole file would match its own explanation.
	for _, copied := range []string{
		"from homeassistant.auth import auth_manager_from_config",
		"from homeassistant.helpers import device_registry",
		"from homeassistant.core import HomeAssistant",
	} {
		if strings.Contains(scriptSource, copied) {
			t.Fatalf("the mint still imports %q, so it is rebuilding the bootstrap by hand", copied)
		}
	}
}

// TestExtractionRefusesWhatIsNotAScript: `podman cp` copies a DIRECTORY as happily as a file, and
// a native s6-rc service IS a directory holding a `run`. So a layout change that turned this path
// into a directory would otherwise mount a directory over Home Assistant's run script — the same
// unstartable container a missing bind source produces. Anything that does not read back as a
// non-empty file is refused.
func TestExtractionRefusesWhatIsNotAScript(t *testing.T) {
	for _, tc := range []struct{ name, extracted string }{
		{"nothing readable", ""},
		{"an empty file", ""},
	} {
		f := &fake{extracted: tc.extracted}
		err := Prepare(context.Background(), f, ha(), 1883)
		if err == nil {
			t.Fatalf("%s: extraction was accepted", tc.name)
		}
		if _, ok := f.files[originalPath]; ok {
			t.Fatalf("%s: a bad extraction was installed anyway", tc.name)
		}
	}
}

// TestExtractionFailureNamesTheLayout is the whole of the breadth this package has, and it is
// DIAGNOSTIC, never adaptive. The bind path is fixed when the unit is rendered, before this code
// can look at anything, and an upstream layout change is meant to fail a blessed image in catalog
// CI rather than be papered over on a household's node. What the search buys is that the CI
// failure names the layout the image actually has — including the native s6-rc tree that a
// migration off the legacy one would use — instead of an errno.
func TestExtractionFailureNamesTheLayout(t *testing.T) {
	f := &fake{}
	// The image answers the layout probe but not the script itself: the shape of a service that
	// moved.
	f.fail = func(name string, args []string) error {
		if name == "podman" && len(args) == 3 && args[0] == "cp" && strings.HasSuffix(args[1], s6Run) {
			return errors.New("exit status 125")
		}
		return nil
	}
	err := Prepare(context.Background(), f, ha(), 1883)
	if err == nil {
		t.Fatal("a failed extraction was accepted")
	}
	for _, root := range serviceRoots {
		if !strings.Contains(err.Error(), root) {
			t.Fatalf("the error does not report %s, so a layout change reads as an errno: %v", root, err)
		}
	}
	// And it must have LOOKED rather than guessed.
	if !f.ran("podman", "cp", extractCtr+":/etc/s6-overlay/s6-rc.d", Dir+"/layout") {
		t.Fatalf("the native s6-rc tree was never probed; ran: %v", f.runs)
	}
}

// TestPrepareMaterialisesTheIntegration — briard's own integration is delivered the same way the
// mint is, and the broker's port is SUBSTITUTED rather than hardcoded ([B.124]). The port belongs
// to the other service, so it arrives from the registry; what this checks is that it lands.
func TestPrepareMaterialisesTheIntegration(t *testing.T) {
	f := withImage()
	if err := Prepare(context.Background(), f, ha(), 1883); err != nil {
		t.Fatal(err)
	}
	src, ok := f.files[implPath]
	if !ok {
		t.Fatalf("the implementation was not materialised; the stub would find nothing to import")
	}
	if strings.Contains(src, mqttPortToken) {
		t.Error("the integration kept its placeholder; it would dial a port that is not a number")
	}
	if !strings.Contains(src, "BROKER_PORT = 1883") {
		t.Error("the broker's port did not reach the integration")
	}
	// The stub is what lands in /config, so it has to be staged for the planter to copy — both
	// files, because a package without its manifest is an integration HA refuses to load.
	for _, p := range []string{stubDir + "/__init__.py", stubDir + "/manifest.json"} {
		if _, ok := f.files[p]; !ok {
			t.Errorf("%s was not staged; the planter would copy a missing file", p)
		}
	}
	// THE STUB AND THE IMPLEMENTATION AGREE ON THE PATH, and nothing else checks it: the stub
	// carries it as a literal because it is a permanent ABI that outlives the briard that wrote
	// it, so a change on this side that did not reach it would silently stop resolving.
	if !strings.Contains(stubSource, mountPoint+"/integration") {
		t.Errorf("the stub does not look for the implementation where Prepare mounts it")
	}
	// The wrapper must actually run the planter, or none of the above reaches /config.
	if w := f.files[wrapperPath]; !strings.Contains(w, "plant.py") {
		t.Errorf("the wrapper does not run the planter:\n%s", w)
	}
}

// TestPrepareRefusesAnIntegrationThatLostItsPlaceholder — the token is the contract between the Go
// side and the embedded file. Without this check a file that stopped carrying it would be
// installed verbatim and fail forever, silently, on every Home Assistant start in the fleet — the
// same reasoning mosquitto's bind address is checked under.
func TestPrepareRefusesAnIntegrationThatLostItsPlaceholder(t *testing.T) {
	saved := implSource
	t.Cleanup(func() { implSource = saved })
	implSource = "BROKER_PORT = 1883\n"
	if err := writeIntegration(context.Background(), withImage(), 1883); err == nil {
		t.Fatal("an integration with no placeholder was installed; the port would be whatever it says")
	}
}

// TestStubDelegatesAndSurvivesAnAbsentImplementation — the stub is the one artifact that outlives
// the briard that wrote it: a household's backup carries it, and restoring that backup onto a
// Home Assistant with no briard brings this exact file back. So it must do one thing and it must
// not fail when the implementation is not mounted — an integration that raises would be a red
// error in a house briard does not even manage.
func TestStubDelegatesAndSurvivesAnAbsentImplementation(t *testing.T) {
	if !strings.Contains(stubSource, "except ImportError:") {
		t.Error("the stub does not survive an absent implementation")
	}
	if !strings.Contains(stubSource, "return await impl.async_setup(hass, config)") {
		t.Error("the stub does not delegate async_setup")
	}
	// One job only. Anything else here is a second thing an old stub could get wrong.
	if strings.Count(stubSource, "async def ") != 1 {
		t.Errorf("the stub defines more than async_setup:\n%s", stubSource)
	}
	if !strings.Contains(stubManifest, `"domain": "briard"`) {
		t.Errorf("the stub's manifest does not claim the briard domain:\n%s", stubManifest)
	}
	// No config_flow: a YAML-only integration has no card and no disable switch, which is right
	// for plumbing and is what keeps briard out of the household's decisions.
	if strings.Contains(stubManifest, "config_flow") {
		t.Errorf("the stub declares a config flow; it would be UI-visible and deletable:\n%s", stubManifest)
	}
	// A custom integration with no `version` is BLOCKED FROM LOADING by HA's loader, with an
	// error and no other symptom.
	if !strings.Contains(stubManifest, `"version"`) {
		t.Errorf("the stub's manifest has no version; HA blocks it from loading:\n%s", stubManifest)
	}
}

// TestWrapperIsAWellFormedScript — the cheapest possible guard on the file we mount over Home
// Assistant's own `run`, and it exists because its absence cost a VM run: an edit dropped the
// shebang, every Go test still passed, and s6 answered with `unable to spawn ./run (waiting 60
// seconds): Exec format error` and retried forever, so HA never started at all.
//
// Asserted on the EMBEDDED source rather than on what Prepare wrote, because the defect is in the
// file as shipped: by the time it reaches a node it is already too late to be a unit test.
func TestWrapperIsAWellFormedScript(t *testing.T) {
	if !strings.HasPrefix(wrapperSource, "#!") {
		t.Fatalf("the wrapper has no shebang; s6 cannot spawn it at all:\n%.80s", wrapperSource)
	}
	// It must END by handing over to the image's own run -- at the path the CONTAINER sees, which
	// is the mount point and not the node's. Anything after the exec is dead code, and anything
	// instead of it is briard authoring HA's bootstrap rather than relaying it.
	handover := "exec " + mountPoint + "/run.original"
	if !strings.HasSuffix(strings.TrimSpace(wrapperSource), handover) {
		t.Errorf("the wrapper does not end with %q; it would not hand over", handover)
	}
	// And it must run the planter, or briard's integration never reaches /config and nothing
	// inside Home Assistant runs at all.
	if !strings.Contains(wrapperSource, mountPoint+"/plant.py") {
		t.Error("the wrapper does not run the planter")
	}
}
