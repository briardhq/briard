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
# once; nothing it does can be reached by a release afterwards. So it does only what supervises
# the agent's own arrival: be root, get an agent onto the box, call it, enable the service it laid
# down, and hand over to it.
#
# EVERYTHING ELSE IS THE AGENT'S -- `briard-agent install` (agent/host/install.go): the machine
# report card, the signed fetch, the /opt/briard layout, config.env, the units, and the closing
# wait for the node's one-time link. That is Go a release can fix and a Windows installer calls
# too; this is shell that runs on exactly one kind of host. Adding something back here means
# showing it has to happen before an agent can run.
#
# BRIARD_ARTIFACTS=<dir> installs from a local, already-verified staging dir (the hermetic install
# tests, and a future offline install). Unset = the signed network fetch over the channel.
set -eu
# ---- what this script needs to know -------------------------------------------------
# ⚠️ THE KNOBS ARE NOT HERE, and adding one back is the mistake. Every `BRIARD_*` in the
# environment reaches the agent, which copies it into config.env with the prefix stripped --
# `BRIARD_CPU=qemu64` becomes `CPU=qemu64` -- so the installer carries no list of them, no
# defaults for them and no documentation of them. What each key means and what it defaults to is
# agent/host/config.go's, which is a binary the channel can fix.
#
# Below is the short list this script READS, because it acts on it before any agent runs.
PREFIX=/opt/briard
# ⚠️ A CONSTANT. /opt/briard is baked into the qemu bundle's own ELF interpreter
# (/opt/briard/qemu/lib/ld-linux...) and into the agent's defaults, so an install anywhere else
# produces a qemu that cannot execute. The shipped unit files spell the same path.

# NixOS's /etc/systemd/system is a read-only store path, so the install rigs point this elsewhere.
# The agent reads the same variable to put the units there; this script reads it to enable them.
UNIT_DIR="${BRIARD_UNIT_DIR:-/etc/systemd/system}"

# The signed release channel root, and WHICH release off it -- read here only to fetch the
# bootstrap agent from the target's own path. The agent reads the same two to fetch the rest.
CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}"
RELEASE="${BRIARD_RELEASE:-stable}"
# The release public key: the verify root, before anything is on disk to trust.
KEYRING="${BRIARD_UPDATE_KEYRING:-$PREFIX/keyring.pem}"

# The release signing public key(s), embedded at release time: this script is fetched over TLS from
# the channel, so the key travels with it (the installer-carries-the-pubkey pattern). A placeholder
# in the source tree that the release pipeline fills. Used only when no keyring is on disk.
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

# ---- 1. the agent that installs ----------------------------------------------------
# The ONLY artifact this script fetches: everything heavy (the qemu bundle, the 2.5 GB guest
# image) is the agent's to fetch, AFTER its report card has admitted the host. Asking before
# taking is what lets a refusal say "nothing was installed" truthfully.
BOOT=""
if [ -n "${BRIARD_ARTIFACTS:-}" ]; then
	# Offline / hermetic-test path: the staging dir's own agent installs from it.
	AGENT="$BRIARD_ARTIFACTS/briard-agent"
	[ -x "$AGENT" ] || die "staging dir $BRIARD_ARTIFACTS has no briard-agent"
else
	# Signed network fetch. Bootstrap a briard-agent over TLS -- the channel's integrity anchors
	# this FIRST binary -- and let it fetch and verify the whole set against the bundled release
	# keyring. The bootstrap only RUNS the verified fetch; what lands under /opt is the
	# Ed25519-verified set, so a compromised bootstrap cannot seed bad cattle.
	mkdir -p "$PREFIX"
	if [ ! -f "$KEYRING" ]; then
		case "$RELEASE_KEYRING_PEM" in
		*"BEGIN PUBLIC KEY"*) printf '%s\n' "$RELEASE_KEYRING_PEM" >"$KEYRING" ;;
		*) die "no release keyring at $KEYRING (the embedded key is a build placeholder; set BRIARD_UPDATE_KEYRING)" ;;
		esac
	fi
	say "bootstrapping the installer agent from $CHANNEL (briard/$RELEASE) ..."
	# Under $PREFIX, NOT /run: Debian and Ubuntu mount /run `noexec`, so a bootstrap staged
	# there cannot be executed at all. $PREFIX is where the agent lives anyway.
	#
	# Fetched from the TARGET's path, never a fixed one: the bootstrap is the binary that parses
	# the manifest and lays the release down, and a stale one that cannot is exactly the
	# forward-compat bricking the channel layout exists to prevent. `briard-agent` is the
	# one artifact the channel duplicates under its pointers for this fetch.
	BOOT="$PREFIX/bootstrap-agent"
	fetch_url "$CHANNEL/briard/$RELEASE/linux-amd64/briard-agent" "$BOOT" || die "could not fetch the bootstrap agent from $CHANNEL/briard/$RELEASE/linux-amd64"
	chmod +x "$BOOT"
	# Fail with the REASON. A bootstrap that cannot exec (noexec mount, wrong arch, a dynamically
	# linked binary whose interpreter this host lacks) is not a verification failure, and reporting
	# it as one sends the reader hunting for a bad signature.
	"$BOOT" help >/dev/null 2>&1 ||
		die "the bootstrap agent at $BOOT will not run on this host (see the error above); nothing installed"
	AGENT="$BOOT"
fi

# ---- 2. the install itself: the agent's ---------------------------------------------
# The report card first (a refusal with the fix named, before a byte of the release is fetched),
# then the verified set laid down under /opt/briard, config.env and the units. It says what it is
# doing as it goes, and why when it stops.
#
# The bootstrap goes either way: on a refusal it is the one thing we put down, and "nothing was
# installed" must be true while it sits there; on success the verified agent has replaced it.
if ! "$AGENT" install; then
	[ -z "$BOOT" ] || rm -f "$BOOT"
	exit 1
fi
[ -z "$BOOT" ] || rm -f "$BOOT"

# ---- 3. the service ------------------------------------------------------------------
# Belt, not the gate: the report card refuses a host that is not systemd-booted before anything
# is written. Reaching HERE without systemctl is not a shape worth a nicer message -- it is worth
# not pretending the install finished.
command -v systemctl >/dev/null 2>&1 || die "no systemd (this install path targets systemd hosts)"
say "registering briard with systemd"
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

# ---- 4. the hand-over ----------------------------------------------------------------
# The installed agent waits for its own node to answer and prints the one-time link to the
# household's Briard page -- the last thing the installer prints -- or says how to get one.
exec "$PREFIX/agent/briard-agent" install -wait
