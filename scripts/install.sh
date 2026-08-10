#!/bin/sh
# briard one-command install.
#
#   curl -fsSL https://get.briard.io/install.sh | sh
#
# Brings a stock single-node Linux host to GREEN: a wired L2 bridge enslaving the host
# NIC, a guest VM (booted by our BUNDLED qemu -- no distro qemu) holding
# the VIP, a single-node DRBD data volume (guest-side), and Briard answering at the VIP
# on the LAN. No cloud, no name (rung 0).
#
# It installs NO SERVICE. A node is a node first: ready, replicating, able to fail
# over -- and then you choose what runs on it. So the VIP answers with Briard's own page
# rather than a workload nobody picked, and HEALTH_URL probes that front door, which is the
# one address that answers whether or not anything is installed.
#
# Cattle/pet FHS:
#   /opt/briard   = cattle: signed, self-updating binaries + qemu bundle + guest image.
#                   `rm -rf /opt/briard` + reinstall = a fresh host.
#   /var/lib/briard = pet: the DRBD data volume + identity -- survives reinstall.
#   /run/briard   = tmpfs flags.
#
# Artifact source: BRIARD_ARTIFACTS=<dir> installs from a local, already-verified staging
# dir (the hermetic install tests, and a future offline install). Unset = the
# signed network fetch over the channel -- assertion (e), not yet wired here.
set -eu

# ---- knobs (env-overridable; the test pins the deterministic ones) -----------------
PREFIX="${BRIARD_PREFIX:-/opt/briard}"
STATE="${BRIARD_STATE:-/var/lib/briard}"
RUNDIR="${BRIARD_RUN:-/run/briard}"
NIC="${BRIARD_NIC:-}"                 # host NIC to enslave/parent; empty = the default-route NIC
BRIDGE="${BRIARD_BRIDGE:-br-briard}"
TAP="${BRIARD_TAP:-briard0}"          # the guest's service NIC (eth2, the VIP)
DRBD_TAP="${BRIARD_DRBD_TAP:-briard-drbd0}" # the guest's DRBD NIC (eth1); idle single-node, addressed on pairing
# Net substrate. "macvtap" (DEFAULT) makes the guest's NICs macvtap children of the host
# NIC directly: L2 citizenship with NO bridge and NO host-IP move, so no SSH-risk moment and no net
# guard -- the least invasive substrate, proven green and validated no worse than bridge. "bridge" is the validated fallback (BRIARD_NET_MODE=bridge): enslaves the host NIC to an
# L2 bridge and puts the guest's NICs on it as taps -- more invasive (moves the host IP), but lets a
# user AT the host reach the guest VIP (macvtap isolates host↔guest). TAP/DRBD_TAP name the devices
# either way.
NET_MODE="${BRIARD_NET_MODE:-macvtap}"
# The service address, in CIDR form -- it must carry a prefix because it is an address ON THE
# USER'S LAN, and the LAN's prefix is not ours to assume. Until V3.19 this was a bare address that
# fed only HEALTH_URL (the address the HOST probes) while the guest claimed a *baked* one, so
# setting it moved the probe off the real VIP instead of moving the VIP. It now reaches the guest.
#
# UNSET MEANS DHCP, and there is deliberately no default (V3.19c step 3). Any address we could
# pick here is a guess about someone else's network, whereas a lease is the router TELLING us the
# answer -- from inside its own pool, so it will not hand the same address to anyone else while we
# hold it. A static default squats an address the router still believes it owns. It also removes
# the last place our lab's subnet reached the product path, which is exactly why the original
# defect was invisible: the default matched the lab, so every test agreed with it.
VIP="${BRIARD_VIP:-}"
VIP_IP="${VIP%%/*}"   # the bare address; EMPTY under DHCP, where nobody knows it yet
# The pet data volume: THICK-allocated (see step 6) and sized for a real service's data, not for a
# test fixture. 1G was the fixture's size and it is not a Home Assistant's: `.storage` plus the
# recorder SQLite outgrows it in months, and growing a DRBD-backed volume afterwards is not a
# one-liner. Written in whole GiB -- the dd fallback parses it that way.
DATA_SIZE="${BRIARD_DATA_SIZE:-4G}"
# The guest's CPU model. "max" = every feature the accelerator can expose, which under KVM is this
# host's own CPU: qemu's DEFAULT (qemu64) is below x86-64-v2 and costs the guest aes/sha-ni/sse4.2
# (so software TLS, sha256 and crc32c) plus the CPUID bits its kernel needs to mitigate Spectre.
# Passing the host CPU through is free for us because a briard guest never migrates and never
# saves RAM state. Set BRIARD_CPU=qemu64 to fall back if a host's passthrough is ever the suspect.
CPU_MODEL="${BRIARD_CPU:-max}"
# This node's name and this flock's name are NOT constants and NOT knobs: both are minted into pet
# state in step 6b, once $STATE exists and the agent binary is on disk. See there for why they are
# two identifiers rather than the one hardcoded `guest` this used to be.
NET_GUARD_SECS="${BRIARD_NET_GUARD_SECS:-45}"
UNIT_DIR="${BRIARD_UNIT_DIR:-/etc/systemd/system}" # /run/systemd/system for a read-only-/etc host
NET_PEER="${BRIARD_NET_PEER:-}"       # a LAN host to ping to confirm we kept our footing
CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}" # signed release channel base (network fetch)
KEYRING="${BRIARD_KEYRING:-$PREFIX/keyring.pem}"       # the bundled release public key (verify root)

