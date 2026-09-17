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

# ---- knobs (env-overridable; the tests pin the deterministic ones) ------------------
# ⚠️ THE THREE PATHS ARE CONSTANTS, NOT KNOBS ([B.157]). /opt/briard is baked into the qemu
# bundle's own ELF interpreter (/opt/briard/qemu/lib/ld-linux...), into the agent's default config
# path and into the CLI's UPDATE_BASE default, so an install anywhere else produces a qemu that
# cannot execute. They are named here because the script reads better for it, and because the
# shipped unit files spell the same paths -- a move changes both.
PREFIX=/opt/briard
STATE=/var/lib/briard
RUNDIR=/run/briard
# The one real knob of the four: NixOS's /etc/systemd/system is a read-only store path, so the
# install rigs point this at /run/systemd/system.
UNIT_DIR="${BRIARD_UNIT_DIR:-/etc/systemd/system}"

# The device the guest's L2 hangs off. Empty -- every ordinary install -- means the agent selects
# the one holding the default route and re-asks at every start. Naming a BRIDGE is how a user who
# already built one gets us to join it: the substrate is derived from the device, never chosen, so
# a bridge parent gets one port and the guest makes its own service identity on top, and anything
# else gets macvtap children. We never create a bridge.
NIC="${BRIARD_NIC:-}"

# The guest's three NICs, by the name of the host device behind each:
TAP="${BRIARD_TAP:-briard0}"                # eth2, the service NIC -- where the VIP lives
DRBD_TAP="${BRIARD_DRBD_TAP:-briard-drbd0}" # eth1, the system NIC -- this node's node IP, and where DRBD binds
# eth3, the private host<->guest link: a plain tap on neither the parent nor the bridge, and the
# host's only network path to the VM it runs (macvtap deliberately isolates the two). It is
# addressed at both ends, because avahi joins the IPv4 mDNS group only on an interface that has a
# v4 address. Pure L2 substrate: it does not exist on a Windows host, so NO BRIARD CODE MAY
# REFERENCE ITS RANGE -- code that dials it could not run there.
PRIV_TAP="${BRIARD_PRIV_TAP:-briard-priv0}"

# The three private ranges this node numbers itself from: the flock's system subnet, the private
# link's, and the guest-internal pod pool. Each is DRAWN by the agent against the network this
# machine can see, recorded in $STATE/subnets and kept for the life of the node (agent/subnet,
# agent/host/subnets.go). Set one to a bare "10.42.7" to pin it -- the escape hatch for a machine
# whose 10/8 is carved up enough that the draw refuses.
BRIARD_SYSTEM_SUBNET="${BRIARD_SYSTEM_SUBNET:-}"
BRIARD_PRIV_SUBNET="${BRIARD_PRIV_SUBNET:-}"
BRIARD_POD_SUBNET="${BRIARD_POD_SUBNET:-}"

# The service address, in CIDR form -- it is an address on the USER'S LAN and the LAN's prefix is
# not ours to assume. UNSET MEANS DHCP, and there is deliberately no default: any address we could
# pick is a guess about someone else's network, while a lease is the router telling us the answer
# out of its own pool.
VIP="${BRIARD_VIP:-}"
VIP_IP="${VIP%%/*}"   # the bare address; EMPTY under DHCP, where nobody knows it yet

# The pet data volume: thick-allocated (step 5), sized for a real service's data. Home Assistant's
# `.storage` plus the recorder SQLite outgrows a gigabyte in months, and growing a DRBD-backed
# volume afterwards is not a one-liner. Whole GiB -- the dd fallback parses it that way.
DATA_SIZE="${BRIARD_DATA_SIZE:-4G}"

# The data volume's encryption policy, pushed to the guest at every bring-up. "auto" encrypts
# wherever the guest's CPU has AES and runs in the clear where it does not; "off" is somebody
# deciding otherwise; "adiantum" is the cipher for hardware with no AES acceleration (Pi 4 and
# older, pre-AES-NI x86), a documented opt-in.
# ⚠️ IT APPLIES AT FORMAT TIME ONLY. Changing it on an installed node does not convert its volume:
# that is a live `pvmove` between a plaintext and an encrypted PV -- a verb, not a config flip.
DATA_ENCRYPTION="${BRIARD_DATA_ENCRYPTION:-auto}"

# The guest's CPU model. "max" = every feature the accelerator can expose, which under KVM is this
# host's own CPU; qemu's default (qemu64) is below x86-64-v2 and costs the guest aes/sha-ni/sse4.2
# plus the CPUID bits its kernel needs to mitigate Spectre. Free for us -- a briard guest never
# migrates and never saves RAM state. BRIARD_CPU=qemu64 falls back where passthrough is the suspect.
CPU_MODEL="${BRIARD_CPU:-max}"

