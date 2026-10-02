# THE RECORDER CHECK AGAINST A REAL HOME ASSISTANT: latent damage in the history database is found
# before Home Assistant finds it, and the newest copy that checks clean is put back.
#
# Home Assistant sets a damaged recorder database aside and starts an empty one the first time it
# reads the bad page, silently, and its only routine full read is the monthly repack. The product's
# answer (agent/hass/dbcheck.go) is unit-tested against fakes; this is the part only a real Home
# Assistant can say: that `quick_check` in a throwaway container of HA's own image opens a real
# read-only member (`immutable=1`) and sees real damage, that the restore's staged copy swaps in
# with RENAME_EXCHANGE on a real btrfs subvolume, and that Home Assistant comes back on the copy.
#
# THE SCENARIOS, in order on one Home Assistant:
#   1. A fresh install's first check is clean, which is the floor every later search stops at.
#   2. History is written, and a clock sample taken minutes later holds it -- a sample NOBODY
#      HOLDS, since the last hold is minutes old. The floor predates the history. A household edit
#      (scripts.yaml) follows, so the next sample's evaluation puts a "changed" event on the fresh
#      copy: that event is what keeps it. An unheld sample that anchors nothing is replaced by
#      the next one, so the unheld copies a search can find are the ones the History keeps.
#   3. One leaf page of `states`, from the middle of its tree, is overwritten with Home Assistant
#      stopped. It starts again and runs: the damage is latent, as it is in a household.
#   4. The check finds the damage in the newest member and names the fresh unheld copy, not the
#      older floor. The history being back after the restore is what proves which one it chose.
#   5. Something that is not a subvolume sits where the restore stages its copy: the restore
#      refuses, and Home Assistant is running again on its own data with no History row claiming
#      a restore.
#   6. The restore: Home Assistant is healthy on the copy, the copy passes quick_check, the
#      history row from before the damage is there, and the undo point holds the damaged database.
#   7. Home Assistant's own full read (recorder.purge with repack) on the checked copy: no reset.
#   8. The same read on the UNCHECKED damaged copy, put back by hand: Home Assistant sets it aside
#      and starts empty. This is why the check must run before the copy is restored.
#
# Agent-less, like every rig that runs Home Assistant: the harness flags `--dbcheck` and
# `--dbrestore` run the guest's own check and restore in line, as `--clock` runs the clock
# sample. The 05:30 schedule, the background runner and the host's polling are unit-tested.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  helper = ./recorder-check.py;
  node = h.mkNode {
    fixtures = [ fixture ];
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  root = "/var/lib/briard/${fixture.name}";
  live = "${root}/${fixture.container}";
  db = "${live}/home-assistant_v2.db";
  ring = "/var/lib/briard/.snapshots";
in
pkgs.testers.runNixOSTest {
  name = "hass-recorder-check";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      environment.systemPackages = [ pkgs.python3 ];
      # hass-payload's measured 2048, plus room for the throwaway check containers running
      # beside Home Assistant.
      virtualisation.memorySize = 3072;
      virtualisation.diskSize = 10240;
    };

  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    import json
    import time

    helper = "python3 ${helper}"

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    name_the_flock(node1)
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    node1.wait_until_succeeds("test -S /run/briard/agent.sock", timeout=60)

    dataroot = install_fixture(node1)
    assert dataroot == "${root}", f"the renderer chose {dataroot}"
    units = fixture_units(node1)

    def healthy():
        node1.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:8123/manifest.json", timeout=360)
        # Through the token, which is the API a household's history is written by.
        node1.wait_until_succeeds(f"{helper} set sensor.b31_alive {time.time()}", timeout=180)

    def restart():
        # The way converge bounces a service: stop the container, start the units in order.
        node1.succeed(f"systemctl stop {units[-1]}")
        for u in units:
            node1.succeed(f"systemctl start {u}")

    def members(trigger):
        return node1.succeed(
            f"ls -1 ${ring} | grep '^${fixture.name}-{trigger}-' | grep -v '[.]json$' || true"
        ).split()

    def app_state(member):
        raw = node1.succeed(f"cat ${ring}/{member}.app.json 2>/dev/null || true").strip()
        return json.loads(raw).get("hass-db", {}).get("state", "") if raw else ""

    def row(path, snap, entity, state):
        return node1.succeed(f"{helper} row {path} {snap} {entity} {state}").strip()

    def quick(path, snap):
        return node1.succeed(f"{helper} quick {path} {snap}").strip()

    def corrupt_files():
        return node1.succeed("ls -1 ${live} | grep '[.]corrupt[.]' || true").split()

    def dbcheck():
        t0 = time.time()
        out = node1.succeed("briard-guest-agent --dbcheck 2>/tmp/dbcheck.log")
        rep = json.loads(out.strip().splitlines()[-1])
        print(f"dbcheck took {time.time() - t0:.1f}s: {rep}")
        print(node1.succeed("tail -20 /tmp/dbcheck.log"))
        return rep

    def wait_row(entity, state):
        node1.wait_until_succeeds(f"test -n \"$({helper} row ${db} 0 {entity} {state})\"", timeout=120)

    healthy()
    node1.wait_until_succeeds("test -f ${db}", timeout=60)

    # ---- 1. THE FLOOR: a fresh install's first check is clean ----
    # This is also the first of the measurements: the check opens a REAL read-only member
    # with immutable=1, through podman, with Home Assistant's own sqlite. A verdict at all is that.
    rep = dbcheck()
    assert rep["verdict"] == "clean", f"the first check of a fresh install is not clean: {rep}"
    floor = rep["checked"].rsplit("/", 1)[-1]
    assert app_state(floor) == "clean", f"the checked member {floor} is not held clean"

    # ---- 2. HISTORY, and a fresh sample nobody holds ----
    node1.succeed(f"{helper} seed 3000", timeout=900)
    node1.succeed(f"{helper} set sensor.b31_named before-damage")
    wait_row("sensor.b31_named", "before-damage")
    named = row("${db}", 0, "sensor.b31_named", "before-damage")
    # The clock sample needs the briard integration's quiesce view, which comes up after the API
    # does (hass-payload records the trap).
    node1.wait_until_succeeds(f"{helper} ready", timeout=120)
    node1.succeed("briard-guest-agent --clock=${fixture.name}")
    fresh = members("clock")[-1]
    meta = json.loads(node1.succeed(f"cat ${ring}/{fresh}.json"))
    assert meta["consistency"] == "quiesced", f"the clock sample is {meta['consistency']}; the check reads only quiesced copies"
    assert app_state(fresh) == "", f"{fresh} is held ({app_state(fresh)}); the scenario needs a copy nobody holds"
    # NON-VACUITY: the fresh copy holds the row and the floor does not, so the row being back
    # after the restore says which copy was restored.
    assert row(f"${ring}/{fresh}/${fixture.container}/home-assistant_v2.db", 1, "sensor.b31_named", "before-damage") == named
    assert row(f"${ring}/{floor}/${fixture.container}/home-assistant_v2.db", 1, "sensor.b31_named", "before-damage") == "", (
        "the floor already holds the row, so restoring it would pass this test too"
    )

    # A household edit after the fresh copy. The next sample is compared with the fresh one, and the
    # "changed" event lands on the fresh one: the reason it is still in the ring for the search.
    node1.succeed("printf 'b31_script:\n  sequence: []\n' >> ${live}/scripts.yaml")

    # ---- 3. LATENT DAMAGE: one leaf page of `states`, with Home Assistant stopped ----
    node1.succeed(f"systemctl stop {units[-1]}")
    node1.succeed("test ! -s ${db}-wal")  # a clean stop checkpoints: the main file is the database
    page, size = node1.succeed(f"{helper} leaf ${db}").split()
    node1.succeed(f"dd if=/dev/urandom of=${db} bs={size} seek={int(page) - 1} count=1 conv=notrunc")
    assert quick("${db}", 0) != "ok", "the overwritten page did not damage the database"
    print(f"overwrote page {page} of `states` ({size} bytes)")
    for u in units:
        node1.succeed(f"systemctl start {u}")
    healthy()
    assert corrupt_files() == [], (
        f"Home Assistant read the damaged page at start and reset ({corrupt_files()}); pick a page it does not read"
    )

    # The damaged start's evaluation, which compares it with the fresh copy, has landed.
    node1.wait_until_succeeds(f"grep -q '\"kind\":\"changed\"' ${ring}/{fresh}.json", timeout=420)
    assert app_state(fresh) == "", "the fresh copy picked up a hold; it is meant to be kept by its event alone"

    # ---- 4. THE CHECK finds it, and names the fresh unheld copy over the held floor ----
    rep = dbcheck()
    assert rep["verdict"] == "corrupt", f"the check missed the damage: {rep}"
    assert rep.get("candidate", "").endswith("/" + fresh), (
        f"the search named {rep.get('candidate')!r}, want the newest clean copy {fresh} (the floor is {floor})"
    )
    cand = rep["candidate"]
    assert app_state(fresh) == "clean", "the chosen copy is not held clean for the restore"

    # ---- 5. A RESTORE THAT CANNOT STAGE changes nothing, and Home Assistant runs again ----
    node1.succeed("mkdir ${root}.dbrestore")
    node1.fail(f"briard-guest-agent --dbrestore={cand} 2>/tmp/refused.log")
    print(node1.succeed("tail -5 /tmp/refused.log"))
    node1.succeed("grep -q 'nothing was changed' /tmp/refused.log")
    for u in units:
        node1.succeed(f"systemctl is-active {u}")
    healthy()
    assert quick("${db}", 0) != "ok", "the live database changed on a refused restore"
    assert members("hass-db-restore-before") == [], "a refused restore left a History row saying it restored"
    node1.succeed("rmdir ${root}.dbrestore")

    # ---- 6. THE RESTORE ----
    node1.succeed(f"briard-guest-agent --dbrestore={cand}")
    healthy()
    assert quick("${db}", 0) == "ok", f"the restored database does not pass quick_check: {quick('${db}', 0)}"
    assert row("${db}", 0, "sensor.b31_named", "before-damage") == named, "the history from before the damage is not back"
    node1.succeed("btrfs subvolume show ${root} >/dev/null")
    node1.succeed("test ! -e ${root}.dbrestore")
    undo = members("hass-db-restore-before")
    assert len(undo) == 1, f"undo points: {undo}"
    undo = undo[0]
    umeta = json.loads(node1.succeed(f"cat ${ring}/{undo}.json"))
    kinds = [r["kind"] for r in (umeta.get("event") or {}).get("reasons", [])]
    assert "hass-db-restore" in kinds, f"the undo point carries no restore event: {umeta.get('event')}"
    undodb = f"${ring}/{undo}/${fixture.container}/home-assistant_v2.db"
    assert quick(undodb, 1) != "ok", "the undo point does not hold the damaged database it would put back"

    # ---- 7. HOME ASSISTANT'S FULL READ on the checked copy: nothing is reset ----
    # The purge task and the state written after it run in order on the recorder's thread, so the
    # state landing means the repack has finished.
    node1.succeed(f"{helper} repack")
    node1.succeed(f"{helper} set sensor.b31_marker after-clean-repack")
    wait_row("sensor.b31_marker", "after-clean-repack")
    assert corrupt_files() == [], f"the full read reset a copy that checked clean: {corrupt_files()}"
    assert row("${db}", 0, "sensor.b31_named", "before-damage") == named, "the history did not survive the full read"

    # ---- 8. THE SAME READ on the unchecked damaged copy: Home Assistant resets it ----
    node1.succeed(f"systemctl stop {units[-1]}")
    node1.succeed(f"cp {undodb} ${db} && rm -f ${db}-wal ${db}-shm")
    for u in units:
        node1.succeed(f"systemctl start {u}")
    healthy()
    assert corrupt_files() == [], "the damaged copy was reset at start; the read below would prove nothing"
    node1.succeed(f"{helper} repack")
    node1.succeed(f"{helper} set sensor.b31_marker after-damaged-repack")
    node1.wait_until_succeeds("ls -1 ${live} | grep -q '^home-assistant_v2[.]db[.]corrupt[.]'", timeout=300)
    wait_row("sensor.b31_marker", "after-damaged-repack")
    assert row("${db}", 0, "sensor.b31_named", "before-damage") == "", "Home Assistant kept the history it set aside"
    print(f"the unchecked copy was set aside on the full read: {corrupt_files()}")
  '';
}
