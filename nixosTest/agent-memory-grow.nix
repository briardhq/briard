# The host agent GROWS its guest's memory on its own when the guest runs short.
#
# The real product agent boots the real guest disk at 1 GB under nested QEMU, launched with room
# to grow (-m 1024,slots=…,maxmem=…). The test then makes the guest short of memory the one way
# that cannot be argued with -- incompressible data in a tmpfs, which is neither page cache nor
# anything zram can shrink -- and does nothing else. What must follow is the agent's own decision:
# MemAvailable below the floor for the level window, one DIMM hot-added over QMP, and the guest's
# kernel counting the memory (its MemTotal, as the resources verb reports it in the host's status
# line). Growth lasts until the next launch, which boots back at 1 GB -- by construction, since
# the launch reads the configured size alone; the unit tests hold that line.
{ pkgs, guestDisk, agent, netWrap, dressBase }:
let
  # Typed into the guest's root shell (the debug console), one line at a time. It leaves ~150 MB
  # available -- under the 256 MB floor -- and prints what it found, as HOG=<MB available before>.
  hog = pkgs.writeText "hog.sh" ''
    mkdir -p /run/hog && mount -t tmpfs -o size=2G hog /run/hog
    a=$(awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo); head -c $((a - 150))M /dev/urandom > /run/hog/f; echo HOG=$a
  '';
in
pkgs.testers.runNixOSTest {
  name = "agent-memory-grow";
  skipTypeCheck = true;

  nodes.host =
    { ... }:
    {
      # 4 GB of host: the 2 GB reserve leaves the guest a ceiling of just under 2 GB, so a 1 GB
      # guest has room for one step and not two.
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ];
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];
    };

  testScript = ''
    import re

    host.wait_for_unit("multi-user.target")
    # The shipped NIC contract, as agent-bringup lays it down (see there for why all three).
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")
    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        "--setenv=UPDATE_BASE=/opt/briard/agent "
        "--setenv=GUEST_IMAGE=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img --setenv=STATE_DISK=/tmp/state.img "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest --setenv=GUEST_SERIAL=/tmp/guest-console.log "
        "--setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 "
        "--setenv=STATUS_EVERY=2s "
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "--setenv=MEMORY_MB=1024 "
        "${agent}/bin/briard-agent run"
    )
    try:
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    except Exception:
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -80 || true"))
        raise

    # Launched with room to grow: the boot size, then slots and the address space above it.
    argv = host.succeed("tr '\\0' ' ' < /proc/$(pgrep -f guest.qcow2 | head -1)/cmdline")
    m = re.search(r"-m (\S+)", argv)
    assert m and re.fullmatch(r"1024,slots=\d+,maxmem=\d+M", m.group(1)), f"-m is {m and m.group(1)!r}: no room to grow"
    print(f"launched with -m {m.group(1)}")

    # The newest status line's ` mem=<avail>/<total>k`: the guest's own numbers, via the verb.
    last_mem = "journalctl -u briard-agent -o cat | grep -o ' mem=[0-9]*/[0-9]*' | tail -1 | cut -d= -f2"
    def mem_mb():
        avail, total = host.succeed(last_mem).strip().split("/")
        return int(avail) >> 10, int(total) >> 10

    host.wait_until_succeeds(f"test -n \"$({last_mem})\" && test \"$({last_mem} | cut -d/ -f2)\" -gt 0", timeout=60)
    avail0, total0 = mem_mb()
    print(f"before: available {avail0} MB of {total0} MB")

    host.succeed("(printf '\\n'; sleep 2; cat ${hog}; sleep 40; printf '\\035') | briard-agent debug shell > /tmp/hog.out 2>&1")
    hog_out = host.succeed("cat /tmp/hog.out")
    assert re.search(r"HOG=\d+", hog_out), f"the guest was not made short of memory:\n{hog_out}"
    # Short, by the agent's own reading -- under the 256 MB floor. The agent samples the guest once
    # a minute (resourcesEvery), so each wait on its reading allows two samples.
    host.wait_until_succeeds(f"test \"$({last_mem} | cut -d/ -f1)\" -lt {256 << 10}", timeout=120)
    print(f"short: available {mem_mb()[0]} MB")

    # The agent's own decision, after the level window: one step, logged -- its size read off the VM.
    host.wait_until_succeeds("journalctl -u briard-agent -o cat | grep -q 'memory: grew the guest to 1536 MB'", timeout=600)
    print(host.succeed("journalctl -u briard-agent -o cat | grep 'memory: '"))

    # ...and the guest's kernel counts it: MemTotal up by (nearly exactly) one step.
    host.wait_until_succeeds(f"test \"$({last_mem} | cut -d/ -f2)\" -ge {(total0 + 500) << 10}", timeout=120)
    avail1, total1 = mem_mb()
    print(f"after: available {avail1} MB of {total1} MB (was {avail0} of {total0})")
  '';
}
