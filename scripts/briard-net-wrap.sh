#!/bin/sh
# briard macvtap launch wrapper.
#
# The agent launches the guest via `systemd-run`, i.e. through PID 1 -- so it cannot hand
# qemu an inherited fd. A macvtap NIC has no ifname qemu can open; its datapath is the
# /dev/tap<ifindex> chardev, attached as `-netdev tap,fd=N`. This dumb shim is the guest
# unit's ExecStart: for each macvtap NIC it pins the device MAC (so it matches qemu's mac=),
# brings it up, opens the chardev on the requested fd, then execs qemu -- which inherits those
# fds.
#
# It is CATTLE, not part of the frozen pivot, although it sits beside briard-exec/briard-commit
# in $PREFIX/agent and looks like them. Those two are written once by install.sh and never
# updated; this is a shipped artifact of the host bundle, staged as briard-net-wrap.next
# and committed WITH the agent by briard-commit -- because its contract is with the
# agent (the argv below, and the fd numbers both sides agree on), so the two must never
# straddle a release boundary.
#
#   briard-net-wrap <dev> <mac> <fd> [<dev> <mac> <fd> ...] -- <qemu> <args...>
#
# Triples are separate argv words (a MAC's colons never need escaping). The witness NIC is
# NOT passed here: it stays a plain tap qemu opens by name (macvtap would isolate the
# private guest<->host link the witness-forwarder answers).
#
# A <dev> written <netns>/<dev> is an ipvtap child the agent holds in that namespace (a slash
# cannot occur in an interface name). Its MAC is its parent's and cannot be changed, so its
# <mac> is empty and nothing is pinned; its /dev/tap<ifindex> node is opened from here all the
# same, because the chardev is not namespaced -- only the index is read inside the namespace.
set -eu

# Run under systemd (the guest unit), whose PATH is minimal -- pin one that finds `ip`/`cat`
# on a stock host (/usr/sbin, /sbin) AND on NixOS (/run/current-system/sw/bin), like net-up.sh.
export PATH="/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin"

while [ "${1:-}" != "--" ]; do
	[ "$#" -ge 3 ] || { echo "briard-net-wrap: dangling NIC triple (need dev mac fd)" >&2; exit 2; }
	dev=$1 mac=$2 fd=$3
	shift 3
	case $dev in
	*/*)
		ns=${dev%%/*} dev=${dev#*/}
		ip -n "$ns" link set "$dev" up
		# `7: briard0@if3: <...> ...` -- the index is the first field. A missing device makes
		# idx empty, which the chardev check below refuses.
		line=$(ip -n "$ns" -o link show dev "$dev")
		idx=${line%%:*}
		;;
	*)
		# Bounce down->up around the MAC change: the device is fresh (qemu not yet attached),
		# so the flap is invisible, and some drivers reject a MAC change while up.
		ip link set "$dev" down
		[ -n "$mac" ] && ip link set "$dev" address "$mac"
		ip link set "$dev" up
		idx=$(cat "/sys/class/net/${dev}/ifindex")
		;;
	esac
	# The tap chardev minor IS the device ifindex. `<>` CREATES a missing path as a regular
	# file, which qemu then rejects with a misleading TUNGETIFF error -- so refuse anything that
	# is not a character device first: an empty idx, or a /dev without devtmpfs (a container,
	# whose node the kernel made in the host's /dev instead).
	[ -c "/dev/tap${idx}" ] || {
		echo "briard-net-wrap: /dev/tap${idx} for $dev is not a character device (no devtmpfs here -- a container?)" >&2
		exit 1
	}
	# eval expands $fd into the redirection operator position (POSIX sh can't take a variable
	# fd number literally).
	eval "exec ${fd}<>/dev/tap${idx}"
done
shift # drop the -- sentinel

exec "$@"