# The guest's serial console (its kernel + systemd), captured to the host. Under macvtap the host
# cannot reach the guest over the network at all, so this file is the only witness to anything that
# happens inside the VM, and it is what every field diagnosis runs on. Set BRIARD_CONSOLE= (empty)
# to opt out -- `-` and not `:-` below, so that empty reads as a DECISION rather than as unset.
CONSOLE="${BRIARD_CONSOLE-/var/log/briard-guest-console.log}"

# The signed release channel root. Under it, one directory per release CHAIN -- `host/` (agent,
# net-wrap, qemu) and `guest/` (the OS image) -- each holding one directory per version plus the
# pointers `stable` and `latest`, which are byte-copies of one version's signed manifest
# (scripts/publish-release.sh spells the tree out). The bucket also serves `catalog/` and THIS
# script at the root, each on its own lifecycle.
CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}"
# WHICH release: `stable` (what strangers get, a tested pair by construction), `latest` (what was
# published most recently -- how a release is proven before promotion), or an exact host id
# (`v3.<date>.<rev>`; its guest release is the one the host manifest names). One selector, both chains.
RELEASE="${BRIARD_RELEASE:-stable}"
KEYRING="${BRIARD_KEYRING:-$PREFIX/keyring.pem}"       # the bundled release public key (verify root)

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
		*) die "no release keyring at $KEYRING (the embedded key is a build placeholder; set BRIARD_KEYRING)" ;;
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
# always resolve to the running tree, and QEMU= / QEMU_DATADIR= below point through it. Named by
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
QEMU="$PREFIX/qemu/bin/qemu-system-x86_64"
QEMU_DATADIR="$PREFIX/qemu/share/qemu"

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

# ---- 6. what the units run: the generated scripts and the config file ---------------
# The three unit FILES are shipped and copied at the end of this step; everything between here and
# there is what they point at: the self-update pivot pair, the node's config file and the update
# script. Each is generated because each bakes a value.
mkdir -p "$UNIT_DIR"

# The console capture. The PATH is all that is decided here; the BOUND and the MODE are the
# agent's, applied at every guest launch before qemu opens the file (agent/platform, [B.157]).
# That is the granularity the cap needs -- the guest is relaunched on every OS upgrade, every
# rollback and every rung of the recovery ladder, and a check that ran once when the agent started
# missed all of them, which is to say it missed the crash-looping guest it exists for.
CONSOLE_CONF=""
[ -n "$CONSOLE" ] && CONSOLE_CONF="GUEST_SERIAL=$CONSOLE"
# The fd-passing launch wrapper, which the agent renders the guest launch behind under macvtap.
# NET_MODE is NOT written: the agent derives it from the device ([B.150](c)), and writing it here
# would be install-time frozen and free to disagree with what the machine turns out to be. The
# wrapper's PATH is written unconditionally because it is a path, not a decision -- it costs
# nothing on a node that turns out to be on a bridge and does not use it.
NET_CONF="NET_WRAP_BIN=$NET_WRAP"
# The release keyring is the agent's trust root for BOTH signed host-agent self-updates and the
# signed service catalog (`briard service install` verifies a manifest against it). Both fail
# CLOSED without it -- self-update simply switches itself off, silently -- so a node installed
# without this env is one that can never update itself and can never install a service, with the
# key sitting right there on disk unread. Only wired when a keyring actually exists: the
# BRIARD_ARTIFACTS path (hermetic tests, install-from-source) has no channel and no key, and
# pointing the agent at a missing file would be worse than leaving it unset.
# The CATALOG the agent installs services FROM. Parameterised for the same reason the channel
# is, and separately from it: /catalog/ is live runtime content on its own lifecycle, not
# release content (nothing in a release publish touches it). Unset = the published default in
# the agent, so a normal install is unchanged. It exists because a channel signed with any key
# but the release key cannot use a catalog signed WITH it -- UPDATE_KEYRING is one trust root
# for both -- which made a staged channel untestable end to end without a drop-in [V3b.21f].
CATALOG_CONF=""
[ -n "${BRIARD_CATALOG_URL:-}" ] && CATALOG_CONF="CATALOG_URL=$BRIARD_CATALOG_URL"

