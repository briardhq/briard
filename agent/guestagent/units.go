package guestagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"briard.io/agent/guestfirmware"
)

// THE UNITS THE PUSHED AGENT OWNS ([B.160]).
//
// Since [B.86j] the guest image bakes exactly one binary -- briard-guest-firmware, the push
// protocol -- and every other briard binary rides the HOST bundle. The units that start them were
// left behind in the image, and a unit is mostly an ExecStart plus the environment that binary
// needs, so the two moved on different cadences: the binary on the host's, the unit on the
// image's. Measured, [B.159](c): a pushed agent asked its older image for a unit that image did
// not define, and the node crash-looped 53 times with the household's app gone.
//
// So the agent writes them itself, at every start, and the defect class stops being
// representable: a unit on tmpfs is rewritten by the binary that needs it, so it can never be
// older than that binary. There is nothing to detect and nothing to version-gate.
//
// ⚠️ /run/systemd/system IS THE ONLY PLACE THIS CAN WORK, and that constraint is the feature.
// NixOS makes /etc/systemd/system a read-only store path (the reason scripts/install.sh has a
// UNIT_DIR knob at all), so runtime units are the only writable kind -- and tmpfs is exactly the
// lifetime we want, because a reboot must land on firmware with no units at all until the host
// dresses the guest again. The precedent is already in this image: drbd-reactor generates its
// promoter units into this directory, and the agent reaches into it to manage a drop-in and to
// remove a generated target (reactorDropIn, guestagent.go).
//
// ⚠️ WHAT DOES NOT MOVE, and the line is AGENTS §5's frozen-by-necessity one drawn inside the
// guest: a unit that starts the thing writing it cannot be written by that thing. briard-guest-
// agent.service and briard-deadman.service supervise the agent's own arrival, so they stay baked
// (guest-image/disk-image.nix), as do the upstream DRBD units and briard-stage, whose content is
// a build-time fact about the image rather than anything the agent knows.
//
// ⚠️ AND THE IMAGE STILL OWNS THE CLOSURE. A unit's PATH is a set of store paths, which a pushed
// binary cannot invent; the image publishes them as ONE profile at ToolsBin and the agent names
// that path. The two halves share a name and nothing else -- no manifest, no schema, no
// handshake. An image that predates all this has no profile, which WriteUnits treats as a refusal
// to run rather than as something to work around (see below).

// The two paths, overridable for tests exactly as guestfirmware's binDir/binRunDir are, and for
// the same reason: everything here is an absolute path on a real guest, so a test that could not
// move them could only ever assert the refusal.
const (
	// defaultUnitDir is where the units below are written.
	defaultUnitDir = "/run/systemd/system"
	// defaultToolsBin is the image's tool profile: everything an agent-written unit may exec,
	// under one fixed path. PAIRED with guest-image/configuration.nix's `guestTools` +
	// `toolsEtc`, which carry the reasoning for whole-packages-not-named-commands. Different
	// languages, so no shared import; the nix-side comment names this const back.
	defaultToolsBin = "/etc/briard/tools/bin"
)

func unitDir() string {
	if d := os.Getenv("BRIARD_UNIT_DIR"); d != "" {
		return d
	}
	return defaultUnitDir
}

func toolsBin() string {
	if d := os.Getenv("BRIARD_TOOLS_BIN"); d != "" {
		return d
	}
	return defaultToolsBin
}

