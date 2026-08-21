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
      environment.systemPackages = [ pkgs.curl ]; # test-only, probing the VIP
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
      ++ lib.optionals promoter [
        "d /run/briard/drbd-reactor.d 0755 root root -"
        "L+ /run/briard/drbd-reactor.d/briard.toml - - - - ${pkgs.writeText "briard.toml" (promoterSnippet payload)}"
      ];
    };
in
{
  inherit mkResource mkNode;
}
