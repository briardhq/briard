package guestagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"briard.io/agent/guestfirmware"
	"briard.io/shared/chain"
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

// holdSecs is how long a node refuses promotion after one of its chain members gave up
// ([V3b.5](c)).
//
// 300s is a judgement, not a measurement, and the trade is legible: a fault that TRAVELS with
// the replicated volume (a bad routes table, a corrupt cert, a name that collides LAN-wide) is
// hit identically by whoever promotes next, so the resource ping-pongs with a period of roughly
// this value -- five minutes is about twelve hand-overs an hour, each costing a mount cycle and
// a service restart. Longer is calmer and sidelines a recovered node for longer. A node-local
// fault (memory pressure, a failing disk, a leaked process holding :80) does not ping-pong at
// all: the peer simply keeps serving and this node's hold expires against a resource it cannot
// have.
//
// It was a nix option while the unit was baked, for exactly one reason: the contract rigs drive
// the whole lifecycle and cannot wait five minutes. It is a constant with a test override now,
// the BRIARD_DEADMAN precedent -- nothing in the field varies it.
const defaultHoldSecs = 300

func holdSecs() string {
	if s := os.Getenv("BRIARD_PROMOTION_HOLD_SECS"); s != "" {
		return s
	}
	return strconv.Itoa(defaultHoldSecs)
}

// tool returns an absolute path into the image's profile. Every Exec* line below goes through it
// rather than naming a bare command, because systemd requires an absolute path for the first
// word -- PATH only reaches what the command itself then shells out to.
func tool(name string) string { return filepath.Join(toolsBin(), name) }

// WHAT HANDS THE RESOURCE ON WHEN A CHAIN MEMBER GIVES UP ([V3b.5](c)), and it has to be a STATE
// hook rather than a dependency, which is the whole finding.
//
// `Requires=` is JOB-level: systemd consults it when a stop or restart job is enqueued on the
// depended-upon unit, and never against that unit's state (transaction.c, atom
// UNIT_ATOM_PROPAGATE_STOP on UNIT_REQUIRED_BY). That is why the promoter target's default
// `Requires=` on its members demoted this node on EVERY crash: `Restart=` enqueues its
// auto-restart with job mode JOB_RESTART_DEPENDENCIES, which propagates a TRY_RESTART up to the
// target, which stops drbd-promote@ (PartOf) -- measured, and it took the resource away from a
// live peer in 2 of 5 door crashes. With `target-as = Wants` (agent/drbd/config.go) nothing
// upstream reacts to a member at all, which is right for a crash and wrong for a member that has
// genuinely given up.
//
// OnFailure= is the missing half: unit.c fires it on the transition INTO UNIT_FAILED, with no
// reference to restart mode. Under `RestartMode=direct` a member never enters that state while
// it is being auto-restarted, so this stays silent through the transient crashes and fires
// exactly once -- when the start limit is exhausted and the unit really has stopped trying.
//
// It points at OUR hold unit rather than upstream's drbd-demote-or-escalate@ directly, and the
// reason is ordering: reactor re-promotes ~2s after a demote completes, so the mask that says
// "not me, for now" has to go on BEFORE the demote, not after it. briard-promotion-hold does
// both in that order and carries the same FailureAction=reboot for a demote DRBD refuses.
//
// THE BUDGET IS UNIFORM ACROSS THE CHAIN ([B.125](b)): five starts in five minutes, after which
// the member gives up and the resource moves. A judgement rather than a measurement, and it
// matters more than the shape suggests -- with no StartLimit at all systemd's 5-in-10s default
// applies, and at RestartSec=2 that IS reachable, so a door would hand the resource on after
// ~10s of trying.
const chainMemberFailure = `OnFailure=` + holdUnit + `
OnFailureJobMode=replace-irreversibly
StartLimitIntervalSec=300
StartLimitBurst=5
`

const (
	holdUnit           = "briard-promotion-hold.service"
	primaryStorageUnit = "briard-primary-storage.service"
	servicesUnit       = "briard-services.service"
	vipUnit            = "briard-vip.service"
	vipRenewUnit       = "briard-vip-renew.service"
	vipRenewTimer      = "briard-vip-renew.timer"
	reverseProxyUnit   = "briard-reverse-proxy.service"
	dashboardUnit      = "briard-dashboard.service"
)

