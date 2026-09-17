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
# once; nothing it writes can be reached by a release afterwards. So it configures only what never
# changes -- where things live, which identifiers are minted, which units exist -- and every
# decision that is a property of THIS MACHINE ON THE DAY IT IS ASKED belongs to the agent, which
# re-asks it at every start and ships fixes through the channel. Which device the guest's L2 hangs
# off, which substrate that implies, which addresses this node numbers itself from and whether the
# tun driver is loaded are all the agent's ([B.150]). Adding one back here is the mistake.
#
# It installs NO SERVICE. A node is a node first: ready, replicating, able to fail over -- then you
# choose what runs on it. So the VIP answers with Briard's own page, and the health probe watches
# that front door, the one address that answers whether or not anything is installed.
#
# Cattle/pet FHS:
#   /opt/briard     = cattle: signed self-updating binaries + qemu bundle + guest image, plus
#                     config.env. `rm -rf /opt/briard` + reinstall = a fresh host.
#   /var/lib/briard = pet: the data volume, this node's identifiers, the subnets it drew.
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
VIP_IP="${VIP%%/*}"   # the bare address, for the closing message; EMPTY under DHCP
NIC="${BRIARD_NIC:-}"

# The pet data volume's size, because this script allocates it (step 5). Sized for a real service's
# data: Home Assistant's `.storage` plus the recorder SQLite outgrows a gigabyte in months, and
# growing a DRBD-backed volume afterwards is not a one-liner. Whole GiB -- the dd fallback parses
# it that way.
DATA_SIZE="${BRIARD_DATA_SIZE:-4G}"

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
AGENT="$PREFIX/agent/briard-agent"

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

# ---- 5. disks: the pet data volume + the (cattle) guest overlay ---------------------
# data.img is the node's data disk (the guest lays LUKS/LVM/btrfs on it; DRBD only once a
# peer exists) -- pet, created once, preserved across a
# reinstall. The guest overlay is cattle: a writable qcow2 backed by the
# read-only base image, recreated every install (the base may have moved).
DATA="$STATE/data.img"
if [ ! -f "$DATA" ]; then
	# ⚠️ CREATE IT EXCLUSIVELY, and the test above is not enough on its own ([B.126]): `[ ! -f ]`
	# is true for "absent" AND for "present but cannot be stat'd", and the dd fallback below
	# writes from byte 0 -- so the test alone would let an unreadable-but-present data volume be
	# zeroed. noclobber makes the CREATION the proof of absence: it refuses if anything is there,
	# so "I could not tell" can no longer route into a destructive write.
	if ! (set -o noclobber; : >"$DATA") 2>/dev/null; then
		die "$DATA already exists but could not be read -- refusing to touch it; move it aside if this node really is new"
	fi
	say "creating the $DATA_SIZE data volume at $DATA -- your services' data lives here"
	# THICK, not sparse. This is the one volume whose failure mode is unacceptable: DRBD replicates
	# it and the guest writes service data into it, so a `truncate` sparse file that the host cannot
	# actually back turns into ENOSPC *underneath a replicated filesystem*, mid-write, on the node
	# holding the primary role. Allocating it up front makes "is there room for this node's data?"
	# a question answered once, at install time, by a command that either succeeds or refuses --
	# rather than months later, by a write that fails. fallocate is the fast path (extent
	# reservation, no I/O); dd is the portable fallback for filesystems without it.
	if ! fallocate -l "$DATA_SIZE" "$DATA" 2>/dev/null; then
		say "fallocate unavailable; preallocating with dd (slower)"
		if ! dd if=/dev/zero of="$DATA" bs=1M count="$(($(echo "$DATA_SIZE" | tr -d 'Gg') * 1024))" status=none; then
			rm -f "$DATA"
			die "could not allocate the ${DATA_SIZE} data volume at $DATA (out of disk?)"
		fi
	fi
fi
# The flock's identity -- PET, so it survives the `rm -rf /opt/briard` cattle reset along with the
# data it belongs to. The VIP's MAC derives from it, so keeping it is what keeps this node's address
# stable across a reinstall, and (once pairing carries it) across a failover to a second node. It is
# per-install random: a MAC derived from anything shared would collide inside a house that runs two.
FLOCK_ID_FILE="$STATE/flock-id"
if [ ! -s "$FLOCK_ID_FILE" ]; then
	# /proc/sys/kernel/random/uuid is on every Linux we target and needs no coreutils.
	(cat /proc/sys/kernel/random/uuid 2>/dev/null || od -An -N16 -tx1 /dev/urandom | tr -d ' \n') \
		>"$FLOCK_ID_FILE" || die "could not write the flock id to $FLOCK_ID_FILE"
	chmod 0600 "$FLOCK_ID_FILE"
	say "generated this home's identity at $FLOCK_ID_FILE -- keep this file to keep your address"
fi
FLOCK_ID="$(cat "$FLOCK_ID_FILE")"
[ -n "$FLOCK_ID" ] || die "the flock id at $FLOCK_ID_FILE is empty; remove it to regenerate"

