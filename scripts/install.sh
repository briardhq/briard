#!/bin/sh
# briard one-command install.
#
#   curl -fsSL https://get.briard.io/install.sh | sudo sh
#
# Brings a stock single-node Linux host to GREEN: a guest VM on our BUNDLED qemu holding the VIP,
# its NICs hung off the host's own NIC, a data volume the guest lays out (DRBD only once a peer
# exists), and Briard answering at the VIP on the LAN. No cloud, no name.
#
# ⚠️ THIS SCRIPT IS OUTSIDE THE VERSIONING SYSTEM. It is fetched from the channel root and run
# once; nothing it writes can be reached by a release afterwards. So it does only what never
# changes: fetch and verify, lay the files down, say where they went, register the units, report.
#
# EVERYTHING ELSE IS THE AGENT'S ([B.157]), and there are two tests for whether something belongs
# here, the second wider than the first:
#
#   1. would a RELEASE ever need to change it?  Which device the guest's L2 hangs off, which
#      substrate that implies, which addresses this node numbers itself from, whether the tun
#      driver is loaded -- all revisited at every start, none of them frozen here ([B.150]).
#   2. would a WINDOWS installer have to write it again?  The disks, the identifiers, the console's
#      rotation. None of those is a decision a release revisits, so the first test alone would have
#      left them in this file -- and left a PowerShell installer to reimplement every one.
#
# Adding something back here means answering both. The agent is Go that already cross-compiles;
# this is shell that runs on exactly one kind of host.
#
# It installs NO SERVICE. A node is a node first: ready, replicating, able to fail over -- then you
# choose what runs on it. So the VIP answers with Briard's own page, and the health probe watches
# that front door, the one address that answers whether or not anything is installed.
#
# Cattle/pet FHS:
#   /opt/briard     = cattle: signed self-updating binaries + qemu bundle + guest image, plus
#                     config.env. `rm -rf /opt/briard` + reinstall = a fresh host.
#   /var/lib/briard = pet: the data volume, the state disk, this node's identifiers and the
#                     subnets it drew -- all of them the AGENT's to create and keep.
#   /run/briard     = tmpfs flags.
#   /var/log/briard-guest-console.log = the guest's serial console. Neither cattle nor pet: a host
#                     log, so it outlives a cattle reset and stays off the replicated volume.
#
# BRIARD_ARTIFACTS=<dir> installs from a local, already-verified staging dir (the hermetic install
# tests, and a future offline install). Unset = the signed network fetch over the channel.
set -eu
# ---- what this script needs to know -------------------------------------------------
# ⚠️ THE KNOBS ARE NOT HERE, and adding one back is the mistake ([B.157]). Every `BRIARD_*` in the
# environment is COPIED into config.env with the prefix stripped -- `BRIARD_CPU=qemu64` becomes
# `CPU=qemu64` -- so the installer carries no list of them, no defaults for them and no
# documentation of them. What each key means and what it defaults to is agent/host/config.go's,
# which is a binary the channel can fix; a default frozen here would be reachable by nothing.
#
# Below is the short list this script READS, because it acts on it before any agent runs.
PREFIX=/opt/briard
STATE=/var/lib/briard
RUNDIR=/run/briard
# ⚠️ THE THREE PATHS ABOVE ARE CONSTANTS. /opt/briard is baked into the qemu bundle's own ELF
# interpreter (/opt/briard/qemu/lib/ld-linux...) and into the agent's defaults, so an install
# anywhere else produces a qemu that cannot execute. The shipped unit files spell the same paths.

# NixOS's /etc/systemd/system is a read-only store path, so the install rigs point this elsewhere.
UNIT_DIR="${BRIARD_UNIT_DIR:-/etc/systemd/system}"

# The signed release channel root, and WHICH release off it: `stable` (what strangers get, a tested
# pair by construction), `latest` (what was published most recently -- how a release is proven
# before promotion), or an exact host id. One selector, both chains. The channel's tree is spelled
# out in scripts/publish-release.sh.
CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}"
RELEASE="${BRIARD_RELEASE:-stable}"
# The release public key: this script's verify root, before anything is on disk to trust.
KEYRING="${BRIARD_UPDATE_KEYRING:-$PREFIX/keyring.pem}"

