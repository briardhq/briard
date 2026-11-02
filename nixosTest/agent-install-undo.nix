# An install INTERRUPTED INSIDE ITS HEALTH GATE is undone at the agent's next start, from the
# volume alone.
#
# An install has to put the new manifest on the volume before its gate decides, so for the
# gate's whole window the volume holds a version nothing has accepted. It holds it STAGED, beside
# the accepted manifest, with the rollback point that undoes its data (agent/guestagent/staging.go)
# -- so an installer that dies in that window leaves nothing that only its memory knew. This rig
# kills one there, with SIGKILL, on the worst shape there is: an upgrade to a version that has
# already POISONED the service's data and never becomes healthy. The agent that systemd starts in
# its place must find the staged install, put the data back from the recorded point, discard the
# stage and converge to the version the volume accepted -- the same undo a failed gate runs.
#
# FAILABLE BOTH WAYS. Before the kill the rig asserts the stage and the rollback point are on the
# volume and the data is poisoned, so a pass cannot come from an install that never got that far;
# after it, the poison must be gone, the stage gone, the accepted manifest the old version and the
# service healthy again.
#
# AND THE QUOTA TREE, on the same volume, because this is the rig where the real agent both
# installs an app and restores its data by swap. Every subvolume the product made must be born
# into the tree (the app's service area, its user area, the area the undo put back), the
# volume must be in simple-quota mode, and a limit on the app's group must stop ROOT with full
# capabilities -- after the swap, which is when a limit held on the subvolume itself would
# have reset. Each assertion proves itself live: the write the limit stops goes through once
# `quota_override` is set, and the tree check flags a stray subvolume made without `-i`.
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
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.darkhttpd ];

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
    poison = 999_999_999  # the dummy's BRIARD_BROKEN tick (nixosTest/dummy-service)

    host.wait_for_unit("multi-user.target")
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")
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
        """The volume's .services listing and the service's tick, read in one shell."""
        # The console echoes the typed line, so each marker is split by a "" there and whole only in
        # the output: a regex can then match nothing but the answer.
        out = guest(f"echo L\"\"S=$(ls -1 /var/lib/briard/.services | tr \"\\n\" \" \"); echo T\"\"ICKS=$(cat /var/lib/briard/{svc}/app/state.json 2>/dev/null | tr -dc 0-9)")
        ls = re.search(r"LS=(.*)", out)
        ticks = re.search(r"TICKS=(\d*)", out)
        assert ls and ticks, f"no answer from the guest:\n{out}"
        # Only what staging writes: the directory also holds other per-service markers (`.clean`).
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
    before_swap = rootid(f"/var/lib/briard/{svc}")

    # === THE BROKEN UPGRADE, killed inside its gate. ===
    publish("${fixture}/variants/bad/catalog")
    # From here only: the good install already logged the same "waiting for health" line.
    before = host.succeed("journalctl -u briard-agent -n1 --show-cursor | sed -n 's/^-- cursor: //p'").strip()
    host.succeed(f"systemd-run --unit=inst --collect ${pkgs.bash}/bin/sh -c '${agent}/bin/briard-agent app install {svc} > /tmp/inst.out 2>&1'")
    try:
        host.wait_until_succeeds(f"journalctl -u briard-agent -o cat --after-cursor='{before}' | grep -q 'service install {svc}: converged from the volume; waiting for health'", timeout=600)
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
    accepted = guest(f"echo BAD=$(grep -c 0.0.0-bad /var/lib/briard/.services/{svc}.json) VER=$(grep -c 0.0.0 /var/lib/briard/.services/{svc}.json)")
    assert re.search(r"BAD=0 VER=1", accepted), f"the accepted manifest is not the good version:\n{accepted}"
    assert ticks != poison and ticks >= good, f"the data was not put back (tick={ticks}, good was {good})"
    out = guest("curl -fsS -o /dev/null -w CODE=%{http_code} http://127.0.0.1:8080/healthz; echo", wait=15)
    assert "CODE=200" in out, f"the good version is not healthy after the undo:\n{out}"
    print("the interrupted install was undone from the volume alone")

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
    svc_group, user_group = group(1, svc), group(2, svc)
    live = rootid(f"/var/lib/briard/{svc}")
    assert live != before_swap, f"the undo did not swap the live area ({live}); the restore claim would be vacuous"
    for sub, want in [(f"/var/lib/briard/{svc}", svc_group), (f"/var/lib/briard/user/{svc}", user_group)]:
        sid = f"0/{rootid(sub)}"
        assert groups.get(sid, (0, []))[1] == [want], f"{sub} ({sid}) is in {groups.get(sid)}, want {want}"
    assert groups[svc_group][1] == [aggregate(1)], f"{svc_group} is under {groups[svc_group][1]}"
    assert groups[user_group][1] == [aggregate(2)], f"{user_group} is under {groups[user_group][1]}"

    # A limit on the app's group stops root, on the area the swap put back. 4 MiB of headroom
    # over what the group holds now, and a 16 MiB write.
    limit = groups[svc_group][0] + 4 * 1024 * 1024
    fill = f"/var/lib/briard/{svc}/quota-fill"
    def write():
        out = guest(f'rm -f {fill}; echo D""Q=$(dd if=/dev/urandom of={fill} bs=1M count=16 conv=fsync 2>&1 | grep -c "Disk quota exceeded") S""Z=$(stat -c %s {fill})', wait=8)
        m = re.search(r"DQ=(\d+) SZ=(\d+)", out)
        assert m, f"no answer from the write:\n{out}"
        return int(m.group(1)), int(m.group(2))
    guest(f"{btrfs} qgroup limit {limit} {svc_group} /var/lib/briard")
    refused, landed = write()
    print(f"under the limit: EDQUOT={refused}, {landed} bytes landed")
    assert refused >= 1 and landed < 8 * 1024 * 1024, f"root wrote past the limit (EDQUOT={refused}, {landed} bytes)"
    guest(f"echo 1 > /sys/fs/btrfs/{uuid}/quota_override")
    refused, landed = write()
    guest(f"echo 0 > /sys/fs/btrfs/{uuid}/quota_override; rm -f {fill}; {btrfs} qgroup limit none {svc_group} /var/lib/briard")
    print(f"with quota_override: EDQUOT={refused}, {landed} bytes landed")
    assert refused == 0 and landed == 16 * 1024 * 1024, f"the override did not let the write through: the limit assertion proves nothing (EDQUOT={refused}, {landed} bytes)"

    # And the tree check is live: a subvolume made without -i is the one it names.
    guest(f"{btrfs} subvolume create /var/lib/briard/.snapshots/quota-stray && head -c 1048576 /dev/urandom > /var/lib/briard/.snapshots/quota-stray/x && sync -f /var/lib/briard")
    stray = f"0/{rootid('/var/lib/briard/.snapshots/quota-stray')}"
    flagged = outside(tree())
    guest(f"{btrfs} subvolume delete /var/lib/briard/.snapshots/quota-stray")
    assert flagged == [stray], f"the tree check flagged {flagged}, want exactly the stray {stray}"
    print("every subvolume the product made is in the quota tree, and its limit binds root")
  '';
}
