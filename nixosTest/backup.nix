# THE NIGHTLY BACKUP, END TO END ON ONE NODE: the guest's own backup reads a real ring member off
# the replicated volume and writes it, through the real store, into a folder a person owns.
#
# The parts are unit-tested on both sides -- the guest's command lines against a fake executor,
# the store against the real restic client in a Go test. This is what only a real node can say:
# that a read-only btrfs member bind-mounts at its stable path, that the snapshot holds the data
# beside its manifest and the volume's facts, that HA's own tarballs stay out, that a root
# process leaves every file the folder's owner's, that the folder opens on its own as a standard
# repository, and that the second night adds only what changed and restores what changed.
#
# Agent-less, as every rig that runs a service is: `briard-guest-agent --backup` runs the guest's
# backup in line (the product starts it through the host's verb, in the background), and
# `backup-store` serves agent/host's store with no agent around it, on loopback. The 02:00
# schedule, the key and the host's facts are unit-tested.
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
        node1.succeed("briard-guest-agent --clock=${fixture.name}")

    def backup():
        out = node1.succeed("BRIARD_BACKUP_PASSWORD=correct-horse briard-guest-agent --backup=${url} 2>/tmp/backup.log")
        print(node1.succeed("tail -5 /tmp/backup.log"))
        rep = json.loads(out.strip().splitlines()[-1])
        print(f"backup: {rep}")
        assert rep.get("snapshot") and not rep.get("error"), f"the backup did not complete: {rep}"
        assert rep.get("services") == ["${fixture.name}"], f"backed up {rep.get('services')}"
        return rep

    def plain(cmd):
        # THE EXIT GUARANTEE: the folder on its own, opened with nothing but restic and the key.
        return node1.succeed(f"RESTIC_PASSWORD=correct-horse restic --no-cache -r '${folder}' {cmd}")

    # ---- the first night ----
    # WEIGHT, so the sizes below mean something: 4 MiB of the service's own data (random, so it
    # neither compresses nor dedups) and 8 MiB of tarball where Home Assistant keeps its own backups.
    node1.succeed("head -c 4M /dev/urandom > ${live}/big.bin")
    node1.succeed("mkdir -p ${live}/backups && head -c 8M /dev/urandom > ${live}/backups/ha.tar")
    sample("first")
    first = backup()
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
  '';
}
