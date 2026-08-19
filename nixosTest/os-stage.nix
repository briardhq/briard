# A guest receives a system closure it does not have.
#
# This is the gap the binary cache closes. Every closure a guest could ever switch to had
# arrived BAKED ONTO ITS DISK at image-build time (system.extraDependencies), so the
# upgrade machinery — announce, snapshot, os.switch, health-gate, roll back — was real and
# tested while the delivery half underneath it did not exist. A field node could only ever
# switch to the one generation it shipped with. This test is the other half: a closure the
# guest genuinely lacks is fetched over a binary cache and activated.
#
# WHAT MAKES IT A REAL PROOF. The target is the booted guest plus one marker file, and it
# is baked into no disk — note it is deliberately NOT the disk's own `.v1System`, which IS
# baked and would be satisfied from the local store. The driver then proves the absence
# rather than assuming it, using a read that cannot answer for an absent closure: `os.components`
# resolves paths INSIDE it. Reading BEFORE staging must fail; reading AFTER must succeed. Drop
# the fetch and the test fails at the second read — it cannot pass vacuously against a disk that
# had the closure all along ([[verification-assertions-must-fail]]).
#
# THE CACHE IS REAL, AND SIGNED — see ./test-cache.nix, which this was the first caller of
# and which serves the fetched-target tests too. The guest does a genuine signature-checked
# substitution with `require-sigs` at its production default.
#
# The guest is pointed at that cache by the host, per call (os.stage's StageSource), NOT by
# rebuilding the image with different substituters — a test-only disk variant is a
# multi-GB build, and killing exactly those is what the fetched-target work is for.
#
# Heavy (nested VM), so it rides the `integration` tag; `nix flake check` skips it. Run:
#   nix build .#tests.os-stage -L
{
  pkgs,
  guestDisk,
  stagedSystem,
  driver,
  netWrap,
}:
let
  cache = import ./test-cache.nix { inherit pkgs; } { paths = [ stagedSystem ]; };
in
pkgs.testers.runNixOSTest {
  name = "os-stage";
  skipTypeCheck = true;

  nodes.host =
    { ... }:
    {
      imports = [ cache.module ]; # the stand-in cache.briard.io, holding + serving the target
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 12288;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
      environment.systemPackages = [
        pkgs.qemu
        driver
        pkgs.iproute2
        pkgs.curl
      ];

      # The nested guest reaches the cache on the L1 host's shim address; nothing here
      # faces a real network (— the same reason the fleet's L1 launchers do this).
      networking.firewall.enable = false;
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # Mint the key, serve, and prove the target is served signed before the guest is asked
    # to fetch it. The guest is told this public key for this one os.stage call, which swaps
    # WHICH key it trusts without ever weakening require-sigs.
    ${cache.start "host"}

    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name shim0 type macvlan mode bridge && ip addr add 192.168.1.1/24 dev shim0 && ip link set shim0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up"
    )

    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Boot + bring up, then drive the staging run (STAGE_SYSTEM set).
    host.succeed(
        "systemd-run --unit=briard-driver --collect "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest "
        "--setenv=SERVICE_TAP=svc0 --setenv=SERVICE_MAC=52:54:00:aa:bb:cc --setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        # Declared even though this test never curls the VIP: the promoter chain still ends at
        # briard-vip, and with nothing baked (V3.19c step 3) an unset address means DHCP -- which
        # would sit waiting for a server that does not exist on this L2.
        "--setenv=VIP_DEV=eth1 --setenv=VIP_ADDR=192.168.1.100/24 "
        # NO_PAYLOAD: the zero-service promoter chain (the shipped shape). This test boots
        # the SHIPPED disk, whose payload slot is empty -- naming a unit that does not exist
        # fails the whole chain and takes the VIP down with it.
        "--setenv=NO_PAYLOAD=1 "
        "--setenv=STAGE_SYSTEM=${stagedSystem} "
        f"--setenv=STAGE_FROM={cache_url} "
        f"--setenv=STAGE_FROM_KEY={cache_pubkey} "
        "--setenv=GUEST_SERIAL=/tmp/guest-console.log "
        "${driver}/bin/driver"
    )

    # CONVERGED = booted + brought up; then the staging sequence.
    host.wait_until_succeeds("journalctl -u briard-driver | grep -q CONVERGED", timeout=900)
    # STAGED = os.components could not read the closure, the fetch ran, and then it could — so
    # bytes crossed the wire into a store that did not have them. Anchored at end-of-line
    # only: journalctl prefixes every line with a timestamp/unit, so '^STAGED$' would never
    # match, and an unanchored 'STAGED' would be satisfied by STAGED_SYSTEM.
    host.wait_until_succeeds("journalctl -u briard-driver | grep -qE 'STAGED$'", timeout=900)
    # The activation method was decided from a REAL toplevel's boot components,
    # before activating. A userland-only delta must read as `switch`; `reboot` here would
    # mean every routine update costs a boot.
    host.wait_until_succeeds("journalctl -u briard-driver | grep -q 'ACTIVATION method=switch'", timeout=900)
    # STAGED_SYSTEM = the fetched closure is not just present but activated and running.
    host.wait_until_succeeds("journalctl -u briard-driver | grep -q STAGED_SYSTEM", timeout=900)

    line = host.succeed("journalctl -u briard-driver | grep STAGED_SYSTEM | tail -1")
    assert "${stagedSystem}" in line, f"guest runs the wrong system: {line}"
    print("guest fetched a closure it did not have, and is running it —", line.strip())
  '';
}
