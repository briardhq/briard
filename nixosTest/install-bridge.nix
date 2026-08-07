# The free-local install on the BRIDGE fallback substrate -- a pure DELTA over install-macvtap.nix,
# which owns the mode-independent chain (report-card gate, bundled qemu, DRBD converge, VIP,
# off-box reach, cattle/pet reinstall). Nothing here re-proves any of that.
#
# What is bridge-specific and load-bearing:
#   1. the host NIC is enslaved to an L2 bridge and the host's own L3 identity MOVES onto it --
#      without cutting the host's footing. This is the named SSH risk, and it is the one thing
#      macvtap structurally cannot exercise (it never re-plumbs host config).
#   2. the installer aborts cleanly at that IRREVERSIBLE step on a bad NIC, leaving no bridge --
#      the never-a-half-install guarantee where it actually costs something.
#   3. an OFF-BOX LAN client reaches Briard at the VIP *through the enslaved bridge*.
#
# The shared install chain lives in install-macvtap.nix, on the default substrate; this file is
# only the bridge delta. The point of that split: when the bridge fallback is dropped, deleting
# this file is a clean delete -- it proves nothing that outlives bridge.
#
# Heavy (a nested guest boot on the bundled qemu), rides the `install` nightly tag.
# Run: nix build .#tests.install-bridge -L
{ pkgs, guestDisk, agent, qemuBundle }:
let
  # The staging dir install.sh reads with BRIARD_ARTIFACTS: the agent binary, the relocatable
  # qemu bundle, and the base guest image. Laid out as install.sh expects.
  staging = pkgs.runCommand "briard-install-staging-bridge" { } ''
    mkdir -p "$out/qemu"
    cp ${agent}/bin/briard-agent "$out/briard-agent"
    cp ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    cp -r ${qemuBundle}/. "$out/qemu/"
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
      { ... }:
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
        # Static, scripted (not DHCP/networkd) so install.sh owns eth1: it snapshots eth1's addr,
        # enslaves it to the bridge, and moves the addr over -- the real NIC-enslave path.
        networking.useDHCP = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.1"; prefixLength = 24; }
        ];
        # Host tools install.sh needs; NOTE no pkgs.qemu (the bundle is the only qemu).
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl ];
      };

    # A plain LAN peer -- the off-box client that must reach the VIP through the enslaved bridge.
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

    # --- DELTA 2: refuse cleanly at the IRREVERSIBLE step ---
    # A bogus NIC must die before the enslave, leaving no bridge and the host still on the LAN.
    # Sharper here than on macvtap: this is the path where a half-done networking step would
    # strand the box off the net (the SSH risk).
    host.fail(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NET_MODE=bridge BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.fail("ip link show br-briard")  # nothing half-built
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still on the LAN

    # Diagnostic: the framework's QEMU vlan is not reliably symmetric for HOST-initiated pings
    # (client->host works above; host->client often does not, even unbridged). The product's own
    # post-enslave guard pings a peer -- correct on a real switch, but here it would gate on that
    # asymmetry. So we drive the install WITHOUT BRIARD_NET_PEER (the guard then only verifies the
    # address survived onto the bridge) and prove reachability the way that matters and that the
    # vlan supports: the CLIENT reaching the VIP (client-initiated, like the working direction).
    print("host->client (pre-bridge, diagnostic):", host.execute("ping -c2 -W2 192.168.1.2")[1])

    # --- the install on the bridge substrate: one command -> green ---
    # BRIARD_UNIT_DIR=/run/systemd/system: NixOS's /etc/systemd/system is a read-only store
    # symlink (a stock host's is writable), so the hermetic test drops the units in /run.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NET_MODE=bridge BRIARD_NIC=eth1 BRIARD_UNIT_DIR=/run/systemd/system "
        "sh ${installScript}"
    )

    # --- DELTA 1 (the substrate's whole point of difference): the enslave happened, the host's L3
    # identity MOVED onto the bridge, and the host kept its footing across it.
    host.succeed("ip link show br-briard")
    host.succeed("bridge link show | grep -q 'eth1'")
    host.succeed("ip -o -4 addr show dev br-briard | grep -qw 192.168.1.1")
    host.fail("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")  # it really MOVED, not copied
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still reachable THROUGH the bridged NIC

    # The guest's NICs are plain taps ON the bridge (not macvtap children) -- the substrate itself.
    host.succeed("bridge link show | grep -q briard0")
    host.succeed("bridge link show | grep -q briard-drbd0")

    # The guest boots on the BUNDLED qemu and the agent converges to quorate Primary holding the VIP.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")

    # --- DELTA 3 (THE PROOF): the OFF-BOX client reaches Briard at the VIP, through the
    # enslaved host bridge -- not the install host curling itself.
    client.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    print("off-box client reached the VIP through the enslaved bridge")

    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'role=anchor primary=true quorate=true.*healthy=true'",
        timeout=60,
    )
  '';
}