# The two the REPORT CARD is told, because it judges them against this machine before a byte is
# written: the service address (unset = DHCP, and there is deliberately no default -- any address
# we could pick is a guess about someone else's network) and the device the guest's L2 hangs off
# (unset = the agent selects the one holding the default route, and re-asks at every start).
VIP="${BRIARD_VIP_ADDR:-}"
NIC="${BRIARD_NIC:-}"

# The release signing public key(s), embedded at release time: this script is fetched over TLS from
# the channel, so the key travels with it (the installer-carries-the-pubkey pattern). A placeholder
# in the source tree that the release pipeline fills. Used only when no keyring is on disk.
RELEASE_KEYRING_PEM='__BRIARD_RELEASE_KEYRING_PEM__'

say() { printf 'briard: %s\n' "$*"; }
die() { printf 'briard: ERROR: %s\n' "$*" >&2; exit 1; }
# open_for_user hands the dashboard link to the invoking user's browser, on a desktop and only
# there ([V3b.31h]): a user to run it AS (the browser must be theirs, never root's), a display
# they can see (sudo keeps DISPLAY and XAUTHORITY; a Wayland session carries DISPLAY through
# Xwayland), and xdg-open present. Best-effort and silent otherwise -- a headless install is the
# printed link opened on a phone, not a failure -- and never waited on: xdg-open may block until
# the browser exits.
open_for_user() {
	[ -n "${1:-}" ] || return 0   # the report carried no link: nothing to hand over
	[ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] || return 0
	[ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] || return 0
	command -v xdg-open >/dev/null 2>&1 || return 0
	say "also asking your desktop to open it"
	sudo -u "$SUDO_USER" -H env DISPLAY="${DISPLAY:-}" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-}" XAUTHORITY="${XAUTHORITY:-}" \
		xdg-open "$1" >/dev/null 2>&1 &
}
fetch_url() { # url dest -- TLS download for the bootstrap agent (curl or wget, whatever the box has)
	if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
	else die "need curl or wget to fetch $1"; fi
}

# ---- 0. root -----------------------------------------------------------------------
[ "$(id -u)" = 0 ] || die "run as root (curl ... | sudo sh)"

# ---- 1. the gate's agent -- the ONLY artifact staged before the host is admitted ----
# The report card needs an executable agent to run, so exactly that much is staged here and not a
# byte more: everything heavy (the qemu bundle, the 2.5 GB guest image) waits until step 3, AFTER
# admission. Asking before taking is what lets the refusal say "nothing was installed" truthfully,
# and what keeps the card's DISK CHECK measuring a disk the installer has not already eaten into.
mkdir -p "$PREFIX/agent" "$STATE" "$RUNDIR"
if [ -n "${BRIARD_ARTIFACTS:-}" ]; then
	# Offline / hermetic-test path: install from an already-verified local staging dir.
	src="$BRIARD_ARTIFACTS"
	[ -x "$src/briard-agent" ] || die "staging dir $src has no briard-agent"
	install -m0755 "$src/briard-agent" "$PREFIX/agent/briard-agent"
	CARD_AGENT="$PREFIX/agent/briard-agent"
	# A local staging dir is FLAT (one directory, both chains' artifacts side by side); the
	# network fetch below lays the two chains out separately. Both are named through these two
	# so the install steps read one shape.
	HOSTSRC="$src"; GUESTSRC="$src"
