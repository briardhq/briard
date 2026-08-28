# 3-node majority topology: kill the primary, VIP + data move to a survivor.
#
# Same stock-unit + promoter setup as drbd-promote, now with three full nodes
# (quorum majority) and a real failure. Kill the elected primary and assert the
# survivors — which still hold quorum (2 of 3) — promote one of their own,
# reclaim the VIP, and serve the dummy's last persisted tick (replicated under
# protocol C). The minority self-fence is the separate drbd-fence test.
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
  name = "drbd-failover";

  # crash() is QemuMachine-only, and the primary is selected dynamically, so the
  # static checker can't follow it.
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
        m.wait_for_unit("briard-test-fixture-install.service") # image warm on EVERY node, before any promotion
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    # Offline failover: the outage that triggers a takeover often kills WAN
    # too, so failover must need zero internet. Sever any default route and assert
    # it's gone — a local routing fact, not a curl to a public IP (which can fail
    # for unrelated reasons and is vacuous in the WAN-less test sandbox). DRBD + VIP
    # are directly-connected (LAN), so the takeover is unaffected.
    for m in machines:
        m.succeed("ip route del default 2>/dev/null || true")
        m.fail("ip route show default | grep -q .")

    # Each node reaches both peers, then we skip the initial sync once.
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # drbd-reactor elects a primary and the VIP comes up there -- with nothing installed yet,
    # which is the shipped state and answers its own /healthz at the front door.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in machines if role(m) == "Primary")
    # The service is installed onto the VOLUME by whoever holds it. The survivor never runs an
    # install: it renders from that same volume when it promotes ([V3b.3](f)), which is the half
    # this test is really about.
    dataroot = install_fixture(primary)
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    # Let the counter climb past a clear threshold so "data survived" is decisive:
    # a lost volume would be re-formatted blank and restart near 0, far below this.
    primary.wait_until_succeeds(f"[ $(grep -oE '[0-9]+' {dataroot}/app/state.json) -ge 3 ]")
    t1 = int(json.loads(primary.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    print(f"primary={primary.name} tick={t1}")

    # Kill the primary abruptly (power-loss shape).
    primary.crash()
    survivors = [m for m in machines if m != primary]

    # The two survivors keep quorum, promote one of their own, and the VIP answers
    # again — from any survivor on the segment (gratuitous ARP moved it).
    surv = survivors[0]
    surv.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    # Data moved with it: the last persisted tick came across (protocol C). A lost
    # volume would have been re-formatted blank and restarted near 0.
    t2 = int(json.loads(surv.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t2 >= t1 - 2, f"data lost across failover: t1={t1} t2={t2}"

    # Single-primary preserved: exactly one survivor is now the DRBD primary.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
