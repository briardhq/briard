#!/bin/sh
# briard macvtap launch wrapper.
#
# The agent launches the guest via `systemd-run`, i.e. through PID 1 -- so it cannot hand
# qemu an inherited fd. A macvtap NIC has no ifname qemu can open; its datapath is the
# /dev/tap<ifindex> chardev, attached as `-netdev tap,fd=N`. This dumb, agent-INDEPENDENT
# shim (the briard-exec pattern) is the guest unit's ExecStart: for each macvtap NIC
# it pins the device MAC (so it matches qemu's mac=), brings it up, opens the chardev on the
# requested fd, then execs qemu -- which inherits those fds.
#
#   briard-net-wrap <dev> <mac> <fd> [<dev> <mac> <fd> ...] -- <qemu> <args...>
#
# Triples are separate argv words (a MAC's colons never need escaping). The witness NIC is
# NOT passed here: it stays a plain tap qemu opens by name (macvtap would isolate the
# private guest<->host link the witness-forwarder answers).
set -eu

# Run under systemd (the guest unit), whose PATH is minimal -- pin one that finds `ip`/`cat`
# on a stock host (/usr/sbin, /sbin) AND on NixOS (/run/current-system/sw/bin), like net-up.sh.
export PATH="/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin"

while [ "${1:-}" != "--" ]; do
	[ "$#" -ge 3 ] || { echo "briard-net-wrap: dangling NIC triple (need dev mac fd)" >&2; exit 2; }
	dev=$1 mac=$2 fd=$3
	shift 3
	# Bounce down->up around the MAC change: the device is fresh (qemu not yet attached), so
	# the flap is invisible, and some drivers reject a MAC change while up.
	ip link set "$dev" down
	[ -n "$mac" ] && ip link set "$dev" address "$mac"
	ip link set "$dev" up
	# The tap chardev minor IS the device ifindex. eval expands $fd into the redirection
	# operator position (POSIX sh can't take a variable fd number literally). A missing device
	# makes $(cat ...) empty -> `exec N<>/dev/tap` fails -> set -e aborts (fail fast).
	eval "exec ${fd}<>/dev/tap$(cat "/sys/class/net/${dev}/ifindex")"
done
shift # drop the -- sentinel

exec "$@"
