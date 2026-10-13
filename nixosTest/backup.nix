# THE NIGHTLY BACKUP, END TO END ON ONE NODE: the guest's own backup reads a real ring member off
# the replicated volume and writes it, through the real store, into a folder a person owns.
#
# The parts are unit-tested on both sides -- the guest's command lines against a fake executor,
# the store against the real restic client in a Go test. This is what only a real node can say:
# that a read-only btrfs member bind-mounts at its stable path, that the snapshot holds the data
# beside its manifest and the volume's facts, that HA's own tarballs stay out, that a root
# process leaves every file the folder's owner's, that the folder opens on its own as a standard
# repository, that the second night adds only what changed and restores what changed, and that
# an earlier household's nights in the same repository survive this one's retention -- and that a
# night comes back out of the repository as a read-only ring member holding that night's data,
# labelled with the manifest it was written under, which is what the host's restore then puts
# back exactly as it puts back any member -- and that a folder holding a backup under another key
# is named as such rather than written, and is this household's to write once that key has added
# ours.
#
# Agent-less, as every rig that runs a service is: `briard-guest-agent --backup` runs the guest's
# backup in line (the product starts it through the host's verb, in the background), and
# `backup-store` serves agent/host's store with no agent around it, on loopback. The 02:00
# schedule, the key's rules and the host's facts are unit-tested.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  store = pkgs.callPackage ./backup-store-pkg.nix { };
  node = h.mkNode {
    fixtures = [ fixture ];
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  live = "/var/lib/briard/${fixture.name}/${fixture.container}";
  folder = "/home/ana/Briard Backup";
  folder2 = "/home/ana/Earlier Backup";
  url = "http://127.0.0.1:7791/";
in
pkgs.testers.runNixOSTest {
  name = "backup";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      environment.systemPackages = [ pkgs.restic store pkgs.jq ];
      users.users.ana = { isNormalUser = true; };
    };

  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    import json
    import time

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    name_the_flock(node1)
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    install_fixture(node1)

    # The person's folder, made as them -- what the installer does -- and the store serving it.
    node1.succeed("su ana -c 'mkdir -p \"${folder}\"'")
    node1.succeed("systemd-run --unit=backup-store backup-store '${folder}' 127.0.0.1 127.0.0.1")
    node1.wait_for_open_port(7791)

    def sample(payload):
        # What the service wrote, and a member of the ring taken after it.
        node1.succeed(f"echo {payload} > ${live}/payload.txt")
        # A member's name carries its second, and two members in one second are unordered. The
        # app's start took one as it came up -- an instant ago, and empty. The product's clock can
        # never land beside it (it waits an hour from the ring's newest member); this in-line take
        # can, and the backup would then read "the newest member" off the alphabet. Let that
        # second pass: a take after a full second is in a later one.
        time.sleep(1)
        node1.succeed("briard-guest-agent --clock=${fixture.name}")

    def backup(ok=True):
        out = node1.succeed("briard-guest-agent --backup=${url} 2>/tmp/backup.log")
        print(node1.succeed("tail -5 /tmp/backup.log"))
        rep = json.loads(out.strip().splitlines()[-1])
        print(f"backup: {rep}")
        if not ok:
            return rep
        assert rep.get("snapshot") and not rep.get("error"), f"the backup did not complete: {rep}"
        assert rep.get("services") == ["${fixture.name}"], f"backed up {rep.get('services')}"
        return rep

    def plain(cmd, repo="'${folder}'"):
        # THE EXIT GUARANTEE: the folder on its own, opened with nothing but restic and the key --
        # the one the volume holds, which the dashboard shows the household.
        return node1.succeed(f"RESTIC_PASSWORD=\"$(cat /var/lib/briard/.backup-key)\" restic --no-cache -r {repo} {cmd}")

    # ---- the first night ----
    # WEIGHT, so the sizes below mean something: 4 MiB of the service's own data (random, so it
    # neither compresses nor dedups) and 8 MiB of tarball where Home Assistant keeps its own backups.
    node1.succeed("head -c 4M /dev/urandom > ${live}/big.bin")
    node1.succeed("mkdir -p ${live}/backups && head -c 8M /dev/urandom > ${live}/backups/ha.tar")
    sample("first")
    first = backup()
    # The key was minted on the volume by that first run, for the household alone.
    assert node1.succeed("stat -c %a /var/lib/briard/.backup-key").strip() == "600", "the key is not 0600"
    # Between the two: the service's data went in, and the tarball did not.
    assert 4 << 20 < first["bytesAdded"] < 8 << 20, f"the first night added {first['bytesAdded']} bytes"

    files = plain("ls latest").split()
    for want in [
        "/run/briard/backup/services/${fixture.name}/${fixture.container}/payload.txt",
        "/run/briard/backup/briard/members/${fixture.name}.json",
        "/run/briard/backup/briard/volume/.services/${fixture.name}.json",
    ]:
        assert want in files, f"{want} is not in the snapshot: {files}"
    assert not any("/app/backups" in f for f in files), "Home Assistant's own tarballs were backed up"
    assert not any("backup-key" in f for f in files), "the key went into the repository it opens"
    side = json.loads(plain("dump latest /run/briard/backup/briard/members/${fixture.name}.json"))
    assert side.get("manifest"), f"the member's sidecar carries no manifest: {side}"

    # A root process wrote all of it; the person owns all of it.
    strangers = node1.succeed("find '${folder}' ! -user ana -o ! -group users").strip()
    assert strangers == "", f"files the folder's owner does not own:\n{strangers}"
    assert int(node1.succeed("find '${folder}' -type f | wc -l")) > 0, "the folder is empty"
    # Whoever finds the folder is told what it is and how to restore it -- and the README beside
    # the repository did not stop plain restic opening it (the `ls` and `dump` above).
    node1.succeed("grep -q 'restic -r' '${folder}/README.txt'")

    # Nothing of the run is left behind: no mount, no copies (the host's secrets ride those), no password.
    node1.fail("findmnt /run/briard/backup/services/${fixture.name}")
    node1.fail("test -e /run/briard/backup/briard")
    node1.fail("test -e /run/briard/backup.password")

    # ---- the second night: only what changed, and it restores ----
    sample("second")
    second = backup()
    # The stable path is what lets restic find last night's snapshot as the parent: the 4 MiB is
    # neither re-read as new nor re-sent.
    assert second["bytesAdded"] < 1 << 20, f"the second night re-sent the unchanged data: {first} then {second}"
    assert len(json.loads(plain("snapshots --json"))) == 2, "two nights, not two snapshots"
    plain("restore latest --target /tmp/restored")
    got = node1.succeed("cat /tmp/restored/run/briard/backup/services/${fixture.name}/${fixture.container}/payload.txt").strip()
    assert got == "second", f"restored {got!r}, want the second night's"
    plain("check --read-data")

    # ---- an earlier household's nights are not this one's to prune ----
    # Three snapshots under another group, through the same store, as a previous install of this
    # folder would have left them; one more night here; the policy keeps 2 daily + 1 weekly of OUR
    # group and touches nothing of theirs.
    for n in range(3):
        node1.succeed(f"echo earlier-{n} > /tmp/earlier.txt")
        plain("backup --host earlier-house /tmp/earlier.txt", repo="rest:${url}")
    sample("third")
    backup()
    groups = {s["hostname"] for s in json.loads(plain("snapshots --json"))}
    assert groups == {"briard", "earlier-house"}, f"groups {groups}"
    earlier = json.loads(plain("snapshots --host earlier-house --json"))
    assert len(earlier) == 3, f"the earlier household kept {len(earlier)} of its 3 nights"
    ours = json.loads(plain("snapshots --host briard --json"))
    assert len(ours) == 2, f"our group holds {len(ours)}, want 2: three nights on one day, the policy applied to us"

    # ---- a night comes back as a ring member ----
    # The FIRST night, by id (it is the one the retention would prune next, so a stale id must
    # still name it while it is there), into the ring: a read-only subvolume with that night's
    # payload and the sidecar that names the manifest, beside the members the ring took itself.
    member = node1.succeed(f"briard-guest-agent --backup-restore=${url} -service ${fixture.name} -snapshot {first['snapshot']}").strip()
    assert member.startswith("/var/lib/briard/.snapshots/${fixture.name}-from-backup-"), f"member {member}"
    got = node1.succeed(f"cat {member}/${fixture.container}/payload.txt").strip()
    assert got == "first", f"the member holds {got!r}, want the first night's"
    node1.succeed(f"btrfs subvolume show {member} | grep -q 'readonly'")
    node1.fail(f"touch {member}/x")
    side = json.loads(node1.succeed(f"cat {member}.json"))
    assert side["trigger"] == "from-backup" and side["service"] == "${fixture.name}" and side["manifest"], f"sidecar {side}"
    assert not side.get("pending"), "a member from the backup is pending nothing"
    # The night that is not there is a refusal, and the ring is as it was.
    before = node1.succeed("ls /var/lib/briard/.snapshots").split()
    node1.fail(f"briard-guest-agent --backup-restore=${url} -service nothing-installed -snapshot {first['snapshot']}")
    assert node1.succeed("ls /var/lib/briard/.snapshots").split() == before, "a refused night changed the ring"
    node1.fail("test -e /run/briard/backup.password")

    # ---- a folder under another key: named, not written; then adopted ----
    # An earlier install's folder, as a reinstall finds it: a repository plain restic made under
    # a key this volume does not hold, served by the same store.
    node1.succeed("systemctl stop backup-store")
    node1.succeed("su ana -c 'mkdir -p \"${folder2}\"'")
    node1.succeed("su ana -c 'RESTIC_PASSWORD=earlier-key restic -q -r \"${folder2}\" init'")
    node1.succeed("systemd-run --unit=backup-store2 backup-store '${folder2}' 127.0.0.1 127.0.0.1")
    node1.wait_for_open_port(7791)
    refused = backup(ok=False)
    assert refused.get("wrongKey") and not refused.get("snapshot"), f"a foreign repository was not named: {refused}"
    assert len(node1.succeed("ls '${folder2}/snapshots' 2>/dev/null || true").split()) == 0, "a foreign repository was written"
    node1.fail("briard-guest-agent --backup-adopt=${url} -key not-the-key")
    node1.succeed("briard-guest-agent --backup-adopt=${url} -key earlier-key")
    node1.fail("test -e /run/briard/backup.adopt")
    adopted = backup()
    # Both keys open it now: ours (the night above wrote under it) and the earlier household's.
    assert len(json.loads(node1.succeed("RESTIC_PASSWORD=earlier-key restic --no-cache -r '${folder2}' snapshots --json"))) == 1, "the earlier key no longer opens the adopted folder"
    assert len(json.loads(plain("snapshots --json", repo="'${folder2}'"))) == 1, "the volume's key does not open the adopted folder"
  '';
}