else
	# Signed network fetch. Bootstrap a briard-agent over TLS -- the channel's integrity anchors
	# this FIRST binary -- then let it fetch and verify the whole set against the bundled release
	# keyring. The bootstrap only RUNS the verified fetch; what lands under /opt is the
	# Ed25519-verified set, so a compromised bootstrap cannot seed bad cattle.
	if [ ! -f "$KEYRING" ]; then
		case "$RELEASE_KEYRING_PEM" in
		*"BEGIN PUBLIC KEY"*) printf '%s\n' "$RELEASE_KEYRING_PEM" >"$KEYRING" ;;
		*) die "no release keyring at $KEYRING (the embedded key is a build placeholder; set BRIARD_UPDATE_KEYRING)" ;;
		esac
	fi
	say "bootstrapping the installer agent from $CHANNEL (host/$RELEASE) ..."
	# Under $PREFIX, NOT $RUNDIR: Debian and Ubuntu mount /run `noexec`, so a bootstrap staged
	# there cannot be executed at all. $PREFIX is where the agent lives anyway.
	#
	# Fetched from the TARGET's path, never a fixed one: the bootstrap is the binary that parses
	# the manifest, and a stale one that cannot parse a newer manifest is exactly the
	# forward-compat bricking the channel layout exists to prevent ([B.86]). `briard-agent` is the
	# one artifact the channel duplicates under its pointers for this fetch.
	boot="$PREFIX/bootstrap-agent"
	fetch_url "$CHANNEL/host/$RELEASE/linux/briard-agent" "$boot" || die "could not fetch the bootstrap agent from $CHANNEL/host/$RELEASE/linux"
	chmod +x "$boot"
	# Fail with the REASON. A bootstrap that cannot exec (noexec mount, wrong arch, a dynamically
	# linked binary whose interpreter this host lacks) is not a verification failure, and reporting
	# it as one sends the reader hunting for a bad signature.
	"$boot" help >/dev/null 2>&1 ||
		die "the bootstrap agent at $boot will not run on this host (see the error above); nothing installed"
	# The bootstrap IS the card's agent: it is a full briard-agent, and running the gate with it
	# means an unfit host is turned away before a single artifact is downloaded.
	CARD_AGENT="$boot"
fi

# ---- 2. the machine report card (the admission gate) -------------------------------
# Refuse-with-the-fix-named on an unbringable host, before we fetch gigabytes or boot a VM --
# never a half-install.
say "checking host readiness ..."
# The card is told the two things it cannot infer. VIP_ADDR, because judging a service address
# against THIS LAN is the one check that compares our intent with the household's network.
# BRIARD_NIC, because the card SELECTS the device the guest's L2 will hang off and validates it by
# creating a throwaway macvtap on it -- judging a different device than the install will use is the
# worst possible half-truth, a green card and an unreachable guest. The substrate is not passed at
# all: the card derives it from the device, so it cannot be told one the machine will not be on.
if ! VIP_ADDR="$VIP" BRIARD_NIC="$NIC" "$CARD_AGENT" --report-card; then
	# Leave the box as we found it: on the network path the bootstrap agent is the one thing we
	# put down, so take it back rather than claim "nothing was changed" while it sits there.
	[ -n "${BRIARD_ARTIFACTS:-}" ] || rm -f "$CARD_AGENT"
	die "host is not ready (see the fix above); nothing was installed"
fi

# ---- 3. the rest of the artifacts (cattle) -----------------------------------------
# Lay down /opt/briard from the staging dir. The agent binary + qemu bundle + guest
# image are the self-updating cattle; the base guest image is read-only backing.
mkdir -p "$PREFIX/guest-image"
if [ -z "${BRIARD_ARTIFACTS:-}" ]; then
	# The host is admitted: now the bootstrap fetches and verifies the whole set (qemu bundle, guest
	# image, a fresh briard-agent) against the bundled keyring, refusing anything unsigned.
	src="$PREFIX/staging"
	rm -rf "$src"
	say "fetching + verifying the signed artifact set (host/$RELEASE + guest) ..."
	# Both chains, all-or-nothing: the agent stages host/ and guest/ under $src and only places
	# $src once both have verified, so a host bundle never lands without the guest image it was
	# published beside.
	BRIARD_CHANNEL_URL="$CHANNEL" BRIARD_RELEASE="$RELEASE" BRIARD_KEYRING="$KEYRING" \
		"$boot" --fetch-install "$src" || die "artifact verification failed; nothing installed"
	HOSTSRC="$src/host"; GUESTSRC="$src/guest"
	# The verified qemu bundle arrives as a tarball; expand it to the qemu/ tree the install step
	# expects. Its bytes are already trusted (hash-checked against the signed manifest above).
	# The tar is rooted at the bundle itself (bin/ lib/ share/ PROVENANCE), NOT at a qemu/ dir, so
	# it must be extracted INTO one -- unpacking it beside the other artifacts scatters bin/ and
	# lib/ across the staging dir and leaves the copy below with no qemu/ to find.
	mkdir -p "$HOSTSRC/qemu"
	tar -xf "$HOSTSRC/qemu-bundle.tar" -C "$HOSTSRC/qemu" && rm -f "$HOSTSRC/qemu-bundle.tar"
	rm -f "$boot"
	# The verified agent replaces the bootstrap one staged for the card.
	[ -x "$HOSTSRC/briard-agent" ] || die "staging dir $HOSTSRC has no briard-agent"
	install -m0755 "$HOSTSRC/briard-agent" "$PREFIX/agent/briard-agent"