# The three private ranges, PINNED. Each is written only when the operator named one; unset --
# every ordinary install -- means the agent draws it against the network this machine can see,
# records it and keeps it.
SYS_SUBNET_CONF=""
[ -n "$BRIARD_SYSTEM_SUBNET" ] && SYS_SUBNET_CONF="SYSTEM_SUBNET=$BRIARD_SYSTEM_SUBNET"
PRIV_SUBNET_CONF=""
[ -n "$BRIARD_PRIV_SUBNET" ] && PRIV_SUBNET_CONF="PRIV_SUBNET=$BRIARD_PRIV_SUBNET"
POD_SUBNET_CONF=""
[ -n "$BRIARD_POD_SUBNET" ] && POD_SUBNET_CONF="POD_SUBNET=$BRIARD_POD_SUBNET"

KEY_CONF=""
[ -f "$KEYRING" ] && KEY_CONF="UPDATE_KEYRING=$KEYRING"

# ---- the self-update PIVOT (B.84) -------------------------------------------------------
# $RUNDIR was created with the other directories; the flags inside it are tmpfs by virtue of
# living under /run, which is what makes a power loss mid-trial revert for free.
# Two frozen wrapper scripts and the unit fields that use them: the on-disk half of self-update,
# without which the Go half stages a binary the unit does not run from and reports success.
#
# FROZEN, and that is the whole safety property: these two scripts are dumb, agent-INDEPENDENT
# shell, so a bug in the volatile agent can never wedge the mechanism that replaces it. They are
# the verbatim pair proven in nixosTest/agent-selfupdate.nix -- change one and change both, and
# see install-macvtap.nix, which proves the SHIPPED pair rather than a unit a test wrote for itself.
#
# UPDATE_BASE must be the directory ExecStart runs from: a candidate staged anywhere else is on
# the wrong side of the gate, and possibly on another filesystem, which breaks the atomic-rename
# commit below.
UPDATE_BASE="$PREFIX/agent"
cat > "$PREFIX/agent/briard-exec" <<EOF
#!/bin/sh
# Pick the binary this boot runs: a staged candidate if one is armed, else the committed one.
# \`run\` is the daemon subcommand -- since [V3b.23] a bare invocation prints the help, so this line
# and the units are what start an agent. This script is frozen at install time and never rewritten
# (B.84), which is why that change arrives by REINSTALL rather than by update (alpha policy).
set -eu
if [ -e $RUNDIR/update ]; then
	mv $RUNDIR/update $RUNDIR/trial   # consume SINGLE-USE (rename, not delete): a crash
	exec $UPDATE_BASE/briard-agent.next run   #   cannot re-trial forever, and briard-commit can
else                                      #   still tell a trial boot from a normal one
	rm -f $RUNDIR/trial               # discard a failed trial's marker -- this IS the revert
	exec $UPDATE_BASE/briard-agent run
fi
EOF
cat > "$PREFIX/agent/briard-commit" <<EOF
#!/bin/sh
# ExecStartPost: systemd runs this ONLY after READY=1, so reaching it means the trial started.
set -eu
if [ -e $RUNDIR/trial ]; then
	mv $UPDATE_BASE/briard-agent.next $UPDATE_BASE/briard-agent   # atomic same-fs commit
	# The candidate's signed manifest commits WITH it ([B.86a]): it is what the next update run
	# compares the channel against, so a binary that moved without its manifest would be
	# re-staged on every tick. Existence-guarded so a candidate staged without one still commits.
	if [ -e $UPDATE_BASE/manifest.json.next ]; then
		mv $UPDATE_BASE/manifest.json.next $UPDATE_BASE/manifest.json
	fi
	# The rest of the host bundle commits WITH the agent ([B.86b]), each existence-guarded: an
	# update stages only what the release changed, so a partial set is the normal case. qemu is
	# a LINK to a tree, and -T is load-bearing: without it, \`mv qemu.next qemu\` onto a link to a
	# directory would move the staged link INSIDE that directory, silently. With it the commit is
	# one rename(2) of the link; the previous tree stays until the agent prunes it.
	if [ -e $UPDATE_BASE/briard-net-wrap.next ]; then
		mv -T $UPDATE_BASE/briard-net-wrap.next $UPDATE_BASE/briard-net-wrap
	fi
	if [ -L $UPDATE_BASE/qemu.next ]; then
		mv -T $UPDATE_BASE/qemu.next $UPDATE_BASE/qemu
	fi
	# The guest bundle ([B.86j]): the same link-to-a-tree shape as qemu, the same -T.
	if [ -L $UPDATE_BASE/guest.next ]; then
		mv -T $UPDATE_BASE/guest.next $UPDATE_BASE/guest
	fi
	rm -f $RUNDIR/trial
