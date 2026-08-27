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

  # The promoter's ordered unit, identical everywhere it's used. The payload is a member only
  # where one is installed: with an empty slot the unit does not exist, and naming a
  # non-existent unit fails the whole chain — taking the VIP down with it. The front door is
  # not listed here at all; it rides briard-vip (wantedBy + partOf), so it tracks the primary
  # either way. This mirrors what the host agent writes in production (host.promoterUnits).
  promoterSnippet =
    payload:
    let
      units = [ "briard-data.service" ]
        ++ lib.optional payload "podman-briard-payload.service"
        ++ [ "briard-vip.service" ];
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

  # A test-only unit that installs a CATALOGUED fixture the way the agent installs a service:
  # stage the image from its tarball, run the REAL renderer over its manifest, let quadlet turn
  # the output into units, and give the promoter the chain the renderer produced.
  #
  # This is what replaces the baked payload slot ([V3b.3](e)). The slot put a container in the
  # guest image at BUILD time through `oci-containers` — a mechanism no user has, since a shipped
  # node installs at runtime from a manifest. Tests riding the slot therefore proved a path nobody
  # ships; tests riding this one prove the path everyone does.
  #
  # It stands in for the host agent's ORCHESTRATION, which cannot run in a hermetic harness (the
  # node IS the guest, with no host on the other end) — the same standing-in `quadlet-render`
  # already does for service-install.nix, and orchestration is unit-tested in
  # agent/host/service_test.go. Everything below the agent is real: real podman, the real
  # renderer, real quadlet generation, a real promoter driving the result.
  #
  # NO REGISTRY, and that is measured rather than assumed — see fixture-service.nix for the three
  # facts. The image arrives as a tarball whose digest the manifest already pins.
  #
  # NO PROVISIONING STEP either: the renderer emits `Volume=<hostdir>:<mount>` and podman creates
  # a missing bind-mount source, so the data directory appears under the DRBD mount when the
  # container starts. The product provisions a btrfs SUBVOLUME instead (snapshots need one), which
  # is `service.provision`'s job and belongs to the tests that exercise rollback.
  fixtureInstall = fixture: config: {
    description = "Install the ${fixture.name} fixture as a catalogued service (test harness)";
    wantedBy = [ "multi-user.target" ];
    # Before the promoter can be started by hand: the chain it reads is written here.
    before = [ "drbd-reactor.service" ];
    path = [
      config.virtualisation.podman.package # the guest's podman, never a second copy
      quadletRender
      pkgs.coreutils
      pkgs.systemd
    ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      set -eu
      # The tarball load is what makes the manifest's digest resolvable locally, which is what
      # Pull=never requires. Measured: podman records a RepoDigest for a loaded archive.
      podman load -i ${fixture}/image.tar
      podman image exists "$(cat ${fixture}/ref)"

      rm -rf /run/briard/fixture
      mkdir -p /run/briard/fixture /run/containers/systemd /run/briard/drbd-reactor.d
      quadlet-render ${fixture}/manifest.json /run/briard/fixture
      # Only the UNIT files go where quadlet reads; `chain`/`images`/`identity`/`dataroot` are
      # sidecars for the harness and have no business in a generator directory.
      cp /run/briard/fixture/*.pod /run/briard/fixture/*.container /run/briard/fixture/*.image \
         /run/containers/systemd/
      systemctl daemon-reload

      # The chain comes from the RENDERER. A hand-written list here would prove nothing about our
      # output — the same reason service-install.nix reads this file rather than restating it.
      units=$(sed 's/.*/"&"/' /run/briard/fixture/chain | paste -sd, - | sed 's/,/, /g')
      cat > /run/briard/drbd-reactor.d/briard.toml <<EOF
      [[promoter]]
      [promoter.resources.r0]
      adjust-resource-on-start = false
      start = [ $units ]
      EOF
      sed -i 's/^      //' /run/briard/drbd-reactor.d/briard.toml
    '';
  };

  # testScript helpers for a node built with `fixture`. Prepend to a test's script.
  #
  # provision_fixture stands in for the install path's Primary-only `service.provision` step: the
  # data subvolume lives on the REPLICATED volume, so it cannot exist until a node has promoted
  # and mounted it — which in a hermetic test is the promoter's own first pass. That pass
  # therefore fails (podman does NOT create a missing bind-mount source; measured — the container
  # dies with `statfs …/app: no such file or directory`), the storage is made, and the chain is
  # re-run. Exactly the sequence service-install.nix performs by hand, in one place.
  #
  # ONE subvolume with plain per-container SUBDIRECTORIES, never nested subvolumes: `btrfs
  # subvolume delete` refuses on a subvolume containing them, so data.restore would break.
  # Both the subvolume path and the subdirectory list come from the RENDERER's sidecars, so a
  # test never restates the layout the product decides.
  fixtureHelpers = ''
    def provision_fixture(m):
        dataroot = m.succeed("cat /run/briard/fixture/dataroot").strip()
        m.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || btrfs subvolume create {dataroot}")
        for sub in m.succeed("cat /run/briard/fixture/subdirs").split():
            m.succeed(f"mkdir -p {dataroot}/{sub}")
        m.succeed("systemctl restart drbd-reactor.service")
        return dataroot

    def fixture_units(m):
        """The service units the RENDERER put in the promoter chain -- never a list restated here."""
        chain = m.succeed("cat /run/briard/fixture/chain").split()
        units = [u for u in chain if u.startswith("briard-dummy")]
        assert units, f"the renderer put no service units in the chain: {chain}"
        return units
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
      payload ? true,
      # A catalogued fixture (nixosTest/fixture-service.nix) installed at boot the way the agent
      # installs a service, INSTEAD of the build-time payload slot. When set, the promoter chain
      # comes from the renderer rather than from `payload`, so the two are mutually exclusive by
      # construction.
      fixture ? null,
    }:
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
      # provision step, which in the product runs btrfs from the guest agent unit's own PATH.
      environment.systemPackages = [ pkgs.curl ] ++ lib.optional (fixture != null) pkgs.btrfs-progs;
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
          "VIP_DEV=eth1"
          "VIP_ADDR=192.168.1.100/24"
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
        "d /run/briard/drbd.d 0755 root root -"
        "L+ /run/briard/drbd.d/r0.res - - - - ${pkgs.writeText "r0.res" resource}"
      ]
      # With a fixture the snippet is written by fixtureInstall, from the RENDERER's chain, so the
      # static one must not also claim the path.
      ++ lib.optionals (promoter && fixture == null) [
        "d /run/briard/drbd-reactor.d 0755 root root -"
        "L+ /run/briard/drbd-reactor.d/briard.toml - - - - ${pkgs.writeText "briard.toml" (promoterSnippet payload)}"
      ];
      systemd.services.briard-test-fixture-install =
        mkIf (fixture != null) (fixtureInstall fixture config);
    };
in
{
  inherit mkResource mkNode fixtureHelpers;
}