fi
# The macvtap launch wrapper -- the fd-passing shim the agent runs as the guest unit's
# ExecStart on the macvtap substrate. Cattle that rides with the agent ([B.86b]): it lands here,
# and an update stages briard-net-wrap.next beside it for briard-commit to move.
NET_WRAP=""
if [ -f "$HOSTSRC/briard-net-wrap" ]; then
	install -m0755 "$HOSTSRC/briard-net-wrap" "$PREFIX/agent/briard-net-wrap"
	NET_WRAP="$PREFIX/agent/briard-net-wrap"
fi
# THE QEMU TREE, REACHED THROUGH A LINK ([B.86b]). One extracted bundle per release lives at
# $PREFIX/agent/qemu-<release>/ -- inside the directory the frozen pivot commits in -- and
# $PREFIX/agent/qemu is a symlink to the current one. An update extracts the next release's
# tree beside it and stages qemu.next as a link; briard-commit then commits qemu with ONE
# rename of that link (\`mv -T\`), so no recursive delete ever runs near a frozen script and
# the previous tree simply stays on disk until the agent prunes it at a guest launch.
#
# $PREFIX/qemu stays as the PUBLIC path, a fixed link onto that moving one: the bundle bakes
# /opt/briard/qemu/lib/ld-linux... into its ELF interpreter (qemu-bundle.nix), so that path must
# always resolve to the running tree, and the agent's QEMU / QEMU_DATADIR defaults point through
# it (agent/host/config.go). Named by
# the installed release when a manifest is present, so a later pin back to it reuses the tree;
# the local staging path has no manifest and gets a fixed name.
QEMU_REL=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$HOSTSRC/manifest.json" 2>/dev/null || true)
QEMU_TREE="$PREFIX/agent/qemu-${QEMU_REL:-install}"
rm -rf "$QEMU_TREE"
mkdir -p "$QEMU_TREE"
cp -a "$HOSTSRC/qemu/." "$QEMU_TREE/"
chmod -R u+w "$QEMU_TREE"
ln -sfnT "$(basename "$QEMU_TREE")" "$PREFIX/agent/qemu"
# An install that predates the link laid a real directory here; a link cannot replace one.
if [ -d "$PREFIX/qemu" ] && [ ! -L "$PREFIX/qemu" ]; then rm -rf "$PREFIX/qemu"; fi
ln -sfnT agent/qemu "$PREFIX/qemu"
# THE GUEST BUNDLE ([B.86j]): the briard binaries the guest runs, shipped in the host chain and
# pushed into the guest by the agent at every bring-up. Same tree-and-link shape as qemu, same
# commit (briard-commit moves guest.next with -T). Existence-guarded: a channel that predates the
# bundle carries none, and the guest then runs the image's firmware -- which is a REFUSAL now
# ([B.138]), not a degraded mode, so an install that finds a bundle must land it.
if [ -f "$HOSTSRC/guest-bundle.tar" ]; then
	GUEST_TREE="$PREFIX/agent/guest-${QEMU_REL:-install}"
	rm -rf "$GUEST_TREE"
	mkdir -p "$GUEST_TREE"
	tar -xf "$HOSTSRC/guest-bundle.tar" -C "$GUEST_TREE"
	# Freeing the tarball is an OPTIMISATION for the network path, where $HOSTSRC is our own temp
	# dir -- not a step whose failure may abort an install. BRIARD_ARTIFACTS points at a read-only
	# staging dir (a Nix store path, which is how the install rigs run), and under `set -e` the
	# bare rm would take the whole install down with it ([B.141]).
	rm -f "$HOSTSRC/guest-bundle.tar" 2>/dev/null || true
	chmod -R u+w "$GUEST_TREE"
	ln -sfnT "$(basename "$GUEST_TREE")" "$PREFIX/agent/guest"