fi
EOF
chmod +x "$PREFIX/agent/briard-exec" "$PREFIX/agent/briard-commit"
# ---- the node's configuration: A FILE, NOT THE UNIT ([B.150](a)) -------------------------
# Everything the agent is told about this host lives here, and the agent reads it with the
# environment layered ON TOP (agent/host/config.go, loadConfigFile), so a rig that exports a
# variable still wins.
#
# A FILE BECAUSE A UNIT CANNOT BE REWRITTEN. Some of these are decisions the agent revisits, and a
# decision it can revisit has to live somewhere it can rewrite. Plain and greppable at 2am; 0600
# because it is root's business alone.
cat > "$PREFIX/config.env" <<EOF
# briard node configuration, written by install.sh. KEY=value, one per line; blank lines and
# '#' comments are ignored, whitespace either side of the '=' is trimmed, and NOTHING else is
# parsed -- no quoting, no expansion, no export. Every value is a path, a device name, an
# address or a duration.
QEMU=$QEMU
QEMU_DATADIR=$QEMU_DATADIR
ACCEL=kvm:tcg
CPU=$CPU_MODEL
GUEST_DISK=$OVERLAY
GUEST_IMAGE=$PREFIX/guest-image/nixos.qcow2
DATA_DISK=$DATA
DATA_ENCRYPTION=$DATA_ENCRYPTION
STATE_DISK=$STATE_DISK
CONTROL_SOCK=$RUNDIR/ctl.sock
NODE=$NODE_NAME
# The NIC layout, by the device name behind each guest NIC. SYSTEM_TAP -> eth1, this node's node IP
# and where DRBD binds (it replicates over loopback until a pairing gives it a peer); SERVICE_TAP ->
# eth2, where the VIP lives, held ready so a second anchor can join without a guest reboot.
#
# THE ADDRESSES ON THEM ARE NOT HERE. This node numbers itself at convergence and records the
# result in $STATE/subnets (agent/host/subnets.go); writing an address here would freeze it where
# no release and no re-parent could revisit it.
SYSTEM_TAP=$DRBD_TAP
SYSTEM_DEV=eth1
# ⚠️ THIS FILE CARRIES THE MACVTAP SHAPE, AND THE AGENT NARROWS IT ([B.150](c)). The substrate is
# the answer to "is the device the guest's L2 hangs off a bridge", which only the agent can ask, so
# what is written here is the shape every Linux node ships in and \`applySubstrate\` blanks
# SERVICE_TAP / WITNESS_TAP and sets VIP_PARENT on the node that turns out to be on a bridge.
# Empty reads exactly as unset downstream: qemu renders no second or third NIC, and the routes that
# would ride them no-op.
SERVICE_TAP=$TAP
# WITNESS_TAP -> the guest's eth3, the private host<->guest link (see PRIV_TAP above). The name is
# historical: the cloud-witness forwarder was its first user, not its only one -- the host's
# recovery rung reads the guest's reboot gate over it, on every node including a lone one.
WITNESS_TAP=$PRIV_TAP
# The device the guest's L2 hangs off, when the operator named one. Empty is every ordinary
# install: the agent selects the device holding the default route, and re-asks at every start.
BRIARD_NIC=$NIC
VIP_DEV=eth2
VIP_ADDR=$VIP
FLOCK_ID=$FLOCK_ID
# The visible name, handed to the guest for mDNS over the control channel like the VIP is. NEVER
# baked into the image: the image is cattle and an identity is pet.
FLOCK_NAME=$FLOCK_NAME
$NET_CONF
$KEY_CONF
$CATALOG_CONF
$SYS_SUBNET_CONF
$PRIV_SUBNET_CONF
$POD_SUBNET_CONF
$CONSOLE_CONF
# NO HEALTH_URL. Under DHCP the address is acquired inside the guest at promotion, so only the
# guest knows it: the agent asks (VIP_DEV says where to look) and rebuilds the probe target each
# cycle. Writing the address twice is writing two things that can disagree, and the one that would
# silently win here gates readiness, the OS health gate and a rollback.
STATUS_EVERY=5s
ASSIGNMENT_CACHE=$STATE/assignment.json
# The release channel root, for the guest chain ([B.86d]): the agent resolves guest/<target>
# here and applies the closure a release names. The host chain's fetch does not read this --
# it lives in the frozen update unit, with the same root baked into its script.
CHANNEL_URL=$CHANNEL
# The layout the agent stages a self-update INTO, which must be the directory ExecStart runs
# from ([B.84]); the two frozen wrappers above bake the same path.
UPDATE_BASE=$UPDATE_BASE
EOF
chmod 0600 "$PREFIX/config.env"