# The release signing public key(s), embedded at release time. install.sh is fetched over TLS from
# the release channel, so the key travels WITH the script (the standard installer-carries-the-pubkey
# pattern) -- a placeholder in the source tree that the release pipeline fills. Used only when no
# keyring is already on disk (BRIARD_KEYRING unset and no prior install).
RELEASE_KEYRING_PEM='__BRIARD_RELEASE_KEYRING_PEM__'

say() { printf 'briard: %s\n' "$*"; }
die() { printf 'briard: ERROR: %s\n' "$*" >&2; exit 1; }
fetch_url() { # url dest -- TLS download for the bootstrap agent (curl or wget, whatever the box has)
	if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
	else die "need curl or wget to fetch $1"; fi
}

# ---- 0. root -----------------------------------------------------------------------
[ "$(id -u)" = 0 ] || die "run as root (curl ... | sudo sh)"

# ---- 1. the gate's agent -- the ONLY artifact staged before the host is admitted ----
# The report card needs an executable agent to run, so exactly that much is staged here and not a
# byte more. Everything heavy (the qemu bundle, the 2.5 GB guest image) waits until step 3, AFTER
# admission.
#
# It used to be the other way round: step 1 staged the whole set -- and on the network path
# DOWNLOADED it first, so a refusal could be preceded by ~6 GB of writes -- and only then ran the
# card, whose refusal line still claimed "nothing was changed". Two things were wrong with that.
# The claim was false; and the DISK CHECK was measuring a disk the installer had already eaten
# into, so a host that genuinely met the 8 GB floor could be refused for failing to meet it after
# we spent 3 GB of it. Both are fixed by asking before taking.
mkdir -p "$PREFIX/agent" "$STATE" "$RUNDIR"
if [ -n "${BRIARD_ARTIFACTS:-}" ]; then
	# Offline / hermetic-test path: install from an already-verified local staging dir.
	src="$BRIARD_ARTIFACTS"
	[ -x "$src/briard-agent" ] || die "staging dir $src has no briard-agent"
	install -m0755 "$src/briard-agent" "$PREFIX/agent/briard-agent"
	CARD_AGENT="$PREFIX/agent/briard-agent"
