# Off-site encrypted `.storage` backup, restore path exercised.
#
# The sacred half of a home's data (`.storage` config + YAML; loss = re-pair every
# device) gets no off-box copy from DRBD replication alone — both replicas share one
# fate. This proves the durability floor (the tarball half of DR; the full btrfs-stream cold-restore ladder is future work): a real HA
# home's `.storage` is sealed client-side (age) to an off-box target and restored
# byte-faithfully, with the sacred config intact and the disposable recorder DB excluded.
#
# Harness scope matches hass-upgrade*.nix: single-node lib.nix, the backup CLI drives
# shared/backup directly (the same code the guest agent's backup.save/backup.restore
# verbs run in the product — unit-tested in agent/guestagent). Restoring *over* a live
# `.storage` + rebooting HA onto it needs the payload stopped, which in a lib.nix rig
# makes the promoter demote+unmount (the maintenance-bracket hazard, host-orchestrated
# in the product) — so, as in hass-upgrade-rollback.nix, this restores into a clean tree and
# asserts byte-fidelity + content, the two things that need REAL HA `.storage`.
{ pkgs, guestModule, briardBackup }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  haDir = "/var/lib/briard/ha"; # the HA data subvolume (== /config in the container)
  storage = "${haDir}/.storage";
  db = "${haDir}/home-assistant_v2.db";
  # The off-box target: a tmpfs OUTSIDE the DRBD volume, standing in for a mounted NAS /
  # object store (a real backup must not share the volume it protects).
  offbox = "/var/offbox";
  restored = "/var/restored";
in
pkgs.testers.runNixOSTest {
  name = "hass-backup";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      virtualisation.memorySize = 4096;
      virtualisation.diskSize = 20480;
      environment.systemPackages = [ pkgs.sqlite pkgs.diffutils briardBackup ];
      # The off-box target on its own tmpfs — genuinely off the DRBD volume.
      virtualisation.fileSystems."${offbox}" = {
        device = "tmpfs";
        fsType = "tmpfs";
      };
    };

  skipTypeCheck = true;

  testScript = ''
    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-data.service", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")

    # HA boots and serves; the recorder DB + the sacred .storage are created.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=360)
    node1.wait_until_succeeds("test -d ${storage}", timeout=120)
    # Non-vacuous: wait for the sacred config-entries store (a recognizable named datum).
    node1.wait_until_succeeds("test -f ${storage}/core.config_entries", timeout=120)
    node1.wait_until_succeeds("test -f ${db}", timeout=60)
    n_files = int(node1.succeed("find ${storage} -type f | wc -l").strip())
    assert n_files > 0, "no .storage files to back up"
    print(f".storage has {n_files} files; recorder DB present")

    # ---- Mint a household keypair; the private identity stays home (a file), the public
    # recipient is all the backup needs to seal. ----
    node1.succeed("briard-backup keygen > /tmp/keys.txt")
    node1.succeed("grep -o 'age1[a-z0-9]*' /tmp/keys.txt > /tmp/recip.txt")
    node1.succeed("grep '^identity:' /tmp/keys.txt | sed 's/identity: //' > /tmp/id.txt")

    # ---- Back up the sacred config (.storage + configuration.yaml) to the off-box target ----
    node1.succeed(
        "briard-backup save --base ${haDir} --dest ${offbox}/home.age "
        "--recipient $(cat /tmp/recip.txt) --include .storage --include configuration.yaml"
    )
    node1.succeed("test -s ${offbox}/home.age")
    # The blob is encrypted: no plaintext config-entry data leaks, and it's an age file.
    node1.succeed("head -c 100 ${offbox}/home.age | grep -q 'age-encryption.org'")
    node1.fail("grep -qa 'config_entries' ${offbox}/home.age")
    print("sealed .storage -> encrypted off-box blob")

    # ---- Restore path: decrypt + extract into a clean tree with the household identity ----
    node1.succeed("rm -rf ${restored} && mkdir -p ${restored}")
    node1.succeed("briard-backup restore --base ${restored} --src ${offbox}/home.age --identity-file /tmp/id.txt")

    # ---- History-intact for the SACRED data: byte-identical .storage, disposable DB excluded ----
    # diff -r is non-vacuous: every file matches, or it fails naming the difference.
    node1.succeed("diff -r ${storage} ${restored}/.storage")
    node1.succeed("test -f ${restored}/configuration.yaml")
    # A specific named sacred datum survived (core.config_entries is HA's device/integration config).
    node1.succeed("cmp ${storage}/core.config_entries ${restored}/.storage/core.config_entries")
    # The disposable recorder DB was NOT in the backup (kept the archive small).
    node1.fail("test -e ${restored}/home-assistant_v2.db")

    # HA is still serving throughout — this backed up a LIVE home.
    node1.succeed("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json")
    n_restored = int(node1.succeed("find ${restored}/.storage -type f | wc -l").strip())
    assert n_restored == n_files, f"restored {n_restored} .storage files, backed up {n_files}"
    print(f"restored {n_restored} .storage files byte-identical; recorder DB excluded; HA still serving")
  '';
}