fi
cp -f "$GUESTSRC/nixos.qcow2" "$PREFIX/guest-image/nixos.qcow2"
# THE INSTALLED MANIFESTS, one per chain, each beside what it describes: the exact signed bytes
# that verified, so the node can say which release it is on -- and so the update path ([B.86b])
# can diff the target's manifest against it and fetch only what changed. Absent on the local
# staging path, which has no manifest to keep.
[ -f "$HOSTSRC/manifest.json" ]  && install -m0644 "$HOSTSRC/manifest.json"  "$PREFIX/agent/manifest.json"
[ -f "$GUESTSRC/manifest.json" ] && install -m0644 "$GUESTSRC/manifest.json" "$PREFIX/guest-image/manifest.json"
# The guest chain's node-local RECORD of the release this node runs ([B.86d]): pet state, beside
# the identity, because the guest itself knows only a closure path and the stable path orders
# on a release id. The copy under guest-image/ describes the IMAGE on disk; this one follows the
# running OS as updates land. Same bytes today, and they part ways at the first guest update.
[ -f "$GUESTSRC/manifest.json" ] && install -m0644 "$GUESTSRC/manifest.json" "$STATE/guest-release.json"
# `briard` -- the operator CLI, which is a MODE of the agent binary rather than a second
# one. Nothing under $PREFIX is on $PATH, so this symlink IS the CLI's existence as far as a user
# is concerned. A symlink rather than a copy: self-update replaces the binary in place, and
# a copy would leave a stale CLI talking to a newer agent -- the exact skew one binary exists to
# prevent. Best-effort: an unwritable /usr/local/bin costs the CLI, never the install.
mkdir -p /usr/local/bin 2>/dev/null || true
ln -sfn "$PREFIX/agent/briard-agent" /usr/local/bin/briard 2>/dev/null ||
	say "note: could not link /usr/local/bin/briard; run $PREFIX/agent/briard-agent directly"

# ---- 4. networking -----------------------------------------------------------------
# NOTHING IS CONFIGURED HERE. The agent owns the whole of it: which device the guest's L2 hangs
# off, whether that device's being a bridge makes the substrate one port or two macvtap children,
# the addresses on the host's side, and the tun driver behind them. All of it converges on the
# agent's ordinary status tick, out of a binary the channel can fix ([B.150]).
#
# One thing still has to be true, and it is checked rather than assumed: the fd-passing launch
# wrapper must be staged. Without it the agent cannot attach a macvtap to qemu, and that failure
# belongs at the install rather than at the first guest launch.
[ -n "$NET_WRAP" ] || die "the briard-net-wrap wrapper is absent from staging; the guest cannot be given a NIC"