# ---- 5b. the other two identifiers -------------------------------------------------
# THREE identifiers, one job each:
#
#   node id     node-scoped,  hidden   DRBD `on <name>`, guest hostname, cloud key   <- below
#   flock id    flock-scoped, hidden   service MAC -> DHCP client-id -> THE LEASE     <- above
#   flock name  flock-scoped, VISIBLE  mDNS `briard-<name>.local`                     <- below
#
# The property this buys: a NAME is a label and an IDENTITY is an id, so renaming what humans see
# never touches the MAC, the client-id or the DRBD metadata -- A RENAME NEVER MOVES THE ADDRESS.

# This node's name. PET, and it has to be: DRBD writes `on <name>` into the resource and matches it
# against the running hostname, so a node id regenerated by a cattle reinstall would leave the guest
# unable to recognise its OWN metadata on the pet data volume it just kept.
#
# `briard-node-<6 hex>` -- readable in a journal, opaque in a database (the cloud keys on the bare
# `3f9a2c`). NOT derived from anything: not the hostname (the household renames their desktop), not
# the MAC (that is the flock id's job, and this must stay node-scoped), not the login name (V3.19d
# measured what leaking that onto a LAN looks like).
#
# NO MIGRATION, deliberately: an installed node cannot be renamed, because DRBD metadata and the
# .res `on <name>` are keyed to the old value and deriveMAC would shift under it. Pre-beta does not
# owe that, so the id is generated on a FRESH install only -- exactly like the flock id above.
NODE_ID_FILE="$STATE/node-id"
if [ ! -s "$NODE_ID_FILE" ]; then
	printf 'briard-node-%s\n' "$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')" \
		>"$NODE_ID_FILE" || die "could not write the node id to $NODE_ID_FILE"
	chmod 0600 "$NODE_ID_FILE"
	say "generated this machine's identity at $NODE_ID_FILE -- keep this file; the replicated disk is keyed to it"
fi
NODE_NAME="$(cat "$NODE_ID_FILE")"
[ -n "$NODE_NAME" ] || die "the node id at $NODE_ID_FILE is empty; remove it to regenerate"

# The flock's NAME -- the one identifier in this whole install a human is ever shown. PET, so the
# name on the LAN survives a cattle reinstall along with the address it points at.
#
# Minted by the agent binary rather than here: it is two words drawn from an 846-word list that the
# cloud validates a claimed name against, so a shell copy of that list would be a second one to
# keep in step for no gain. Random words rather than $SUDO_USER because the name is not local --
# an account turns it into `<name>.briard.casa`, so the offline name and the domain want to be the
# same string, collisions stop mattering across 178,928 of them, no sanitiser is needed (a curated
# word is a valid DNS label by construction), and a login name never reaches the router's client
# list.
FLOCK_NAME_FILE="$STATE/flock-name"
if [ ! -s "$FLOCK_NAME_FILE" ]; then
	"$AGENT" --mint-flock-name >"$FLOCK_NAME_FILE" ||
		die "could not mint a flock name with $AGENT --mint-flock-name"
	chmod 0644 "$FLOCK_NAME_FILE" # world-readable: it is a public name, not a secret
	say "this install is called $(cat "$FLOCK_NAME_FILE") -- that is the name it answers to on your network"
fi
FLOCK_NAME="$(cat "$FLOCK_NAME_FILE")"
[ -n "$FLOCK_NAME" ] || die "the flock name at $FLOCK_NAME_FILE is empty; remove it to regenerate"

# THE STATE DISK ([B.86g]): node-local, beside the data disk and like it PET -- it holds the
# guest's podman storage (the service images, content-addressed and worth every byte not
# re-pulled), its journal and the deadman's backoff state; a reinstall or a rescue must not cost
# those. Sparse, so the 8 GiB is a ceiling and not a charge against the report card's floor;
# the guest formats it on first boot when it finds no filesystem, so no mkfs is needed here.
STATE_DISK="$STATE/state.img"
if [ ! -e "$STATE_DISK" ]; then
	truncate -s 8G "$STATE_DISK" || die "could not create the state disk at $STATE_DISK"
	chmod 0600 "$STATE_DISK"
	say "created the guest's state disk at $STATE_DISK (formatted by the guest on first boot)"
fi
OVERLAY="$PREFIX/guest.qcow2"   # cattle: recreated each install, dropped by `rm -rf /opt/briard`
say "creating the VM disk at $OVERLAY"
rm -f "$OVERLAY"
if ! "$PREFIX/qemu/bin/qemu-img" create -f qcow2 \
	-b "$PREFIX/guest-image/nixos.qcow2" -F qcow2 "$OVERLAY"; then
	die "qemu-img create failed (rc=$?)"
