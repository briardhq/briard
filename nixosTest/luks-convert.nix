# [V3b.33] — ENCRYPTION IS A `pvmove`, AND IT IS REVERSIBLE.
#
# The item was admitted on a premise that turned out to be false — "format the volume encrypted at
# install, because you can never add it later". You can. What has to exist in advance is not the
# encryption, it is a SEAM: a single-LV VG between the disk and DRBD. With one there, disarming a
# node is a `pvmove` onto a plaintext PV and arming it is the same move back, both with the
# workload serving throughout; without one there is no table to reload and DRBD would have to
# close and reopen its backing device.
#
# So this rig proves the claim in the direction that actually matters — BOTH — on the shape a
# household runs: real Home Assistant, on a btrfs volume, over a promoted DRBD.
#
#   the shipped ENCRYPTED node → (hotplug a blank disk, pvcreate, vgextend) → pvmove → vgreduce
#                              → (device_del the retired disk) → and all the way back onto LUKS.
#
# DISARM FIRST, deliberately. Encryption on by default (c) is defensible only if a household can
# get back off it without an outage — otherwise the default is a one-way door for the installed
# base, which is precisely the trap the original "impossible later" premise would have set.
#
# WHAT IS UNDER TEST IS THE ABSENCE OF EVENTS, which is why every assertion here is a negative
# with a positive control beside it. `pvmove` suspends the LV to insert and to retire its
# transient mirror, and a dm suspend QUEUES bios rather than failing them — so the claim is "a
# latency bump, not an outage". That is not something a green run asserts by finishing: it is
# asserted by an I/O prober that writes and fsyncs continuously across both moves, records what
# every write cost, and would record the failures if the queue became an error instead. DRBD must
# never demote, never detach and never resync; btrfs must see no error, in the log or in its own
# counters; HA must keep answering.
#
# AND IT MEASURES, because "a latency bump" is a number or it is a hope. The run prints the stall
# distribution per move and the copy throughput, which is what the rule of thumb in the docs is
# derived from.
#
# ⚠️ THE MOVE IS BOUNDED BY THE DISK, NOT BY THE CIPHER. AES-NI runs at GB/s and dm-crypt
# parallelises across per-CPU workqueues, so what this measures on any host is the copy.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };

  vg = "briard";
  lvDev = "/dev/mapper/${vg}-data";

  # The data disk, and the CONVERSION TARGETS, in MiB. A target is deliberately LARGER: the seam
  # unit gives the LV the whole PV, and a LUKS2 header takes ~16 MiB off the front of the target
  # plus another 1 MiB for the PV label, so a target the same size as the source could never hold
  # it. The host sizes the file it hotplugs, so this is its constraint to meet -- and meeting it
  # is why the seam reserves nothing on every node forever.
  diskMB = 4096;
  targetMB = 4352;

  # ── THE TWO PROBERS ────────────────────────────────────────────────────────────────────────
  # Separate because they measure separate claims at separate rates: the volume must not miss a
  # write (as tight a loop as a synchronous write allows), and the service must not miss a request
  # (once a second is what a household would notice).
  ioProbe = pkgs.writeShellScript "convert-io-probe" ''
    export PATH=${pkgs.lib.makeBinPath [ pkgs.coreutils ]}
    # One 4 KiB append + fsync per iteration, timestamped with the START of the write: a write
    # that blocks on the mirror's suspend begins before the suspend and ends after it, and it is
    # the one this exists to catch.
    : >/run/briard-probe.lat
    : >/run/briard-probe.err
    : >/var/lib/briard/.convert-probe
    while [ -e /run/briard-probe.run ]; do
      s=$(date +%s%N)
      if dd if=/dev/zero of=/var/lib/briard/.convert-probe bs=4096 count=1 \
           oflag=append conv=notrunc,fsync status=none; then
        e=$(date +%s%N)
        echo "$s $(( (e - s) / 1000000 ))" >>/run/briard-probe.lat
      else
        echo "$s write failed" >>/run/briard-probe.err
      fi
    done
  '';

  haProbe = pkgs.writeShellScript "convert-ha-probe" ''
    export PATH=${pkgs.lib.makeBinPath [ pkgs.coreutils pkgs.curl ]}
    : >/run/briard-ha.err
    : >/run/briard-ha.ok
    while [ -e /run/briard-probe.run ]; do
      if curl -fsS -o /dev/null --max-time 5 http://192.168.1.100:8123/manifest.json; then
        echo ok >>/run/briard-ha.ok
      else
        echo "$(date +%s%N) unreachable" >>/run/briard-ha.err
      fi
      sleep 1
    done
  '';

  node = h.mkNode {
    inherit fixture;
    # The resource names the LV -- as every node does since [V3b.33](b) -- and every move below
    # happens underneath that name, which is the whole now-decision the item turns on.
    resource = h.mkResource [
      {
        name = "node1";
        id = 0;
      }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "luks-convert";

  nodes.node1 =
    { lib, ... }:
    {
      imports = [ node ];
      # HA's numbers, measured in hass-payload: the 2.4 GB image is loaded onto the writable root
      # and HA's Python stack wants ~1 GB live.
      virtualisation.memorySize = 2048;
      virtualisation.diskSize = 10240;
      # The data disk the seam unit will claim, resized from lib.nix's 256 MiB (a 4 GiB volume is
      # what `install.sh` lays down by default, and a 256 MiB move would measure nothing) and given
      # a qdev id + a serial. Both are load-bearing: the id is what `device_del` names when the
      # conversion retires this disk, and the serial is what makes `/dev/disk/by-id/virtio-plain0`
      # a stable name in the guest rather than a `/dev/vd?` letter that shifts as disks come and go.
      virtualisation.emptyDiskImages = lib.mkForce [
        {
          size = diskMB;
          driveConfig.deviceExtraOpts = {
            id = "plain0-dev";
            serial = "plain0";
          };
        }
      ];
    };

  # HA boot is slow and the promoter selects the primary dynamically.
  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}

    VG = "${vg}"
    LV = "${lvDev}"
    TARGET_MB = ${toString targetMB}
    # The disk the seam unit claimed at boot, and the crypt device it opened on top of it. Both
    # are the PRODUCT's names, restated here rather than invented: this rig moves the product's
    # own volume, so it has to say what the product said.
    DATA_DISK = "/dev/vdb"
    CRYPT_NAME = "briard-crypt"
    CRYPT = "/dev/mapper/briard-crypt"
    # The conversion targets, hotplugged. plain0 is the shipped disk itself, named by its serial
    # so `device_del` can retire it once the volume has moved off.
    PLAIN1 = "/dev/disk/by-id/virtio-plain1"
    ENC1 = "/dev/disk/by-id/virtio-enc1"


    def hotplug(m, serial, size_mb):
        """Hand the RUNNING guest a blank disk over QMP. This is the host-side half of a
        conversion and it is not incidental: the target has to arrive without stopping the VM,
        or the whole exercise is an outage with extra steps."""
        path = m.state_dir / f"{serial}.raw"
        with open(path, "wb") as f:
            f.truncate(size_mb * 1024 * 1024)
        m.qmp_client.send("blockdev-add",
                          {"driver": "file", "node-name": f"{serial}-file", "filename": str(path)})
        m.qmp_client.send("blockdev-add",
                          {"driver": "raw", "node-name": f"{serial}-raw", "file": f"{serial}-file"})
        m.qmp_client.send("device_add", {"driver": "virtio-blk-pci", "id": f"{serial}-dev",
                                         "drive": f"{serial}-raw", "serial": serial})
        m.wait_until_succeeds(f"test -b /dev/disk/by-id/virtio-{serial}", timeout=60)


    def unplug(m, serial):
        """...and take the retired disk away again, because a conversion that cannot free what it
        left behind has not converted anything -- it has doubled the storage."""
        m.qmp_client.send("device_del", {"id": f"{serial}-dev"})
        m.wait_for_qmp_event(lambda e: e["event"] == "DEVICE_DELETED", timeout=120)
        m.wait_until_fails(f"test -b /dev/disk/by-id/virtio-{serial}", timeout=60)


    def has_btrfs_magic(m, dev):
        """Whether a btrfs superblock is visible in the RAW bytes of a device -- the direct test of
        "encrypted at rest", asked of the disk itself rather than of the commands that set it up.

        ⚠️ NOT A PIPELINE, and this is measured rather than stylistic: `head -c N dev | grep -q`
        makes grep exit at the FIRST match, head then dies of SIGPIPE, and under the driver's
        pipefail shell the pipeline's status is head's. So a device that HAS the magic reports
        exit 1 -- the answer inverted, silently, on exactly the assertion whose whole job is to
        tell the two apart. It cost a run, and the negative would have passed forever.

        flushbufs first for a different reason and the same spirit: DRBD and dm submit bios
        straight to their backing device, so a raw read can otherwise be answered from a page
        cache filled before the volume was ever written."""
        m.succeed(f"blockdev --flushbufs {dev}")
        m.succeed(f"head -c 67108864 {dev} >/tmp/rawprobe")
        found = m.execute("grep -qa _BHRfS_M /tmp/rawprobe")[0] == 0
        m.succeed("rm -f /tmp/rawprobe")
        return found


    def now(m):
        return int(m.succeed("date +%s%N"))


    def move(m, src, dst, label):
        """One `pvmove --atomic`, timed. Atomic so the LV is either wholly on the source or wholly
        on the target: an interrupted conversion has no third state to be recovered from."""
        t0 = now(m)
        m.succeed(f"pvmove --atomic -n data {src} {dst}", timeout=1800)
        t1 = now(m)
        secs = (t1 - t0) / 1e9
        print(f"[{label}] pvmove {src} -> {dst}: {secs:.1f}s for {lv_mb(m)} MiB "
              f"({lv_mb(m) / secs:.0f} MiB/s)")
        return t0, t1


    def lv_mb(m):
        """The LV size as the PRODUCT chose it -- the seam unit gives it the whole PV, so this rig
        reads the number rather than restating one it did not decide."""
        return int(float(m.succeed(f"lvs --noheadings --units m --nosuffix -o lv_size {LV}").strip()))


    def stall(m, t0, t1, label):
        """THE MEASUREMENT. What the suspend/resume cost the writer, over the window the move
        actually ran -- not an average over the whole test, which would hide it."""
        window = []
        for line in m.succeed("cat /run/briard-probe.lat").splitlines():
            parts = line.split()
            if len(parts) != 2:
                continue  # the prober was mid-line when we read it
            ts, ms = int(parts[0]), int(parts[1])
            if t0 <= ts <= t1:
                window.append(ms)
        assert window, f"[{label}] the prober recorded no write while the move ran -- it is not probing"
        window.sort()
        p99 = window[min(len(window) - 1, int(len(window) * 0.99))]
        print(f"[{label}] {len(window)} synced 4K writes across the move: "
              f"median {window[len(window) // 2]}ms  p99 {p99}ms  max {window[-1]}ms")
        return window[-1]


    def undisturbed(m, since, label):
        """WHAT HAD TO STAY TRUE, as negatives. DRBD never demoted, never detached, never
        resynced; btrfs saw no error in the log OR in its own counters; the prober lost no write;
        HA answered every poll; and the resource still names the same backing device it was
        created with, which is the property the seam exists for."""
        m.fail(f"journalctl -k --since '{since}' | grep -q 'role( Primary -> Secondary )'")
        m.fail(f"journalctl -k --since '{since}' | grep -q 'disk( UpToDate -> '")
        m.fail(f"journalctl -k --since '{since}' | grep -qiE 'drbd.*(detaching|io error|disk failure)'")
        m.fail(f"journalctl -k --since '{since}' | grep -qiE 'BTRFS (error|warning|critical)'")
        for line in m.succeed("btrfs device stats /var/lib/briard").splitlines():
            assert line.split()[-1] == "0", f"[{label}] btrfs recorded an I/O error: {line}"
        assert "Primary" in m.succeed("drbdadm role r0")
        assert "UpToDate" in m.succeed("drbdadm dstate r0")
        m.succeed(f"drbdsetup show r0 | grep -q '{LV}'")
        errs = m.succeed("cat /run/briard-probe.err")
        assert not errs.strip(), f"[{label}] the volume failed a write: {errs}"
        ha_errs = m.succeed("cat /run/briard-ha.err")
        assert not ha_errs.strip(), f"[{label}] HA stopped answering: {ha_errs}"
        polls = len(m.succeed("cat /run/briard-ha.ok").splitlines())
        print(f"[{label}] undisturbed: {polls} HA polls answered, no DRBD or btrfs event")


    node1.start()
    node1.wait_for_unit("multi-user.target")
    # HA's 2.4 GB image is resident before anything promotes, as on a real node.
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    # ── THE SEAM, BUILT BY THE PRODUCT ─────────────────────────────────────────────────────────
    # briard-node-storage.service lays this down from the spec the host writes ([V3b.33](d)), and
    # it also loads dm-mirror -- the target pvmove builds its transient mirror out of, which LVM
    # cannot autoload on a NixOS guest (it shells out to /sbin/modprobe, which does not exist
    # there; [V3b.33](a) measured the refusal). Nothing here builds a seam of its own: a rig that
    # did would be proving a stack no household runs.
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    node1.succeed(f"test -b {LV}")
    node1.succeed("lsmod | grep -q '^dm_mirror'")
    # The fence, asserted here because everything below depends on it: one linear segment, the
    # same target and the same linear_map() a bare disk would have had. It is what keeps the
    # seam free, and `lab/oracle` asserts it continuously on the soak fleet.
    table = node1.succeed(f"dmsetup table {LV}").strip().splitlines()
    assert len(table) == 1 and " linear " in f" {table[0]} ", f"the LV is not one linear segment: {table}"

    # ── AND THE RESOURCE ON TOP OF IT, from the same unit run above: it created the LV, created
    # metadata on it, attached, and -- being the seed -- declared it UpToDate and armed the
    # one-time format. One unit, which is what [V3b.33](d) bought.
    name_the_flock(node1)
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")
    # The resource attached to the LV and not to a disk -- if this ever reads /dev/vd*, the rest
    # of the test is measuring something else.
    node1.succeed(f"drbdsetup show r0 | grep -q '{LV}'")

    # THE WORKLOAD: real HA, installed onto the volume it now holds, through the product's converge.
    dataroot = install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=300)
    node1.wait_until_succeeds(f"test -f {dataroot}/app/home-assistant_v2.db", timeout=120)

    # A megabyte of data whose checksum has to survive both moves. HA's own recorder DB is the
    # realistic load; this is the deterministic witness beside it.
    node1.succeed("head -c 1048576 /dev/urandom >/var/lib/briard/.convert-token")
    node1.succeed("sync -f /var/lib/briard")
    witness = node1.succeed("sha256sum /var/lib/briard/.convert-token").split()[0]

    # ── ARM THE PROBERS, AND THE CLOCK THE NEGATIVES ARE READ FROM ─────────────────────────────
    node1.succeed("touch /run/briard-probe.run")
    node1.succeed("systemd-run --unit=convert-io-probe ${ioProbe}")
    node1.succeed("systemd-run --unit=convert-ha-probe ${haProbe}")
    node1.sleep(5)
    assert int(node1.succeed("wc -l </run/briard-probe.lat")) > 0, "the I/O prober wrote nothing"
    since = node1.succeed("date -u +'%Y-%m-%d %H:%M:%S'").strip()
    # THE NEGATIVES BELOW ARE NOT VACUOUS, and this is what proves it rather than asserting it:
    # the SAME grep over the SAME journal finds the transitions this node really did make at
    # bring-up. A pattern that can never match would pass every check in `undisturbed()` while the
    # volume burned, which is the failure mode a suite of negatives has.
    node1.succeed("journalctl -k | grep -q 'role( Secondary -> Primary )'")
    node1.succeed("journalctl -k | grep -q 'disk( Inconsistent -> UpToDate )'")

    print(f"cpu aes: {'yes' if node1.succeed('grep -c aes /proc/cpuinfo || true').strip() != '0' else 'no'}")

    # ── THE SHIPPED STATE IS ENCRYPTED ─────────────────────────────────────────────────────────
    # Nobody asked for this and nobody typed a passphrase: the seam unit formatted the disk LUKS2
    # because this CPU has AES, and opened it from a token that holds slot 0's passphrase in the
    # clear. The honest claim is "ready to be armed", never "protected" -- so what is asserted is
    # the SHAPE (the data really goes through dm-crypt) and the AUDIT SURFACE (the briard-clear
    # token is what says this node is not yet armed), not a secrecy this state does not have.
    node1.succeed(f"cryptsetup isLuks {DATA_DISK}")
    deps = node1.succeed(f"dmsetup deps -o devname {LV}")
    assert CRYPT_NAME in deps, f"the shipped LV is not sitting on a crypt device: {deps}"
    token = node1.succeed(f"cryptsetup token export --token-id 0 {DATA_DISK}")
    assert '"type":"briard-clear"' in token.replace(" ", ""), f"no clear-key token: {token}"
    print(node1.succeed(f"cryptsetup luksDump {DATA_DISK} | grep -iE 'cipher|version|sector|Tokens'"))

    # AND IT IS ACTUALLY OPAQUE, which is the one claim a green boot does not make on its own: the
    # btrfs superblock is findable through the crypt device and NOT on the raw disk under it. The
    # positive half is what keeps this from passing over a search that never worked.
    assert has_btrfs_magic(node1, LV), "no btrfs through the crypt device -- the search is broken"
    assert not has_btrfs_magic(node1, DATA_DISK), "the volume is READABLE on the raw disk"
    print("the data volume is opaque on the raw disk and readable through the crypt device")

    # ── DISARM: LUKS → PLAINTEXT, LIVE ─────────────────────────────────────────────────────────
    # This direction first, because it is the one that decides whether the default is a default or
    # a one-way door. Encryption on by default is defensible only if a household can get back off
    # it without an outage; that it can be put ON is proven by the second move below.
    hotplug(node1, "plain1", TARGET_MB)
    node1.succeed(f"pvcreate -ff -y {PLAIN1}")
    node1.succeed(f"vgextend {VG} {PLAIN1}")
    t0, t1 = move(node1, CRYPT, PLAIN1, "disarm")
    node1.succeed(f"vgreduce {VG} {CRYPT}")
    node1.succeed(f"pvremove -ff -y {CRYPT}")
    node1.succeed(f"cryptsetup close {CRYPT_NAME}")
    unplug(node1, "plain0")  # the shipped disk itself: a disarm frees the encrypted backing

    deps = node1.succeed(f"dmsetup deps -o devname {LV}")
    assert CRYPT_NAME not in deps, f"the crypt device is still under the LV: {deps}"
    assert has_btrfs_magic(node1, PLAIN1), "disarmed, but the volume is not readable in the clear"
    stall(node1, t0, t1, "disarm")
    undisturbed(node1, since, "disarm")
    assert witness == node1.succeed("sha256sum /var/lib/briard/.convert-token").split()[0]

    # ── RE-ARM: PLAINTEXT → LUKS, LIVE ─────────────────────────────────────────────────────────
    # The same move the other way, onto a header this rig makes itself. v5 owns the arming VERB;
    # what this proves is the only thing the verb will need from the substrate.
    hotplug(node1, "enc1", TARGET_MB)
    node1.succeed("head -c 32 /dev/urandom >/run/briard-rearm.key")
    node1.succeed(f"cryptsetup luksFormat --batch-mode --type luks2 {ENC1} /run/briard-rearm.key")
    node1.succeed(f"cryptsetup open --key-file /run/briard-rearm.key {ENC1} {CRYPT_NAME}")
    node1.succeed(f"pvcreate -ff -y {CRYPT}")
    node1.succeed(f"vgextend {VG} {CRYPT}")
    t0, t1 = move(node1, PLAIN1, CRYPT, "rearm")
    node1.succeed(f"vgreduce {VG} {PLAIN1}")
    node1.succeed(f"pvremove -ff -y {PLAIN1}")
    unplug(node1, "plain1")

    deps = node1.succeed(f"dmsetup deps -o devname {LV}")
    assert CRYPT_NAME in deps, f"the LV is not back on a crypt device: {deps}"
    assert not has_btrfs_magic(node1, ENC1), "re-armed, but the volume is READABLE on the raw disk"
    stall(node1, t0, t1, "rearm")

    # ── STOP PROBING, THEN JUDGE ───────────────────────────────────────────────────────────────
    node1.succeed("rm /run/briard-probe.run")
    node1.wait_until_fails("systemctl is-active convert-io-probe.service", timeout=60)
    node1.wait_until_fails("systemctl is-active convert-ha-probe.service", timeout=60)

    undisturbed(node1, since, "rearm")
    assert witness == node1.succeed("sha256sum /var/lib/briard/.convert-token").split()[0], \
        "the witness file did not survive the round trip"

    # HA is not merely still answering -- it is still the same HA, with the recorder DB it wrote
    # before the volume moved underneath it twice.
    node1.succeed(f"test -f {dataroot}/app/home-assistant_v2.db")
    node1.succeed(f"test -d {dataroot}/app/.storage")
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")
    node1.succeed("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json")

    # And the seam is exactly what it was: one linear segment, on one PV, under the same name.
    table = node1.succeed(f"dmsetup table {LV}").strip().splitlines()
    assert len(table) == 1 and " linear " in f" {table[0]} ", f"the LV is not one linear segment: {table}"
    # `pvs` names the mapper path, not the /dev/dm-N it resolves to -- compare what LVM says.
    pvs = node1.succeed(f"pvs --noheadings -o pv_name --select vg_name={VG}").split()
    assert pvs == [CRYPT], f"the VG is not on the single encrypted PV it was moved back to: {pvs}"
  '';
}
