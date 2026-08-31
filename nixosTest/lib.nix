# Shared scaffolding for the DRBD failover nixosTests. Each test supplies
# its topology (a node list) and its own testScript; this builds the resource
# config and the per-node NixOS module they all share.
#
# A node is { name; id; diskless ? false; }. Addresses follow the id: node-id N
# lives at 10.0.0.<N+1>, which matches the framework's 10.0.0.<nodeNumber> on eth1
# (the private DRBD subnet; the VIP stays on 192.168.1.100).
{ pkgs, guestModule }:

let
  inherit (pkgs.lib) concatMapStringsSep mkForce mkIf;
  inherit (pkgs) lib;

  # The REAL renderer, as a helper binary. Shared with service-install.nix so a fixture installed
  # by the harness is rendered by the same code the agent runs.
  quadletRender = pkgs.callPackage ./quadlet-render-pkg.nix { };

  # The promoter's ordered unit: the same three units everywhere, on every node, whatever is
  # installed ([V3b.3](f)/(e1)). `briard-services` is what converges this node to the manifests on
  # the volume once the mount exists, which is why the services themselves are not members. The
  # front door is not listed either; it rides briard-vip (wantedBy + partOf), so it tracks the
  # primary regardless. This mirrors what the host agent writes in production
  # (host.promoterUnits), which also takes no arguments any more.
  promoterSnippet =
    let
      units = [
        "briard-data.service"
        "briard-services.service"
        "briard-vip.service"
      ];
    in
    ''
      [[promoter]]
      [promoter.resources.r0]
      adjust-resource-on-start = false
      start = [ ${concatMapStringsSep ", " (u: ''"${u}"'') units} ]
    '';

  # R0 over the node list. Per-node volume form so a witness can be `disk none`;
  # the production safety config. new-current-uuid then needs the
  # volume id (r0/0) — the one testScript quirk of this form.
  mkResource =
    nodes:
    let
      onBlock = n: ''
        on ${n.name} {
          node-id ${toString n.id};
          address 10.0.0.${toString (n.id + 1)}:7789;
          volume 0 {
            device /dev/drbd0;
            ${if n.diskless or false then "disk none;" else "disk /dev/vdb; meta-disk internal;"}
          }
        }'';
    in
    ''
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
      }
    '';

  # A test-only unit that PREWARMS a catalogued fixture: load its image tarball into local podman
  # storage and lay down the manifest the harness will later install from.
  #
  # It used to do far more — run the renderer, copy the units into the quadlet directory and write
  # the promoter chain from the renderer's output. [V3b.3](f) takes all of that away from the
  # harness and gives it to the PRODUCT: `briard-services` renders from the volume at promotion,
  # from a static chain the harness no longer writes. So what is left here is exactly the part a
  # test legitimately stands in for — the bytes a host agent would have fetched from the catalog
  # and warmed onto the node before anything promoted.
  #
  # This is what replaced the baked payload slot ([V3b.3](e2)). The slot put a container in the
  # guest image at BUILD time through `oci-containers` — a mechanism no user has, since a shipped
  # node installs at runtime from a manifest. Tests riding the slot therefore proved a path nobody
  # ships; tests riding this one prove the path everyone does.
  #
  # What still cannot run in a hermetic harness is the host agent's ORCHESTRATION — the node IS
  # the guest, with no host on the other end — so `install_fixture` below performs the Primary-only
  # half by hand (fetch, verify and health-gate are unit-tested in agent/host/service_test.go).
  # Everything under it is now the real thing: the real renderer, in the product's own binary,
  # reading the product's own volume layout.
  #
  # NO REGISTRY, and that is measured rather than assumed — see fixture-service.nix for the three
  # facts. The image arrives as a tarball whose digest the manifest already pins.
  # ONE UNIT, N FIXTURES ([V3b.4]). A node may carry more than one catalogued service, because the
  # coordination is plural and a harness that can stage only one can only ever prove the singular
  # case — which is exactly how "N services" stayed a fixture-shaped claim.
  #
  # Each fixture is staged under /run/briard/fixtures/<service name>, and /run/briard/fixture is a
  # symlink to the FIRST one: that is the path every single-service test and every helper default
  # already reads, so adding the plural form changed no existing test.
  fixturesInstall = fixtures: config: {
    description = "Prewarm ${lib.concatMapStringsSep " + " (f: f.name) fixtures} onto this node (test harness)";
    wantedBy = [ "multi-user.target" ];
    # Before the promoter can be started by hand: a converge that runs before the image is
    # resident would have to pull it, and there is no registry on a hermetic node.
    before = [ "drbd-reactor.service" ];
    path = [
      config.virtualisation.podman.package # the guest's podman, never a second copy
      quadletRender
      pkgs.coreutils
    ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      set -eu
      rm -rf /run/briard/fixtures /run/briard/fixture
      mkdir -p /run/briard/fixtures
      # THE POD POOL, which on a real node arrives over net.configure from a HOST that drew it
      # (agent/subnet). These rigs have no host -- the node IS the guest -- so the fact is seeded
      # here, exactly as the VIP's device and address are. Without it a private service cannot be
      # rendered at all, and converge says so rather than quietly using the guest's namespace.
      #
      # A fixed octet rather than a draw: the draw exists to miss a HOUSEHOLD's addressing, and a
      # hermetic rig has none to miss.
      printf '10.12.99\n' >/run/briard/pod.subnet
    ''
    + lib.concatMapStrings (fixture: ''
      # The tarball load is what makes the manifest's digest resolvable locally, which is what
      # Pull=never requires. Measured: podman records a RepoDigest for a loaded archive.
      podman load -i ${fixture}/image.tar
      podman image exists "$(cat ${fixture}/ref)"

      # The manifest, plus the RENDERER's sidecars (`dataroot`, `subdirs`, `units`, `identity`)
      # that install_fixture reads. The sidecars exist so a test never restates the layout the
      # product decides; no unit file is copied anywhere, because converge writes those itself.
      mkdir -p /run/briard/fixtures/${fixture.serviceName}
      quadlet-render ${fixture}/manifest.json /run/briard/fixtures/${fixture.serviceName}
      cp ${fixture}/manifest.json /run/briard/fixtures/${fixture.serviceName}/manifest.json
      # The image ref the manifest pins, exposed so a test can assert WARMTH by digest
      # rather than by a tag (a tag can match an image the manifest does not name).
      cp ${fixture}/ref /run/briard/fixtures/${fixture.serviceName}/ref

      # EVERY OTHER PUBLISHED VERSION is warmed here too, and only its manifest is kept aside:
      # a test that upgrades installs a different manifest under the SAME name, which is what a
      # version change is. Warming them all up front is also what the product does -- an upgrade
      # must not pull on the promotion path -- so the ordering a test exercises is the real one.
      ${lib.concatMapStrings (label: ''
        podman load -i ${fixture}/variants/${label}/image.tar
        podman image exists "$(cat ${fixture}/variants/${label}/ref)"
        mkdir -p /run/briard/fixtures/${fixture.serviceName}/variants/${label}
        cp ${fixture}/variants/${label}/manifest.json /run/briard/fixtures/${fixture.serviceName}/variants/${label}/manifest.json
        cp ${fixture}/variants/${label}/ref /run/briard/fixtures/${fixture.serviceName}/variants/${label}/ref
      '') (fixture.variantLabels or [ ])}
    '') fixtures
    + ''
      ln -sfn /run/briard/fixtures/${(lib.head fixtures).serviceName} /run/briard/fixture
    '';
  };

  # The singular form every existing test is written against.
  fixtureInstall = fixture: fixturesInstall [ fixture ];

  # testScript helpers for a node built with `fixture`. Prepend to a test's script.
  #
  # install_fixture stands in for the ONE half of the install path a hermetic harness cannot run:
  # the host agent's Primary-only orchestration. It writes the manifest to the REPLICATED volume
  # (what `service.provision` records as the service's identity), creates its storage, and then
  # hands over to the PRODUCT — `briard-agent --converge` is the same code drbd-reactor runs at
  # every promotion, not a harness re-implementation of it ([V3b.3](f)).
  #
  # It must run AFTER a node has promoted, because everything it touches is on the replicated
  # volume and only a Primary has it mounted. That is not a harness quirk; it is the exact
  # constraint the product's install path lives under, and the reason converge exists.
  #
  # ONE subvolume with plain per-container SUBDIRECTORIES, never nested subvolumes: `btrfs
  # subvolume delete` refuses on a subvolume containing them, so data.restore would break.
  # Podman does NOT create a missing bind-mount source (measured — the container dies with
  # `statfs …/app: no such file or directory`), so the provision step is required, not defensive.
  # Both the subvolume path and the subdirectory list come from the RENDERER's sidecars, so a
  # test never restates the layout the product decides.
  fixtureHelpers = ''
    def fixture_dir(service=None):
        """Where a staged fixture lives. `service` names one when the node carries several
        ([V3b.4]); the default is the symlink to the first, which is what a single-service node
        has always read."""
        return "/run/briard/fixture" if service is None else f"/run/briard/fixtures/{service}"

    def install_fixture(m, variant=None, service=None):
        """Install the fixture -- or, with `variant`, a DIFFERENT VERSION of it under the same
        name, which is what an upgrade is. Both go through the product's own converge."""
        d = fixture_dir(service)
        dataroot = m.succeed(f"cat {d}/dataroot").strip()
        m.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || btrfs subvolume create {dataroot}")
        for sub in m.succeed(f"cat {d}/subdirs").split():
            m.succeed(f"mkdir -p {dataroot}/{sub}")
        # The service's name IS the last element of its data root -- taken from the renderer
        # rather than restated, like everything else here.
        name = dataroot.rstrip("/").split("/")[-1]
        src = f"{d}/manifest.json" if variant is None \
            else f"{d}/variants/{variant}/manifest.json"
        m.succeed("mkdir -p /var/lib/briard/.services")
        m.succeed(f"cp {src} /var/lib/briard/.services/{name}.json")
        m.succeed("sync")
        # The product's own converge, by the same entry point briard-services.service uses. On a
        # version change it is also what BOUNCES the container: converge restarts what it has not
        # started with exactly these bytes ([V3b.3](e1)).
        m.succeed("briard-agent --converge")
        return dataroot

    def fixture_units(m, service=None):
        """The service units the RENDERER produced -- never a list restated here. They are NOT
        promoter chain members ([V3b.3](f)): briard-services starts them, which is what keeps a
        crashed container from demoting the node."""
        units = m.succeed(f"cat {fixture_dir(service)}/units").split()
        assert units, f"the renderer produced no service units: {units}"
        return units

    def name_the_flock(m, name="brave-elf"):
        """Give the node a flock name the way the AGENT does -- net.mdnsname writes exactly this
        file, and converge composes the per-service names from it ([B.48]). Without it a node has
        no name to route on: the service is installed and reachable on its port, but nothing
        answers for it at the front door, which is the honest state of a node whose flock has
        never been named."""
        m.succeed("mkdir -p /run/briard")
        m.succeed(f"printf 'FLOCK_NAME={name}\\n' >/run/briard/mdns.env")

    def routed_host(m, service):
        """The name the front door routes `service` on, read from the table the PRODUCT wrote --
        never a name restated here, which would assert our own arithmetic rather than the node's."""
        import json
        table = json.loads(m.succeed("cat /run/briard/routes.json"))
        for s in table["services"]:
            if s["name"] == service:
                assert s.get("hosts"), f"{service} is routed but unnamed: {s}"
                return s["hosts"][0]
        raise AssertionError(f"{service} is not in the node's routing table: {table}")
  '';

  # A test node: the unit image + a backing disk (unless diskless) + its private
  # DRBD address + the resource. `promoter` adds the promoter snippet; the tests provision DRBD,
  # then start drbd-reactor by hand -- which since [V3b.16a] is what the PRODUCT does too (the
  # agent arms the promoter at bring-up), so this file no longer has to force it off. That
  # divergence was the last one between the disk-image guest and these nodes, and it was the
  # divergence [V3b.16] fell into.
  mkNode =
    {
      resource,
      diskless ? false,
      promoter ? true,
      # A catalogued fixture (nixosTest/fixture-service.nix) prewarmed onto the node at boot; the
      # test then installs it onto the volume with install_fixture once something has promoted.
      # This is the ONLY way a test node gets a workload ([V3b.3](e2) deleted the build-time service
      # slot), and it is the way a shipped node gets one, which is the point: a test that put a
      # container on a node by a mechanism no user has proves a path nobody runs.
      fixture ? null,
      # SEVERAL catalogued services on one node ([V3b.4]). `fixture` is the one-service spelling
      # and stays exactly as it was; anything listed here is staged beside it, each under its own
      # name, so a test can prove what the plural coordination actually does — that installing or
      # upgrading one service leaves the others alone.
      fixtures ? [ ],
    }:
    let
      allFixtures = lib.optional (fixture != null) fixture ++ fixtures;
      # THE HARNESS'S SERVICE ADDRESS, stated once and consumed twice -- see the briard-vip
      # override below for why the harness states it at all. briard-vip takes it as unit
      # Environment; the mDNS publishers read it out of the file the agent would have written
      # (they have no Environment override, and requiring one per consumer is how the two halves
      # would drift). One value, so they cannot disagree.
      vipDev = "eth1";
      vipAddr = "192.168.1.100/24";
      vipEnvFile = pkgs.writeText "vip.env" "VIP_DEV=${vipDev}\nVIP_ADDR=${vipAddr}\n";
    in
    { config, ... }:
    {
      imports = [ guestModule ];
      virtualisation.emptyDiskImages = mkIf (!diskless) [ 256 ];
      networking.interfaces.eth1.ipv4.addresses = [
        {
          address = "10.0.0.${toString config.virtualisation.test.nodeNumber}";
          prefixLength = 24;
        }
      ];
      # test-only: probing the VIP, and -- with a fixture -- standing in for the install path's
      # Primary-only half, which in the product runs btrfs from the guest agent unit's own PATH
      # and converge from briard-services'. install_fixture runs both from a test shell, so both
      # have to be reachable there.
      environment.systemPackages = [ pkgs.curl ]
        ++ lib.optionals (allFixtures != [ ]) [ pkgs.btrfs-progs config.briard.agentPackage ];
      # THE FRAMEWORK DECLARES ITS OWN SERVICE ADDRESS (V3.19c step 3). The guest image bakes
      # none any more: unset means DHCP, and there is no DHCP server on a nixosTest's synthetic
      # L2. So the harness states the address it is going to curl, rather than inheriting one
      # from the product image -- which is the point of the change and not merely its cost. A
      # baked default that every test happens to agree with is how the original defect stayed
      # invisible: nothing disagreed with it, so nothing revealed it.
      #
      # eth1 here, not eth2: these guests are agent-less and have one service NIC, which is also
      # where their private DRBD address lives. That co-location is the reason briard-vip only
      # takes the NIC down when the address came from DHCP.
      #
      # EnvironmentFile is dropped with it: the product REQUIRES /run/briard/vip.env now
      # ([V3b.16a]), and there is no agent here to write one. Stating both halves is the harness
      # declaring its own configuration -- the same rule the reactor snippet above follows.
      systemd.services.briard-vip.serviceConfig = {
        Environment = mkForce [
          "VIP_DEV=${vipDev}"
          "VIP_ADDR=${vipAddr}"
        ];
        EnvironmentFile = mkForce [ ];
      };
      # /etc/drbd.conf is the one file drbdadm looks for at a path we do not choose, so the harness
      # states it here (the shipped image states the identical glob in disk-image.nix, which these
      # nodes do not import).
      environment.etc."drbd.conf".text = ''include "/run/briard/drbd.d/*.res";'';
      # BOTH agent-written files are on TMPFS in the product now ([V3b.16b]), so neither can be
      # declared through environment.etc any more. tmpfiles symlinks put the same store files at the
      # exact paths the agent would write, which keeps the harness stating its own configuration
      # while running the product's own layout -- and drbd-reactor reads a symlinked snippet
      # identically (the etc form was a store symlink too, which is why `drbd-reactorctl evict`
      # never minded). Paths that match the product are the point: the divergence between these
      # nodes and the disk-image guest is where [V3b.16] lived.
      systemd.tmpfiles.rules = [
        # The VIP configuration at the path the AGENT would have written it. briard-vip has the
        # Environment override above and does not need this; the two mDNS publishers do, because
        # they read the same file and have no override -- without it they fail on a missing
        # EnvironmentFile and restart forever, which is how a rig produces 47 failed starts while
        # every assertion in it still passes. Only a test that names its flock ever starts them
        # (ConditionPathExists on mdns.env), so the noise was invisible until one asserted mDNS.
        "d /run/briard 0755 root root -"
        "L+ /run/briard/vip.env - - - - ${vipEnvFile}"
        "d /run/briard/drbd.d 0755 root root -"
        "L+ /run/briard/drbd.d/r0.res - - - - ${pkgs.writeText "r0.res" resource}"
      ]
      # The snippet is STATIC ([V3b.3](f)): the chain names briard-services, and what the node runs
      # comes from the VOLUME at promotion, so no test's workload choice can change it.
      ++ lib.optionals promoter [
        "d /run/briard/drbd-reactor.d 0755 root root -"
        "L+ /run/briard/drbd-reactor.d/briard.toml - - - - ${pkgs.writeText "briard.toml" promoterSnippet}"
      ];
      systemd.services.briard-test-fixture-install =
        mkIf (allFixtures != [ ]) (fixturesInstall allFixtures config);
    };
in
{
  inherit
    mkResource
    mkNode
    fixtureInstall
    fixturesInstall
    fixtureHelpers
    ;
}