else
	# Signed network fetch (assertion e). Bootstrap a briard-agent over TLS -- the release channel's
	# integrity anchors this FIRST binary -- then let it fetch+verify the whole set (qemu bundle,
	# guest image, and a fresh briard-agent) against the bundled release keyring, refusing any
	# tampered/unsigned artifact. The bootstrap agent only RUNS the verified fetch; the binaries that
	# land under /opt are the Ed25519-verified set, so a compromised bootstrap can't seed bad cattle.
	if [ ! -f "$KEYRING" ]; then
		case "$RELEASE_KEYRING_PEM" in
		*"BEGIN PUBLIC KEY"*) printf '%s\n' "$RELEASE_KEYRING_PEM" >"$KEYRING" ;;
		*) die "no release keyring at $KEYRING (the embedded key is a build placeholder; set BRIARD_KEYRING)" ;;
		esac
	fi
	say "bootstrapping the installer agent from $CHANNEL ..."
	# The bootstrap lands under $PREFIX, NOT $RUNDIR: Debian (and Ubuntu) mount /run `noexec`, so a
	# bootstrap staged there cannot be executed at all -- the install died on `Permission denied`
	# before it fetched a single artifact. $PREFIX is where the agent lives anyway, so if it is
	# noexec the install has no home on this host regardless; /run has no claim to being executable.
	boot="$PREFIX/bootstrap-agent"
	fetch_url "$CHANNEL/briard-agent" "$boot" || die "could not fetch the bootstrap agent from $CHANNEL"
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
# Refuse-with-the-fix-named on an unbringable host, before we fetch gigabytes, touch networking or
# boot a VM -- never a half-install (assertion c, already built).
say "checking host readiness ..."
# NET_MODE is passed so the card appends the macvtap advisories (USB-NIC promiscuous
# fallback, MAC port-security) when this is a macvtap install; they never change the verdict.
# VIP_ADDR is passed for the same reason NET_MODE is: the card cannot judge an address it is not
# told about. It is the one check that compares OUR intent against THIS LAN, and without it the
# gate admitted a machine whose home network the service address was not even on (V3.19).
if ! NET_MODE="$NET_MODE" VIP_ADDR="$VIP" "$CARD_AGENT" --report-card; then
	# Leave the box as we found it: on the network path the bootstrap agent is the one thing we
	# put down, so take it back rather than claim "nothing was changed" while it sits there.
	[ -n "${BRIARD_ARTIFACTS:-}" ] || rm -f "$CARD_AGENT"
	die "host is not ready (see the fix above); nothing was installed"
fi

# ---- 3. the rest of the artifacts (cattle) -----------------------------------------
# Lay down /opt/briard from the staging dir. The agent binary + qemu bundle + guest
# image are the self-updating cattle; the base guest image is read-only backing.
mkdir -p "$PREFIX/qemu" "$PREFIX/guest-image"
if [ -z "${BRIARD_ARTIFACTS:-}" ]; then
	# Now that the host is admitted, let the bootstrap fetch+verify the whole set (qemu bundle,
	# guest image, and a fresh briard-agent) against the bundled release keyring, refusing any
	# tampered/unsigned artifact. The bootstrap agent only RUNS the verified fetch; the binaries
	# that land under /opt are the Ed25519-verified set, so a compromised bootstrap can't seed bad
	# cattle.
	src="$PREFIX/staging"
	rm -rf "$src"
	say "fetching + verifying the signed artifact set ..."
	BRIARD_CHANNEL_URL="$CHANNEL" BRIARD_KEYRING="$KEYRING" \
		"$boot" --fetch-install "$src" || die "artifact verification failed; nothing installed"
	# The verified qemu bundle arrives as a tarball; expand it to the qemu/ tree the install step
	# expects. Its bytes are already trusted (hash-checked against the signed manifest above).
	# The tar is rooted at the bundle itself (bin/ lib/ share/ PROVENANCE), NOT at a qemu/ dir, so
	# it must be extracted INTO one -- unpacking it beside the other artifacts scatters bin/ and
	# lib/ across the staging dir and leaves the copy below with no qemu/ to find.
	mkdir -p "$src/qemu"
	tar -xf "$src/qemu-bundle.tar" -C "$src/qemu" && rm -f "$src/qemu-bundle.tar"
	rm -f "$boot"
	# The verified agent replaces the bootstrap one staged for the card.
	[ -x "$src/briard-agent" ] || die "staging dir $src has no briard-agent"
	install -m0755 "$src/briard-agent" "$PREFIX/agent/briard-agent"
fi
# The macvtap launch wrapper -- the fd-passing shim the agent runs as the guest unit's
# ExecStart under NET_MODE=macvtap. Bundled alongside the agent; a dumb, versioned shell artifact.
NET_WRAP=""
if [ -f "$src/briard-net-wrap" ]; then
	install -m0755 "$src/briard-net-wrap" "$PREFIX/agent/briard-net-wrap"
	NET_WRAP="$PREFIX/agent/briard-net-wrap"
fi
cp -a "$src/qemu/." "$PREFIX/qemu/"
chmod -R u+w "$PREFIX/qemu"
cp -f "$src/nixos.qcow2" "$PREFIX/guest-image/nixos.qcow2"
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

