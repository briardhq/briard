# AN APP'S EDGE: its previous version to the candidate, through the real agent, the way a
# household takes the update. One test for every catalogued app, built from two of its version
# files (catalog-fixture.nix `edge`): install the previous version, write a marker into its data,
# upgrade to the candidate through the real health gate, check the marker, then `briard app undo`
# to the point the upgrade took and check that the data is the data from before it.
#
# What it proves is the release, not Briard: the platform's own upgrade and undo are proven on
# the dummy elsewhere (agent-install-undo, hass-upgrade); here the subject is whether THIS
# upstream version comes up healthy over THAT one's data, and whether going back still works.
# A household that skipped releases takes an untested jump; upstream's own migrations are built
# for that, and the free catalog accepts it.
#
# FAILABLE BOTH WAYS. The version a node runs is read from the agent's own status line (the
# accepted manifest's identity, and no `!unhealthy` after it), never from what the test asked
# for; the marker written after the upgrade must be GONE after the undo, so a pass cannot come
# from an undo that put nothing back. `app-edge-dummy-bad` is this test over the dummy's
# poisoning version: red by design, it is the proof the gate's verdict reaches the test.
{
  pkgs,
  guestDisk,
  agent,
  netWrap,
  dressBase,
  # A fixture whose base is the previous version and whose variant `next` is the candidate.
  fixture,
}:
let
  # Room for the guest's image store and the data, measured from what the entry says it holds.
  diskMB = 10240 + 3 * (fixture.installedSize / 1048576);
in
pkgs.testers.runNixOSTest {
  name = "app-edge-${fixture.appName}";
  skipTypeCheck = true;

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 6144;
      virtualisation.cores = 4;
      virtualisation.diskSize = diskMB;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ];
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.darkhttpd pkgs.socat ];

      systemd.services.briard-agent = {
        description = "Briard host agent (an app's edge)";
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
          CATALOG_URL = "http://127.0.0.1:8098/latest.json";
          UPDATE_KEYRING = "/srv/catalog/keyring.pem";
        };
      };
    };

  testScript = ''
    import re

    svc = "${fixture.appName}"
    data = f"/var/lib/briard/data/{svc}/${fixture.dataDir}"
    prev_id = open("${fixture}/catalog/${fixture.appName}.identity").read().strip()
    next_id = open("${fixture}/variants/next/catalog/${fixture.appName}.identity").read().strip()

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
        host.succeed(f"mkdir -p /srv/catalog && cp -rf {src}/. /srv/catalog/ && chmod -R u+w /srv/catalog")
    publish("${fixture}/catalog")
    host.succeed("systemd-run --unit=catalog --collect darkhttpd /srv/catalog --addr 127.0.0.1 --port 8098")
    host.wait_until_succeeds("curl -fsS http://127.0.0.1:8098/latest.json -o /dev/null", timeout=30)

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

    def running(identity, timeout=300):
        """The agent's newest status line names this app at this identity, not unhealthy. The
        journal is read whole before it is searched (a `| grep -q` pipe fails under pipefail)."""
        host.wait_until_succeeds(
            "journalctl -u briard-agent -o cat > /tmp/j.txt && grep 'status node=' /tmp/j.txt | tail -1 > /tmp/s.txt "
            f"&& grep -qE ' apps=([^ ]*,)?{svc}@{identity}[, ]' /tmp/s.txt",
            timeout=timeout,
        )

    def markers():
        """Which of the two markers the app's data holds, read in one shell."""
        out = guest(f"echo B\"\"EFORE=$(cat {data}/.briard-edge-before 2>/dev/null) A\"\"FTER=$(cat {data}/.briard-edge-after 2>/dev/null) E\"\"ND")
        m = re.search(r"BEFORE=(\S*) AFTER=(\S*) END", out)
        assert m, f"no answer from the guest:\n{out}"
        return m.group(1), m.group(2)

    def install():
        rc, out = host.execute(f"briard-agent app install {svc} 2>&1")
        print(f"app install {svc}: rc={rc}\n{out}")
        if rc != 0:
            print(guest(f"systemctl status briard-stage --no-pager | head -5; journalctl -b --no-pager -u briard-stage -u briard-{svc}-app-image | tail -30", wait=10))
        return rc

    # BOTH VERSIONS' IMAGES ARE STAGED, loaded by the guest at boot beside everything else: an
    # install before the load is done finds no image and, with no registry here, cannot pull one.
    # Seconds for the dummy, minutes for two real images.
    for _ in range(120):
        if re.search(r"STAGED=yes", guest("echo S\"\"TAGED=$(test -s /run/briard/staged-images && echo yes)")):
            break
    else:
        print(guest("systemctl status briard-stage --no-pager | head -8; df -h / /var/tmp /var/lib/containers; free -m; dmesg | grep -i -E \"oom|killed\" | tail -5; journalctl -b --no-pager -u briard-stage | tail -15", wait=15))
        raise Exception("the guest never finished loading the staged images")

    # === THE PREVIOUS VERSION, installed and healthy, with data of its own. ===
    assert install() == 0, "installing the previous version failed"
    running(prev_id)
    guest(f"echo before > {data}/.briard-edge-before; sync -f /var/lib/briard; echo D\"\"ONE")
    assert markers() == ("before", ""), f"the marker did not land in the app's data: {markers()}"
    print(f"{svc}: the previous version runs ({prev_id})")

    # === THE CANDIDATE, through the real health gate. ===
    publish("${fixture}/variants/next/catalog")
    assert install() == 0, "the upgrade to the candidate did not pass its health gate"
    running(next_id)
    assert markers() == ("before", ""), f"the upgrade lost the previous version's data: {markers()}"
    guest(f"echo after > {data}/.briard-edge-after; sync -f /var/lib/briard; echo D\"\"ONE")
    assert markers() == ("before", "after"), f"the candidate's marker did not land: {markers()}"
    print(f"{svc}: the candidate runs over the previous version's data ({next_id})")

    # === AND BACK: the point the upgrade took, restored. ===
    history = host.succeed(f"briard-agent app history {svc}")
    print(history)
    points = re.findall(r"^\s+(\S*app-update-before\S*)\s*$", history, re.M)
    assert points, f"the upgrade left no point to undo to:\n{history}"
    rc, out = host.execute(f"briard-agent app undo {points[-1]} 2>&1")
    print(f"app undo {points[-1]}: rc={rc}\n{out}")
    assert rc == 0, "the undo failed"
    running(prev_id)
    assert markers() == ("before", ""), f"the undo did not put the previous data back: {markers()}"
    print(f"{svc}: undone to the previous version and its data")
  '';
}
