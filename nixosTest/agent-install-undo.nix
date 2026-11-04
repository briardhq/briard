# An install INTERRUPTED INSIDE ITS HEALTH GATE is undone at the agent's next start, from the
# volume alone.
#
# An install has to put the new manifest on the volume before its gate decides, so for the
# gate's whole window the volume holds a version nothing has accepted. It holds it STAGED, beside
# the accepted manifest, with the rollback point that undoes its data (agent/guestagent/staging.go)
# -- so an installer that dies in that window leaves nothing that only its memory knew. This rig
# kills one there, with SIGKILL, on the worst shape there is: an upgrade to a version that has
# already POISONED the app's data and never becomes healthy. The agent that systemd starts in
# its place must find the staged install, put the data back from the recorded point, discard the
# stage and converge to the version the volume accepted -- the same undo a failed gate runs.
#
# FAILABLE BOTH WAYS. Before the kill the rig asserts the stage and the rollback point are on the
# volume and the data is poisoned, so a pass cannot come from an install that never got that far;
# after it, the poison must be gone, the stage gone, the accepted manifest the old version and the
# app healthy again.
#
# AND THE QUOTA TREE, on the same volume, because this is the rig where the real agent both
# installs an app and restores its data by swap. Every subvolume the product made must be born
# into the tree (the app's area in each tier, the area the undo put back), the
# volume must be in simple-quota mode, and a limit on the app's group must stop ROOT with full
# capabilities -- after the swap, which is when a limit held on the subvolume itself would
# have reset. Each assertion proves itself live: the write the limit stops goes through once
# `quota_override` is set, and the tree check flags a stray subvolume made without `-i`.
#
# AND THE OTHER TIERS, since the fixture mounts one of each: the undo empties the app's cache (a
# newer version's cache beside older data is how a downgrade breaks an app) and leaves its
# unversioned area; the Storage card counts the unversioned area as data and the cache apart; and
# the card's Clear empties the cache and starts the app again on the new one.
{ pkgs, guestDisk, agent, netWrap, dressBase, fixture }:
pkgs.testers.runNixOSTest {
  name = "agent-install-undo";
  skipTypeCheck = true;

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ];
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.darkhttpd pkgs.socat ];

      # A REAL systemd service, so SIGKILL is a crash systemd restarts -- the shape a power cut of
      # the agent alone takes, and the one an installed node is in.
      systemd.services.briard-agent = {
        description = "Briard host agent (install-undo proof)";
        wantedBy = [ ];
        path = [ pkgs.qemu pkgs.iproute2 pkgs.systemd ];
        serviceConfig = {
          ExecStart = "${agent}/bin/briard-agent run";
          Restart = "on-failure";
          RestartSec = 2;
        };
        environment = {
          QEMU = "${pkgs.qemu}/bin/qemu-system-x86_64";
          ACCEL = "kvm:tcg";
          GUEST_IMAGE = "/tmp/guest.qcow2";
          UPDATE_BASE = "/opt/briard/agent";
          DATA_DISK = "/tmp/data.img";
          STATE_DISK = "/tmp/state.img";
          CONTROL_SOCK = "/run/briard-ctl.sock";
          NODE = "guest";
          SYSTEM_TAP = "sys0";
          SYSTEM_DEV = "eth1";
          SYSTEM_CIDR = "10.0.0.1/24";
          SYSTEM_HOST_CIDR = "10.0.0.129/32";
          WITNESS_CIDR = "10.11.9.2/24";
          SERVICE_TAP = "svc0";
          WITNESS_TAP = "briard-priv0";
          VIP_DEV = "eth2";
          VIP_ADDR = "192.168.1.100/24";
          NET_MODE = "macvtap";
          NET_WRAP_BIN = "${netWrap}/bin/briard-net-wrap";
          STATUS_EVERY = "2s";
          GUEST_SERIAL = "/tmp/guest-serial.log";
          CATALOG_URL = "http://127.0.0.1:8098";
          UPDATE_KEYRING = "/srv/catalog/keyring.pem";
        };
      };
    };

  testScript = ''
    import re

    svc = "${fixture.serviceName}"
    poison = 999_999_999  # the dummy's BRIARD_BROKEN tick (nixosTest/dummy-app)

    host.wait_for_unit("multi-user.target")
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 3G /tmp/data.img")
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")

    def publish(src):
        host.succeed(f"mkdir -p /srv/catalog && cp -f {src}/* /srv/catalog/ && chmod -R u+w /srv/catalog")
    publish("${fixture}/catalog")
    host.succeed("systemd-run --unit=catalog --collect darkhttpd /srv/catalog --addr 127.0.0.1 --port 8098")
    host.wait_until_succeeds(f"curl -fsS http://127.0.0.1:8098/{svc}.json -o /dev/null", timeout=30)

    host.succeed("systemctl start briard-agent")
    try:
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
        host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    except Exception:
        print(host.succeed("tr -d '\\r' < /tmp/guest-serial.log | tail -80 || true"))
        raise

    def guest(line, wait=5):
        """One command line in the guest's root shell; its output, as the console relayed it."""
        host.succeed(f"(printf '\\n'; sleep 2; printf '%s\\n' '{line}'; sleep {wait}; printf '\\035') | briard-agent debug shell > /tmp/g.out 2>&1")
        return host.succeed("tr -d '\\r' < /tmp/g.out")

    def volume():
        """The volume's .apps listing and the app's tick, read in one shell."""
        # The console echoes the typed line, so each marker is split by a "" there and whole only in
        # the output: a regex can then match nothing but the answer.
        out = guest(f"echo L\"\"S=$(ls -1 /var/lib/briard/.apps | tr \"\\n\" \" \"); echo T\"\"ICKS=$(cat /var/lib/briard/data/{svc}/app/state.json 2>/dev/null | tr -dc 0-9)")
        ls = re.search(r"LS=(.*)", out)
        ticks = re.search(r"TICKS=(\d*)", out)
        assert ls and ticks, f"no answer from the guest:\n{out}"
        # Only what staging writes: the directory also holds other per-app markers (`.clean`).
        staging = [f for f in ls.group(1).split() if f.endswith((".json", ".json.next", ".rollback"))]
        return staging, int(ticks.group(1) or 0)

    # === THE GOOD VERSION, installed and committed: one accepted manifest, nothing staged. ===
    rc, out = host.execute(f"briard-agent app install {svc} 2>&1")
    print(f"install v0: rc={rc}\n{out}")
    assert rc == 0, f"installing the good version failed (rc={rc}):\n{out}"
    files, _ = volume()
    assert files == [f"{svc}.json"], f"after a committed install the volume holds {files}"

    # Let the good version write some ticks, so the data the undo must put back is not empty.
    def ticked():
        _, t = volume()
        return t
    for _ in range(12):
        if ticked() >= 2:
            break
    good = ticked()
    assert 0 < good < poison, f"the good version wrote no data to undo back to (tick={good})"

    # The live area's subvolume, so the quota section can tell the undo really swapped it.
    btrfs = "$(ls /nix/store/*-btrfs-progs-*/bin/btrfs | head -1)"
    def rootid(path):
        out = guest(f'echo R""ID=$({btrfs} inspect-internal rootid {path})')
        m = re.search(r"RID=(\d+)", out)
        assert m, f"no subvolume id for {path}:\n{out}"
        return int(m.group(1))
    before_swap = rootid(f"/var/lib/briard/data/{svc}")
    # What the undo must empty, and what it must leave: a cache the newer version would have
    # written, and 4 MiB in the unversioned area, which is never rolled back.
    cache, unversioned = f"/var/lib/briard/cache/{svc}", f"/var/lib/briard/unversioned/{svc}"
    guest(f"echo newer > {cache}/cache/stale; head -c 4194304 /dev/urandom > {unversioned}/keep/kept.bin; sync -f /var/lib/briard; echo D\"\"ONE")
    cache_before = rootid(cache)

    # === THE BROKEN UPGRADE, killed inside its gate. ===
    publish("${fixture}/variants/bad/catalog")
    # From here only: the good install already logged the same "waiting for health" line.
    before = host.succeed("journalctl -u briard-agent -n1 --show-cursor | sed -n 's/^-- cursor: //p'").strip()
    host.succeed(f"systemd-run --unit=inst --collect ${pkgs.bash}/bin/sh -c '${agent}/bin/briard-agent app install {svc} > /tmp/inst.out 2>&1'")
    try:
        host.wait_until_succeeds(f"journalctl -u briard-agent -o cat --after-cursor='{before}' | grep -q 'app install {svc}: converged from the volume; waiting for health'", timeout=600)
    except Exception:
        print(host.succeed("cat /tmp/inst.out || true"))
        raise
    for _ in range(12):
        files, ticks = volume()
        if ticks == poison:
            break
    print(f"inside the gate: volume {files}, tick {ticks}")
    assert f"{svc}.json.next" in files and f"{svc}.rollback" in files, \
        f"the install is not staged with its rollback point inside the gate: {files}"
    assert ticks == poison, f"the broken version did not poison the data (tick={ticks}); the undo proof would be vacuous"

    cursor = host.succeed("journalctl -u briard-agent -n1 --show-cursor | sed -n 's/^-- cursor: //p'").strip()
    host.succeed("systemctl kill -s KILL briard-agent")
    print("agent killed inside the gate")

    # === THE AGENT SYSTEMD STARTS IN ITS PLACE UNDOES IT. ===
    try:
        host.wait_until_succeeds(f"journalctl -u briard-agent -o cat --after-cursor='{cursor}' | grep -q 'undo {svc}: back on what the volume accepted'", timeout=600)
    finally:
        print(host.succeed(f"journalctl -u briard-agent -o cat --after-cursor='{cursor}' | grep -E 'undo|converge' || true"))
    files, ticks = volume()
    print(f"after the undo: volume {files}, tick {ticks}")
    assert files == [f"{svc}.json"], f"the undo left {files} on the volume"
    accepted = guest(f"echo BAD=$(grep -c 0.0.0-bad /var/lib/briard/.apps/{svc}.json) VER=$(grep -c 0.0.0 /var/lib/briard/.apps/{svc}.json)")
    assert re.search(r"BAD=0 VER=1", accepted), f"the accepted manifest is not the good version:\n{accepted}"
    assert ticks != poison and ticks >= good, f"the data was not put back (tick={ticks}, good was {good})"
    out = guest("curl -fsS -o /dev/null -w CODE=%{http_code} http://127.0.0.1:8080/healthz; echo", wait=15)
    assert "CODE=200" in out, f"the good version is not healthy after the undo:\n{out}"
    print("the interrupted install was undone from the volume alone")
    # The undo emptied the cache -- a new subvolume, holding the directory its container mounts and
    # nothing else -- and left the unversioned area as it was.
    out = guest(f'echo C""ACHE=$(ls -A {cache} | tr "\\n" ",")$(ls -A {cache}/cache | tr "\\n" ",") K""EPT=$(stat -c %s {unversioned}/keep/kept.bin)')
    assert re.search(r"CACHE=cache, KEPT=4194304", out), f"after the undo the cache and the unversioned area are:\n{out}"
    assert rootid(cache) != cache_before, "the undo did not make the cache again"
    print("the undo emptied the cache and kept the unversioned area")

    # === THE QUOTA TREE. ===
    import hashlib

    def group(tier, app):
        """The app's level-1 group: the tier in the top byte of 48 bits, then the first 40 bits of
        SHA-256 of its name. Written here a second time, independently of the agent's Go."""
        return f"1/{tier << 40 | int.from_bytes(hashlib.sha256(app.encode()).digest()[:5], 'big')}"

    def aggregate(tier):
        return f"2/{tier << 40}"

    uuid = "$(findmnt -no UUID /var/lib/briard)"
    out = guest(f'echo M""ODE=$(cat /sys/fs/btrfs/{uuid}/qgroups/mode)')
    assert re.search(r"MODE=squota", out), f"the volume is not in simple-quota mode:\n{out}"

    def tree():
        """`qgroup show -p` as {id: (exclusive bytes, [parents])}."""
        out = guest(f'echo Q""T=$({btrfs} qgroup show -p --raw /var/lib/briard | tr -s " " | cut -d " " -f1,3,4 | tr "\\n" ";")')
        m = re.search(r"QT=(.*)", out)
        assert m, f"no quota tree from the guest:\n{out}"
        groups = {}
        for row in m.group(1).split(";"):
            f = row.split()
            if len(f) == 3 and f[0][0].isdigit():
                groups[f[0]] = (int(f[1]), [] if f[2] == "-" else f[2].split(","))
        assert groups, f"the quota tree is empty:\n{out}"
        return groups

    def outside(groups):
        """The runtime check: a level-0 group other than the top level that owns bytes and has
        no parent is a subvolume no limit can see."""
        return [g for g, (excl, parents) in groups.items() if g.startswith("0/") and g != "0/5" and excl > 0 and not parents]

    groups = tree()
    print(f"the quota tree: {groups}")
    assert outside(groups) == [], f"subvolumes outside the tree: {outside(groups)}"
    data_group, files_group = group(1, svc), group(2, svc)
    live = rootid(f"/var/lib/briard/data/{svc}")
    assert live != before_swap, f"the undo did not swap the live area ({live}); the restore claim would be vacuous"
    for sub, want in [(f"/var/lib/briard/data/{svc}", data_group), (f"/var/lib/briard/files/{svc}", files_group),
                      (cache, data_group), (unversioned, data_group)]:
        sid = f"0/{rootid(sub)}"
        assert groups.get(sid, (0, []))[1] == [want], f"{sub} ({sid}) is in {groups.get(sid)}, want {want}"
    assert groups[data_group][1] == [aggregate(1)], f"{data_group} is under {groups[data_group][1]}"
    assert groups[files_group][1] == [aggregate(2)], f"{files_group} is under {groups[files_group][1]}"

    # === THE FILES TIER'S TICK: the allocation rule's cap, the valve, the warning. ===
    # The real agent's tick, which runs at its start and every five minutes after; a restart is
    # how this rig asks for one now. The volume is 3 GB, so the rule leaves a cap a test can fill.
    def last_tick(after, want, timeout=900):
        """The agent's latest `files tier:` line after the cursor, once one matching `want` exists."""
        host.wait_until_succeeds(f"journalctl -u briard-agent -o cat --after-cursor='{after}' | grep 'files tier: cap=' | grep -q '{want}'", timeout=timeout)
        return host.succeed(f"journalctl -u briard-agent -o cat --after-cursor='{after}' | grep 'files tier: cap=' | grep '{want}' | tail -1")

    def restart_for_a_tick():
        c = host.succeed("journalctl -u briard-agent -n1 --show-cursor | sed -n 's/^-- cursor: //p'").strip()
        host.succeed("systemctl restart briard-agent")
        host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=600)
        return c

    def limit_of(gid):
        out = guest(f'echo L""IM=$({btrfs} qgroup show -pr --raw /var/lib/briard | grep -a "^{gid} " | tr -s " " | cut -d " " -f4)')
        m = re.search(r"LIM=(\S+)", out)
        assert m, f"no limit read for {gid}:\n{out}"
        return m.group(1)

    # The first tick after the undo's restart: the area's first member, and every limit set.
    line = last_tick(cursor, "cap=")
    print(f"the first tick: {line}")
    cap = int(re.search(r"cap=(\d+)", line).group(1))
    target = int(re.search(r"target=(\d+)", line).group(1))
    slack = int(re.search(r"slack=(\d+)", line).group(1))
    # The rule, written a second time from btrfs's own numbers: the slack is what is on the disk
    # past what the tree charges (the groups with no parent), and one metadata chunk in every
    # copy. The data tier moves a little between the tick and now.
    out = guest(f'echo U""SE=$({btrfs} filesystem usage -b /var/lib/briard | grep -E "Device size:|^[[:space:]]+Used:|Metadata ratio:" | tr -s " \\t" " " | tr "\\n" ";")')
    m = re.search(r"USE= ?Device size: (\d+); Used: (\d+); Metadata ratio: ([\d.]+);", out)
    assert m, f"no usage from the guest:\n{out}"
    volume, on_disk, copies = int(m.group(1)), int(m.group(2)), float(m.group(3))
    groups = tree()
    charged = sum(excl for excl, parents in groups.values() if not parents)
    chunk = min(1 << 30 if volume > 50 << 30 else 256 << 20, volume // 10)
    want_slack = max(on_disk - charged, 0) + int(chunk * copies)
    files_used = groups[aggregate(2)][0]
    data_used = charged - files_used
    expected = volume - data_used - max(data_used // 5, 300_000_000) - want_slack
    print(f"volume {volume}, on the disk {on_disk}, charged {charged}, slack {want_slack} (the tick's {slack})")
    assert abs(slack - want_slack) < 64 * 1024 * 1024, f"slack {slack}, btrfs's numbers give {want_slack}"
    assert abs(cap - expected) < 64 * 1024 * 1024, f"cap {cap}, the rule gives {expected} (volume {volume}, data {data_used})"
    assert target == max(1_000_000_000, cap // 20), f"target {target} for cap {cap}"
    assert limit_of(aggregate(2)) == str(cap), f"the files tier's limit is {limit_of(aggregate(2))}, the tick's cap {cap}"
    # Each app's share: the volume's physical free space for its data group, the files tier's
    # for its files group, less a tenth or the tier's keep.
    def app_cap(total, keep):
        return max(total * 9 // 10, total - keep, 0)
    for gid, free, keep in [(data_group, volume - want_slack - charged, max(300_000_000, volume // 100)),
                            (files_group, cap - files_used, max(300_000_000, cap // 100))]:
        got, want = limit_of(gid), app_cap(groups[gid][0] + free, keep)
        assert got != "none" and abs(int(got) - want) < 64 * 1024 * 1024, f"{gid}'s limit is {got}, the rule gives {want}"
    assert "short=false" in line, f"a fresh install is short: {line}"
    assert "alert open files-storage-short" not in host.succeed("journalctl -u briard-agent -o cat"), "a fresh install warned"

    # === WHAT THE APP STORES, as its page's Storage card asks it: the real agent relays it, the
    # guest walks the real volume with compsize. 32 MiB of text in the live area first, which zstd
    # shrinks many times over: du would call it 32 MiB, and on the disk it is a fraction of that.
    # Beside it the 4 MiB kept in the unversioned area, counted as data, and 2 MiB in the cache,
    # said apart and in neither the data nor its history.
    import json, time
    text = f"/var/lib/briard/data/{svc}/storage-text"
    guest(f'yes "the storage card reads the extent tree" | head -c 33554432 > {text}; head -c 2097152 /dev/urandom > {cache}/cache/blob; sync -f {text}; echo D""ONE', wait=8)
    def usage():
        out = host.succeed(f"""echo '{{"id":"u","kind":"app-usage","payload":"{svc}"}}' | socat -t30 - UNIX-CONNECT:/run/briard/admin.sock""")
        o = json.loads(out)
        assert o["state"] == "done", f"app-usage: {o}"
        return json.loads(o["detail"])
    u = usage()
    assert u.get("measuring") and not u["data"].get("measured"), f"the first ask did not start a walk: {u}"
    for _ in range(60):
        u = usage()
        if not u.get("measuring"):
            break
        time.sleep(2)
    print(f"what {svc} stores: {u}")
    dat, fil, cac = u["data"], u["files"], u.get("cache", {})
    assert dat.get("measured") and fil.get("measured") and cac.get("measured"), f"the walk did not finish: {u}"
    assert dat["uncompressed"] >= 36 << 20 and 4 << 20 <= dat["current"] < dat["uncompressed"] // 4, \
        f"the current data is {dat['current']} on disk for {dat['uncompressed']} before compression; 32 MiB of text should shrink many times over, beside 4 MiB kept unversioned"
    assert 2 << 20 <= cac["current"] < 3 << 20, f"the cache holds 2 MiB; the card says {cac}"
    assert dat["history"] == max(dat["total"] - dat["current"] - cac["current"], 0), \
        f"the history is not the group less the current data and the cache: {dat}, {cac}"
    groups = tree()
    for t, gid in [(dat, data_group), (fil, files_group)]:
        assert abs(t["total"] - groups[gid][0]) < 16 << 20, f"{gid} holds {groups[gid][0]}, the card says {t['total']}"
        assert t["cap"] == int(limit_of(gid)), f"{gid}'s limit is {limit_of(gid)}, the card says {t['cap']}"
    out = guest(f'echo M""EM=$(ls -1 /var/lib/briard/data/.snapshots | grep "^{svc}-" | grep -vc "\\.json$") U""MEM=$(ls -1 /var/lib/briard/files/.snapshots | grep "^{svc}-" | grep -vc "\\.json$")')
    m = re.search(r"MEM=(\d+) UMEM=(\d+)", out)
    assert m and (dat["members"], fil["members"]) == (int(m.group(1)), int(m.group(2))), f"the card counts {dat['members']}/{fil['members']} snapshots:\n{out}"
    guest(f"rm -f {text}; sync -f /var/lib/briard; echo D\"\"ONE")

    # THE CARD'S CLEAR, through the real agent: the app stopped, its cache emptied and made again,
    # the app started again on it -- a new start of its container, whose bind source exists or
    # podman would refuse it, serving again.
    def started():
        out = guest(f'echo S""TART=$(systemctl show -p ActiveEnterTimestampMonotonic --value briard-{svc}-${fixture.container}.service)')
        m = re.search(r"START=(\d+)", out)
        assert m, f"no start time for the app's container:\n{out}"
        return int(m.group(1))
    first_start = started()
    out = host.succeed(f"""echo '{{"id":"c","kind":"app-cache-empty","payload":"{svc}"}}' | socat -t120 - UNIX-CONNECT:/run/briard/admin.sock""")
    assert json.loads(out)["state"] == "done", f"app-cache-empty: {out}"
    out = guest(f'echo C""ACHE=$(ls -A {cache}/cache | tr "\\n" ",")E K""EPT=$(stat -c %s {unversioned}/keep/kept.bin)')
    assert re.search(r"CACHE=E KEPT=4194304", out), f"after the Clear the cache and the unversioned area are:\n{out}"
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    assert started() > first_start, "the Clear did not start the app's container again"
    print("the Clear emptied the cache and started the app again on it")

    # A limit on the app's group stops root, on the area the swap put back. 4 MiB of headroom
    # over what the group holds now, and a 16 MiB write; the tick owns the limit again at its next run.
    limit = tree()[data_group][0] + 4 * 1024 * 1024
    fill = f"/var/lib/briard/data/{svc}/quota-fill"
    def write():
        out = guest(f'rm -f {fill}; echo D""Q=$(dd if=/dev/urandom of={fill} bs=1M count=16 conv=fsync 2>&1 | grep -c "Disk quota exceeded") S""Z=$(stat -c %s {fill})', wait=8)
        m = re.search(r"DQ=(\d+) SZ=(\d+)", out)
        assert m, f"no answer from the write:\n{out}"
        return int(m.group(1)), int(m.group(2))
    guest(f"{btrfs} qgroup limit {limit} {data_group} /var/lib/briard")
    refused, landed = write()
    print(f"under the limit: EDQUOT={refused}, {landed} bytes landed")
    assert refused >= 1 and landed < 8 * 1024 * 1024, f"root wrote past the limit (EDQUOT={refused}, {landed} bytes)"
    guest(f"echo 1 > /sys/fs/btrfs/{uuid}/quota_override")
    refused, landed = write()
    guest(f"echo 0 > /sys/fs/btrfs/{uuid}/quota_override; rm -f {fill}; {btrfs} qgroup limit none {data_group} /var/lib/briard")
    print(f"with quota_override: EDQUOT={refused}, {landed} bytes landed")
    assert refused == 0 and landed == 16 * 1024 * 1024, f"the override did not let the write through: the limit assertion proves nothing (EDQUOT={refused}, {landed} bytes)"

    # And the tree check is live: a subvolume made without -i is the one it names.
    guest(f"{btrfs} subvolume create /var/lib/briard/data/.snapshots/quota-stray && head -c 1048576 /dev/urandom > /var/lib/briard/data/.snapshots/quota-stray/x && sync -f /var/lib/briard")
    stray = f"0/{rootid('/var/lib/briard/data/.snapshots/quota-stray')}"
    flagged = outside(tree())
    guest(f"{btrfs} subvolume delete /var/lib/briard/data/.snapshots/quota-stray")
    assert flagged == [stray], f"the tree check flagged {flagged}, want exactly the stray {stray}"
    print("every subvolume the product made is in the quota tree, and its limit binds root")

    # History to shed: a member ten days old and one two hours old, each pinning its own bytes,
    # and the household's files on top -- together past the room the target leaves, and under it
    # once the old member is gone.
    area, snaps = f"/var/lib/briard/files/{svc}", "/var/lib/briard/files/.snapshots"
    room = (cap - target) // 1_000_000
    def plant(age, mb):
        out = guest(f'head -c {mb}M /dev/urandom > {area}/plant && S={snaps}/{svc}-clock-$(date -u -d "-{age}" +%Y%m%dT%H%M%SZ) && {btrfs} subvolume snapshot -r -i {files_group} {area} $S >/dev/null && echo "{{}}" > $S.json && rm {area}/plant && sync -f /var/lib/briard && echo P""M=$S', wait=45)
        m = re.search(r"PM=(\S+)", out)
        assert m, f"could not plant a member:\n{out}"
        return m.group(1)
    old = plant("10 days", room * 6 // 10)
    young = plant("2 hours", room * 2 // 10)
    guest(f"head -c {room * 3 // 10}M /dev/urandom > {area}/files && sync -f /var/lib/briard", wait=45)
    line = last_tick(restart_for_a_tick(), "shed=1")
    print(f"the valve's tick: {line}")
    there = guest(f"test -e {old} && echo O""LD=1 || echo O""LD=0; test -e {young} && echo Y""OUNG=1 || echo Y""OUNG=0")
    assert "OLD=0" in there and "YOUNG=1" in there, f"the valve did not shed the old member alone:\n{there}"
    assert "short=false" in line, f"ten days of history were enough, and the tick says short: {line}"

    # At the floor: the household's files fill the cap. The uploader meets EDQUOT at its app's
    # share -- the tier's last tenth or keep is left for other apps, so the tier is not at zero --
    # while the data tier keeps writing, and the next tick has only the day's members: it sheds
    # none of them and warns.
    out = guest(f'echo D""Q=$(head -c {cap // 1_000_000}M /dev/urandom 2>/dev/null > {area}/more; echo $?) S""VC=$(head -c 32M /dev/urandom > /var/lib/briard/data/{svc}/svc-fill && sync -f /var/lib/briard && rm /var/lib/briard/data/{svc}/svc-fill && echo ok)', wait=90)
    print(f"at the floor: {out}")
    assert re.search(r"DQ=[1-9]", out), f"the uploader wrote past the cap:\n{out}"
    assert "SVC=ok" in out, f"the data tier could not write while the files tier was full:\n{out}"
    after = restart_for_a_tick()
    line = last_tick(after, "short=true")
    print(f"the floor's tick: {line}")
    assert "shed=0" in line, f"the valve shed a member younger than a day: {line}"
    left = int(re.search(r"free=(-?\d+)", line).group(1))
    assert left > 50_000_000, f"the one app took the files tier to {left} bytes: its share did not stop it"
    assert "YOUNG=1" in guest(f"test -e {young} && echo Y""OUNG=1"), "the day's member is gone"
    host.wait_until_succeeds(f"journalctl -u briard-agent -o cat --after-cursor='{after}' | grep -q 'alert open files-storage-short'", timeout=60)
    print("the files tier is capped by the rule, its valve sheds the oldest history, and at the floor it warns")
  '';
}
