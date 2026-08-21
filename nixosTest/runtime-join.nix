# Runtime anchor pairing: grow a single-node DRBD green into a 3-node mesh (2 anchors +
# a diskless witness) at RUNTIME, without the static-PEERS-edit + restart the fleet tests bake.
#
# This is the substrate proof for the pairing mechanism (the "second anchor joins
# blank" join). The host agent's role (a DirectivePair -> cfg.applyPair -> Adjust/BringUp) is
# validated at-rest in agent/host/pair_test.go + agent/guestagent (the drbd.adjust verb); HERE we
# drive the exact DRBD operation that verb runs -- rewrite the .res + `drbdadm adjust` -- to prove
# the SUBSTRATE supports it: the serving primary keeps its data + quorum while a blank anchor
# resyncs to UpToDate and the witness casts the 3rd vote, and the whole thing survives a failover
# onto the freshly-joined anchor. Same split as the rest of the HA net (lib.nix drives the DRBD
# primitives, the agent logic is at-rest -- [[v3-2-real-ha-upgrade]]).
#
# lib.nix declares r0.res as a read-only store symlink; runtime growth needs to REWRITE it, so this
# test writes its own .res straight into the product path the agent uses (/run/briard/drbd.d) --
# exactly what the guest's drbd.provision/drbd.adjust verbs do. No nested KVM (the L1 node
# runs DRBD directly), so it rides the fast `drbd` tag.
{ pkgs, guestModule }:

let
  inherit (pkgs.lib) concatMapStringsSep mkForce mkIf;
  inherit (pkgs) lib;

  # The promoter's ordered chain (identical to lib.nix): workload -> VIP on the primary.
  promoterSnippet = ''
    [[promoter]]
    [promoter.resources.r0]
    adjust-resource-on-start = false
    start = [ "briard-data.service", "podman-briard-payload.service", "briard-vip.service" ]
  '';

  # A node like lib.nix's mkNode, but with no baked r0.res so the testScript can write and rewrite
  # it at runtime (the whole point).
  mkNode =
    { diskless ? false, promoter ? true }:
    { config, ... }:
    {
      imports = [ guestModule ];
      virtualisation.emptyDiskImages = mkIf (!diskless) [ 256 ];
      networking.interfaces.eth1.ipv4.addresses = [
        { address = "10.0.0.${toString config.virtualisation.test.nodeNumber}"; prefixLength = 24; }
      ];
      environment.systemPackages = [ pkgs.curl ];
      # The service address, NAMED -- exactly as lib.nix's mkNode does, and for the reason that
      # node exists. This file builds its own node instead of using mkNode, so it does not inherit
      # that mkForce. [V3.19] removed the guest's baked VIP (`vipFallback = ""`) because no address
      # is right in a house we have not seen, which means every agent-less harness must name one.
      # This one did not: briard-vip fell through to the DHCP branch on a network with no DHCP
      # server, failed, and took the VIP and the payload with it -- surfacing three layers away as
      # a connect timeout on `curl http://192.168.1.100:8080/healthz`. It has had no full green
      # nightly since 2026-08-08, the night before [V3.19] landed.
      #
      # EnvironmentFile goes with it: the product REQUIRES /run/briard/vip.env now ([V3b.16a]) and
      # there is no agent here to write one. drbd-reactor needs no forcing any more either -- the
      # product no longer starts it at boot, because the agent arms the promoter at bring-up.
      systemd.services.briard-vip.serviceConfig = {
        Environment = mkForce [
          "VIP_DEV=eth1"
          "VIP_ADDR=192.168.1.100/24"
        ];
        EnvironmentFile = mkForce [ ];
      };
      # THE PRIVATE WRITABLE DIR IS GONE, and that is [V3b.16b] paying this test back. It pointed
      # drbd.conf at its own /run/briard/drbd.d because the product's .res lived in read-only
      # environment.etc while this test has to REWRITE it at runtime. The product's .res is on
      # tmpfs now, at a path the agent writes and rewrites, so the test drives the real one --
      # which is what "exactly what the guest's drbd.provision/drbd.adjust verbs do" was always
      # meant to mean. The promoter snippet moved with it, declared as a tmpfiles symlink.
      systemd.tmpfiles.rules = [ "d /run/briard/drbd.d 0755 root root -" ]
        ++ lib.optionals promoter [
          "d /run/briard/drbd-reactor.d 0755 root root -"
          "L+ /run/briard/drbd-reactor.d/briard.toml - - - - ${pkgs.writeText "briard.toml" promoterSnippet}"
        ];
      environment.etc."drbd.conf".text = ''include "/run/briard/drbd.d/*.res";'';
    };

  # R0 over a node list; the exact form agent/drbd/config.go renders (what drbd.adjust writes).
  onBlock = n: ''
    on ${n.name} {
      node-id ${toString n.id};
      address 10.0.0.${toString (n.id + 1)}:7789;
      volume 0 {
        device /dev/drbd0;
        ${if n.disk then "disk /dev/vdb; meta-disk internal;" else "disk none;"}
      }
    }'';
  resFor = nodes: ''
    resource r0 {
      net { protocol C; }
      options {
        auto-promote                  no;
        quorum                        majority;
        on-no-quorum                  io-error;
        on-suspended-primary-outdated force-secondary;
      }
      ${concatMapStringsSep "\n  " onBlock nodes}
      connection-mesh { hosts ${concatMapStringsSep " " (n: n.name) nodes}; }
    }'';

  # The mesh-of-one anchor1 starts on (peer-less, its own real address), and the 3-node mesh it
  # grows into. node-id/address follow the id: node-id N at 10.0.0.<N+1> (matches eth1).
  singleRes = pkgs.writeText "r0-single.res" (resFor [ { name = "anchor1"; id = 0; disk = true; } ]);
  threeRes = pkgs.writeText "r0-three.res" (resFor [
    { name = "anchor1"; id = 0; disk = true; }
    { name = "anchor2"; id = 1; disk = true; }
    { name = "witness"; id = 2; disk = false; }
  ]);
