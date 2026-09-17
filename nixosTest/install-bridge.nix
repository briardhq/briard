# The free-local install on BRIDGE mode -- Windows' official L2 shape, and the Linux clone of it we
# run for tests -- a pure DELTA over install-macvtap.nix, which owns the mode-independent chain
# (report-card gate, bundled qemu, DRBD converge, VIP, off-box reach, cattle/pet reinstall).
# Nothing here re-proves any of that.
#
# What is bridge-specific and load-bearing:
#   1. THE SUBSTRATE IS DERIVED FROM THE DEVICE ([B.150](c)): nothing is told this is a bridge
#      install. The host owns a bridge carrying its address and its default route; the agent
#      selects that bridge BECAUSE it holds the route, sees what it is, and adds ONE PORT to it.
#   2. the installer refuses cleanly on a device it cannot use, leaving the user's bridge exactly
#      as it found it -- the never-a-half-install guarantee where it actually costs something.
#   3. an OFF-BOX LAN client reaches Briard at the VIP *through the user's bridge*.
#
# ⚠️ WHAT THIS FILE USED TO TEST AND DELIBERATELY NO LONGER DOES, because the product stopped
# doing it: the installer enslaving the host's own NIC, moving its address and default route onto
# a bridge of ours, and arming a self-cancelling watchdog to undo that if doing so cut the
# operator's SSH session. [B.150](c) deleted the whole gesture -- we never CREATE a bridge, we
# only join one the user already owns -- so there is no irreversible step here any more and no
# footing to lose. The rig declares the bridge in its own NixOS config instead, which is a more
# faithful test of the real case than enslaving one ourselves ever was.
#
# The shared install chain lives in install-macvtap.nix, on the Linux default substrate; this file
# is only the bridge delta.
#
# ⚠️ THIS FILE IS NOT A DELETION CANDIDATE, and its header said it was until [V3b.26]. Bridge mode
# stopped being a fallback on its way out and became Windows' ONLY possible L2 shape, plus the Linux
# clone of it we support for testing -- so this CONVERTED into the bridge-mode test rather than
# being deleted, in [V3b.26d] (2026-08-25).
#
# What "bridge mode" means since that conversion, and what DELTA 2 below is here to catch if it
# ever silently reverts: ONE tap on the bridge, not two. The guest gets one kernel NIC (eth1,
# holding the per-node system MAC) and MAKES its service identity itself -- a macvlan child named
# eth2 carrying the flock MAC, created over the control channel. There is no eth3 and no private
# host<->guest link: a bridged tap already puts host and guest on one L2, so the host holds its
# system-subnet address on the BRIDGE and reaches its guest natively.
#
# ⚠️ The honest limit, per [[fleet-macvtap-fidelity]]'s both-ways rule: this clones the
# GUEST-VISIBLE shape, not the Windows pathology underneath it (tap-windows6's boot-establishment
# rule, `netsh bridge`, the checksum offload). Those stay [V3b.1a]'s territory and a real desktop's.
#
# Heavy (a nested guest boot on the bundled qemu), rides the `install` nightly tag.
# Run: nix build .#tests.install-bridge -L
{ pkgs, guestDisk, agent, qemuBundle, guestBundle }:
let
  # The staging dir install.sh reads with BRIARD_ARTIFACTS: the agent binary, the relocatable
  # qemu bundle, the guest bundle, and the base guest image. Laid out as install.sh expects.
  staging = pkgs.runCommand "briard-install-staging-bridge" {
    nativeBuildInputs = [ pkgs.gnutar ];
  } ''
    mkdir -p "$out/qemu"
    cp ${agent}/bin/briard-agent "$out/briard-agent"
    cp ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    # The units and the agent's three frozen scripts, shipped verbatim ([B.157]): install.sh copies
    # them out of staging rather than writing them, so a staging dir without them is one it refuses.
    cp ${../scripts/units/briard-agent.service}  "$out/briard-agent.service"
    cp ${../scripts/units/briard-update.service} "$out/briard-update.service"
    cp ${../scripts/units/briard-update.timer}   "$out/briard-update.timer"
    cp ${../scripts/agent/briard-exec}    "$out/briard-exec"
    cp ${../scripts/agent/briard-commit}  "$out/briard-commit"
    cp ${../scripts/agent/briard-update}  "$out/briard-update"
    cp -r ${qemuBundle}/. "$out/qemu/"
    # THE GUEST BUNDLE ([B.86j]/[B.138]): the briard binaries the host pushes into the guest at
    # bring-up. Without it the guest runs the image's firmware, which REFUSES every non-firmware
    # verb -- the crash loop [B.141] found here, three weeks after the change that made a bundle
    # mandatory. Plain tar: the staging path takes it uncompressed, where the signed channel
    # install-macvtap builds takes the zstd'd one.
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
    # The install host: a stock-ish box with nested KVM. Deliberately NO pkgs.qemu -- the only
    # qemu it may use is the one the installer bundles under /opt/briard.
    host =
      { lib, ... }:
      {
        virtualisation.memorySize = 6144; # clears the report card's 4 GB floor + room for the 2 GB nested guest
        virtualisation.cores = 4;
        # 14 GB, not 10: this test runs install.sh TWICE, and the first (must-fail) invocation stages
        # ~2 GB before dying at the NIC check -- so the real run's report card measures a disk the
        # previous run already ate into. Against the product's own 8 GB floor that left under a GB of
        # margin, and V3.19's artifact growth tipped it: the card refused with "7 GB free" and the
        # install never ran. Headroom, so the test stops measuring the admission floor by accident.
        virtualisation.diskSize = 14336;
        virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
        virtualisation.vlans = [ 1 ]; # eth1 on the shared 192.168.1.0/24 L2 (the LAN)
        # THE USER'S BRIDGE, declared by the HOST rather than built by the installer ([B.150](c)).
        # This is the whole shape of the test now: a machine that already bridges its LAN NIC --
        # which is what a libvirt/Proxmox/Incus box looks like, and what a Windows host running
        # `netsh bridge` looks like -- and briard joining it rather than re-plumbing it.
        networking.useDHCP = false;
        networking.bridges.br0.interfaces = [ "eth1" ];
        # mkForce, because `virtualisation.vlans` gives every vlan interface an address of its own
        # and a bridge SLAVE may hold none -- NixOS asserts on exactly that. The address moves to
        # the bridge below, which is what a real bridged host looks like.
        #
        # BOTH FAMILIES. The vlan module assigns an IPv6 address (2001:db8:1::N/64) beside the v4
        # one, and the assertion counts every "ip" -- so forcing v4 alone fails with the same
        # message and looks like the fix did not apply at all.
        networking.interfaces.eth1.ipv4.addresses = lib.mkForce [ ];
        networking.interfaces.eth1.ipv6.addresses = lib.mkForce [ ];
        networking.interfaces.br0.ipv4.addresses = [
          { address = "192.168.1.1"; prefixLength = 24; }
        ];
        # ⚠️ THE DEFAULT ROUTE IS THE ASSERTION, not scenery. Nothing tells the install which
        # device to use: the agent selects the one holding the main table's default route, so this
        # line is what makes it choose br0 -- and choosing br0 is what derives the substrate. Take
        # it away and the install refuses for want of a default route, which is exactly the
        # behaviour [B.150](b) wanted and precisely why it must be stated here rather than assumed.
        # The client is not a router and never forwards anything; the ROUTE is what is needed.
        networking.defaultGateway = { address = "192.168.1.2"; interface = "br0"; };
        # Host tools install.sh needs; NOTE no pkgs.qemu (the bundle is the only qemu).
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl ];
      };

    # A plain LAN peer -- the off-box client that must reach the VIP through the host's bridge.
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
    start_all()
    host.wait_for_unit("multi-user.target")
    client.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")  # nested KVM present in L1 (the report card gates on it)

    # Baseline: host and client see each other on the LAN before we touch networking.
    client.wait_until_succeeds("ping -c1 -W2 192.168.1.1", timeout=30)

    # The host is on the LAN through its OWN bridge before briard exists -- the precondition the
    # whole file rests on, stated rather than assumed.
    host.succeed("ip link show br0")
    host.succeed("bridge link show | grep -q eth1")
    host.succeed("ip -o -4 route show default | grep -qw br0")

    # --- DELTA 2: refuse cleanly, and leave the USER'S bridge alone ---
    # A bogus device must be refused before anything is written. It is the report card that
    # refuses now ([B.150](b)) rather than a NIC check halfway through the networking step, so
    # this fails earlier and for a better-stated reason -- but the property under test is the same
    # one, and it is sharper here than on macvtap: this is the path where a half-done networking
    # step used to be able to strand the box off the net.
    host.fail(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.succeed("ip -o -4 addr show dev br0 | grep -qw 192.168.1.1")  # untouched
    host.fail("bridge link show | grep -q briard-drbd0")               # nothing half-built
    client.succeed("ping -c1 -W2 192.168.1.1")                         # host still on the LAN

    # --- the install on the bridge substrate: one command -> green ---
    # NOTE WHAT IS *NOT* ON THIS COMMAND LINE: no BRIARD_NET_MODE (the knob is gone -- the
    # substrate is derived) and no BRIARD_NIC (the agent selects the device holding the default
    # route, which this host put on br0). That absence is the test.
    #
    # BRIARD_UNIT_DIR=/run/systemd/system: NixOS's /etc/systemd/system is a read-only store
    # symlink (a stock host's is writable), so the hermetic test drops the units in /run.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_UNIT_DIR=/run/systemd/system "
        # The test DECLARES the address it is about to curl. install.sh has no default any more
        # (V3.19c step 3): unset means DHCP, and this L2 has no server. Stating it here is the
        # point of the change -- a default every test agreed with is what hid the baked VIP.
        "BRIARD_VIP_ADDR=192.168.1.100/24 "
        "sh ${installScript}"
    )

    # --- DELTA 1 (the substrate's whole point of difference): the agent DERIVED the substrate from
    # the device it selected, and joined the user's bridge without touching it.
    #
    # The inverted assertion is the one that carries the weight. The old install moved the host's
    # address from eth1 onto a bridge it had built; this one must leave BOTH exactly where the
    # host's own config put them. A regression that reintroduced enslaving would still pass every
    # reachability check below -- these two lines are what would catch it.
    host.succeed("ip -o -4 addr show dev br0 | grep -qw 192.168.1.1")
    host.succeed("ip -o -4 route show default | grep -qw br0")
    host.fail("ip link show br-briard")  # we do not create a bridge, ever
    host.succeed("journalctl -u briard-agent | grep -q \"the guest's L2 hangs off br0\"")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still reachable through its own bridge

    # --- DELTA 2 (the conversion, [V3b.26d]): ONE tap on the bridge, and no private link.
    #
    # This is the assertion that makes the file worth keeping. Windows admits exactly one tap per
    # qemu process ([V3b.1a]), so a second one here would be a Linux-only shape pretending to be
    # the Windows twin -- the macvlan-shim mistake again ([[fleet-macvtap-fidelity]]), and
    # invisible from every other angle because two taps work perfectly well on Linux.
    host.succeed("bridge link show | grep -q briard-drbd0")
    host.fail("bridge link show | grep -q briard0")
    host.fail("ip link show briard-priv0")

    # The host holds its system-subnet address on the BRIDGE -- it is genuinely on that L2 here,
    # which is why there is no private link to carry it. Read from the drawn subnet ([V3b.26f]),
    # never spelled: a rig that spells a subnet the installer draws asserts about a coincidence.
    system_subnet = host.succeed("sed -n 's/^SYSTEM_SUBNET=//p' /var/lib/briard/subnets").strip()
    node_ip, host_ip = f"{system_subnet}.1", f"{system_subnet}.129"
    host.succeed(f"ip -o -4 addr show dev br0 | grep -qw {host_ip}")
    print(f"host on the system subnet at {host_ip}, on the bridge")

    # The guest boots on the BUNDLED qemu and the agent converges to quorate Primary holding the VIP.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")

    # --- DELTA 3 (THE PROOF): the OFF-BOX client reaches Briard at the VIP, through the
    # host's own bridge -- not the install host curling itself.
    client.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    print("off-box client reached the VIP through the host's bridge")

    # --- DELTA 4 (THE ONE THAT PROVES THE GUEST MADE ITS OWN IDENTITY): from OFF-BOX, the VIP and
    # the node IP resolve to DIFFERENT MACs.
    #
    # Two identities behind one switch port is the whole shape, and it is only observable from
    # outside: asking the guest whether it created a device is asking the actor, and the host sees
    # both addresses over the same bridge either way. The client resolving them to two distinct
    # link addresses is the property that matters -- one is the per-node system MAC on the tap,
    # the other is the flock MAC on the macvlan the guest built, and a failover MOVES the second
    # one alone. If the guest had silently put the VIP on eth1 instead, everything above would
    # still pass and this is the only line that would not.
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

    # --- DELTA 5: A ROLE CYCLE, observed off-box ([V3b.26d]).
    #
    # ⚠️ READ WHAT THIS DOES NOT PROVE FIRST, because an earlier version of this block claimed more
    # and was VACUOUS. It asserted that a demoted node's flock MAC "goes quiet" by flushing the
    # client's neighbour table and re-ARPing for the VIP. That passes identically against the fixed
    # and the UNFIXED guest -- measured, both runs word for word -- because a standby deletes the
    # VIP ADDRESS whatever it does with the link, and a node does not answer ARP for an address it
    # does not hold. The probe measured "the address is gone", which was never in doubt.
    #
    # The [B.100]/[B.101] hazard is that a Secondary keeping the flock MAC UP teaches the switch
    # the wrong port by EMITTING (an IPv6 RS, an mDNS query) -- and on a lone node there is no wrong
    # port, because there is only one port that could serve. That hazard is two-node by nature and
    # is [B.113]'s to catch. Nothing here can stand in for it.
    #
    # What IS provable with one node, and is asserted below:
    #   1. a demote really stops service, seen from off-box rather than from the node's own opinion;
    #   2. re-promotion brings the VIP back on the SAME MAC -- the service identity survives a role
    #      change unchanged. That one is failable and worth having in bridge mode especially: eth2
    #      is a device the GUEST creates, so a re-promotion that rebuilt it without re-applying the
    #      flock MAC would come back on a kernel-random one and this line is what would say so.

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
        f"the VIP came back on a different MAC ({' '.join(back)}, was {vip_mac}) -- the service "
        f"identity is supposed to be the one thing a role change leaves unchanged, and in this "
        f"mode it is a device the guest rebuilt"
    )
  '';
}
