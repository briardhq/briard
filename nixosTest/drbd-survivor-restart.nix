# — can a survivor RESTART while its peer is gone? Vanilla DRBD, no briard machinery.
#
# This exists because a lab finding was doubted, correctly: everything we had was measured through
# the agent, the fleet's containers and an upgrade path, so "the survivor cannot come back" could
# have been any of those rather than DRBD. Here there is no agent, no qemu-in-qemu, no upgrade —
# three ordinary test VMs, the SAME resource options the product ships (quorum majority,
# on-no-quorum io-error, one diskless witness), and the config BAKED into the image so even the
# runtime-config variable is gone.
#
# TWO ACTS, deliberately juxtaposed, because they are different events that are easy to conflate:
#
#   ACT 1 — the documented win, and the control: crash the PRIMARY, and the survivor promotes.
#           It never lost quorum (it was quorate, and the diskless tiebreaker KEEPS quorum when
#           the peer vanishes), so promotion is allowed. This is what drbd-witness.nix already
#           proves and what makes 2-node homes HA; it is repeated here so that if it ever stops
#           being true, this test says so in the same breath as act 2.
#
#   ACT 2 — the case under investigation: with the peer STILL absent, the survivor itself
#           restarts. Runtime quorum state is gone, so it must GAIN quorum rather than keep it —
#           and LINBIT's implementation notes say "a partition of half of the nodes with storage
#           can never gain quorum when it establishes connections to diskless nodes".
#
# The test ASSERTS the outcome we want in act 2 (the node comes back and serves). If DRBD really
# cannot do that, this test fails — and that failure is the finding, stated in the one place that
# cannot be blamed on our own plumbing.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  diskNode = h.mkNode { inherit resource fixture; };
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-survivor-restart";

  # crash()/start() are QemuMachine-only, and the primary is chosen dynamically.
  skipTypeCheck = true;

  nodes = {
    node1 = diskNode;
    node2 = diskNode;
    witness = witnessNode;
  };

  testScript = ''
    ${h.fixtureHelpers}
    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        # The image is warmed on both disk nodes before anything promotes: the survivor renders
        # from the volume when it takes over and must not need a pull to do it.
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("drbdadm create-md --force r0")
    for m in machines:
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")
    # Nothing is installed yet, so the front door answers for itself; then the promoted node puts
    # the service on the volume, which is what the restarted survivor later converges from.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")
    install_fixture(next(m for m in disk_nodes if m.execute("drbdadm role r0")[1].strip() == "Primary"))
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    def quorum(m):
        # `drbdsetup status --json` carries the per-device quorum flag; this is the same
        # field the agent reports upward, read here without the agent.
        code, out = m.execute("drbdsetup status --json r0")
        return "true" if '"quorum": true' in out else ("false" if out else "?")

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    survivor = next(m for m in disk_nodes if m != primary)
    print(f"### primary={primary.name} survivor={survivor.name}")

    ### ACT 1 — the control: the primary crashes, the survivor keeps quorum and promotes.
    primary.crash()
    survivor.wait_until_succeeds("drbdadm role r0 | grep -q Primary")
    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    print(f">>> ACT 1 PASS: {survivor.name} promoted with the peer gone (quorum={quorum(survivor)})")
    print(survivor.succeed("drbdsetup status r0"))

    def bring_drbd_back(m):
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
        # NOT create-md: the metadata must still be there. If this attaches, the replica survived.
        m.succeed("systemctl start drbd@r0.target")
        m.succeed("systemctl start drbd-reactor.service")

    ### ACT 2a — a CLEAN reboot of the survivor, peer still absent.
    # DRBD 9.2.7 added "Preserve tiebreaker quorum over a reboot of the last node", which works by
    # recording MDF_HAVE_QUORUM + prev_members at shutdown and restoring it at attach. A clean
    # shutdown can write that; this act is what says whether it does.
    survivor.shutdown()
    survivor.start()
    bring_drbd_back(survivor)
    survivor.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected", timeout=60)
    clean_ok = survivor.execute("timeout 90 sh -c 'until drbdadm role r0 | grep -q Primary; do sleep 2; done'")[0] == 0
    print(f">>> ACT 2a (CLEAN reboot): regained quorum + promoted = {clean_ok} (quorum={quorum(survivor)})")
    print(survivor.succeed("drbdsetup status r0"))
    print(survivor.succeed("journalctl -k --no-pager | grep -iE 'quorum|nodes visible|Restored' | tail -5 || true"))

    ### ACT 2b — a CRASH of the survivor (a power cut), peer still absent.
    # Same node, same topology, one variable changed: nothing gets the chance to record anything.
    survivor.crash()
    survivor.start()
    bring_drbd_back(survivor)

    # Give it a generous window: the witness has to reconnect and the promoter has to act.
    survivor.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected", timeout=60)
    print(f"### ACT 2b (CRASH): witness reconnected, quorum={quorum(survivor)}")
    print(survivor.succeed("drbdsetup status r0"))
    print(survivor.succeed("journalctl -k --no-pager | grep -iE 'quorum|nodes visible|Restored' | tail -10 || true"))

    # THE ASSERTION. A node that rebooted while its only diskful peer is absent should be able to
    # come back and serve — the witness is connected, and the absent peer demonstrably cannot have
    # quorum (it is off, and the witness is here). If this fails, that is in vanilla DRBD.
    survivor.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=120)
    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=60)
    print(f">>> ACT 2b PASS: {survivor.name} regained quorum after a CRASH (quorum={quorum(survivor)})")
  '';
}
