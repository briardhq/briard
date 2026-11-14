# Acts (a)+(b): what one bad sector does to a node that has a peer -- the companion to
# media-error-lone, the same fault where DRBD has somewhere to turn. Two questions; the first is
# the whole case for a media-error policy:
#
#   (a) `on-io-error detach` + a live peer -> the node drops its disk and keeps serving over the
#       network. Predicted: the application sees nothing at all -- even the read of the broken
#       sector succeeds, DRBD retrying it on the peer: no error, no role or quorum change, no
#       failing unit. The silent fallback in its purest form, which a media-error policy would
#       have to make un-silent (the agent has none). Then, on re-attach: is the resync
#       bitmap-partial or full? Still open, and the economics of "repair rather than rebuild"
#       rest on it.
#   (b) `on-io-error pass_on` + a live peer -> the device stays attached, the disk goes
#       Inconsistent with the bad block marked out-of-sync. Does a reconnect bitmap resync write
#       that block back -- the write that makes real hardware remap the sector?
#
# ⚠️ The alert half of (a) cannot be measured here, structurally: `redundancyAlerter` lives in the
# host agent (agent/host/alert.go) and a hermetic rig has no host (the node is the guest). That
# defect is a `model.Cluster` away from a Go unit test and belongs in one; this rig proves the
# DRBD-side facts the alerter would have to read.
#
# Injector as in media-error-lone (dm-dust); the layering is this rig's own: dm-dust straight
# under DRBD, no LVM, no LUKS, no promoter, no agent.
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
    # ⚠️ BAD sits far from anything the test writes, and that placement is the experiment. Inside
    # the 4 MiB written while diskless (sector 2048, say), the re-attach resync would cover it for
    # an unrelated reason and dm-dust, modelling remap-on-write faithfully ("removed from
    # badblocklist by write"), would heal it: measuring the rig, not DRBD. At ~97 MiB it is outside
    # every written region, so a resync covering it is DRBD tracking the damage, not the window.
    BAD = 200000        # made unreadable on node1 only; ~97 MiB in, well past anything written
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
        # DRBD announces the size of a resync as it begins ("Began resync as SyncTarget (will
        # sync N KB [M bits set])"): that line is the partial-vs-full answer.
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

    # ── Act (a): detach, with a peer holding the data ─────────────────────────────────────────
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
    # `client` separates the intentional disklessness of a witness from this accidental one (a
    # media-error policy would tell them apart by it); true here means that predicate is wrong.
    assert client_a == "false", f"(a) accidental diskless must report client=false, got {client_a}"
    print("VERDICT (a): CONFIRMED -- silent network fallback; only a status field records it")

    # ── Acts (a2)+(a3): how big is the re-attach resync, and does it repair the sector? ───────
    # Write while diskless, so there is a known amount to bring back: 4 MiB of the 16.
    node1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=1M count=4 conv=fsync")

    # ⚠️ Re-attach with the sector still broken: clearing it first would assume the disk was
    # replaced, and never ask what decides whether `detach` can recover at all -- does the
    # re-attach resync rewrite the bad block? Predicted no: under `detach` the out-of-sync bit is
    # set after the device is already Failed with its bitmap freed (`drbd_sender.c:351` runs
    # before `__req_mod` at `:362`), so nothing records the sector as suspect, and the resync
    # covers only what the peer tracked as written meanwhile -- if nothing wrote that sector,
    # nothing rewrites it.
    node1.succeed("drbdadm attach r0")
    node1.wait_until_succeeds("drbdsetup status r0 | grep -q UpToDate")
    print("RESYNC EVIDENCE (a):", resync_lines(node1))
    print(f"VERDICT (a2): after re-attach disk={dev(node1, 'disk-state')} -- read the KB figure above:")
    print("             ~16 MB on a 256 MB device means bitmap-partial; ~the whole device means a rebuild")

    # ⚠️ The read result is the wrong predicate: with a peer the read always succeeds (act (a)'s
    # whole finding). The disk state after it is the answer: detached again means the resync did
    # not rewrite the block.
    bad_after_reattach = read_sector(node1, BAD)
    disk_a3 = dev(node1, "disk-state")
    print("VERDICT (a3): re-attach resynced the region above; reading the still-broken sector")
    print(f"              -> read succeeded={bad_after_reattach} (always true with a peer), disk now={disk_a3}")
    if disk_a3 == "Diskless":
        print("VERDICT (a3): NOT REPAIRED -- the read detached the device AGAIN. `detach` recovers the")
        print("              DEVICE but not the DAMAGE: nothing recorded which block was bad, because")
        print("              the out-of-sync bit is set after the bitmap is freed. In a flock this is a")
        print("              SILENT loop -- the workload keeps being served from the peer while the node")
        print("              bounces to Diskless on every read of that sector and never settles as a")
        print("              usable failover target. Only a FULL invalidate would repair it, by")
        print("              rewriting everything.")
    else:
        print(f"VERDICT (a3): the device survived the read (disk={disk_a3}) -- the resync DID cover the")
        print("              block. Check that the bad sector lies outside every region the test wrote,")
        print("              or this is the rig measuring its own write window again.")

    # Now the disk really is 'replaced', so act (b) starts from a clean device.
    node1.succeed(f"dmsetup message dusty 0 removebadblock {BAD} || true")
    node1.succeed("dmsetup message dusty 0 clearbadblocks || true")
    if dev(node1, "disk-state") == "Diskless":
        node1.succeed("drbdadm attach r0")
    node1.wait_until_succeeds("drbdsetup status r0 | grep -q UpToDate")

    # ── Act (b): the same fault under pass_on ─────────────────────────────────────────────────
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

    # The repair: only a reconnect starts a resync while Established (`consider_resync`,
    # drbd_receiver.c), so drive one and see whether the marked block comes back -- on real
    # hardware that write is what makes the controller remap the sector.
    node1.succeed(f"dmsetup message dusty 0 removebadblock {BAD}")
    # ⚠️ Measured: DRBD refuses this disconnect on a live Primary (`State change failed: Need
    # access to UpToDate data (-2)`, exit 17, the refusal a lone node gives to promoting an
    # Inconsistent disk). The kernel enforces the hazard: with the disk Inconsistent the peer is
    # the only source for the marked blocks, so dropping it would strand a serving Primary. The
    # repair cannot be driven on the serving node; it must hand over first -- exactly the
    # eviction a media-error policy would do, arrived at from the other direction.
    demote_rc, demote_out = node1.execute("drbdadm secondary r0 2>&1")
    print(f"VERDICT (b1): demote before repair rc={demote_rc} out={demote_out!r} role={role(node1)}")
    node1.succeed("drbdadm disconnect r0")
    node1.succeed("drbdadm connect r0")
    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    node1.succeed("sleep 5")
    print("RESYNC EVIDENCE (b):", resync_lines(node1))
    print(f"VERDICT (b2): after reconnect disk={dev(node1, 'disk-state')} role={role(node1)}")
    print("             UpToDate here means the reconnect repaired the marked block")
  '';
}
