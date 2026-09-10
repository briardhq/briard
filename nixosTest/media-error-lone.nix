# [B.144] act (c)+(d)+(e) — WHAT ONE BAD SECTOR DOES TO A LONE NODE, measured.
#
# A spike, not a guard: it prints a verdict per act and asserts only what the thread already
# predicted from the 9.2.19 source, so a surprise shows up as a failed assertion with the full
# state printed above it.
#
# The three questions, in order:
#   (c) `on-io-error detach` + no peer -> does ONE unreadable sector take the WHOLE device away?
#       (predicted yes: the handler drops the disk with no check for whether a peer exists, and
#       `on-no-data-accessible` then defaults to io-error, so every read fails -- including reads
#       of the 99.99% of the device that is fine.)
#   (d) `on-io-error pass_on` + no peer -> is it raw-disk behaviour instead? (predicted yes: EIO
#       on the bad sector only, disk `Inconsistent`, every good sector still readable.)
#   (e) after a reboot in that Inconsistent state, CAN THE LONE NODE STILL PROMOTE? That is the
#       trap that decides whether `pass_on`-when-alone is adoptable, and it is the one act whose
#       answer nobody in the thread could predict.
#
# It also proves the mechanism the per-topology design rests on: that `on-io-error` can be flipped
# at RUNTIME with `drbdadm disk-options`, with no detach and no restart.
#
# THE INJECTOR IS dm-dust, and the choice is load-bearing ([B.144]): a plain `dmsetup error` target
# fails forever, so it cannot model a sector that stops failing. dm-dust's `addbadblock` /
# `removebadblock` messages give exact per-sector control in both directions, which is what lets
# (d) simulate the controller's remap-on-write without needing a real dying disk.
#
# No LVM, no LUKS, no promoter, no agent: DRBD sits straight on the dust device. The question is
# what the DRBD kernel module does with a media error, and every layer between it and the injector
# is a layer that could absorb the thing being measured.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # Mesh-of-one, the shipped single-node form, with the production safety options -- plus the
  # `disk {}` section the product does NOT render today ([B.140a]): `on-io-error` is inherited
  # from the module default everywhere in the tree, which is half of why this was never measured.
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
      on n {
        node-id 0;
        address 10.0.0.1:7789;
        volume 0 {
          device /dev/drbd0;
          disk /dev/mapper/dusty;
          meta-disk internal;
        }
      }
      connection-mesh { hosts n; }
    }
  '';
  # The product writes its rendered `.res` to /run/briard/drbd.d at bring-up
  # (`briard-node-storage`, agent/guestagent). mkNode only puts the text in the storage SPEC, and
  # this rig deliberately skips that unit -- it wants DRBD on the dust device, not on the seam's
  # LV -- so it lays the file down itself, in the same place the product would.
  resFile = pkgs.writeText "r0.res" resource;
