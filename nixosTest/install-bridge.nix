# The free-local install in bridge mode (the Windows official L2 shape, and the Linux clone of it
# we run for tests): a pure delta over install-macvtap.nix, which owns the mode-independent chain
# (report-card gate, bundled qemu, data volume, VIP, off-box reach, cattle/pet reinstall) on the
# Linux default substrate. Nothing here re-proves any of that.
#
# What is bridge-specific and load-bearing:
#   1. the substrate is derived from the device: nothing is told this is a bridge install. The
#      host owns a bridge carrying its address and default route; the agent selects that bridge
#      because it holds the route, sees what it is, and adds one port to it.
#   2. the installer refuses cleanly on a device it cannot use, leaving the user's bridge exactly
#      as it found it: the never-a-half-install guarantee where it actually costs something.
#   3. an off-box LAN client reaches Briard at the VIP *through the user's bridge*.
#
# ⚠️ Deliberately not tested, because the product does not do it: the installer enslaving the
# host's own NIC, moving its address and default route onto a bridge of ours, and arming a
# self-cancelling watchdog in case that cut the operator's SSH session. With agent-owned
# networking we never create a bridge, only join one the user already owns, so there is no
# irreversible step and no footing to lose; the rig declares the bridge in its own NixOS config,
# the faithful test of the real case.
#
# ⚠️ Not a deletion candidate: bridge mode is the only possible L2 shape on Windows, plus the Linux
# clone of it we support for testing. Bridge mode means one tap on the bridge, not two, which the
# one-tap DELTA below ("the conversion") catches if it ever silently reverts. The guest gets one
# kernel NIC (eth1, the per-node system MAC) and makes its service identity itself: a macvlan
# child eth2 carrying the flock MAC, created over the control channel. No eth3 and no private
# host<->guest link: a bridged tap already puts host and guest on one L2, so the host holds its
# system-subnet address on the bridge and reaches its guest natively.
#
# ⚠️ The honest limit, since rig fidelity fails both ways: this clones the guest-visible shape, not
# the Windows pathology underneath (the tap-windows6 boot-establishment rule, `netsh bridge`, the
# checksum offload). Those stay with the Windows host and a real desktop.
#
# Heavy (a nested guest boot on the bundled qemu), rides the `install` nightly tag.
# Run: nix build .#tests.install-bridge -L
{ pkgs, guestDisk, agent, qemuBundle, guestBundle }:
let
  # The staging dir install.sh reads with BRIARD_ARTIFACTS: the agent binary, the relocatable
  # qemu bundle, the guest bundle, and the base guest image. Laid out as briard-agent install
  # expects.
  staging = pkgs.runCommand "briard-install-staging-bridge" {
    nativeBuildInputs = [ pkgs.gnutar ];
  } ''
    mkdir -p "$out/qemu"
    cp ${agent}/bin/briard-agent "$out/briard-agent"
    cp ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    # The units and the agent's frozen update script, shipped verbatim: briard-agent install copies
    # them out of staging rather than writing them, so a staging dir without them is one it refuses.
    cp ${../scripts/units/briard-agent.service}  "$out/briard-agent.service"
    cp ${../scripts/units/briard-update.service} "$out/briard-update.service"
    cp ${../scripts/units/briard-update.timer}   "$out/briard-update.timer"
    cp ${../scripts/agent/briard-update}  "$out/briard-update"
    cp -r ${qemuBundle}/. "$out/qemu/"
    # The guest bundle: the briard binaries the host pushes into the guest at bring-up. Without
    # it the guest runs the image firmware, which refuses every non-firmware verb, and crash-loops.
    # Plain tar: the staging path takes it uncompressed, where the signed channel install-macvtap
    # builds takes the zstd one.
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
        -cf "$out/guest-bundle.tar" -C ${guestBundle} .
    cp ${guestDisk}/nixos.qcow2 "$out/nixos.qcow2"
  '';
  installScript = ../scripts/install.sh;
