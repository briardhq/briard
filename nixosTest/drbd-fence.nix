# Partition the minority → self-fence (3-node majority).
#
# From a healthy 3-node cluster, isolate the elected primary (block its network).
# It becomes a minority of 1, loses DRBD quorum, and must self-fence: with
# on-no-quorum=io-error its writes fail fast (no uninterruptible-I/O wedge), so
# drbd-reactor stops the ordered unit — VIP first (ip addr del is instant and
# doesn't wait on the service), then service, then mount — and demotes. The
# majority (2 of 3) keeps quorum, promotes a survivor, and serves the VIP intact.
#
# The on-no-quorum decision is io-error, not suspend-io: the
# self-fence is drbd-reactor *stopping* the unit and suspend-io would wedge that
# stop in D-state. Both bar minority writes, so safety (Principle 3) holds.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
      { name = "node3"; id = 2; }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-fence";

  # block() is QemuMachine-only + dynamic primary selection — see drbd-failover.
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    node3 = node;
  };

  testScript = ''
    ${h.fixtureHelpers}
    import json

    machines = [node1, node2, node3]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service") # warm on every node: a survivor must not pull
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    # Offline failover: sever any default route and assert it's gone, so the
    # self-fence + survivor takeover are proven to need zero internet — a local
    # routing fact, not a curl to a public IP (vacuous in the WAN-less test sandbox).
    # DRBD + VIP are directly-connected (LAN), so isolation/promotion are unaffected.
    for m in machines:
        m.succeed("ip route del default 2>/dev/null || true")
        m.fail("ip route show default | grep -q .")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")

    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # Nothing is installed yet, so the front door answers for itself -- the shipped state.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in machines if role(m) == "Primary")
    dataroot = install_fixture(primary)
    service_units = fixture_units(primary)
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    primary.wait_until_succeeds(f"[ $(grep -oE '[0-9]+' {dataroot}/app/state.json) -ge 3 ]")
    t1 = int(json.loads(primary.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    print(f"primary={primary.name} tick={t1}")

    # Isolate the primary: it drops to a minority of 1 and must self-fence.
    primary.block()
    survivors = [m for m in machines if m != primary]

    # Self-fence on the isolated node: drbd-reactor stops the ordered unit (VIP
    # first) and demotes. VIP gone, service stopped, DRBD no longer Primary.
    primary.wait_until_fails("ip -4 addr show dev eth1 | grep -q 192.168.1.100")
    # The services stop with the fence too, and by a different route than the VIP: they are not
    # chain members ([V3b.3](f)), so what stops them is briard-services' own ExecStop unwinding
    # converge. A fenced node that kept serving would be exactly the split the fence exists for.
    for unit in service_units:
        primary.wait_until_fails(f"systemctl is-active {unit}")
    primary.wait_until_succeeds("drbdadm role r0 | grep -q Secondary")

    # The majority keeps quorum, promotes a survivor, and serves the VIP intact.
    surv = survivors[0]
    surv.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    t2 = int(json.loads(surv.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t2 >= t1 - 2, f"data lost across self-fence failover: t1={t1} t2={t2}"

    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
    surv_primary = next(m for m in survivors if role(m) == "Primary")

    # ---- REUNION: heal the partition -> the fenced node rejoins, resyncs, and does NOT
    # re-promote (invariants 9/10/11). The break-only half above proved the majority survives; this
    # proves the minority comes home cleanly -- the reconnection audit's core case (assert state
    # *equality* after the heal, not just that the link came back).

    # The survivor kept serving + writing through the partition: capture its advanced tick.
    surv_primary.wait_until_succeeds(f"[ $(grep -oE '[0-9]+' {dataroot}/app/state.json) -ge {t2 + 2} ]")
    t3 = int(json.loads(surv_primary.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t3 > t2, f"survivor did not keep writing during the partition: t2={t2} t3={t3}"

    primary.unblock()

    # Reconverges-after-heal (9): the ex-primary reconnects to both peers and every disk resyncs to
    # UpToDate -- the cluster is whole again (link-up != converged: assert the DISK state, not ping).
    for m in machines:
        m.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
        m.wait_until_succeeds("test $(drbdadm dstate r0 | tr '/' '\\n' | grep -vc UpToDate) -eq 0")

    # No-spurious-action-on-heal (11): the fenced node rejoins as SECONDARY -- no re-promote, no
    # flap; the survivor stays the sole primary across the WHOLE cluster and keeps the VIP.
    assert role(primary) == "Secondary", f"reunited ex-primary must rejoin Secondary, got {role(primary)}"
    all_primaries = sorted(m.name for m in machines if role(m) == "Primary")
    assert all_primaries == [surv_primary.name], f"reunion must keep the survivor the sole primary, got {all_primaries}"
    surv_primary.succeed("curl -fsS http://192.168.1.100:8080/healthz")

    # Resync-integrity (10): the survivor's partition-era writes actually reached the rejoined disks
    # -- proven END-TO-END, not just by DRBD's UpToDate claim. Fail the survivor; the two reunited
    # nodes (2/3 = quorum) promote again, and the newly-served data must be >= t3 (never rewound).
    surv_primary.crash()
    reunited = [m for m in machines if m != surv_primary]
    reunited[0].wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    t4 = int(json.loads(reunited[0].succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t4 >= t3, f"resync lost the survivor's partition-era writes: t3={t3} t4={t4}"
  '';
}