// chainEdges is what makes the members an ordered chain on a node with no promoter, and what
// makes them stop with it: PartOf the target, and Requires=/After= the PREVIOUS member. It is
// the same shape drbd-reactor writes onto its own target's members, so a lone node and a flock
// run ONE chain and the members cannot tell which target started them ([B.145c]).
func chainEdges(i int, members []string) string {
	s := "PartOf=" + chain.Target + "\n"
	if i > 0 {
		s += "Requires=" + members[i-1] + "\nAfter=" + members[i-1] + "\n"
	}
	return s
}

// reversed is the stop order: a chain unwinds the way it was built. Used for the lone node's
// hold, which NAMES every member because `systemctl stop` of a target returns as soon as the
// TARGET is down -- measured, with the volume still mounted -- while a stop of several units
// returns when all of them are down.
func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
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
	members := chain.Members()
	u := map[string]string{}

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
	// the attach never re-tried. Every step it takes is idempotent by construction (a returning
	// node activates its VG and stops), so re-running is the cheaper guarantee.
	//
	// ⚠️ THE COMMITTED PATH DIRECTLY ([B.86j], [B.138]), never through the pivot's picker: the
	// picker's trial flag is keyed by the binary's NAME, so a unit that reached the agent
	// through it would arm a trial every time storage came up.
	u[nodeStorageUnit] = `[Unit]
Description=Briard node storage (tiers, and the DRBD resource on top of them)

[Service]
Type=oneshot
RemainAfterExit=no
Environment=PATH=` + tools + `
ExecStart=` + agent + ` --node-storage
`

	// THE HAND-OVER, AND THE REFUSAL TO TAKE IT STRAIGHT BACK ([V3b.5](c)). One unit owns the
	// whole sequence because the ORDER is the design: mask BEFORE demoting.
	//
	// Measured on a lone node: drbd-reactor re-promotes about 2s after a demote completes. So a
	// hold bolted on AFTER the demote loses that race. Masking first means the promotion is
	// REFUSED rather than undone: `systemctl start drbd-services@r0.target` fails outright,
	// drbd-promote@ never runs, DRBD's role never moves, and there is no second mount/unmount
	// cycle to pay for.
	//
	// EACH STEP IS AN IMAGE-SIDE TOOL taking this agent's chain list as arguments ([B.160]).
	// The topology dispatch, the mask, the shim and the unmount are the image's -- they name
	// store paths and they do not change with a release. What the agent supplies is the one
	// thing only it knows: WHICH units the chain is made of, and in which order to stop them.
	// That is the whole cut: image contributes the tools, agent contributes the facts.
	//
	// ⚠️ ONLY A NODE THAT HOLDS THE RESOURCE MAY HAND IT ON, which is briard-hold-only-if-holding,
	// and it is not defensive programming -- without it the STANDBY masks itself on every boot.
	// Measured: a member whose start job fails with result `dependency` fires `OnFailure=`
	// exactly like one that failed on its own, and on the node that loses the promotion race
	// every member does. ExecCondition rather than ExecStartPre: 1..254 SKIPS the unit and does
	// NOT mark it failed, so a Secondary quietly declines instead of failing into
	// FailureAction=reboot.
	//
	// ⚠️ STEP 2 STOPS drbd-promote@, NOT THE TARGET, on a flock, and that is a barrier rather
	// than a preference: `systemctl stop drbd-services@r0.target` returns as soon as the TARGET
	// is down -- measured at 62.386 with the volume still mounted at 62.412, exit 11, reboot.
	// The members are `After=drbd-promote@`, so waiting for the promote unit is the only
	// spelling that waits for all of them. The lone node has no promote unit to wait on, so its
	// stop NAMES every member, in reverse -- the argument list below.
	//
	// ⚠️ RELEASE CLEARS THE START LIMIT TOO, and that is not housekeeping. Without the reset the
	// member is still inside its StartLimitIntervalSec window when the hold ends, so the next
	// promotion starts a member systemd immediately refuses -- and the hold length would be
	// silently pinned to that window.
	//
	// ⚠️ journalctl --sync LAST, and the reason is FailureAction=reboot: a node that reboots
	// because it could not release the resource must leave the reason on disk first.
	//
	// ⚠️ IT CARRIES THE PROFILE even though every Exec line here is absolute, and the reason is
	// upstream's shim: `drbd-service-shim.sh` is someone else's shell, and what it resolves by
	// bare name is not ours to assume. As a NixOS unit it ran on the default service PATH this
	// profile is a superset of, so the PATH is how the move stays a move rather than a
	// narrowing -- at the one step whose failure mode is a stuck Primary and a reboot.
	u[holdUnit] = `[Unit]
Description=Briard: hand the resource on, and refuse to take it back for a while
FailureAction=reboot

[Service]
Type=simple
Environment=PATH=` + tools + `
ExecCondition=` + tool("briard-hold-only-if-holding") + `
ExecStartPre=` + tool("briard-hold-refuse") + `
ExecStartPre=-` + tool("briard-hold-stop") + " " + chain.Target + " " + strings.Join(reversed(members), " ") + `
ExecStartPre=` + tool("briard-hold-confirm") + `
ExecStart=` + tool("sleep") + ` ` + holdSecs() + `
ExecStopPost=-` + tool("briard-hold-release") + `
ExecStopPost=-` + tool("systemctl") + ` reset-failed ` + strings.Join(members, " ") + `
ExecStopPost=-` + tool("briard-hold-restart") + " " + chain.Target + `
ExecStopPost=-` + tool("journalctl") + ` --sync
`

	// 1. PRIMARY STORAGE -- format on first use, mount the replicated volume ([V3b.33](d)). The
	// FILESYSTEM half: node storage did the block work on every node, and this runs only where
	// the volume is actually mounted, which is the one node that promoted.
	//
	// A oneshot may carry Restart=on-failure -- only `always`/`on-success` are refused for this
	// Type, and a oneshot that exits cleanly is never restarted -- so the budget is uniform
	// across the chain rather than "the simple ones retry and the oneshots get exactly one
	// attempt", which is what the absence of a directive used to mean and nobody had decided.
	u[primaryStorageUnit] = `[Unit]
Description=Briard primary storage (the replicated volume, mounted on the primary)
` + chainEdges(0, members) + chainMemberFailure + `
[Service]
Restart=on-failure
RestartSec=2
Type=oneshot
RemainAfterExit=yes
Environment=PATH=` + tools + `
ExecStart=` + agent + ` --primary-storage
ExecStop=` + agent + ` --primary-storage-stop
`

	// 2. SERVICES -- CONVERGE-AT-PROMOTION ([V3b.3](f)). Once the volume is mounted, read every
	// manifest under its `.services/`, render, warm and start them. This node makes itself match
	// the VOLUME, so what a node was told -- or whether it was even up when the install ran --
	// stops deciding what the household gets after a failover.
	//
	// ITS FAILURE IS LOUD, BY POSITION. A promoter fails the whole promotion if a member fails,
	// and the VIP comes after this -- so a node that cannot converge never takes the service
	// address, and a primary with no address is already reported unhealthy. Built as a
	// side-effect that shrugs, converge would put fallible work on the promotion path and leave
	// the silent-healthy hole exactly as dangerous: a gate that shrugs is not a gate.
	//
	// THE SERVICE UNITS THEMSELVES ARE NOT MEMBERS, which is what makes "a service error alerts
	// but never demotes" mechanically true -- drbd-reactor never sees them, so a crashed
	// container cannot deactivate the target. A crash is the container unit's own Restart=
	// (agent/quadlet), and the STOP is ExecStop below, because reverse-order chain unwinding
	// would otherwise leave containers running on a volume about to be unmounted.
	u[servicesUnit] = `[Unit]
Description=Briard services, converged from the replicated volume at promotion
` + chainEdges(1, members) + chainMemberFailure + `
[Service]
Restart=on-failure
RestartSec=2
Type=oneshot
RemainAfterExit=yes
Environment=PATH=` + tools + `
ExecStart=` + agent + ` --converge
ExecStop=` + agent + ` --converge-stop
`

	// 3. VIP -- claim the service address and gratuitous-ARP it so the L2 segment learns its
	// (new) home. BOTH the address and the device are agent-determined: net.configure writes
	// VIP_ADDR + VIP_DEV to vip.env, and this unit reads that file and NOTHING ELSE.
	//
	// THE FILE IS REQUIRED, not optional, and there is no baked device or address behind it
	// ([V3b.16a]). It can only be missing if something started this unit that the agent did not
	// configure -- which the promoter gate makes impossible, since drbd-reactor itself is
	// agent-started. So "no VIP configuration" is an error rather than a guess, and the one
	// guess it used to make claimed the service address on the replication NIC ([V3b.16]).
	//
	// Wants= THE RENEWAL TIMER, which is the `wantedBy` the timer used to carry, said from this
	// side because a runtime unit has no `systemctl enable` to act on an [Install] section. Same
	// weak edge, same reason: a timer that will not start must not keep the VIP from coming up.
	u[vipUnit] = `[Unit]
Description=Briard service VIP
Wants=` + vipRenewTimer + `
` + chainEdges(2, members) + chainMemberFailure + `
[Service]
Restart=on-failure
RestartSec=2
Type=oneshot
RemainAfterExit=yes
Environment=PATH=` + tools + `
EnvironmentFile=` + vipEnvPath + `
ExecStart=` + tool("briard-vip-up") + `
ExecStartPost=-` + tool("briard-vip-arping") + `
ExecStop=` + tool("briard-vip-down") + `
`

	// 3b. Ten-minute lease renewal, for as long as this node holds the VIP.
	//
	// NOT A CHAIN MEMBER, deliberately and for [V3b.5c]'s reason: a renewal that fails must
	// never be able to demote a serving node. Nothing Requires it, its failure propagates
	// nowhere, and the real consequence of a renewal going wrong is a NAK, which dhcpcd's own
	// hook handles as an address change rather than as a unit failure.
	//
	// PartOf the VIP, so it stops when the node gives the address up; the first tick is ten
	// minutes after the VIP is taken, because the lease is fresh at promotion and an immediate
	// renewal would be pure noise.
	u[vipRenewTimer] = `[Unit]
Description=Renew the Briard VIP's DHCP lease every ten minutes
PartOf=` + vipUnit + `

[Timer]
OnActiveSec=10min
OnUnitActiveSec=10min
AccuracySec=30s
`
	// VIP_DEV, the same source briard-vip itself reads: the renewer must act on the interface
	// that was actually claimed, not on a second opinion about which one that is.
	u[vipRenewUnit] = `[Unit]
Description=Renew the Briard VIP's DHCP lease
After=` + vipUnit + `

[Service]
Type=oneshot
Environment=PATH=` + tools + `
EnvironmentFile=` + vipEnvPath + `
ExecStart=` + tool("briard-vip-renew") + `
`

	// 4. THE FRONT DOOR -- answer the VIP on :80 and terminate HTTPS on :443, and publish the
	// household's mDNS names ([B.152]).
	//
	// Cert/key live on the DRBD volume so they replicate and survive failover; the proxy
	// hot-reloads them, so a renewal is gap-free. ⚠️ A MISSING CERT IS NOT A FAILURE: :443
	// simply does not answer until a cert exists while :80 keeps serving, which is the shipped
	// state of a free node, since a cert needs a domain.
	//
	// NO -routes: the table has a compiled-in default (shared/routes.Path) this unit deliberately
	// does not restate. Ordered AFTER briard-services, which is what makes the table exist before
	// the door reads it.
	//
	// THROUGH THE PIVOT ([B.86j], [B.138]): the picker runs the copy the host pushed, and nothing
	// else -- the image bakes no door. READY means "listening", which is what a trial agent reads
	// as its verdict on the pushed copy.
	//
	// ⚠️ THE PICKER IS THE REASON THIS UNIT HAS A PATH AT ALL. The door is a self-contained Go
	// binary and needed none while it was a NixOS unit -- but `briard-bin-exec` is shell, and it
	// `rm`s the trial flag as it execs. Under NixOS that resolved through the default service
	// PATH nixpkgs hands every unit; a runtime unit gets no such gift, and a picker that cannot
	// consume its own single-use flag would let a crashing candidate re-trial forever.
	//
	// A TRANSIENT CRASH MUST NOT MOVE THE RESOURCE ([V3b.5](c)). Without RestartMode=direct the
	// auto-restart's stop job deactivates drbd-reactor's target -- which unmounts the data volume
	// and demotes the node on ONE crash, measured, with a peer taking the resource about half the
	// time. `direct` restarts through activating instead of failed, so dependents are not
	// notified of the temporary failure, and it is what lets the StartLimit above finally
	// accumulate. NOT on primary-storage/services/vip: for those, failure really does mean this
	// node must not hold the volume.
	u[reverseProxyUnit] = `[Unit]
Description=Briard front door (serves the VIP on :80/:443)
After=` + servicesUnit + `
` + chainEdges(3, members) + chainMemberFailure + `
[Service]
Type=notify
Environment=PATH=` + tools + `
ExecStart=` + tool("briard-bin-exec") + ` briard-reverse-proxy - -http :80 -listen :443 -cert ` + tlsCertPath + ` -key ` + tlsKeyPath + ` -fallback http://127.0.0.1:8087
Restart=on-failure
RestartMode=direct
RestartSec=2
TimeoutStartSec=10
`

	// 5. THE HOUSEHOLD DASHBOARD ([V3b.31b]): loopback only, behind the door, which forwards
	// every name it does not route here. A chain member for the reason the door is one -- its
	// device registry lives on the volume, and only the primary has it -- under the same
	// [V3b.5](c) settings. It reads the routing table converge wrote and Home Assistant's
	// control token, both on /run; it writes only under /var/lib/briard/dashboard.
	u[dashboardUnit] = `[Unit]
Description=Briard household dashboard (behind the front door)
After=` + primaryStorageUnit + ` ` + servicesUnit + `
` + chainEdges(4, members) + chainMemberFailure + `
[Service]
Type=notify
Environment=PATH=` + tools + `
ExecStart=` + tool("briard-bin-exec") + ` briard-dashboard - -listen 127.0.0.1:8087
Restart=on-failure
RestartMode=direct
RestartSec=2
TimeoutStartSec=10
`

	// THE LONE NODE'S TARGET ([B.145c]): the promoter chain with no promoter. A home with one
	// diskful member runs no DRBD, so nothing generates drbd-services@r0.target for it; this
	// static target carries the IDENTICAL member list in the identical order, with the same
	// Wants/After the reactor writes onto its target -- so a lone node and a flock run one
	// chain, and the members cannot tell which target started them. A lone node's bring-up ends
	// in `systemctl start` of this where a flock's ends in starting drbd-reactor; no [Install],
	// so nothing else can.
	u[chain.Target] = `[Unit]
Description=Briard: the promoter chain, on a node that runs no promoter
Wants=` + strings.Join(members, " ") + `
After=` + strings.Join(members, " ") + `
`
	return u
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
	if err := sweep(dir, want); err != nil {
		return err
	}
	// Without this the files are on disk and invisible: systemd reads unit files at load, so a
	// unit written and not reloaded is a unit `systemctl start` says does not exist.
	if out, err := x.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("guest units: daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sweep removes units a PREVIOUS agent rendered here and this one does not.
//
// THE CASE IS A DOWNGRADE INSIDE ONE BOOT, and it is a path the product actually takes: a
// refused release is reverted by pushing the previous bundle and restarting the agent's unit
// (guestfirmware, [B.138]). /run is tmpfs, so a reboot would clear this by itself -- but a
// revert is not a reboot, and what it would otherwise leave behind is the newer agent's units
// pointing at the older agent's binary. That is this item's own defect with the ages swapped,
// and it would be the harder direction to read, because the unit would look freshly written.
//
// ⚠️ IT DELETES BY PREFIX, so what else may write a `briard-*` unit into this directory is the
// whole safety argument, and the answer today is NOTHING. drbd-reactor's generated units and
// the hold's `--runtime` mask are all named for the DRBD resource (`drbd-services@r0.target`),
// and the service units converge renders are podman quadlets: they live under
// /run/containers/systemd and are expanded by a systemd GENERATOR into /run/systemd/generator*,
// never here (agent/quadlet). A component that ever does write one here has to be taught to
// this function in the same change -- enumerate, do not assume the prefix stays ours.
func sweep(dir string, want map[string]string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		// A directory that does not exist holds no strays. Every other error is real: a
		// directory we cannot read is one we cannot keep correct.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("guest units: read %s: %w", dir, err)
	}
	for _, e := range ents {
		n := e.Name()
		if _, ours := want[n]; ours || !strings.HasPrefix(n, "briard-") {
			continue
		}
		switch filepath.Ext(n) {
		case ".service", ".target", ".timer":
		default:
			continue
		}
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("guest units: remove stale %s: %w", n, err)
		}
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