# ---- 5. where this node's disks GO -- the agent makes them ([B.157]) ----------------
# THREE PATHS AND NO mkfs. The agent creates each of these at its first start, before it launches
# anything that would attach one (agent/host/disks.go, agent/platform/alloc.go) -- thick for the
# data volume, sparse for the state disk, a qcow2 overlay on the image for the guest's OS disk.
#
# So what is left here is the LAYOUT: which path each one takes on this host, which is this
# script's decision because it is the one that knows where it put the image and the state dir.
# The agent makes what the paths name, and makes nothing it was not told about -- an empty path is
# how a harness says "this node has no such disk", and inventing one would hand qemu a `-drive`
# for a file nobody created.
#
# ⚠️ The exclusivity that guarded the data volume ([B.126]) moved WITH it and got stronger on the
# way: `[ ! -f ]` is true for "absent" and for "present but unstat-able" alike, and the allocation
# writes from byte 0 -- so the creation has to BE the proof of absence. It was a `noclobber`
# subshell here; it is O_EXCL there, which is the same idea the kernel answers directly.
DATA="$STATE/data.img"
STATE_DISK="$STATE/state.img"
OVERLAY="$PREFIX/guest.qcow2"   # cattle: rebuilt on the image at every launch, not just at install

# ---- 6. the node's own files: the agent's scripts, its config, its units ------------
# SIX FILES, and only ONE of them is generated. The three scripts and the three units are shipped
# artifacts copied out of the verified staging dir ([B.157]); config.env is written here because it
# is the only one whose content is about THIS host.
mkdir -p "$UNIT_DIR"

# ---- the agent's three frozen scripts: copied, not written ([B.157]) -----------------
# briard-exec and briard-commit are self-update's on-disk pivot ([B.84]); briard-update is the
# fetch, which must not be shipped by the thing it updates ([B.86a]) -- an agent that runs fine
# and has a bug in fetch/verify/stage could otherwise never be replaced, fleet-wide at once.
#
# FROZEN, which is the safety property, and SHIPPED, which is new. Frozen means dumb,
# agent-INDEPENDENT shell, so a bug in the volatile agent cannot wedge the mechanism that replaces
# it -- unchanged by where the file comes from. Shipping means the three are signed artifacts of
# the host chain rather than heredocs this script renders, so they are versioned, diffable, and
# covered by the same manifest as the binary they start. They carry no interpolation at all now:
# every path in them is the fixed prefix.
say "installing the agent's scripts"
for s in briard-exec briard-commit briard-update; do
	[ -f "$HOSTSRC/$s" ] || die "$s is absent from staging; this release cannot be installed"
	install -m0755 "$HOSTSRC/$s" "$PREFIX/agent/$s"
done
# ---- the node's configuration: A FILE, NOT THE UNIT ([B.150](a)) -------------------------
# Everything the agent is told about this host lives here, and the agent reads it with the
# environment layered ON TOP (agent/host/config.go, loadConfigFile), so a rig that exports a
# variable still wins.
#
# A FILE BECAUSE A UNIT CANNOT BE REWRITTEN. Some of these are decisions the agent revisits, and a
# decision it can revisit has to live somewhere it can rewrite. Plain and greppable at 2am; 0600
# because it is root's business alone.
#
# ⚠️ TWO KINDS OF LINE, AND ONLY TWO ([B.157]). What this script LAID DOWN -- the files it created
# or staged -- and what the OPERATOR said, copied verbatim from the environment. There is
# deliberately no third kind: NO DEFAULTS. A key that is absent is a key the agent answers from its
# own shipped default, which a release can change and a line in this file could not.
#
# ⚠️ AND NO IDENTIFIERS. The node id, the flock id and the flock name are the AGENT's, minted once
# into pet state at its first start (agent/host/identity.go). This script used to mint them and
# write the values down -- while shelling out to the agent for one of the three, which is the shape
# of that mistake in one line.
#
# The line between the first kind and a default is "did this install make the thing the path names".
# It did make the disks and extract the qemu tree, so those are facts about this host -- and an
# agent handed no STATE_DISK must not invent one, because every agent-* rig runs exactly that way.
cat > "$PREFIX/config.env" <<EOF
# briard node configuration, written by install.sh. KEY=value, one per line; blank lines and
# '#' comments are ignored, whitespace either side of the '=' is trimmed, and NOTHING else is
# parsed -- no quoting, no expansion, no export. Every value is a path, a device name, an
# address or a duration. What each key MEANS, and what it defaults to when absent, is
# agent/host/config.go -- not this file, and not the installer that wrote it.
# The bundle and the disks THIS INSTALL laid down. Paths to files it created or staged, so they
# are facts about this host rather than defaults -- and their absence is a decision too, which is
# why the agent defaults none of them: an empty one is how a harness says it has no such disk.
QEMU=$PREFIX/qemu/bin/qemu-system-x86_64
QEMU_DATADIR=$PREFIX/qemu/share/qemu
NET_WRAP_BIN=$NET_WRAP
GUEST_IMAGE=$PREFIX/guest-image/nixos.qcow2
GUEST_DISK=$OVERLAY
DATA_DISK=$DATA
STATE_DISK=$STATE_DISK
EOF

