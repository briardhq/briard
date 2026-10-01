# A LEAK STAYS IN ITS OWN SERVICE.
#
# A service that declares a minimum memory gets a container limit of quadlet.MemoryLimitFactor
# times it (and its minimum in swap). The renderer's unit test proves the quadlet SOURCE says so;
# this proves the rest of the chain on a booted guest: quadlet passes the lines through into the
# generated systemd unit, the kernel enforces them on that unit's whole cgroup, and a process that
# grows past the limit is killed by a CGROUP out-of-memory -- the kernel names the two differently
# -- while the service and everything beside it carry on.
#
# The leak is a process run inside the unit's cgroup rather than inside the container: the fixture
# image holds nothing but its own binary, so there is nothing to `podman exec`. A child cgroup of
# the unit is what a container's own processes live under too, and the unit's limit covers its
# whole subtree, so the charge lands exactly where a leaking service's would. `tail /dev/zero` is
# the leak: one endless line, buffered without bound.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # 16 MB declared -> MemoryMax 64 MB, MemorySwapMax 16 MB: small enough that the leak meets the
  # service's limit long before it could meet the guest's.
  fixture = import ./fixture-service.nix { inherit pkgs; minMemoryMB = 16; };
  node = h.mkNode {
    inherit fixture;
    replicated = false;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  unit = "briard-dummy-app.service";
in
pkgs.testers.runNixOSTest {
  name = "service-memory-limit";
  nodes.node1 = {
    imports = [ node ];
    # THE PRODUCT'S VALUE, which the test framework overrides. nixpkgs' test instrumentation sets
    # vm.panic_on_oom = mkDefault 2 so a test dies loudly on any OOM -- and mode 2 panics on a
    # CGROUP OOM too, which turns the containment under test into a guest crash (measured: the
    # first run's leak met the service's limit and the kernel panicked). The shipped guest sets
    # nothing, so it runs the kernel default 0, where a cgroup OOM kills inside the cgroup.
    boot.kernel.sysctl."vm.panic_on_oom" = 0;
  };

  testScript = ''
    ${h.fixtureHelpers}
    unit = "${unit}"

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service")
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    # 1. The GENERATED unit carries the limits: quadlet passed the [Service] lines through.
    mem = node1.succeed(f"systemctl show -p MemoryMax --value {unit}").strip()
    swap = node1.succeed(f"systemctl show -p MemorySwapMax --value {unit}").strip()
    assert mem == str(64 << 20), f"{unit} MemoryMax={mem}, want 64 MB (4 x the declared 16)"
    assert swap == str(16 << 20), f"{unit} MemorySwapMax={swap}, want 16 MB (the declared minimum)"
    cg = node1.succeed(f"systemctl show -p ControlGroup --value {unit}").strip()
    main_pid = node1.succeed(f"systemctl show -p MainPID --value {unit}").strip()
    print(f"{unit}: MemoryMax={mem} MemorySwapMax={swap} cgroup={cg} main={main_pid}")

    # 2. The leak, inside the unit's cgroup: a shell moves itself into a child of it, then becomes
    # the leak. Nothing below waits on it except its own death.
    pom = node1.succeed("sysctl -n vm.panic_on_oom").strip()
    assert pom == "0", f"vm.panic_on_oom={pom}: the rig is not the product's, and a cgroup OOM would panic the guest"
    node1.succeed(f"mkdir -p /sys/fs/cgroup{cg}/leak")
    node1.succeed(
        f"setsid sh -c 'echo $$ > /sys/fs/cgroup{cg}/leak/cgroup.procs && echo $$ > /tmp/leak.pid && exec tail /dev/zero' "
        "</dev/null >/dev/null 2>&1 &"
    )
    node1.wait_until_succeeds("test -s /tmp/leak.pid", timeout=10)
    leak = node1.succeed("cat /tmp/leak.pid").strip()
    node1.wait_until_fails(f"kill -0 {leak}", timeout=60)

    # 3. It was the SERVICE's limit that stopped it, not the guest running out: a cgroup OOM, and
    # the unit's own counter says so (memory.events counts its whole subtree).
    node1.succeed("journalctl -k --no-pager | grep -q 'Memory cgroup out of memory'")
    node1.fail("journalctl -k --no-pager | grep -v 'Memory cgroup' | grep -q 'Out of memory:'")
    ooms = int(node1.succeed(f"awk '/^oom_kill /{{print $2}}' /sys/fs/cgroup{cg}/memory.events").strip())
    assert ooms >= 1, f"{unit} records no OOM kill, yet the leak died"
    print(node1.succeed("journalctl -k --no-pager | grep -A1 'Memory cgroup out of memory' | tail -2"))

    # 4. ...and only the leak paid: the service is up on the same main process and still serves,
    # and the guest beside it never noticed.
    node1.succeed(f"systemctl is-active {unit}")
    assert node1.succeed(f"systemctl show -p MainPID --value {unit}").strip() == main_pid, "the service's own process was killed"
    node1.succeed("curl -fsS http://192.168.1.100:8080/healthz")
    node1.succeed("curl -fsS http://192.168.1.100/healthz")
    print(f"the leak hit {unit}'s limit and was killed there ({ooms} OOM kill); the service and the guest carried on")
  '';
}
