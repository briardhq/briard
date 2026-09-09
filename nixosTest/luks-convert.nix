# [V3b.33](a) — THE SPIKE: encryption is a `pvmove`, and it is reversible.
#
# The item it gates was admitted on a premise that turned out to be false — "format the volume
# encrypted at install, because you can never add it later". You can. What has to exist in
# advance is not the encryption, it is a SEAM: a single-LV VG between the disk and DRBD. With one
# there, arming a node is `pvmove` onto an encrypted PV and disarming it is the same move back,
# both with the workload serving throughout; without one there is no table to reload, and DRBD
# would have to close and reopen its backing device.
#
# So this rig proves the claim in the direction that actually matters — BOTH — on the shape a
# household runs: real Home Assistant, on a btrfs volume, over a promoted DRBD.
#
#   plaintext PV → (hotplug a blank disk, luksFormat, pvcreate, vgextend) → pvmove → vgreduce
#                → (device_del the old disk) → and then all the way back onto plaintext.
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

  # The backing disks, in MiB. The LV is deliberately SMALLER than the disk that holds it: a LUKS2
  # header is ~16 MiB off the front of the target and the PV label another 1 MiB, so an LV sized to
  # the whole plaintext disk could never land on an encrypted one of the same size. A household's
  # installer sizes the LV once, at install, under the same constraint.
  diskMB = 4096;
  lvMB = 3072;

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
    # THE SEAM, declared where DRBD reads it: the resource names the LV forever, and every move
    # below happens underneath that name. This is the whole now-decision the item turns on.
    resource = h.mkResource [
      {
        name = "node1";
        id = 0;
        disk = lvDev;
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
      # The data disk, resized from lib.nix's 256 MiB (a 4 GiB LV is what `install.sh` lays down by
      # default, and a 256 MiB move would measure nothing) and given a qdev id + a serial. Both are
      # load-bearing: the id is what `device_del` names when the conversion retires this disk, and
      # the serial is what makes `/dev/disk/by-id/virtio-plain0` a stable name in the guest rather
      # than a `/dev/vd?` letter that shifts as disks come and go.
      virtualisation.emptyDiskImages = lib.mkForce [
        {
          size = diskMB;
          driveConfig.deviceExtraOpts = {
            id = "plain0-dev";
            serial = "plain0";
          };
        }
      ];
      # The seam's userspace. Both are ALREADY in this guest's closure -- lvm2's `bin` output
      # arrives with the device-mapper udev rules every NixOS machine has, and cryptsetup's `lib`
      # output with systemd -- so what is added here is the PATH, plus cryptsetup's binary.
      environment.systemPackages = [
        pkgs.lvm2.bin
        pkgs.cryptsetup
      ];
    };

  # HA boot is slow and the promoter selects the primary dynamically.
  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}

    VG = "${vg}"
    LV = "${lvDev}"
    LV_MB = ${toString lvMB}
    DISK_MB = ${toString diskMB}
    PLAIN0 = "/dev/disk/by-id/virtio-plain0"
    ENC0 = "/dev/disk/by-id/virtio-enc0"
    PLAIN1 = "/dev/disk/by-id/virtio-plain1"
    CRYPT = "/dev/mapper/convert-enc0"


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


    def now(m):
        return int(m.succeed("date +%s%N"))


    def move(m, src, dst, label):
        """One `pvmove --atomic`, timed. Atomic so the LV is either wholly on the source or wholly
        on the target: an interrupted conversion has no third state to be recovered from."""
        t0 = now(m)
        m.succeed(f"pvmove --atomic -n data {src} {dst}", timeout=1800)
        t1 = now(m)
        secs = (t1 - t0) / 1e9
        print(f"[{label}] pvmove {src} -> {dst}: {secs:.1f}s for {LV_MB} MiB "
              f"({LV_MB / secs:.0f} MiB/s)")
        return t0, t1


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

    # ── THE SEAM, BUILT BEFORE DRBD EVER ATTACHES ──────────────────────────────────────────────
    # A single-LV VG is dm-linear and nothing else: one table line, the same target and the same
    # linear_map() a bare disk would have had. Everything LVM adds sits outside the data path.
    node1.succeed(f"pvcreate -ff -y {PLAIN0}")
    node1.succeed(f"vgcreate {VG} {PLAIN0}")
    node1.succeed(f"lvcreate -L {LV_MB}M -n data {VG}")
    node1.succeed(f"test -b {LV}")
    # The fence, asserted here because everything below depends on it: one linear segment, and no
    # other dm device in the way. It is what keeps the seam free.
    table = node1.succeed(f"dmsetup table {LV}").strip().splitlines()
    assert len(table) == 1 and " linear " in f" {table[0]} ", f"the LV is not one linear segment: {table}"

    # ⚠️ `dm-mirror` IS LOADED BY HAND, AND THAT IS A PRODUCT FINDING, NOT A RIG QUIRK. pvmove
    # builds its transient mirror out of the dm-mirror target, and LVM's own autoload cannot reach
    # it on a NixOS guest: lvm shells out to `/sbin/modprobe`, which does not exist here, so the
    # first `pvmove` fails with `Required device-mapper target(s) not detected in your kernel`
    # (measured -- everything up to it succeeded). Whatever drives a conversion on a real node has
    # to load this first. `dm-crypt` needs no such help: cryptsetup loads it through libdevmapper.
    node1.succeed("modprobe dm-mirror")

    # ── A NODE, THE WAY EVERY OTHER RIG BRINGS ONE UP ──────────────────────────────────────────
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")
    name_the_flock(node1)
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-data.service", timeout=120)
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
    token = node1.succeed("sha256sum /var/lib/briard/.convert-token").split()[0]

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

    # ── FORWARD: PLAINTEXT → LUKS, LIVE ────────────────────────────────────────────────────────
    hotplug(node1, "enc0", DISK_MB)
    node1.succeed("head -c 32 /dev/urandom >/run/briard-luks.key")
    node1.succeed(f"cryptsetup luksFormat --batch-mode --type luks2 {ENC0} /run/briard-luks.key")
    node1.succeed(f"cryptsetup open --key-file /run/briard-luks.key {ENC0} convert-enc0")
    node1.succeed(f"pvcreate -ff -y {CRYPT}")
    node1.succeed(f"vgextend {VG} {CRYPT}")
    t0, t1 = move(node1, PLAIN0, CRYPT, "encrypt")
    node1.succeed(f"vgreduce {VG} {PLAIN0}")
    node1.succeed(f"pvremove -ff -y {PLAIN0}")
    unplug(node1, "plain0")

    # The LV's data path now goes through dm-crypt, asserted from the DEVICE STACK rather than
    # from the fact that the commands above returned zero.
    deps = node1.succeed(f"dmsetup deps -o devname {LV}")
    assert "convert-enc0" in deps, f"the LV is not sitting on the crypt device: {deps}"
    node1.succeed("cryptsetup status convert-enc0 | grep -q 'type:.*LUKS2'")
    print(node1.succeed(f"cryptsetup luksDump {ENC0} | grep -iE 'cipher|version|sector'"))
    stall(node1, t0, t1, "encrypt")
    undisturbed(node1, since, "encrypt")
    assert token == node1.succeed("sha256sum /var/lib/briard/.convert-token").split()[0]

    # ── BACK: LUKS → PLAINTEXT, LIVE ───────────────────────────────────────────────────────────
    # The half that decides whether (c) is a commitment or a trap. If encryption can only be added,
    # then choosing it at install is irreversible for the installed base; because it can be taken
    # off the same way it went on, the default is a default and not a one-way door.
    hotplug(node1, "plain1", DISK_MB)
    node1.succeed(f"pvcreate -ff -y {PLAIN1}")
    node1.succeed(f"vgextend {VG} {PLAIN1}")
    t0, t1 = move(node1, CRYPT, PLAIN1, "decrypt")
    node1.succeed(f"vgreduce {VG} {CRYPT}")
    node1.succeed(f"pvremove -ff -y {CRYPT}")
    node1.succeed("cryptsetup close convert-enc0")
    unplug(node1, "enc0")

    deps = node1.succeed(f"dmsetup deps -o devname {LV}")
    assert "convert-enc0" not in deps, f"the crypt device is still under the LV: {deps}"
    assert not node1.succeed("dmsetup ls --target crypt").strip().startswith("convert-enc0")
    stall(node1, t0, t1, "decrypt")

    # ── STOP PROBING, THEN JUDGE ───────────────────────────────────────────────────────────────
    node1.succeed("rm /run/briard-probe.run")
    node1.wait_until_fails("systemctl is-active convert-io-probe.service", timeout=60)
    node1.wait_until_fails("systemctl is-active convert-ha-probe.service", timeout=60)

    undisturbed(node1, since, "decrypt")
    assert token == node1.succeed("sha256sum /var/lib/briard/.convert-token").split()[0], \
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
    pvs = node1.succeed(f"pvs --noheadings -o pv_name --select vg_name={VG}").split()
    assert pvs == [node1.succeed(f"readlink -f {PLAIN1}").strip()], \
        f"the VG is not on the single plaintext PV it was moved back to: {pvs}"
  '';
}