# ---- the update unit BELOW the agent ([B.86a]) ------------------------------------------
# The updater must not be shipped by the thing it updates. An agent that runs fine and has a bug
# in fetch/verify/stage can never be replaced -- no reflex covers it, and it fails fleet-wide at
# once. So the FETCH lives here, in a third frozen script and a oneshot, not in the agent:
# every run pulls a FRESH briard-agent from the target's pointer over TLS and lets THAT binary
# do the Ed25519-verified fetch (install.sh's own bootstrap pattern, on a timer). The script's
# entire knowledge is the channel root, the artifact name, one flag on the fetched binary, the
# pivot's on-disk contract and armed-at -- no product knowledge, so nothing in the product can
# ever make it need a new release. It is the same outer layer the Windows warden will be.
#
# It arms and STOPS. The running agent restarts itself at its safe point (once outcomes have
# drained -- announce-before-act); forcing is the backstop, and only for an arm the agent has
# ignored for longer than the grace, so the case where forcing is risky and the case where it
# happens never overlap. Three triggers, all \`systemctl start\` of this one unit: the cloud's
# agent-update directive (writes the target first), the timer (daily, jittered into the small
# hours, following stable -- the mass-converge path, and the only one that works when the agent
# is dead), and \`briard update host\`. systemd merges a start into a running job, so the unit
# is its own mutual exclusion. The target is a MESSAGE, read and unlinked; the result likewise.
#
# The timer runs EVERYWHERE, this free install included: an OSS node that never converges is
# precisely the un-updatable fleet this exists to prevent. Updates are not the paid feature;
# rollout control is. The off switch is \`systemctl disable briard-update.timer\` and nothing
# else. The channel poll is an anonymous plain GET carrying no node id, flock name or version.
cat > "$PREFIX/agent/briard-update" <<EOF
#!/bin/sh
# briard-update: converge this node's agent to the release channel. Frozen at install ([B.86a]).
set -eu
CHANNEL=$CHANNEL
KEYRING=$KEYRING
BASE=$UPDATE_BASE
RUN=$RUNDIR
GRACE=5400   # seconds an armed update may sit before the restart is forced (1.5h)
report() { printf '%s\n' "\$*" | tee "\$RUN/update-result"; }
# (1) Unfinished business: an update armed longer than the grace is forced; a younger one is
#     left to the agent's own safe point. Never on the same run that armed -- see (3).
if [ -e "\$RUN/update" ]; then
	age=\$(( \$(date +%s) - \$(stat -c %Y "\$RUN/update") ))
	if [ "\$age" -ge "\$GRACE" ]; then
		systemctl restart briard-agent.service
		report "forced the restart: an update had been armed for \${age}s"
	else
		report "an update is already armed; the agent restarts itself at its next safe point (or now: systemctl restart briard-agent)"
	fi
	exit 0
fi
# (2) The target, as a message: stable | latest | an exact id. Absent means stable.
target=stable
if [ -f "\$RUN/update-target" ]; then
	target=\$(head -n1 "\$RUN/update-target")
	rm -f "\$RUN/update-target"
fi
# (3) A fresh agent FROM THE TARGET does the verified fetch. Under \$BASE, not /run: Debian mounts
#     /run noexec. Its last stdout line is the verdict; its stderr goes to the journal.
tmp=\$(mktemp -d "\$BASE/.update.XXXXXX"); trap 'rm -rf "\$tmp"' EXIT
url="\$CHANNEL/host/\$target/linux/briard-agent"
if command -v curl >/dev/null 2>&1; then curl -fsSL "\$url" -o "\$tmp/briard-agent"
elif command -v wget >/dev/null 2>&1; then wget -qO "\$tmp/briard-agent" "\$url"
else report "need curl or wget to fetch \$url"; exit 1; fi || { report "could not fetch a bootstrap agent from \$url"; exit 1; }
chmod +x "\$tmp/briard-agent"
set +e
out=\$(BRIARD_CHANNEL_URL="\$CHANNEL" BRIARD_KEYRING="\$KEYRING" UPDATE_BASE="\$BASE" UPDATE_RUN_DIR="\$RUN" \\
	"\$tmp/briard-agent" --fetch-update "\$target" 2>&1)
rc=\$?
set -e
printf '%s\n' "\$out" >&2
report "\$(printf '%s\n' "\$out" | tail -n1)"
exit \$rc
EOF
chmod +x "$PREFIX/agent/briard-update"
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
