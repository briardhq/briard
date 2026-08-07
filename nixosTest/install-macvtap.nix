# The free-local `curl | sh` install on the MACVTAP substrate -- the DEFAULT substrate as of
# the test that owns the WHOLE install chain end to end:
#   report-card gate -> BUNDLED qemu (no distro qemu on the host) boots the guest -> single-node
#   DRBD data volume -> payload served at the VIP -> an OFF-BOX LAN client reaches it.
#
# The macvtap-specific deltas on top of that chain:
#   1. NO bridge is created and the host's IP never leaves the physical NIC (the invasiveness
#      win — the SSH-risk moment and net guard are gone).
#   2. the guest's NICs are macvtap children of the host NIC (L2 citizens, no bridge).
#   3. qemu is launched behind the fd-passing wrapper (briard-net-wrap) and really holds the
#      /dev/tap<ifindex> chardevs on the inherited fds — the mechanism ifname= can't provide.
#   4. an OFF-BOX LAN client still reaches the payload at the VIP through the macvtap (the guest
#      is a full L2 citizen), which is the whole point.
#
# It also proves assertion (d) -- cattle/pet reinstall: after green, `rm -rf /opt/briard`
# (the cattle) + reinstall reaches green AGAIN with the guest DATA intact (the payload's persisted
# tick counter, on the pet /var/lib/briard data volume, does not reset). The failable control is
# sharp -- a wiped/reformatted volume would restart the counter at ~0.
#
# It carries the mode-independent half (install-bridge.nix, cut down
# to the bridge deltas). It had accumulated on the FALLBACK's test purely because bridge was the
# original default -- so the default substrate was the thin one, and dropping the fallback would
# have silently taken the reinstall proof with it. Now install-bridge.nix is a pure delta: when the
# bridge fallback goes, deleting that file loses nothing mode-independent.
#
# agent-bringup.nix already proves the agent MECHANISM (nested guest, DRBD, VIP) with the host
# itself as the client. This test proves the INSTALLER around it: it runs scripts/install.sh
# verbatim, uses only the bundled qemu (pkgs.qemu is deliberately absent), and a second node on
# the shared L2 -- not the install host -- curls the VIP, so reachability is genuinely off-box.
#
# Heavy (two nested guest boots + a multi-GB guest disk) -> rides the `install` nightly tag
# alongside install-bridge, qemu-bundle + report-card. Run:
#   nix build .#tests.install-macvtap -L
{ pkgs, guestDisk, agent, qemuBundle }:
let
  # Same staging as install-bringup, plus the macvtap launch wrapper the substrate needs.
  staging = pkgs.runCommand "briard-install-staging-macvtap" { } ''
    mkdir -p "$out/qemu"
    cp ${agent}/bin/briard-agent "$out/briard-agent"
    cp ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    cp -r ${qemuBundle}/. "$out/qemu/"
    cp ${guestDisk}/nixos.qcow2 "$out/nixos.qcow2"
  '';
  installScript = ../scripts/install.sh;