# ---- 4. host footprint: the tun module ---------------------------------------------
modprobe tun 2>/dev/null || true
[ -e /dev/net/tun ] || die "/dev/net/tun absent (kernel built without CONFIG_TUN)"
mkdir -p /etc/modules-load.d && printf 'tun\n' > /etc/modules-load.d/briard.conf

# ---- 5. networking: the guest's L2 substrate (bridge enslave, or macvtap) -----------
if [ "$NET_MODE" = macvtap ]; then
	# macvtap substrate: the guest's NICs are macvtap children of the host NIC --
	# full L2 citizens (unsolicited inbound, DHCP, multicast) with NO bridge and NO host-IP move.
	# So there is no SSH-risk moment and no net guard: the host keeps its address on the physical
	# NIC throughout, and the per-VM-start create/destroy the cattle-host model wants comes for
	# free (macvtaps auto-vanish with the parent). Needs the fd-passing launch wrapper.
	[ -n "$NET_WRAP" ] || die "NET_MODE=macvtap needs the briard-net-wrap wrapper (absent from staging)"
	[ -n "$NIC" ] || NIC="$(ip -o route show default 2>/dev/null | awk '{print $5; exit}')"
	[ -n "$NIC" ] || die "no host NIC given and no default route to infer one (set BRIARD_NIC)"
	ip link show "$NIC" >/dev/null 2>&1 || die "host NIC $NIC not found"
	cat > "$PREFIX/net-up.sh" <<EOF
#!/bin/sh
set -eu
export PATH="/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin"
ip link set $NIC up
# The guest's two NIC macvtaps on $NIC: DRBD (eth1) then service (eth2) -- order sets the guest's
# ethN. Created with the kernel's random MAC; the launch wrapper (briard-net-wrap) pins the
# agent-derived per-node MAC (matching qemu's mac=) at guest start. No bridge, no host-IP move --
# idempotent on reboot / re-run.
for t in $DRBD_TAP $TAP; do
	if ! ip link show \$t >/dev/null 2>&1; then
		ip link add link $NIC name \$t type macvtap mode bridge
		ip link set \$t up
	fi
done
EOF
	chmod +x "$PREFIX/net-up.sh"
	say "macvtap substrate on $NIC (no bridge; host keeps its IP on $NIC)"
	sh "$PREFIX/net-up.sh"
else
# Enslaving the host's primary NIC to the bridge briefly moves its L3 identity; on a
# remote-adopted box that can cut the very SSH session running this script. So the move
# is one netlink batch (minimal window) AND armed with a self-cancelling watchdog that
# reverts if we can't confirm we kept our footing (do it atomically, guard it). On reboot the same net-up runs from a oneshot unit -- no SSH to guard.
# Where does the host's L3 identity live right now? On a FRESH install it's on the physical NIC.
# On a REINSTALL (the cattle-reset gesture: `rm -rf /opt/briard` + re-run, assertion d / B.22b) the
# bridge already exists and carries it -- a prior install enslaved the NIC and moved addr+route onto
# the bridge; removing /opt drops the cattle but NOT the live bridge (kernel state) or the pet
# /var/lib. So read the snapshot from wherever the identity is now: the bridge if it's already up
# with a physical port, else the NIC directly. Reading an already-bridged host from the NIC would
# snapshot an empty addr and regenerate a net-up that can't restore it.
SRC=""
if ip link show "$BRIDGE" >/dev/null 2>&1 &&
	ports="$(ls "/sys/class/net/$BRIDGE/brif" 2>/dev/null | grep -vxE "$TAP|$DRBD_TAP")" && [ -n "$ports" ]; then
	SRC="$BRIDGE"
	[ -n "$NIC" ] || NIC="$(printf '%s\n' "$ports" | head -n1)"
	say "reinstall: $BRIDGE already present (port $NIC); reading host identity from the bridge"
else
	[ -n "$NIC" ] || NIC="$(ip -o route show default 2>/dev/null | awk '{print $5; exit}')"
	SRC="$NIC"
fi
[ -n "$NIC" ] || die "no host NIC given and no default route to infer one (set BRIARD_NIC)"
ip link show "$NIC" >/dev/null 2>&1 || die "host NIC $NIC not found"

