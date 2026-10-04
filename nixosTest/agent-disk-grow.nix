# The host agent GROWS its guest's state disk when an install needs the space -- and refuses the
# install, touching nothing, when the host cannot pay for it.
#
# The state disk starts THICK and small (1 GiB): the host pays for what a service brings when the
# service arrives, never underneath a running guest. The fixture here declares sizes no fresh disk
# holds (2 GB installed, 0.5 GB downloading), so installing it must run the whole grow: the file
# extended thick, QEMU's block_resize, the guest's resize2fs online -- and only then the pull.
#
# THE FAILABLE CONTROL COMES FIRST. With the host's own disk filled until it cannot pay for the
# grow and keep its reserve, the install must be refused by name and the disk left exactly as it
# was; a grow that ignored the host, or an install that ignored the grow, fails here. Then the
# filler goes and the same install must succeed, with the file grown, every byte of it allocated,
# and the guest's kernel and filesystem both seeing the new size.
{ pkgs, guestDisk, agent, netWrap, dressBase, fixture }:
pkgs.testers.runNixOSTest {
  name = "agent-disk-grow";
  skipTypeCheck = true;

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 12288; # the data + state disks live here, and the state disk grows
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ];
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.darkhttpd ];
    };

  testScript = ''
    import re

    gib = 1 << 30
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
    # The fixture's signed catalog, served the way the lab serves its own.
    host.succeed("systemd-run --unit=catalog --collect darkhttpd ${fixture}/catalog --addr 127.0.0.1 --port 8098")
    host.wait_until_succeeds("curl -fsS http://127.0.0.1:8098/${fixture.serviceName}.json -o /dev/null", timeout=30)
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
        "--setenv=CATALOG_URL=http://127.0.0.1:8098 --setenv=UPDATE_KEYRING=${fixture}/catalog/keyring.pem "
        "${agent}/bin/briard-agent run"
    )
    try:
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
        host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    except Exception:
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -80 || true"))
        raise

    def size():
        return int(host.succeed("stat -c %s /tmp/state.img").strip())

    def allocated():
        b, unit = host.succeed("stat -c '%b %B' /tmp/state.img").split()
        return int(b) * int(unit)

    # THICK FROM THE START: the agent made it at 1 GiB with every byte reserved.
    size0 = size()
    assert size0 == gib, f"the state disk was made at {size0} bytes, not 1 GiB"
    assert allocated() >= size0, f"the fresh state disk is sparse: {allocated()} of {size0} bytes allocated"

    # === THE CONTROL: a host that cannot pay refuses the install and grows nothing. ===
    # Fill the host's filesystem until it has 3 GiB free (Bfree, what the agent reads): the grow
    # this install needs is 3 GiB, plus the 2 GiB the host keeps for itself.
    f, s = host.succeed("stat -f -c '%f %S' /tmp").split()
    free_now = int(f) * int(s)
    host.succeed(f"fallocate -l {free_now - 3 * gib} /tmp/filler")
    rc, out = host.execute("briard-agent app install ${fixture.serviceName} 2>&1")
    print(f"install with a full host: rc={rc}\n{out}")
    assert rc != 0 and "not enough free space on this computer" in out, \
        f"an install the host could not pay for was not refused by name (rc={rc}):\n{out}"
    assert size() == size0, f"a refused install still grew the state disk to {size()}"
    host.succeed("rm /tmp/filler")

    # === THE GROW: the same install, with room on the host. ===
    rc, out = host.execute("briard-agent app install ${fixture.serviceName} 2>&1")
    print(f"install: rc={rc}\n{out}")
    print(host.succeed("journalctl -u briard-agent -o cat | grep -E 'state disk|service install' || true"))
    assert rc == 0, f"the install failed after the grow (rc={rc}):\n{out}"
    host.succeed("journalctl -u briard-agent -o cat | grep -q 'state disk: grown from 1.07 GB to'")
    size1 = size()
    assert size1 > size0 and size1 % gib == 0, f"the state disk is {size1} bytes after the grow"
    assert allocated() >= size1, \
        f"the grown range is sparse: {allocated()} of {size1} bytes allocated -- the host was not charged for it"
    print(f"state disk: {size0 >> 20} MiB -> {size1 >> 20} MiB, all of it allocated")

    # The guest sees it: the kernel's size of the disk is the file's, and the filesystem fills it.
    host.succeed(
        "(printf '\\n'; sleep 2; "
        "printf 'echo DEV=$(blockdev --getsize64 /dev/disk/by-id/virtio-briard-state) FS=$(df -B1 --output=size /briard-state | tail -1)\\n'; "
        "sleep 3; printf '\\035') | briard-agent debug shell > /tmp/sizes.out 2>&1"
    )
    sizes = host.succeed("tr -d '\\r' < /tmp/sizes.out")
    m = re.search(r"DEV=(\d+) FS=\s*(\d+)", sizes)
    assert m, f"no sizes from the guest:\n{sizes}"
    dev, fs = int(m.group(1)), int(m.group(2))
    assert dev == size1, f"the guest's kernel sees {dev} bytes, the file is {size1}"
    assert fs > size1 * 9 // 10, f"the guest's filesystem is {fs} bytes on a {size1}-byte disk: it was not grown into it"
    print(f"guest: disk {dev}, filesystem {fs}")
  '';
}
