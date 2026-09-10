# [B.144] acts (a)+(b) — WHAT ONE BAD SECTOR DOES TO A NODE THAT HAS A PEER.
#
# The companion to media-error-lone, and the acts are the same fault in the topology where DRBD
# has somewhere to turn. Two questions, and the first one is the whole reason [B.140a] exists:
#
#   (a) `on-io-error detach` + a live peer -> the node drops its disk and keeps serving over the
#       network. Predicted: the APPLICATION SEES NOTHING AT ALL -- even the read of the broken
#       sector succeeds, because DRBD retries it on the peer. No error, no role change, no quorum
#       change, no failing unit. That is the silent fallback in its purest form, and it is what
#       [B.140a]'s agent policy exists to stop being silent.
#       Then, on re-attach: IS THE RESYNC BITMAP-PARTIAL OR FULL? That question is open in both
#       [B.140a] and [V5.7], and the whole economics of "repair rather than rebuild" rests on it.
#   (b) `on-io-error pass_on` + a live peer -> the device stays attached and the disk goes
#       Inconsistent, with the bad block marked out-of-sync. Then a reconnect: does the bitmap
#       resync actually write that block back -- the write that, on real hardware, is what makes
#       the controller remap the sector?
#
# ⚠️ THE ALERT HALF OF (a) CANNOT BE MEASURED HERE, and the reason is structural rather than an
# omission: `redundancyAlerter` lives in the HOST agent (agent/host/alert.go) and a hermetic rig
# has no host -- the node IS the guest. That defect is a `model.Cluster` away from a Go unit test
# and belongs in one; this rig proves the DRBD-side facts the alerter would have to read.
#
# Injector and layering as in media-error-lone: dm-dust straight under DRBD, no LVM, no LUKS, no
# promoter, no agent.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  onBlock = name: id: ''
    on ${name} {
      node-id ${toString id};
      address 10.0.0.${toString (id + 1)}:7789;
      volume 0 {
        device /dev/drbd0;
        disk /dev/mapper/dusty;
        meta-disk internal;
      }
    }'';
  resource = ''
    resource r0 {
      net { protocol C; }
      options {
        auto-promote                  no;
        quorum                        majority;
        on-no-quorum                  io-error;
        on-suspended-primary-outdated force-secondary;
      }
      disk { on-io-error detach; }
      ${onBlock "node1" 0}
      ${onBlock "node2" 1}
      connection-mesh { hosts node1 node2; }
    }
  '';
  resFile = pkgs.writeText "r0.res" resource;
  node = h.mkNode {
    inherit resource;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "media-error-flock";
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
  };

  testScript = ''
    BAD = 2048          # made unreadable on node1 only
    GOOD = 8            # never bad -- the control

    def dev(m, field):
        return m.succeed(f"drbdsetup status r0 --json | ${pkgs.jq}/bin/jq -r '.[0].devices[0][\"{field}\"]'").strip()

    def role(m):
        return m.succeed("drbdsetup status r0 --json | ${pkgs.jq}/bin/jq -r '.[0].role'").strip()

    def read_sector(m, sector):
        status, _ = m.execute(f"dd if=/dev/drbd0 bs=512 skip={sector} count=1 iflag=direct of=/dev/null 2>&1")
        return status == 0

    def show(m, label):
        print(f"--- {label} ---")
        print(m.succeed("drbdsetup status r0 --verbose --statistics || true"))
        print(m.succeed("dmesg | grep -iE 'drbd|dust' | tail -25 || true"))

    def resync_lines(m):
        # DRBD announces the size of a resync as it begins: "Began resync as SyncTarget
        # (will sync N KB [M bits set])". That line IS the partial-vs-full answer.
        return m.succeed("dmesg | grep -iE 'Began resync|resync done|bits set' | tail -10 || true")

    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe -a drbd dm_dust")
        sectors = m.succeed("blockdev --getsz /dev/vdb").strip()
        m.succeed(f"dmsetup create dusty --table '0 {sectors} dust /dev/vdb 0 512'")
        m.succeed("dmsetup message dusty 0 enable")
        m.succeed("mkdir -p /run/briard/drbd.d && cp ${resFile} /run/briard/drbd.d/r0.res")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("drbdadm up r0")

    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    node1.succeed("drbdadm primary r0")
    node1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=1M count=16 conv=fsync")
    node1.succeed("echo 3 > /proc/sys/vm/drop_caches")
    assert dev(node1, "disk-state") == "UpToDate", "precondition: node1 UpToDate"
    print("[setup] two anchors, node1 Primary and UpToDate")

    # ── ACT (a): detach, with a peer holding the data ─────────────────────────────────────────
    node1.succeed(f"dmsetup message dusty 0 addbadblock {BAD}")
    bad_read = read_sector(node1, BAD)
    good_read = read_sector(node1, GOOD)
    disk_a, role_a, quorum_a, client_a = dev(node1, "disk-state"), role(node1), dev(node1, "quorum"), dev(node1, "client")
    show(node1, "act (a): detach with a live peer")
    print(f"VERDICT (a): bad sector readable={bad_read}  good sector readable={good_read}")
    print(f"VERDICT (a): disk={disk_a} role={role_a} quorum={quorum_a} client={client_a}")

    # The finding: nothing above DRBD can tell this happened.
    assert disk_a == "Diskless", f"(a) expected Diskless, got {disk_a}"
    assert role_a == "Primary", f"(a) the node must NOT demote on its own, got {role_a}"
    assert quorum_a == "true", f"(a) quorum must be unaffected, got {quorum_a}"
    assert bad_read, "(a) the read of the BROKEN sector must succeed -- served from the peer"
    assert good_read, "(a) good sectors must still read -- served from the peer"
    # `client` separates the witness's INTENTIONAL disklessness from this accidental one
    # ([B.140a]); if this comes back true the predicate that item rests on is wrong.
    assert client_a == "false", f"(a) accidental diskless must report client=false, got {client_a}"
    print("VERDICT (a): CONFIRMED -- silent network fallback; only a status field records it")

    # ── The open question: how big is the re-attach resync? ───────────────────────────────────
    # Write while diskless, so there IS a known amount to bring back: 4 MiB of the 16.
    node1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=1M count=4 conv=fsync")
    node1.succeed(f"dmsetup message dusty 0 removebadblock {BAD}")
    node1.succeed("dmsetup message dusty 0 clearbadblocks || true")
    node1.succeed("drbdadm attach r0")
    node1.wait_until_succeeds("drbdsetup status r0 | grep -q UpToDate")
    print("RESYNC EVIDENCE (a):", resync_lines(node1))
    print(f"VERDICT (a2): after re-attach disk={dev(node1, 'disk-state')} -- read the KB figure above:")
    print("             ~4 MB means bitmap-partial; ~the whole device means a full rebuild")

    # ── ACT (b): the same fault under pass_on ─────────────────────────────────────────────────
    node1.succeed("drbdadm disk-options --on-io-error=pass_on r0")
    flipped = node1.succeed("drbdsetup show r0 | grep on-io-error || true").strip()
    print(f"VERDICT (flip): runtime disk-options -> {flipped!r}")
    assert "pass_on" in flipped, "the runtime flip did not take"

    node1.succeed("echo 3 > /proc/sys/vm/drop_caches")
    node1.succeed(f"dmsetup message dusty 0 addbadblock {BAD}")
    bad_read_b = read_sector(node1, BAD)
    disk_b, role_b = dev(node1, "disk-state"), role(node1)
    oos_b = node1.succeed("drbdsetup status r0 --statistics | grep -oE 'out-of-sync:[0-9]+' | head -1 || true").strip()
    show(node1, "act (b): pass_on with a live peer")
    print(f"VERDICT (b): bad sector readable={bad_read_b}  disk={disk_b} role={role_b} {oos_b}")
    assert bad_read_b, "(b) the read must still be served from the peer"
    assert disk_b != "Diskless", f"(b) pass_on must keep the device attached, got {disk_b}"

    # The repair: a reconnect is the only thing that starts a resync while Established
    # (`consider_resync`, drbd_receiver.c) -- so drive it and see whether the marked block
    # comes back. On real hardware that write is what makes the controller remap the sector.
    node1.succeed(f"dmsetup message dusty 0 removebadblock {BAD}")
    node1.succeed("drbdadm disconnect r0")
    node1.succeed("drbdadm connect r0")
    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    node1.succeed("sleep 5")
    print("RESYNC EVIDENCE (b):", resync_lines(node1))
    print(f"VERDICT (b2): after reconnect disk={dev(node1, 'disk-state')} role={role(node1)}")
    print("             UpToDate here means the reconnect repaired the marked block")
  '';
}