// units is every unit this agent renders, name -> file contents. One map rather than a list of
// structs: the name is the key systemd knows the unit by, and having it in two places is how a
// rename goes half-done.
//
// ⚠️ THE ExecStart PATH DOES NOT EXIST YET when this runs, and that is correct rather than
// sloppy. The committed binary is laid down by guestfirmware.BinCommit, which runs AFTER the
// control port opens ([B.148] says why it cannot run earlier), while rendering runs BEFORE it --
// so the unit names a path the commit creates moments later, exactly as the baked unit did. What
// IS checked is the tool profile, because that is the image's half and the one an old image lacks.
func units() map[string]string {
	agent := filepath.Join(guestfirmware.BinDir(), "briard-guest-agent")
	tools := toolsBin()
	return map[string]string{
		// NODE STORAGE -- every tier this node holds, and the DRBD resource on top of them
		// ([V3b.33](d)). NOT a promoter chain member and NOT started at boot: the HOST starts it,
		// once per bring-up, after writing /run/briard/node-storage.json.
		//
		// No [Install] section, which is the old `wantedBy = [ ]` and the same statement: storage
		// bring-up provably cannot run before the host has dressed the guest, which is fine (the
		// host is always present at guest start -- `-no-reboot`, and the agent is the guest's sole
		// supervisor) and turns something accidental into something stated. Under [B.160] it is
		// true twice over: the unit does not exist at all until the agent that execs it is here.
		//
		// It runs on EVERY node, witness included: a diskless node builds no tier and still needs
		// its `.res` written and its resource attached.
		//
		// ⚠️ NO RemainAfterExit, and that is deliberate rather than an omission: the host starts
		// this unit at EVERY bring-up, including a re-adopt of a warm guest, and `systemctl start`
		// on a unit that stayed "active" would be a silent no-op -- the `.res` never re-asserted,
		// the attach never re-tried. Every step it takes is idempotent by construction (a
		// returning node activates its VG and stops), so re-running is the cheaper guarantee.
		//
		// ⚠️ THE COMMITTED PATH DIRECTLY ([B.86j], [B.138]), never through the pivot's picker: the
		// picker's trial flag is keyed by the binary's NAME, so a unit that reached the agent
		// through it would arm a trial every time storage came up.
		nodeStorageUnit: `[Unit]
Description=Briard node storage (tiers, and the DRBD resource on top of them)

[Service]
Type=oneshot
RemainAfterExit=no
Environment=PATH=` + tools + `
ExecStart=` + agent + ` --node-storage
`,
	}
}

// WriteUnits renders every unit this agent owns into /run/systemd/system and reloads systemd, so
// that what the host is about to start is the unit this binary defines.
//
// WHERE IT RUNS: before the control port opens, and before anything else the agent's start does
// (cmd/briard-guest-agent's runGuest). The host's gate is the PORT, not READY -- the lesson
// [B.148] paid for -- so anything rendered after the port is a unit the first bring-up verb can
// ask for and not find, which is the very crash this item exists to remove.
//
// A FAILURE HERE IS FATAL TO THE START, on purpose. The only realistic cause is an image with no
// tool profile, i.e. a pushed agent that is newer than the image beneath it. Refusing to serve is
// what makes that safe: a trial agent that exits takes the whole staged set down with it (the
// picker restores the committed binaries, guestfirmware.BinStartup), so the node keeps running
// the release it already had instead of promoting into units it cannot support. Stopping the host
// from OFFERING that upgrade at all is [B.159](e)'s floor, which this does not replace.
func WriteUnits(ctx context.Context, x Executor) error {
	if tools := toolsBin(); !isDir(tools) {
		return fmt.Errorf("guest units: no tool profile at %s -- this image predates [B.160] and cannot run this agent", tools)
	}
	dir := unitDir()
	want := units()
	// Sorted, so a journal reading two starts of this agent compares line for line.
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := x.WriteFile(filepath.Join(dir, n), []byte(want[n])); err != nil {
			return fmt.Errorf("guest units: write %s: %w", n, err)
		}
	}
	// Without this the files are on disk and invisible: systemd reads unit files at load, so a
	// unit written and not reloaded is a unit `systemctl start` says does not exist.
	if out, err := x.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("guest units: daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isDir asks the question the refusal above actually cares about: a tool profile that is a FILE,
// or a dangling /etc symlink, is as unusable as one that is absent, and all three have to route
// to the same refusal rather than to a unit that fails at exec time.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