# Snapshot the current IPv4 (addr/prefix) + default gateway from SRC (the NIC on a fresh install,
# the bridge on a reinstall) so net-up can carry them onto the bridge and net-revert can restore them.
ADDR="$(ip -o -4 addr show dev "$SRC" scope global 2>/dev/null | awk '{print $4; exit}')"
# Capture the default gateway ONLY if it currently routes over SRC -- on a single-NIC home box it
# does (and must live on the bridge); on a multi-NIC box the default may live on another NIC, which
# we must NOT disturb.
GW="$(ip -o -4 route show default 2>/dev/null | awk -v n="$SRC" 'index($0, "dev " n){print $3; exit}')"

# net-up: idempotent, guardless -- the reboot path. Baked with this host's values.
cat > "$PREFIX/net-up.sh" <<EOF
#!/bin/sh
set -eu
# Run under systemd (briard-net.service), whose default PATH is minimal -- pin one that finds ip
# on a stock host (/usr/sbin, /sbin) AND on NixOS (/run/current-system/sw/bin).
export PATH="/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin"
# Already bridged? (idempotent on reboot / re-run.)
if ! { ip link show $BRIDGE >/dev/null 2>&1 && [ "\$(ip -o -4 addr show dev $BRIDGE scope global | awk '{print \$4; exit}')" = "$ADDR" ]; }; then
	ip link add name $BRIDGE type bridge 2>/dev/null || true
	ip link set $NIC master $BRIDGE
	# Pin the bridge MAC to the enslaved NIC's own MAC (a fresh bridge otherwise keeps a random
	# MAC): keeps the host's L2 identity stable across the enslave, so peers' ARP caches for our
	# address stay valid and frames still egress from the MAC the NIC was assigned.
	ip link set $BRIDGE address \$(cat /sys/class/net/$NIC/address)
	ip addr flush dev $NIC scope global 2>/dev/null || true
	[ -n "$ADDR" ] && ip addr replace $ADDR dev $BRIDGE || true
	ip link set $NIC up
	ip link set $BRIDGE up
	[ -n "$GW" ] && ip route replace default via $GW dev $BRIDGE || true
fi
# The guest's two NIC taps on the bridge: the DRBD NIC (eth1, idle single-node -> addressed on
# pairing) and the service NIC (eth2, the VIP). Both on the same L2 so a paired second anchor
# reaches this one directly. Order sets the guest's ethN: SYSTEM_TAP is eth1, SERVICE_TAP eth2.
for t in $DRBD_TAP $TAP; do
	if ! ip link show \$t >/dev/null 2>&1; then
		ip tuntap add \$t mode tap
		ip link set \$t master $BRIDGE
		ip link set \$t up
	fi
done
EOF
chmod +x "$PREFIX/net-up.sh"

net_revert() {
	say "net guard: reverting bridge (kept the host on $NIC)"
	for t in "$TAP" "$DRBD_TAP"; do
		ip link set "$t" nomaster 2>/dev/null || true
		ip link del "$t" 2>/dev/null || true
	done
	ip link set "$NIC" nomaster 2>/dev/null || true
	ip addr flush dev "$BRIDGE" scope global 2>/dev/null || true
	ip link del "$BRIDGE" 2>/dev/null || true
	[ -n "$ADDR" ] && ip addr replace "$ADDR" dev "$NIC" 2>/dev/null || true
	[ -n "$GW" ] && ip route replace default via "$GW" dev "$NIC" 2>/dev/null || true
}

confirm_net() {
	# We kept our footing iff the bridge carries our address and (if we have a peer or gateway)
	# it is still reachable. The caller settles first: an ARP fired the instant the port comes up
	# fails and parks the neighbor in FAILED, and rapid retries then reuse that dead entry instead
	# of re-soliciting. So flush any stale neighbor and space the probes by a second so each is a
	# fresh solicitation.
	ip -o -4 addr show dev "$BRIDGE" scope global 2>/dev/null | grep -qw "${ADDR%%/*}" || return 1
	target="${NET_PEER:-$GW}"
	[ -z "$target" ] && return 0
	ip neigh flush dev "$BRIDGE" 2>/dev/null || true
	i=0
	while [ "$i" -lt 6 ]; do
		ping -c1 -W2 "$target" >/dev/null 2>&1 && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}