in
pkgs.testers.runNixOSTest {
  name = "install-bridge";
  skipTypeCheck = true; # dynamic asserts, systemd units created at runtime

  nodes = {
    # The install host: a stock-ish box with nested KVM. Deliberately no pkgs.qemu: the only qemu
    # it may use is the one the installer bundles under /opt/briard.
    host =
      { lib, ... }:
      {
        virtualisation.memorySize = 6144; # clears the report card's 4 GB floor + room for the 2 GB nested guest
        virtualisation.cores = 4;
        # 14 GB: headroom over the report card's disk floor, so a release's artifact growth never
        # tips the card into refusing and the test never measures the admission floor by accident.
        virtualisation.diskSize = 14336;
        virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
        virtualisation.vlans = [ 1 ]; # eth1 on the shared 192.168.1.0/24 L2 (the LAN)
        # The user bridge, declared by the host rather than built by the installer: the whole
        # shape of the test is a machine that already bridges its LAN NIC (a libvirt/Proxmox/Incus
        # box, or a Windows host running `netsh bridge`), and briard joining it, not re-plumbing it.
        networking.useDHCP = false;
        networking.bridges.br0.interfaces = [ "eth1" ];
        # mkForce, because `virtualisation.vlans` gives every vlan interface an address and a
        # bridge slave may hold none (NixOS asserts on it); the address moves to the bridge below,
        # as on a real bridged host. Both families: the vlan module also assigns an IPv6 address
        # (2001:db8:1::N/64) and the assertion counts every "ip", so forcing v4 alone fails with the
        # same message and looks like the fix did not apply.
        networking.interfaces.eth1.ipv4.addresses = lib.mkForce [ ];
        networking.interfaces.eth1.ipv6.addresses = lib.mkForce [ ];
        networking.interfaces.br0.ipv4.addresses = [
          { address = "192.168.1.1"; prefixLength = 24; }
        ];
        # ⚠️ The default route is the assertion, not scenery. Nothing tells the install which
        # device to use: the agent selects the one holding the main-table default route, so this
        # line makes it choose br0, and choosing br0 derives the substrate. Without it the install
        # refuses for want of a default route, as device-derived selection should, which is why it
        # is stated here, not assumed. The client is not a router and never forwards anything; the
        # route is what is needed.
        networking.defaultGateway = { address = "192.168.1.2"; interface = "br0"; };
        # Host tools install.sh needs; no pkgs.qemu (the bundle is the only qemu).
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl ];
      };

    # A plain LAN peer: the off-box client that must reach the VIP through the host's bridge.
    client =
      { ... }:
      {
        virtualisation.vlans = [ 1 ];
        networking.useDHCP = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.2"; prefixLength = 24; }
        ];
        environment.systemPackages = [ pkgs.curl pkgs.iputils ];
      };
  };

  testScript = ''
    ${import ./multicast-receive.nix { inherit pkgs; }}
    start_all()
    host.wait_for_unit("multi-user.target")
    client.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")  # nested KVM present in L1 (the report card gates on it)

    # Baseline: host and client see each other on the LAN before we touch networking.
    client.wait_until_succeeds("ping -c1 -W2 192.168.1.1", timeout=30)

    # The host is on the LAN through its own bridge before briard exists: the precondition the
    # whole file rests on, stated rather than assumed.
    host.succeed("ip link show br0")
    host.succeed("bridge link show | grep -q eth1")
    host.succeed("ip -o -4 route show default | grep -qw br0")

    # The host footprint, for the DELTA 6 residue check: the scan install-macvtap uses, taken
    # before the refused install below because that one leaves empty dirs. IPv4 addresses and
    # routes only: briard never configures IPv6 on the host (it only turns it off on its own
    # devices, which the link scan covers), and the kernel adds the br0 link-local address and
    # route a second or two after link-up, which a baseline inside that window would misread as
    # residue.
    def footprint():
        return set(host.succeed(
            "for d in /opt /var/lib /var/lib/systemd/timers /var/log /usr/local/bin /run "
            "/run/systemd/system /run/systemd/transient; do find $d -mindepth 1 -maxdepth 1 2>/dev/null || true; done; "
            "find /run/systemd/system -mindepth 2 -maxdepth 2 2>/dev/null || true; "
            "ip -o link show | awk -F': ' '{print \"link \" $2}' | cut -d@ -f1; "
            "ip -4 -o addr show | awk '{print \"addr \" $2 \" \" $4}'; "
            "ip -4 route show table all | awk '{r=$1\" \"$2; for (i=2; i<NF; i++) "
            "if ($i==\"dev\" || $i==\"table\") r=r\" \"$(i+1); print \"route \" r}'; "
            "systemctl list-units --all --plain --no-legend 'briard*' | awk '{print \"unit \" $1}'"
        ).splitlines())
    pristine = footprint()

    # --- DELTA 2: refuse cleanly, and leave the user bridge alone ---
    # A bogus device must be refused before anything is written; the report card refuses, before
    # the networking step begins. Sharper here than on macvtap: this is the path where a half-done
    # networking step could strand the box off the net.
    host.fail(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.succeed("ip -o -4 addr show dev br0 | grep -qw 192.168.1.1")  # untouched
    host.fail("bridge link show | grep -q briard-drbd0")               # nothing half-built
    client.succeed("ping -c1 -W2 192.168.1.1")                         # host still on the LAN

    # --- the install on the bridge substrate: one command -> green ---
    # Not on this command line, and that absence is the test: no BRIARD_NET_MODE (no such knob; the
    # substrate is derived) and no BRIARD_NIC (the agent selects the device holding the default
    # route, which this host put on br0). BRIARD_UNIT_DIR=/run/systemd/system: the NixOS
    # /etc/systemd/system is a read-only store symlink (writable on a stock host), so the
    # hermetic test drops the units in /run.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_UNIT_DIR=/run/systemd/system "
        # The test declares the address it curls. The agent has no default VIP (unset means DHCP,
        # and this L2 has no server), and a default every test agreed with would hide a baked VIP.
        "BRIARD_VIP_ADDR=192.168.1.100/24 "
        "sh ${installScript}"
    )

    # --- DELTA 1 (the substrate's whole point of difference): the agent derived the substrate from
    # the device it selected, and joined the user's bridge without touching it. The inverted
    # assertion carries the weight: an install that enslaved the NIC would move the host address
    # onto a bridge it had built, and would still pass every reachability check below; these two
    # lines, which require both address and route where the host config put them, catch it.
    host.succeed("ip -o -4 addr show dev br0 | grep -qw 192.168.1.1")
    host.succeed("ip -o -4 route show default | grep -qw br0")
    host.fail("ip link show br-briard")  # we do not create a bridge, ever
    host.succeed("journalctl -u briard-agent | grep -q \"the guest's L2 hangs off br0\"")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still reachable through its own bridge

    # --- DELTA 2 (the conversion): one tap on the bridge, and no private link. The assertion that
    # makes the file worth keeping: Windows admits exactly one tap per qemu process, so a second
    # one here would be a Linux-only shape posing as the Windows twin (a rig granting itself a
    # shape the product lacks), invisible from every other angle because two taps work on Linux.
    host.succeed("bridge link show | grep -q briard-drbd0")
    host.fail("bridge link show | grep -q briard0")
    host.fail("ip link show briard-priv0")

    # The host holds its system-subnet address on the bridge: it is genuinely on that L2 here, so
    # no private link carries it. Read from the drawn subnet, never spelled: a rig that spells a
    # subnet the installer draws asserts about a coincidence. A /32 and one on-link route to its
    # own guest, never the /24, which would put a connected route for the whole system subnet on
    # the host primary device. Both halves are asserted, and the absence of the /24 route too: a
    # re-prefix creeping back would pass every reachability line below.
    system_subnet = host.succeed("sed -n 's/^SYSTEM_SUBNET=//p' /var/lib/briard/subnets").strip()
    node_ip, host_ip = f"{system_subnet}.1", f"{system_subnet}.129"
    host.wait_until_succeeds(f"ip -o -4 addr show dev br0 | grep -q ' {host_ip}/32 '", timeout=300)
    host.wait_until_succeeds(f"ip -4 route show {node_ip} dev br0 | grep -q 'src {host_ip}'", timeout=60)
    host.fail(f"ip -4 route show {system_subnet}.0/24 | grep -q br0")
    print(f"host on the system subnet at {host_ip}, on the bridge")

    # The guest boots on the bundled qemu and the agent converges to quorate Primary holding the VIP.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")
    # The route is what the /32 reaches the guest through: the host dials its own guest's node IP.
    host.succeed(f"ping -c1 -W2 {node_ip}")

    # --- DELTA 3 (the proof): the off-box client reaches Briard at the VIP, through the host's own
    # bridge, not the install host curling itself.
    client.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    print("off-box client reached the VIP through the host's bridge")
    # ...and from another subnet, routed in without NAT. The client is the host's gateway, so it is
    # the router the agent hands the guest, and it asks from an address off the VIP's subnet: the
    # reply returns only through that route. Here the VIP is hand-set (no lease carries a router)
    # and VIP_DEV is the macvlan child the guest made, the two things install-macvtap's twin is not.
    client.succeed("ip addr add 10.99.0.2/32 dev lo")
    client.wait_until_succeeds("curl -fsS -m 10 --interface 10.99.0.2 http://192.168.1.100/healthz", timeout=60)
    print("a client on another subnet reached the VIP through the router the agent named")

    # --- DELTA 4 (proves the guest made its own identity): from off-box, the VIP and the node IP
    # resolve to different MACs. Two identities behind one switch port is the whole shape, only
    # observable from outside: asking the guest is asking the actor, and the host sees both
    # addresses over the same bridge either way. One is the per-node system MAC on the tap, the
    # other the flock MAC on the macvlan the guest built, and a failover moves the second alone.
    # Had the guest silently put the VIP on eth1, everything above would pass and only this fails.
    client.succeed(f"ip route replace {system_subnet}.0/24 dev eth1")
    client.wait_until_succeeds(f"ping -c1 -W2 {node_ip}", timeout=60)
    client.succeed("ping -c1 -W2 192.168.1.100")

    def mac_of(addr):
        for _ in range(12):
            out = client.succeed(f"ip -4 neigh show {addr} dev eth1 || true").split()
            if "lladdr" in out:
                return out[out.index("lladdr") + 1]
            client.sleep(2)
        return ""

    vip_mac, node_mac = mac_of("192.168.1.100"), mac_of(node_ip)
    print(f"off-box: VIP -> {vip_mac}, node IP -> {node_mac}")
    assert vip_mac and node_mac, f"the client could not resolve both (vip={vip_mac!r} node={node_mac!r})"
    assert vip_mac != node_mac, (
        f"the VIP and the node IP share one MAC ({vip_mac}) -- the guest did not make a second "
        f"identity, so a failover would have no MAC to move and this is macvtap's shape wearing "
        f"bridge mode's name"
    )

    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'role=anchor primary=true quorate=true.*healthy=true'",
        timeout=60,
    )

    # Inbound multicast to a group the guest joined, through the user's bridge and the macvlan the
    # guest holds the VIP on, a filter of its own kept by the guest's kernel from its joins.
    assert_guest_receives_multicast(client, "eth1", "192.168.1.2", "192.168.1.100")

    # --- DELTA 5: a role cycle, observed off-box.
    #
    # ⚠️ What this does not prove: the obvious check is vacuous. Asserting that a demoted node's
    # flock MAC "goes quiet" by flushing the client neighbour table and re-ARPing for the VIP
    # passes whether or not the guest takes the MAC down, because a standby deletes the VIP address
    # whatever it does with the link and a node does not answer ARP for an address it does not
    # hold; it measures "the address is gone", never in doubt. The real hazard, a Secondary keeping
    # the flock MAC up and teaching the switch the wrong port by emitting (an IPv6 RS, an mDNS
    # query), is two-node by nature (a lone node has only one port that could serve) and a
    # two-node rig's to catch; nothing here can stand in for it.
    #
    # Provable with one node, and asserted below:
    #   1. a demote really stops serving, seen from off-box rather than the node's own opinion;
    #   2. re-promotion brings the VIP back on the same MAC: the service identity survives a role
    #      change unchanged. Failable, and worth having in bridge mode especially: eth2 is a device
    #      the guest creates, so a re-promotion that rebuilt it without re-applying the flock MAC
    #      would come back on a kernel-random one, and this line would say so.

    host.succeed("/opt/briard/agent/briard-agent handover -keep-masked")
    client.wait_until_fails("curl -fsS --max-time 3 http://192.168.1.100/healthz", timeout=120)
    print("a demoted node stops serving, seen from off-box")

    host.succeed("/opt/briard/agent/briard-agent handover -unmask")
    client.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=180)
    client.succeed("ip neigh flush dev eth1")
    client.wait_until_succeeds("ping -c1 -W2 192.168.1.100")
    back = client.succeed("ip -4 neigh show 192.168.1.100 dev eth1").split()
    print(f"off-box after re-promotion: {' '.join(back)}")
    assert "lladdr" in back and back[back.index("lladdr") + 1] == vip_mac, (
        f"the VIP came back on a different MAC ({' '.join(back)}, was {vip_mac}) -- the "
        f"service identity is supposed to be the one thing a role change leaves unchanged, and in this "
        f"mode it is a device the guest rebuilt"
    )

    # --- DELTA 6: uninstall -delete-data leaves the host as it found it ---
    # Here uninstall touches something it must leave standing: the agent put its system-subnet
    # address on the user's bridge, and that one address must come off while the bridge, its port
    # and the host's own address stay. The macvtap rig proves the default (keep); this proves the
    # other half, so nothing of ours may survive at all.
    installed = footprint() - pristine
    for want in ("/opt/briard", "/var/lib/briard", "/usr/local/bin/briard", "link briard-drbd0",
                 f"addr br0 {host_ip}/32", f"route {node_ip} dev br0", "unit briard-guest.service"):
        assert want in installed, f"the footprint cannot see {want!r}, so it cannot prove it gone: {sorted(installed)}"

    # Without -yes it only says what it would do, including that the data goes, as asked.
    code, out = host.execute("/usr/local/bin/briard uninstall -delete-data 2>&1")
    assert code == 2 and "DELETES /var/lib/briard" in out, f"exit {code}: {out}"
    host.succeed("systemctl is-active briard-agent.service")

    host.succeed("/usr/local/bin/briard uninstall -yes -delete-data")
    residue = footprint() - pristine
    assert not residue, f"uninstall -delete-data left these behind: {sorted(residue)}"
    # The user's bridge is exactly as the host's own config made it, and still carries the host.
    host.succeed("ip -o -4 addr show dev br0 | grep -qw 192.168.1.1")
    host.succeed("bridge link show | grep -q eth1")
    client.succeed("ping -c1 -W2 192.168.1.1")
    client.wait_until_fails("curl -fsS --max-time 3 http://192.168.1.100/healthz", timeout=60)
    print("uninstall -delete-data removed everything of ours and left the user's bridge standing")
  '';
}