fi
say "VM disk created"

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
# ⚠️ TWO KINDS OF LINE, AND ONLY TWO ([B.157]). What this script COMPUTED about this host -- the
# identifiers it minted, the files it created or staged -- and what the OPERATOR said, copied
# verbatim from the environment. There is deliberately no third kind: NO DEFAULTS. A key that is
# absent is a key the agent answers from its own shipped default, which a release can change and a
# line in this file could not.
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
NODE=$NODE_NAME
FLOCK_ID=$FLOCK_ID
FLOCK_NAME=$FLOCK_NAME
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
# from, which release, where the units go, how big the data volume is -- and BRIARD_CONFIG, which
# names this very file, so copying it in would be a node telling itself where it already is.
#
# Sorted, so two installs given the same environment produce byte-identical files. Values are
# single-line by the format's own rule, which is what makes a line-oriented copy correct.
env | grep '^BRIARD_[A-Z0-9_]*=' |
	grep -Ev '^BRIARD_(ARTIFACTS|RELEASE|UNIT_DIR|DATA_SIZE|CONFIG)=' |
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
	# Lead with the NAME and keep the address as the fallback. The name is the one that stays true
	# if the address ever moves, and the address is the one that still works if a client's mDNS
	# does not (Android is the usual offender). Naming both costs a line and removes a support
	# round-trip; naming only the address is what made the docs wrong in every house but ours.
	#
	# Under DHCP we cannot name the address at all: it is acquired inside the guest at promotion,
	# which has not happened yet. So the name carries the whole message, and we say where the
	# address will show up rather than inventing one to print -- printing a plausible-but-wrong
	# address is the exact failure this item exists to end.
	#
	# NOTE what this deliberately does NOT promise: that the router's client list shows this same
	# name. It does not, and that is a decision rather than an oversight (V3.20) -- DHCP option 12
	# stays `briard-<mac tail>`, derived in-guest from the NIC's own address, because changing a
	# hostname mid-lease is a change no one can predict a server's reaction to and a rename must
	# never risk the address. So the wording says "a briard- client", which is true of both.
	# The service line is a BRANCH rather than a constant: "no service is installed on it yet" is
	# true of a first install and false of a cattle reinstall, because the service manifests are pet
	# ($STATE/services/, one file per service) and the agent brings them back on its own. The files
	# are right there and they are the very files the agent reads, so this script can say which.
	# Best-effort on the names: a manifest we cannot parse still gets a true sentence, a vaguer one.
	svcs=""
	for f in "$STATE"/services/*.json; do
		[ -f "$f" ] || continue
		name=$(grep -o '"name":"[^"]*"' "$f" 2>/dev/null | head -1 | cut -d'"' -f4)
		[ -n "$name" ] || name="a service"
		svcs="${svcs:+$svcs, }$name"
	done
	if [ -n "$svcs" ]; then
		SERVICE_NOTE="$svcs is already installed on it and is coming back up"
	else
		SERVICE_NOTE="no service is installed on it yet"
	fi
	if [ -n "$VIP_IP" ]; then
		say "installed. the guest is booting; briard will answer at http://briard-$FLOCK_NAME.local/ (or http://$VIP_IP/) -- $SERVICE_NOTE"
	else
		say "installed. the guest is booting; briard will answer at http://briard-$FLOCK_NAME.local/ -- it takes its address from your router, where it shows up as a \"briard-\" client -- $SERVICE_NOTE"
	fi
	# THE LINK IS THE LAST THING THE INSTALLER PRINTS ([V3b.31h]). The dashboard's only door is a
	# one-time link the agent mints ([V3b.31b]), and the guest has to be up for it -- so wait for
	# it, bounded, on the agent's OWN word: the status line it logs once the node is primary and
	# the chain (the front door, the dashboard) passed its health gate. That is the same signal
	# the tier-4 rig waits on, and it is local -- no name to resolve, no address to know under
	# DHCP. Then mint once, the way `sudo briard dashboard` does. Past the bound the sentence says
	# how instead, as it always did: a link printed before the door is up would 503 in the
	# household's face, and a link that expired during a slow boot would be worse.
	link=""
	if command -v journalctl >/dev/null 2>&1; then
		say "waiting for the guest to come up (up to 3 minutes), to hand you a link..."
		waited=0
		while [ "$waited" -lt 180 ]; do
			if journalctl -u briard-agent --since "$UNITS_STARTED" --no-pager 2>/dev/null | grep -q 'primary=true.*healthy=true'; then
				link=$("$PREFIX/agent/briard-agent" dashboard 2>/dev/null | grep -oE 'https?://[^ ]+/\?code=[0-9a-f]+' | head -1) || link=""
				[ -n "$link" ] && break
			fi
			sleep 5
			waited=$((waited + 5))
		done
	fi
	if [ -n "$link" ]; then
		say "your dashboard is ready. open this on any device on your network (it works once, within 10 minutes):"
		say ""
		say "    $link"
		say ""
		say "another link, any time: sudo briard dashboard"
		open_for_user "$link"
	else
		say "the guest is still booting. once it answers, get a one-time link to your dashboard with: sudo briard dashboard"
	fi
else
	# Belt, not the gate: the report card refuses a host that is not systemd-booted before anything
	# is written (step 2). Reaching HERE means systemctl vanished between that check and this line,
	# which is not a shape worth a nicer message -- it is worth not pretending the install finished.
	die "no systemd (this install path targets systemd hosts)"
fi