in
pkgs.testers.runNixOSTest {
  name = "media-error-lone";
  skipTypeCheck = true;

  nodes.n = h.mkNode {
    inherit resource;
    promoter = false; # bare DRBD: drbd-reactor would only add a second opinion about the role
  };

  testScript = ''
    BAD = 2048          # the sector we make unreadable: 1 MiB in, well inside the data area
    GOOD = 8            # a sector that is never bad -- the control, and the whole point of act (c)

    def dstate(m):
        return m.succeed("drbdsetup status r0 --json | ${pkgs.jq}/bin/jq -r '.[0].devices[0][\"disk-state\"]'").strip()

    def read_sector(m, sector):
        """True if a 512-byte direct read of that sector succeeds."""
        status, _ = m.execute(f"dd if=/dev/drbd0 bs=512 skip={sector} count=1 iflag=direct of=/dev/null 2>&1")
        return status == 0

    def show_state(m, label):
        print(f"--- {label} ---")
        print(m.succeed("drbdsetup status r0 --verbose --statistics || true"))
        print(m.succeed("dmesg | grep -iE 'drbd|dust' | tail -20 || true"))

    n.start()
    n.wait_for_unit("multi-user.target")
    n.succeed("modprobe drbd")

    # The injector. dm-dust must be present in the guest's module tree -- assert it rather than
    # let a missing module surface later as an unexplained pass.
    n.succeed("modprobe dm_dust")
    sectors = n.succeed("blockdev --getsz /dev/vdb").strip()
    n.succeed(f"dmsetup create dusty --table '0 {sectors} dust /dev/vdb 0 512'")
    n.succeed("dmsetup message dusty 0 enable")
    n.succeed("mkdir -p /run/briard/drbd.d && cp ${resFile} /run/briard/drbd.d/r0.res")

    # A lone UpToDate Primary with real bytes on it.
    n.succeed("drbdadm create-md --force r0")
    n.succeed("drbdadm up r0")
    n.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    n.succeed("drbdadm primary r0")
    n.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=1M count=16 conv=fsync")
    n.succeed("echo 3 > /proc/sys/vm/drop_caches")
    assert dstate(n) == "UpToDate", f"precondition: expected UpToDate, got {dstate(n)}"
    assert read_sector(n, GOOD), "precondition: the control sector must be readable"
    print("[setup] lone Primary, UpToDate, both sectors readable")

    # ── ACT (c): one bad sector under `on-io-error detach` ────────────────────────────────────
    n.succeed(f"dmsetup message dusty 0 addbadblock {BAD}")
    bad_read_c = read_sector(n, BAD)
    good_read_c = read_sector(n, GOOD)
    disk_c = dstate(n)
    show_state(n, "act (c): detach, after one bad sector")
    print(f"VERDICT (c): bad sector readable={bad_read_c}  GOOD sector readable={good_read_c}  disk={disk_c}")

    assert not bad_read_c, "(c) the injector did not fail the read -- the act measured nothing"
    # The finding this act exists for: the device is gone, so a sector that is PERFECTLY FINE is
    # unreadable too. That is DRBD amplifying a localized fault into total unavailability, which
    # on a lone node is the whole volume.
    assert disk_c == "Diskless", f"(c) expected Diskless after detach, got {disk_c}"
    assert not good_read_c, "(c) UNEXPECTED: a good sector still reads after the detach -- re-derive the model"

    # ...and it does not terminate on its own: nothing re-attaches, so the node stays dead.
    n.succeed("sleep 5")
    assert dstate(n) == "Diskless", "(c) the node recovered by itself -- that would change the item"
    print("VERDICT (c): CONFIRMED -- one bad sector, whole volume unreadable, no self-recovery")

    # ── The runtime flip the per-topology design rests on ─────────────────────────────────────
    n.succeed(f"dmsetup message dusty 0 removebadblock {BAD}")   # the disk is 'replaced'
    n.succeed("drbdadm attach r0")
    for _ in range(30):
        if dstate(n) != "Diskless":
            break
        n.sleep(1)
    print(f"[recover] re-attached without forcing; disk={dstate(n)}")

    n.succeed("drbdadm disk-options --on-io-error=pass_on r0")
    flipped = n.succeed("drbdsetup show r0 | grep on-io-error || true").strip()
    print(f"VERDICT (flip): runtime disk-options -> {flipped!r}")
    assert "pass_on" in flipped, "the runtime flip did not take -- the per-topology design needs another mechanism"

    # ── ACT (d): the same bad sector under `on-io-error pass_on` ──────────────────────────────
    n.succeed("echo 3 > /proc/sys/vm/drop_caches")
    n.succeed(f"dmsetup message dusty 0 addbadblock {BAD}")
    bad_read_d = read_sector(n, BAD)
    good_read_d = read_sector(n, GOOD)
    disk_d = dstate(n)
    show_state(n, "act (d): pass_on, after one bad sector")
    print(f"VERDICT (d): bad sector readable={bad_read_d}  GOOD sector readable={good_read_d}  disk={disk_d}")

    assert not bad_read_d, "(d) the injector did not fail the read"
    # The claim: raw-disk behaviour. The damage is confined to the sector that is actually broken.
    assert good_read_d, "(d) a good sector must still read under pass_on -- if not, pass_on buys nothing"
    print(f"VERDICT (d): confined damage, disk state is {disk_d}")

    # ── ACT (e): the trap -- can a lone node in that state come back? ─────────────────────────
    n.shutdown()
    n.start()
    n.wait_for_unit("multi-user.target")
    n.succeed("modprobe -a drbd dm_dust")
    n.succeed(f"dmsetup create dusty --table '0 {sectors} dust /dev/vdb 0 512'")
    n.succeed("dmsetup message dusty 0 enable")
    n.succeed("mkdir -p /run/briard/drbd.d && cp ${resFile} /run/briard/drbd.d/r0.res")
    n.succeed(f"dmsetup message dusty 0 addbadblock {BAD}")
    n.succeed("drbdadm up r0")
    n.succeed("sleep 3")
    disk_e = dstate(n)
    promote_rc, promote_out = n.execute("drbdadm primary r0 2>&1")
    show_state(n, "act (e): after reboot in the post-pass_on state")
    print(f"VERDICT (e): disk after reboot={disk_e}  promote_rc={promote_rc}  promote_out={promote_out!r}")

    # No assertion on the outcome: this act EXISTS to find out. Both answers are findings, and the
    # forced-UpToDate escape ([B.144]) is only needed if this one refuses.
    if promote_rc == 0:
        print("VERDICT (e): the lone node PROMOTED -- pass_on-when-alone needs no forcing")
    else:
        print("VERDICT (e): the lone node REFUSED to promote -- the forced-UpToDate step is required")
        forced_rc, forced_out = n.execute("drbdadm new-current-uuid --clear-bitmap r0/0 2>&1; drbdadm primary r0 2>&1")
        print(f"VERDICT (e2): after forced UpToDate, promote_rc={forced_rc} out={forced_out!r} disk={dstate(n)}")
  '';
}