in
pkgs.testers.runNixOSTest {
  name = "install-macvtap";
  skipTypeCheck = true; # dynamic asserts, systemd units created at runtime

  nodes = {
    host =
      { ... }:
      {
        virtualisation.memorySize = 6144; # report-card 4 GB floor + the 2 GB nested guest
        virtualisation.cores = 4;
        # 14 GB, not 10: this test runs install.sh TWICE, and the first (must-fail) invocation stages
        # ~2 GB before dying at the NIC check -- so the real run's report card measures a disk the
        # previous run already ate into. Against the product's own 8 GB floor that left under a GB of
        # margin, and V3.19's artifact growth tipped it: the card refused with "7 GB free" and the
        # install never ran. Headroom, so the test stops measuring the admission floor by accident.
        virtualisation.diskSize = 14336;
        virtualisation.vlans = [ 1 ]; # eth1 on the shared 192.168.1.0/24 L2 (the LAN)
        virtualisation.qemu.options = [ "-cpu" "host" ]; # vmx -> nested KVM in L1
        networking.useDHCP = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.1"; prefixLength = 24; }
        ];
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl ];
      };

    # The off-box client that must reach the VIP through the guest's macvtap.
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
    host.succeed("ls -l /dev/kvm")
    client.wait_until_succeeds("ping -c1 -W2 192.168.1.1", timeout=30)

    # --- refuse-with-fix / never-a-half-install: a bogus NIC dies before touching networking ---
    # (The report-card refusals themselves are proven in report-card.nix; here we prove the
    # installer aborts cleanly at the networking step, leaving no macvtap behind. Under macvtap
    # that step is not irreversible the way the bridge enslave is -- install-bridge.nix keeps the
    # sharper version of this check -- but "refuses and changes nothing" must hold on the default
    # substrate too, so it is asserted here in the mode's own terms.)
    host.fail(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NET_MODE=macvtap BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.fail("ip link show briard0")       # nothing half-built
    host.fail("ip link show briard-drbd0")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still on the LAN

    # --- the install on the macvtap substrate: one command -> green ---
    # BRIARD_UNIT_DIR=/run/systemd/system: NixOS's /etc/systemd/system is a read-only store
    # symlink (a stock host's is writable), so the hermetic test drops the units in /run.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NIC=eth1 BRIARD_NET_MODE=macvtap "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )

    # DELTA 1: NO bridge, and the host IP NEVER left eth1 (macvtap's invasiveness win).
    host.fail("ip link show br-briard")
    host.succeed("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host never lost its footing

    # DELTA 2: the guest's NICs are macvtap children of eth1 (not taps on a bridge).
    host.succeed("ip -d link show briard-drbd0 | grep -q macvtap")
    host.succeed("ip -d link show briard0 | grep -q macvtap")
    host.succeed("ip -o link show briard-drbd0 | grep -q 'briard-drbd0@eth1'")

    # The guest converges on the bundled qemu, launched behind the fd-passing wrapper.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")

    # DELTA 3: the guest unit was started THROUGH briard-net-wrap, and qemu really holds the
    # macvtap chardevs on the inherited fds — the fd-passing mechanism ifname= cannot provide.
    host.succeed("systemctl show briard-guest.service -p ExecStart | grep -q briard-net-wrap")
    qpid = host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64").strip().split()[0]
    fds = host.succeed(f"ls -l /proc/{qpid}/fd/")
    print("qemu fds:\n" + fds)
    assert "/dev/tap" in fds, "qemu is not holding any /dev/tap<ifindex> chardev (fd-passing failed)"

    # DELTA 4 (THE PROOF): the OFF-BOX client reaches Briard at the VIP through the macvtap.
    client.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    print("off-box client reached the VIP over the macvtap substrate")

    # What a stranger actually gets. The install ships NO service, so the front door is
    # what answers -- and it says so, rather than the node looking broken or serving a workload
    # nobody chose. This is the assertion that would catch a payload sneaking back into the
    # shipped disk.
    page = client.succeed("curl -fsS http://192.168.1.100/")
    assert "No service is installed" in page, f"the VIP served: {page!r}"
    health = client.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "no service installed" in health, f"/healthz said: {health!r}"

    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'role=anchor primary=true quorate=true.*healthy=true'",
        timeout=60,
    )

    # FHS: pet volume under /var/lib, cattle under /opt (the cattle/pet split assertion d builds on).
    host.succeed("test -f /var/lib/briard/data.img")   # pet
    host.succeed("test -f /opt/briard/guest.qcow2")     # cattle overlay
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")
    print(host.succeed("journalctl -u briard-agent | tail -20"))

    # --- assertion (d): cattle/pet reinstall ---
    # The pet (/var/lib/briard) must survive a cattle (/opt/briard) wipe: `rm -rf /opt/briard` +
    # reinstall re-fetches the cattle and reaches green again with the GUEST DATA intact -- proving
    # the guest REATTACHES the existing data volume (blkid-guarded mkfs skipped, create-md without
    # --force refuses to re-seed) rather than reformatting it.
    #
    # The handle is the btrfs FILESYSTEM UUID on the pet volume, read straight off the backing
    # file: mkfs generates a fresh one, so a reformat across the reinstall changes it and an
    # honest reattach cannot. The handle was once the fixture's monotonic tick counter,
    # which was strictly stronger (it proved committed *content* crossed, not just that the
    # filesystem was the same one) -- but the shipped node runs no service, so there is nothing
    # writing data to compare. A runtime service install gets the stronger proof back by installing a service at
    # runtime and resuming the tick comparison on top of this one.
    def fsid(m):
        # btrfs primary superblock at 0x10000: fsid at +0x20, magic ("_BHRfS_M") at +0x40.
        # Asserting the magic keeps this honest -- wrong offsets would otherwise compare two
        # identical blobs of zeroes and "pass".
        magic = m.succeed(
            "dd if=/var/lib/briard/data.img bs=1 skip=65600 count=8 status=none | od -An -c | tr -d ' \\n'"
        ).strip()
        assert magic == "_BHRfS_M", f"no btrfs superblock where expected (read {magic!r})"
        return m.succeed(
            "dd if=/var/lib/briard/data.img bs=1 skip=65568 count=16 status=none | od -An -tx1 | tr -d ' \\n'"
        ).strip()

    pre = fsid(host)
    print(f"pre-wipe data volume fsid={pre}")

    # The honest cattle-reset gesture: stop briard (the agent AND its detached guest unit -- the
    # guest runs as a sibling transient service, so stopping the agent alone leaves
    # qemu holding the overlay AND the macvtap chardev), then remove ONLY /opt/briard.
    host.succeed("systemctl stop briard-agent.service briard-guest.service")
    host.succeed("rm -rf /opt/briard")
    host.fail("test -e /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle really gone
    host.succeed("test -f /var/lib/briard/data.img")               # pet survives the wipe
    # The live macvtaps (kernel state) survive the cattle wipe just as the bridge did on the old
    # path -- net-up.sh is gone, but `ip link` state is not owned by /opt. And the host's own
    # address never moved in the first place, so there is nothing to restore.
    host.succeed("ip -d link show briard0 | grep -q macvtap")
    host.succeed("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")
    # Non-vacuity for the re-green proof below: with the guest gone the VIP no longer answers.
    client.wait_until_fails("curl -fsS --max-time 3 http://192.168.1.100/healthz", timeout=60)

    # Reinstall: the SAME one command. It re-lays /opt from staging, recreates a FRESH guest overlay
    # (cattle), and does NOT recreate the pet data.img. net-up.sh is idempotent, so it adopts the
    # macvtaps that are already up rather than re-creating them.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NIC=eth1 BRIARD_NET_MODE=macvtap "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle re-fetched

    # Green again on the re-fetched bundle: the OFF-BOX client reaches the VIP.
    client.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=600)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")
    print("reinstall reached green again on the re-fetched cattle")

    # THE PROOF (assertion d): the guest re-attached the existing data volume rather than
    # reformatting it -- same filesystem, not a fresh one wearing the same path. A reformat
    # would mint a new fsid, which is the sharp failable control.
    post = fsid(host)
    print(f"post-reinstall data volume fsid={post} (pre-wipe={pre})")
    assert post == pre, f"PET LOST: data volume reformatted across reinstall ({pre} -> {post})"
    print("pet volume survived the cattle wipe: the guest reattached it")
  '';
}