say "bridging $NIC -> $BRIDGE (guard: auto-revert in ${NET_GUARD_SECS}s if this stalls)"
rm -f "$RUNDIR/net-ok"
# Watchdog: a pure backstop for the case where this script is KILLED between enslaving the NIC and
# confirming (e.g. the operator's SSH dies mid-run) -- it reverts so the box isn't stranded off the
# net. The normal success/failure paths below signal or cancel it explicitly, so it never fires
# during an install that runs to completion. The subshell inherits net_revert + the vars.
( sleep "$NET_GUARD_SECS"; [ -e "$RUNDIR/net-ok" ] || net_revert ) &
guard=$!
sh "$PREFIX/net-up.sh"
# Let the bridge port + the NIC's L2 settle before probing (see confirm_net): a probe fired the
# instant the port comes up parks the neighbor in FAILED and poisons the retries.
sleep 3
if confirm_net; then
	: > "$RUNDIR/net-ok"
	kill "$guard" 2>/dev/null || true
	say "bridge up, host connectivity confirmed"
else
	net_revert # a genuine loss -- revert now (don't wait out the backstop) and bail
	: > "$RUNDIR/net-ok"
	kill "$guard" 2>/dev/null || true
	die "lost host connectivity after bridging; reverted to $NIC"
fi
fi

# ---- 6. disks: the pet data volume + the (cattle) guest overlay ---------------------
# data.img is the single-node DRBD backing -- pet, created once, preserved across a
# reinstall. The guest overlay is cattle: a writable qcow2 backed by the
# read-only base image, recreated every install (the base may have moved).
DATA="$STATE/data.img"
if [ ! -f "$DATA" ]; then
	say "creating the pet data volume ($DATA_SIZE) at $DATA"
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
# stable across a reinstall, and (once pairing carries it) across a failover to a second node.
#
# It also ends something quietly true of every install so far: the service MAC derived from NODE,
# which install.sh hardcodes to "guest" -- so EVERY briard node on earth presented the SAME service
# MAC. Harmless across houses, an L2 collision inside one. A per-install random id fixes that.
FLOCK_ID_FILE="$STATE/flock-id"
if [ ! -s "$FLOCK_ID_FILE" ]; then
	# /proc/sys/kernel/random/uuid is on every Linux we target and needs no coreutils.
	(cat /proc/sys/kernel/random/uuid 2>/dev/null || od -An -N16 -tx1 /dev/urandom | tr -d ' \n') \
		>"$FLOCK_ID_FILE" || die "could not write the flock id to $FLOCK_ID_FILE"
	chmod 0600 "$FLOCK_ID_FILE"
	say "generated this flock's identity at $FLOCK_ID_FILE (pet -- keep it to keep your address)"
fi
FLOCK_ID="$(cat "$FLOCK_ID_FILE")"
[ -n "$FLOCK_ID" ] || die "the flock id at $FLOCK_ID_FILE is empty; remove it to regenerate"

# ---- 6b. the other two identifiers -------------------------------------------------
# THREE identifiers, one job each. Until V3.20 there was one string -- the literal `guest` -- doing
# all of these at once, which is why nothing a household could see was renameable:
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
	say "generated this node's identity at $NODE_ID_FILE (pet -- DRBD metadata is keyed to it)"
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
	say "this flock is called $(cat "$FLOCK_NAME_FILE") (pet -- it is the name on your network)"
fi
FLOCK_NAME="$(cat "$FLOCK_NAME_FILE")"
[ -n "$FLOCK_NAME" ] || die "the flock name at $FLOCK_NAME_FILE is empty; remove it to regenerate"

OVERLAY="$PREFIX/guest.qcow2"   # cattle: recreated each install, dropped by `rm -rf /opt/briard`
say "creating the guest overlay at $OVERLAY"
rm -f "$OVERLAY"
if ! "$PREFIX/qemu/bin/qemu-img" create -f qcow2 \
	-b "$PREFIX/guest-image/nixos.qcow2" -F qcow2 "$OVERLAY"; then
	die "qemu-img create failed (rc=$?)"
fi
say "guest overlay created"

# ---- 7. the units: net (reboot re-create) + the agent ------------------------------
say "writing systemd units to $UNIT_DIR"
mkdir -p "$UNIT_DIR"
cat > "$UNIT_DIR/briard-net.service" <<EOF
[Unit]
Description=briard host networking (bridge + service tap)
After=network-pre.target
Wants=network-pre.target
Before=briard-agent.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$PREFIX/net-up.sh
[Install]
WantedBy=multi-user.target
EOF

# In macvtap mode the agent renders the guest launch behind the fd-passing wrapper;
# in bridge mode neither var is set and the agent opens taps by name (the default).
NET_ENV=""
if [ "$NET_MODE" = macvtap ]; then
	NET_ENV="Environment=NET_MODE=macvtap
