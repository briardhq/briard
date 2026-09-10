# WHAT ONE BAD SECTOR COSTS A LONE NODE: ONE FILE ([B.145c]'s exit assertion, [B.144]'s question).
#
# [B.144] measured the DRBD answer on this same injector: with no UpToDate diskful peer, DRBD turns
# a single unreadable sector into a whole-volume outage in every `on-io-error` mode -- `detach`
# drops the disk and fails everything, `pass_on` downgrades it to Inconsistent and
# `drbd_data_accessible()` finds no UpToDate copy, and forcing UpToDate is undone by the next read
# of the bad sector. That measurement is why a lone node runs no DRBD ([B.145]); this rig is the
# other half of the argument, on the product's own stack: LVM on a dm-dust device, btrfs on the
# data LV, the chain started from its target, a fixture serving -- then one sector under a data
# file goes bad. The verdict is that EXACTLY ONE file becomes unreadable, every other file still
# reads, the front door still answers, the chain stays up and nothing was held or rebooted.
#
# THE INJECTOR IS dm-dust, as in [B.144]: `addbadblock` fails reads of one sector, in place, with
# the filesystem mounted. The tier is built on the dust device (mkNode's tierDevice), not on
# /dev/vdb, so the sector is bad underneath everything the product stacks on it. Encryption is
# OFF for this rig only so the arithmetic from a btrfs chunk to a dust sector has one constant
# layer (the LV's start on the PV) rather than two; the claim is about btrfs, which sits above
# the cipher either way, and every other lone-node rig runs encrypted.
#
# WHERE THE BAD SECTOR GOES. btrfs data lives in chunks whose physical placement the chunk tree
# states (`btrfs inspect-internal dump-tree -t chunk`); the first DATA chunk is filled with 1 MiB
# files, so 1 MiB into it is inside some file's extent. Metadata is DUP and never targeted --
# a metadata sector would be healed from its copy, which is a different (and also good) story.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    replicated = false;
    tierDevice = "/dev/mapper/dusty";
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "media-error-lone";
  nodes.node1 = node;

  testScript = ''
    import re
    ${h.fixtureHelpers}

    FILES = 6  # 1 MiB each, into an 8 MiB first data chunk: the sector 1 MiB in is inside a file

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service")
    boot_id = node1.succeed("cat /proc/sys/kernel/random/boot_id").strip()

    # The injector under everything: the tier is built on it.
    node1.succeed("modprobe dm_dust")
    sectors = node1.succeed("blockdev --getsz /dev/vdb").strip()
    node1.succeed(f"dmsetup create dusty --table '0 {sectors} dust /dev/vdb 0 512'")
    node1.succeed("dmsetup message dusty 0 enable")

    # The product's lone-node stack, and a serving chain on it.
    node1.succeed("briard-test-storage --seed --mode off")
    assert node1.succeed("cat /run/briard/topology.env").strip() == "BRIARD_TOPOLOGY=alone"
    node1.fail("lsmod | grep -qw drbd")
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    # Real bytes, fsynced, then dropped from the page cache so every read below hits the disk.
    node1.succeed("mkdir -p /var/lib/briard/media-error")
    for i in range(FILES):
        node1.succeed(f"dd if=/dev/urandom of=/var/lib/briard/media-error/f{i} bs=1M count=1 conv=fsync 2>/dev/null")
    node1.succeed("sync -f /var/lib/briard && echo 3 > /proc/sys/vm/drop_caches")
    for i in range(FILES):
        node1.succeed(f"cat /var/lib/briard/media-error/f{i} > /dev/null")

    # The first DATA chunk's physical start on the LV, from the chunk tree; the LV's start on
    # the dust device, from its dm table. Their sum plus 1 MiB is a sector inside one file.
    tree = node1.succeed("btrfs inspect-internal dump-tree -t chunk /dev/mapper/briardservice-data")
    data_chunk = None
    for block in tree.split("\titem ")[1:]:
        if "type DATA" in block:
            m = re.search(r"stripe 0 devid 1 offset (\d+)", block)
            assert m, f"a DATA chunk with no stripe:\n{block}"
            data_chunk = int(m.group(1))
            break
    assert data_chunk is not None, f"no DATA chunk in the chunk tree:\n{tree}"
    table = node1.succeed("dmsetup table briardservice-data").strip()
    m = re.match(r"0 \d+ linear \S+ (\d+)$", table)
    assert m, f"the data LV is not one linear segment: {table!r}"
    lv_start = int(m.group(1))
    BAD = lv_start + data_chunk // 512 + 2048
    print(f"[inject] data chunk at LV byte {data_chunk}, LV starts at dust sector {lv_start}: bad sector {BAD}")

    node1.succeed(f"dmsetup message dusty 0 addbadblock {BAD}")
    node1.succeed("echo 3 > /proc/sys/vm/drop_caches")

    # === THE VERDICT: one file, and only one =============================================
    unreadable = []
    for i in range(FILES):
        rc, out = node1.execute(f"cat /var/lib/briard/media-error/f{i} > /dev/null 2>&1")
        if rc != 0:
            unreadable.append(f"f{i}")
    print(f"VERDICT: unreadable files = {unreadable}")
    print(node1.succeed("dmesg | grep -iE 'btrfs|dust' | tail -20 || true"))
    assert len(unreadable) == 1, (
        f"one bad sector cost {len(unreadable)} file(s) ({unreadable}); the claim is exactly one"
    )
    # Everything else still serves: the mount, the chain, the front door, the fixture -- and
    # nothing held or rebooted, because a damaged file is not a failed member.
    node1.succeed("mountpoint -q /var/lib/briard")
    node1.succeed("systemctl is-active briard-chain.target briard-primary-storage.service briard-services.service")
    node1.succeed("curl -fsS http://192.168.1.100/healthz")
    node1.succeed("curl -fsS http://192.168.1.100:8080/healthz")
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")
    node1.fail("journalctl -u briard-promotion-hold.service --no-pager | grep -q .")
    assert node1.succeed("cat /proc/sys/kernel/random/boot_id").strip() == boot_id, "the node REBOOTED"
    # A fresh file lands elsewhere and reads back: the volume is not merely surviving, it works.
    node1.succeed("dd if=/dev/urandom of=/var/lib/briard/media-error/after bs=1M count=1 conv=fsync 2>/dev/null")
    node1.succeed("echo 3 > /proc/sys/vm/drop_caches && cat /var/lib/briard/media-error/after > /dev/null")
    print("VERDICT: one bad sector, one file; the lone node kept serving")
  '';
}