# The operator's own settings, copied verbatim. EVERY `BRIARD_*` in the environment lands here with
# the prefix stripped, so `BRIARD_CPU=qemu64` becomes `CPU=qemu64` and the rule is one sentence
# rather than a table this script has to keep in step with config.go.
#
# The exclusions are the names that configure THE INSTALL rather than the node: where to fetch
# from, which release, and where the units go -- plus BRIARD_CONFIG, which names this very file, so
# copying it in would be a node telling itself where it already is. The list shrank when the disks
# moved ([B.157]): BRIARD_DATA_SIZE is an ordinary knob now, because the AGENT allocates the volume
# and therefore the size is a value the node holds rather than a step this script performs.
#
# Sorted, so two installs given the same environment produce byte-identical files. Values are
# single-line by the format's own rule, which is what makes a line-oriented copy correct.
env | grep '^BRIARD_[A-Z0-9_]*=' |
	grep -Ev '^BRIARD_(ARTIFACTS|RELEASE|UNIT_DIR|CONFIG)=' |
	sed 's/^BRIARD_//' | LC_ALL=C sort >> "$PREFIX/config.env"
chmod 0600 "$PREFIX/config.env"

# ---- the units: copied, not written ([B.157]) ---------------------------------------
# The three unit files are SHIPPED ARTIFACTS of the host chain, so they arrive in the staging dir
# beside the agent binary and are hashed by the same signed manifest. Copying them rather than
# rendering them is the whole point: a unit written here by heredoc is frozen where no release can
# reach it, and unsigned into the bargain. Every path in them is /opt/briard, which is a constant
# on every install (see the top of this file), so there is nothing left to interpolate.
#
# Refused rather than skipped when one is missing: a staging dir without briard-agent.service is
# one this host cannot run briard from, and finding that out at the first boot is worse than here.
say "installing the systemd units to $UNIT_DIR"
for u in briard-agent.service briard-update.service briard-update.timer; do
	[ -f "$HOSTSRC/$u" ] || die "$u is absent from staging; this release cannot be installed"
	install -m0644 "$HOSTSRC/$u" "$UNIT_DIR/$u"
done