Environment=NET_WRAP_BIN=$NET_WRAP"
fi
# The release keyring is the agent's trust root for BOTH signed host-agent self-updates and the
# signed service catalog (`briard service install` verifies a manifest against it). Both fail
# CLOSED without it -- self-update simply switches itself off, silently -- so a node installed
# without this env is one that can never update itself and can never install a service, with the
# key sitting right there on disk unread. Only wired when a keyring actually exists: the
# BRIARD_ARTIFACTS path (hermetic tests, install-from-source) has no channel and no key, and
# pointing the agent at a missing file would be worse than leaving it unset.
KEY_ENV=""
[ -f "$KEYRING" ] && KEY_ENV="Environment=UPDATE_KEYRING=$KEYRING"
cat > "$UNIT_DIR/briard-agent.service" <<EOF
[Unit]
Description=briard host agent (single node)
After=briard-net.service
Requires=briard-net.service
[Service]
# The agent shells out to systemd-run/systemctl by name; give it a PATH that resolves them on a
# stock host (/usr/bin) AND on NixOS (/run/current-system/sw/bin), since a unit's default is minimal.
Environment=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin
Environment=QEMU=$QEMU
Environment=QEMU_DATADIR=$QEMU_DATADIR
Environment=ACCEL=kvm:tcg
Environment=CPU=$CPU_MODEL
Environment=GUEST_DISK=$OVERLAY
Environment=DATA_DISK=$DATA
Environment=CONTROL_SOCK=$RUNDIR/ctl.sock
Environment=NODE=$NODE_NAME
# Unified NIC layout: SYSTEM_TAP -> the guest's eth1 (the DRBD NIC, idle single-node --
# DRBD replicates over loopback until a pairing addresses eth1); SERVICE_TAP -> eth2, where the VIP
# lives (VIP_DEV), held ready so a second anchor can join without a guest reboot. SYSTEM_DEV is left
# unset: single-node needs no DRBD address, just the NIC present.
Environment=SYSTEM_TAP=$DRBD_TAP
Environment=SERVICE_TAP=$TAP
Environment=VIP_DEV=eth2
Environment=VIP_ADDR=$VIP
Environment=FLOCK_ID=$FLOCK_ID
# The visible name, passed so the agent can hand it to the guest for mDNS. It reaches the guest
# over the control channel like the VIP does, NOT baked into the image -- the image is cattle and
# this is pet, and baking an identity into a shared image is the mistake V3.19 was.
Environment=FLOCK_NAME=$FLOCK_NAME
$NET_ENV
$KEY_ENV
# NO HEALTH_URL. It used to bake the address a second time, and under DHCP there is nothing to
# bake -- the address is acquired inside the guest at promotion, so only the guest knows it. The
# agent asks (VIP_DEV above is how it knows where to look) and rebuilds the probe target each
# cycle. Writing an address twice is writing two things that can disagree, and the one that
# would silently win here gates readiness, the OS health gate and a rollback.
Environment=STATUS_EVERY=5s
Environment=ASSIGNMENT_CACHE=$STATE/assignment.json
ExecStart=$AGENT
Restart=on-failure
RestartSec=3
[Install]
WantedBy=multi-user.target
EOF

if command -v systemctl >/dev/null 2>&1; then
	say "daemon-reload"
	systemctl daemon-reload
	if [ "$UNIT_DIR" = /etc/systemd/system ]; then
		# Persistent install: enable (survive reboot) + start now.
		say "enabling briard-net + briard-agent"
		systemctl enable --now briard-net.service briard-agent.service
	else
		# Units in a non-persistent dir (e.g. /run) can't be enabled; just start them.
		say "starting briard-net + briard-agent"
		systemctl start briard-net.service briard-agent.service
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
	if [ -n "$VIP_IP" ]; then
		say "installed. the guest is booting; briard will answer at http://briard-$FLOCK_NAME.local/ (or http://$VIP_IP/) -- no service is installed on it yet"
	else
		say "installed. the guest is booting; briard will answer at http://briard-$FLOCK_NAME.local/ -- it takes its address from your router, where it shows up as a \"briard-\" client -- no service is installed on it yet"
	fi
else
	die "no systemd (this install path targets systemd hosts)"
fi