in
pkgs.testers.runNixOSTest {
  name = "runtime-join";
  skipTypeCheck = true;

  # Declaration order fixes nodeNumber -> the DRBD address: anchor1=10.0.0.1, anchor2=.2, witness=.3.
  nodes = {
    anchor1 = mkNode { };
    anchor2 = mkNode { };
    witness = mkNode { diskless = true; promoter = false; };
  };

  testScript = ''
    start_all()
    for m in [anchor1, anchor2, witness]:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
        m.succeed("mkdir -p /run/briard/drbd.d")

    # --- Phase 1: anchor1 comes up single-node (mesh-of-one) green, serving the VIP ---
    anchor1.succeed("cp ${singleRes} /run/briard/drbd.d/r0.res")
    anchor1.succeed("drbdadm create-md --force r0")
    anchor1.succeed("systemctl start drbd@r0.target")
    anchor1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")  # peer-less -> UpToDate
    anchor1.succeed("systemctl start drbd-reactor.service")
    anchor1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    # Let the payload persist a margin of ticks on the single node, then snapshot the counter --
    # our data-survival handle across the pairing and the later failover.
    anchor1.wait_until_succeeds(
        "test $(curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9) -ge 10", timeout=60
    )
    pre = int(anchor1.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())
    print(f"single-node green, pre-pairing ticks={pre}")

    # --- Phase 2: bring the joiners up BLANK on the 3-node config, ready to connect ---
    # anchor2: a fresh (blank) replica -> Inconsistent, it will SyncTarget from anchor1. It must
    # NOT new-current-uuid (that would declare it UpToDate and split-brain the primary's data).
    anchor2.succeed("cp ${threeRes} /run/briard/drbd.d/r0.res")
    anchor2.succeed("drbdadm create-md --force r0")
    anchor2.succeed("systemctl start drbd@r0.target")
    # Witness: diskless quorum voter -- no metadata, no create-md.
    witness.succeed("cp ${threeRes} /run/briard/drbd.d/r0.res")
    witness.succeed("systemctl start drbd@r0.target")

    # --- THE RUNTIME GROWTH: adjust anchor1 mesh-of-one -> 3-node mesh, in place ---
    # Exactly what the drbd.adjust verb runs (rewrite the .res + `drbdadm adjust`): no create-md,
    # no restart, the primary's disk stays attached + UpToDate and it keeps serving.
    anchor1.succeed("cp ${threeRes} /run/briard/drbd.d/r0.res")
    anchor1.succeed("drbdadm adjust r0")

    # Both peers connect; the blank anchor resyncs from anchor1 and reaches UpToDate; quorum is now
    # majority-of-3 (survives one loss). cstate counts connected peers; dstate is the disk state.
    anchor1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -eq 2", timeout=180)
    # A 3-node dstate prints local + per-peer fields (e.g. UpToDate/UpToDate/Diskless); match the
    # LOCAL field so we assert anchor2's own disk finished resyncing, whatever the peers report.
    anchor2.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=240)
    print("second anchor joined at runtime and resynced to UpToDate; 3-node mesh connected")

    # --- Data survived + VIP unbroken: the primary kept serving across the growth ---
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=60)
    post = int(anchor1.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())
    assert post >= pre, f"data regressed across pairing: {pre} -> {post}"
    print(f"primary kept its data across the growth: ticks {pre} -> {post}")

    # --- Failover onto the freshly-joined anchor: the pre-pairing data crossed the link ---
    anchor2.succeed("systemctl start drbd-reactor.service")  # so it can be promoted
    anchor1.crash()  # anchor2 + witness = 2/3 quorum -> anchor2 promotes, VIP moves
    anchor2.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=120)
    anchor2.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=180)
    survived = int(anchor2.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())
    assert survived >= pre, f"pre-pairing data not on the new primary after failover: pre={pre} got={survived}"
    print(f"runtime pairing proven: single-node -> 3-node mesh, data {pre} present on the joined anchor after failover ({survived})")
  '';
}