if command -v systemctl >/dev/null 2>&1; then
	say "registering briard with systemd"
	# The clock mark the closing wait reads the journal from: a reinstall's journal still holds
	# the previous life's "healthy=true", and a link minted on a stale line is minted before the
	# guest exists.
	UNITS_STARTED=$(date '+%Y-%m-%d %H:%M:%S')
	systemctl daemon-reload
	if [ "$UNIT_DIR" = /etc/systemd/system ]; then
		# Persistent install: enable (survive reboot) + start now.
		say "enabling briard-agent (+ the daily update timer)"
		systemctl enable --now briard-agent.service
		systemctl enable --now briard-update.timer
	else
		# Units in a non-persistent dir (e.g. /run) can't be enabled; just start them.
		say "starting briard-agent (+ the daily update timer)"
		systemctl start briard-agent.service briard-update.timer
	fi
	# THE NAME IS READ, NOT KNOWN. The agent mints it at its first start ([B.157]) and records it
	# at $STATE/flock-name, so this script has to wait for the agent before it can say what the
	# household should type. That reordering is the whole cost of the move, and it buys back the
	# closing message being about a node that EXISTS rather than one about to.
	#
	# Bounded, and the bound is generous because the thing being waited on is a first boot on
	# somebody's spare desktop. Past it the message still goes out, naming the file instead.
	say "waiting for the guest to come up (up to 3 minutes)..."
	FLOCK_NAME=""
	waited=0
	while [ "$waited" -lt 180 ]; do
		[ -s "$STATE/flock-name" ] && FLOCK_NAME="$(cat "$STATE/flock-name")" && break
		sleep 2
		waited=$((waited + 2))
	done

	# THE NAME, AND ONLY THE NAME ([B.157]). The address is deliberately not here any more: under
	# DHCP there is none to print yet, and offering a second way in made the sentence long to say
	# a thing that is true of fewer installs than it sounds. The name is the one that stays true
	# when the address moves, and it is what the front door routes on.
	#
	# NOTE what this deliberately does NOT promise: that the router's client list shows this same
	# name. It does not, and that is a decision rather than an oversight (V3.20) -- DHCP option 12
	# stays `briard-<mac tail>`, derived in-guest from the NIC's own address, because changing a
	# hostname mid-lease is a change no one can predict a server's reaction to and a rename must
	# never risk the address.
	#
	# WHAT IS ON THE NODE is not said here either. That sentence reads the node's own state to talk
	# to a person, which is a thing that changes -- so it is the agent's, printed by the dashboard
	# verb below, which is the same answer a household gets running it a month from now.
	if [ -z "$FLOCK_NAME" ]; then
		say "installed. the agent is still starting; it will record this install's name at $STATE/flock-name"
	else
		say "installed. the guest is booting; briard will answer at http://briard-$FLOCK_NAME.local/"
	fi
	# THE LINK IS THE LAST THING THE INSTALLER PRINTS ([V3b.31h]). The dashboard's only door is a
	# one-time link the agent mints ([V3b.31b]), and the guest has to be up for it -- so wait for
	# it, bounded, on the agent's OWN word: the status line it logs once the node is primary and
	# the chain (the front door, the dashboard) passed its health gate. That is the same signal
	# the tier-4 rig waits on, and it is local -- no name to resolve, no address to know under
	# DHCP. Then mint once, the way `sudo briard dashboard` does. Past the bound the sentence says
	# how instead, as it always did: a link printed before the door is up would 503 in the
	# household's face, and a link that expired during a slow boot would be worse.
	#
	# ⚠️ THE AGENT'S OWN WORDS ARE PRINTED, not re-rendered here ([B.157]). `briard dashboard` says
	# what is on the node and hands over the link; this script shows what it said. The two used to
	# be separate renderings of the same facts -- an install-time sentence in shell and an any-time
	# one in Go -- which is two things that can disagree about a household's own node.
	report=""
	if command -v journalctl >/dev/null 2>&1; then
		waited=0
		while [ "$waited" -lt 180 ]; do
			if journalctl -u briard-agent --since "$UNITS_STARTED" --no-pager 2>/dev/null | grep -q 'primary=true.*healthy=true'; then
				report=$("$PREFIX/agent/briard-agent" dashboard 2>/dev/null) || report=""
				[ -n "$report" ] && break
			fi
			sleep 5
			waited=$((waited + 5))
		done
	fi
	if [ -n "$report" ]; then
		say "your dashboard is ready:"
		say ""
		printf '%s\n' "$report"
		say "another link, any time: sudo briard dashboard"
		# The desktop hand-off needs the bare URL, so it is picked back out of what was printed
		# rather than minted a second time -- a second mint would burn the link just shown.
		open_for_user "$(printf '%s' "$report" | grep -oE 'https?://[^ ]+/\?code=[0-9a-f]+' | head -1)"
	else
		say "the guest is still booting. once it answers, get a one-time link to your dashboard with: sudo briard dashboard"
	fi
else
	# Belt, not the gate: the report card refuses a host that is not systemd-booted before anything
	# is written (step 2). Reaching HERE means systemctl vanished between that check and this line,
	# which is not a shape worth a nicer message -- it is worth not pretending the install finished.
	die "no systemd (this install path targets systemd hosts)"
fi
