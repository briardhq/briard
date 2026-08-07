# Briard VM unit image.
#
# The workload + DRBD 9 + drbd-reactor run *inside* this VM; the host agent runs
# outside it (the host/payload boundary, V0). drbd-reactor's promoter
# drives the ordered failover unit {DRBD-primary → data mount → payload → VIP}
# on whichever node holds the DRBD primary role. The per-resource
# promoter rules are supplied per-deployment as a snippet in /etc/drbd-reactor.d
# (the agent writes them in prod, V0; the harness writes them in tests),
# so this image stays generic and the daemon is idle until one appears.
{ config, pkgs, lib, ... }:
let
  btrfsRoot = "/var/lib/briard"; # the DRBD btrfs volume mount
  snapDir = "${btrfsRoot}/.snapshots"; # pre-upgrade snapshots, siblings of the data subvolume
  tlsDir = "${btrfsRoot}/tls"; # cert/key on the DRBD volume (replicated, survive failover)
  # The code identity the data was written by, stored on the replicated volume so it travels
  # with the data, and read by a promoting node before it serves: imagePinFile, the
  # *payload* OCI image ref. The payload is what writes the service data, so its image is the
  # data-format identity —'s "per-service OCI digest". Converging it is re-creating the
  # container from the pinned image, no OS switch; content-addressed, so node-independent.
  # Empty/absent => no pin (the older path is unchanged).
  #
  # There is deliberately no second form naming a whole *system* closure: that is a property of
  # the NODE, while this file is per service volume — so the multi-service shape (N volumes, one running OS) would have
  # meant N assertions about a single system. What the data actually demands is per-service
  # and lives beside this: the payload image, and the service manifest.
  #
  # imagePinFile/serveImage PAIR with the host agent's Go consts payloadPinPath/
  # payloadServeTag (agent/guestagent/guestagent.go). Different languages, so no shared
  # import; TestPayloadConstantsMatchGuestImage fails the build if either side is renamed
  #.
  imagePinFile = "${btrfsRoot}/.payload-image";
  # The local tag the payload container actually runs. Warm-load points it at the baked
  # default; converge re-points it at the data's pinned image (or refuses). So "which
  # image serves" is a promotion-time decision, not baked into the unit.
  serveImage = "briard-payload:serve";
  # The VIP's address AND device are both agent-determined in prod: net.configure writes
  # VIP_ADDR + VIP_DEV to vipEnvPath, and the EnvironmentFile overrides these baked values.
  #
  # The address used to be baked outright ("v0 fixed service VIP, not a knob"). That made the
  # product work on the one subnet our lab happens to use and **fail green** on every other:
  # the readiness probe runs in-guest, against an address the guest itself owns, so a node no
  # one in the house could reach still reported ready (V3.19). The LAN owns this value now.
  #
  # What is left here is the fallback for agent-less harnesses -- the nixosTest framework
  # (VIP co-located with DRBD on eth1) and single-node/legacy guests whose lone NIC is eth1.
  # Those run on exactly this subnet, which is why the pair still reads as it did.
  vipFallback = "192.168.1.100/24";
  vipDev = "eth1";
  vipEnvPath = "/run/briard/vip.env";
  # `ip` wants the prefix, `arping` wants the bare address. Strip it here rather than carry
  # the address twice: two variables are two things that can disagree, and the one that
  # would silently win is the gratuitous ARP nobody is watching.
  vipArping = pkgs.writeShellScript "briard-vip-arping" ''
    exec ${pkgs.iputils}/bin/arping -A -c 1 -I "$VIP_DEV" "''${VIP_ADDR%%/*}"
  '';

  cfg = config.briard.payload;

  # Whether this guest carries a service at all. Zero is the SHIPPED state: a node is
  # installed first and given something to run afterwards, so the image a stranger downloads
  # must not arrive with a workload they never chose. Everything payload-shaped below is
  # conditional on this, and what remains at zero — the volume, the promoter, the VIP, the
  # front door — is the substrate the node is actually promising.
  havePayload = cfg.image != null;
in
{
  # The payload slot as a NixOS option so the same guest image serves nothing, the test
  # fixture, or HA, without forking the DRBD/promoter/VIP scaffolding around it. Only the
  # container image + where its data subvolume lands and mounts differ; the unit name
  # (briard-payload) stays fixed so the promoter snippet and the host agent's ServiceSpec are
  # payload-agnostic.
  #
  # Selecting a payload is a build-time act here — image identity is already
  # runtime (the unit runs the local `:serve` tag, which converge/pin re-point), so what
  # actually bakes here is the data mount mapping. Moving that onto the DRBD volume beside
  # `.payload-image` is what lets a service be installed at runtime with no OS switch.
  options.briard.payload = {
    image = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "OCI image ref the payload container runs (name:tag); null = no service installed.";
    };
    imageFile = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = "The image tarball loaded for `image` (a dockerTools derivation).";
    };
    dataDir = lib.mkOption {
      type = lib.types.str;
      default = "${btrfsRoot}/payload";
      description = "The payload's data as a btrfs subvolume under the DRBD mount; snapshot/restore target.";
    };
    mountPath = lib.mkOption {
      type = lib.types.str;
      default = cfg.dataDir;
      description = "Where dataDir is bind-mounted inside the container (dummy: same path; HA: /config).";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = ''
        The port the payload listens on (host-networked), i.e. what the front door proxies to.
        The proxy reads this rather than assuming a port, so a payload on any port is served.
      '';
    };
    healthPath = lib.mkOption {
      type = lib.types.str;
      default = "/healthz";
      description = ''
        The path the front door probes on the payload to answer its own /healthz. The dummy
        serves /healthz; HA has none, so it uses / (its frontend answering IS its liveness).
      '';
    };
    stagedImages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = ''
        Extra payload image tarballs pre-staged into local podman storage at boot
       : warm-standby *upgrade targets* a rolling update can pin without a
        pull on the failover path. Each is a dockerTools image derivation; being
        referenced here bakes it into the disk closure. The serving image is unchanged
        (converge/warm still decide what :serve points at) — these are just resident and
        ready for `payload.pin`. Empty by default (the base image ships no upgrade target).
      '';
    };
  };

  config = {
    system.stateVersion = "26.05";

    # Binary-cache substituters, the way a new OS closure reaches a
    # field guest. Nix takes a *list*, ranks them by the `Priority` each serves in
    # its nix-cache-info (lower wins), and fetches each path from the best one that
    # has it: stock nixpkgs comes from the public CDN at 40, and only our overlaid
    # paths (drbd/drbd-reactor/reverse-proxy/briard-agent) + this guest's own
    # `toplevel` come from cache.briard.io at 100 — measured 74 MB of a 1611 MB
    # closure, the closure-*diff* effect, not a re-image. Our cache
    # actually HOLDS the whole closure (nix refuses to write a cache whose
    # references it does not have — see scripts/publish-cache.sh); priority, not
    # content, is what keeps the guest off it for stock paths. The upside of that
    # forced choice: if cache.nixos.org is unreachable, ours alone can still
    # complete an update. cache.briard.io is a trust root DISTINCT from the
    # release keyring: nix's own per-path narinfo signatures, verified by the
    # baked public key below. The matching private key signs at release time via
    # scripts/publish-cache.sh and lives in the release secret store (never here).
    # Baked, not a knob (CONTRIBUTING.md: no new flags). The split holds because
    # flake.nix pins the `nixos-26.05` channel branch (only Hydra-built revs, so
    # the public cache has everything) and the DRBD kernel module is stock.
    # ⚠️ The key below is the ALPHA key, and it is provisional BY DECISION (2026-08-06, owner).
    # It was generated on the development machine — which is also where the R2 publish
    # credential lives, and it is that CO-LOCATION rather than the key's origin that is the real
    # weakness: either secret alone is inert (a forged signature has nowhere to be served, and
    # the bucket serves content that will not verify), but together they are arbitrary code into
    # every guest. Accepted for the alpha because no guest exists in the field yet, so the blast
    # radius is zero and rotation costs one line plus a rebuild — and it stops being cheap the
    # moment a stranger has installed. ** is the item that retires it; its gate is
    # advertising a beta in any way, not any particular version.**
    # Rotating is a THREE-release roll, not an edit to this line: the list is baked into the
    # closure, so the OLD key is what authorises the update that installs the new one (see
    #(3) for the N / N+1 / N+2 sequence). The `-1` suffix is nix's own generation counter
    # — it matches signatures by name, so the successor is `cache.briard.io-2`.
    # cache.nixos.org + its key are the stock NixOS defaults (these lists merge),
    # so we add only our cache and its key — the guest ends up trusting both.
    nix.settings = {
      substituters = [ "https://cache.briard.io" ];
      trusted-public-keys = [
        "cache.briard.io-1:HPewy0Rte7JoAP7SS6InoWeIy+MpFRicMCt0EUE6Jig=" # ALPHA key
      ];
    };

    # Answer the ACPI power button. systemd-logind is what listens for it, and
    # nothing on this appliance was starting it: it ships with no [Install] section and only
    # a dbus alias, so on a box with no logins nothing ever touches login1 and it stays down.
    # The guest therefore ignored QEMU's `system_powerdown` outright — measured, 60 s of
    # silence — which meant every clean shutdown the host can ask for (an OS upgrade's reboot
    # leg, a host reboot, a UPS event) degraded to killing QEMU: a power cut, to a machine
    # whose whole job is not losing data. logind is already in the closure, so this starts it
    # rather than adding anything, and its default HandlePowerKey=poweroff is what we want.
    systemd.services.systemd-logind.wantedBy = [ "multi-user.target" ];

    # DRBD 9: the out-of-tree 9.2.16 module (the one with quorum),
    # built against the default 6.18 LTS kernel and loaded at boot. /proc/drbd then
    # reports a 9.x module.
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    boot.kernelModules = [ "drbd" ];

    # The DRBD kernel module shells out to a userland helper on some events; its
    # built-in default (/sbin/drbdadm) doesn't exist on NixOS, so point it at ours.
    # (We don't enable services.drbd — its boot-time `drbdadm up all` fights our
    # per-resource, agent-fired drbd@<res>.target bring-up — but adopt this bit.)
    boot.extraModprobeConfig = ''
      options drbd usermode_helper=${pkgs.drbd}/bin/drbdadm
    '';

    environment.systemPackages = [
      pkgs.drbd # userland: drbdadm, drbdsetup
      pkgs.drbd-reactor # failover orchestrator (in-repo package)
    ];

    # drbd-reactor daemon: runs from boot watching DRBD, idle until a promoter
    # snippet for some resource is dropped into /etc/drbd-reactor.d.
    environment.etc."drbd-reactor.toml".text = ''
      snippets = "/etc/drbd-reactor.d"
    '';
    systemd.tmpfiles.rules = [
      "d /etc/drbd-reactor.d 0755 root root -"
      # NIX'S CACHE DIR NEVER SURVIVES A BOOT. A crash-consistent guest disk can hand
      # nix a torn `binary-cache-v7.sqlite` -- its narinfo lookup cache, opened `synchronous = off`
      # precisely because it is disposable, which is exactly what removes SQLite's write-ordering
      # guarantees. Nix does not self-heal it: it warns, drops the substituter, and reports
      # "there is no substituter that can build it" -- so a corrupt LOCAL file presents as a
      # DELIVERY failure and the node silently cannot take another update. Upstream has no fix
      # (NixOS/nix#8647 open, nixpkgs#3958 older still); the canonical remedy is to delete it.
      #
      # We produce crash-consistent disks deliberately (the switch path's live snapshot)
      # and the field produces them anyway (a power cut), so this must not be tied to the rollback
      # leg. Clearing at boot covers every producer, and the whole directory rather than the one
      # file we happened to catch: XDG defines it as non-essential data deletable at any time, and
      # the sibling caches tear for the identical reason.
      #
      # `R!` is boot-ONLY on purpose: a mid-life `switch-to-configuration` restarts
      # systemd-tmpfiles-resetup, and a rule that fired there could wipe the cache underneath a
      # running stage. Costs nothing measurable -- the cache is consulted only by `os.stage`, and
      # cannot help an update anyway (it is keyed per store path, and a new release's paths have
      # never been queried).
      "R! /root/.cache/nix - - - - -"
    ];

    systemd.services.drbd-reactor = {
      description = "drbd-reactor — DRBD failover orchestrator";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      path = [ pkgs.drbd pkgs.systemd ]; # drbdsetup/drbdadm + systemd management
      serviceConfig = {
        Type = "notify";
        ExecStart = "${pkgs.drbd-reactor}/bin/drbd-reactor";
        Restart = "on-failure";
      };
    };

    # Stock DRBD systemd integration: drbd-reactor's promoter drives drbd-utils'
    # own drbd-promote@ / drbd-services@ units and the drbd@.target bring-up chain
    # — the tested upstream units, not hand-rolled ones (the
    # overlay patches nixpkgs' drbd to install them; see flake.nix). NixOS loads
    # them but starts nothing until the agent (or, in tests, the harness) writes a
    # .res and fires drbd@<res>.target; promotion is then drbd-reactor's.
    systemd.packages = [ pkgs.drbd ];
    services.udev.packages = [ pkgs.drbd ]; # DRBD udev rules (/dev/drbd/by-res symlinks + perms)

    # The ordered failover unit. Each piece has wantedBy = [] so it never starts on
    # its own — drbd-reactor starts them, in this order, only after it has promoted
    # the resource, and stops them in reverse on demote. So they run on the primary
    # and nowhere else.

    # 1. data — mount the replicated DRBD device, formatting it on first use.
    systemd.services.briard-data = {
      description = "Briard data volume (DRBD device, mounted on the primary)";
      wantedBy = [ ];
      path = [ pkgs.util-linux pkgs.btrfs-progs ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-data-up" ''
          set -eu
          mkdir -p ${btrfsRoot}
          # Format on first use only: a blank DRBD device has no fs signature.
          # btrfs for CoW snapshots -- the atomic {data} half of rollback, and they
          # replicate with the volume.
          if ! blkid /dev/drbd0 >/dev/null 2>&1; then
            mkfs.btrfs -f /dev/drbd0
          fi
          mount /dev/drbd0 ${btrfsRoot}
          # First use: the payload's data as a real subvolume (so a pre-upgrade snapshot
          # can delete+re-create it data.restore) + a sibling snapshots dir. Both
          # replicate with the volume, so they survive failover. Idempotent. With no service
          # installed there is no data subvolume to create — the volume is mounted and empty,
          # which is the honest state of a node nobody has given a workload to yet.
          ${lib.optionalString havePayload
            "btrfs subvolume show ${cfg.dataDir} >/dev/null 2>&1 || btrfs subvolume create ${cfg.dataDir}"}
          mkdir -p ${snapDir}
        '';
        ExecStop = "${pkgs.util-linux}/bin/umount ${btrfsRoot}";
      };
    };

    # 2. payload — the workload as a pinned OCI container, Podman-managed,
    #    its data dir bind-mounted from the DRBD mount and host-networked so it
    #    answers at the VIP. Promoter-driven (podman-briard-payload.service) → runs
    #    only on the primary. The dummy and HA share this slot; only briard.payload
    #    differs. No `cmd` override — each image carries its own entrypoint (the
    #    dummy's baked Cmd; HA's /init s6 supervisor).
    virtualisation.oci-containers = {
      backend = "podman";
      containers = lib.mkIf havePayload {
        briard-payload = {
          image = serveImage; # not cfg.image: converge/warm decide which image :serve points at
          imageFile = cfg.imageFile;
          volumes = [ "${cfg.dataDir}:${cfg.mountPath}" ];
          extraOptions = [ "--network=host" ];
        };
      };
    };

    # Podman belongs to the guest OS, not to any service: it is the runtime a service will be
    # installed INTO. oci-containers only enables it when a container is declared, so with the
    # slot empty we ask for it directly — otherwise a zero-service node would have no runtime,
    # and briard-converge (which retags the serve image) would have nothing to talk to.
    virtualisation.podman.enable = true;

    # Warm standby: keep the pinned payload image resident in *every* node's
    # local podman storage, not just wherever the primary currently runs. A standby
    # is cold by default — the primary can hold the role for months — so without
    # this, promotion pays a cold multi-GB `podman load` *on the failover-critical
    # path*, defeating the point of synchronous HA (a fast takeover). This oneshot
    # runs at boot on all nodes (not promoter-gated), is idempotent, and re-fires
    # when a new generation pins a different image (restartTriggers). The image is
    # per-node *code*, so it lives in node-local storage, never on the DRBD
    # volume. (v0 loads it from the closure-baked tar; the registry-pull model
    # warms the same way — `podman pull` the digest on every node.)
    systemd.services.briard-payload-warm = lib.mkIf havePayload {
      description = "Warm the pinned payload image into local podman storage (standby readiness)";
      wantedBy = [ "multi-user.target" ];
      restartTriggers = [ cfg.imageFile ];
      path = [ pkgs.podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-payload-warm" ''
          set -eu
          podman image exists ${cfg.image} || podman load -i ${cfg.imageFile}
          # Point the serve tag at the baked default; converge may re-point it.
          podman tag ${cfg.image} ${serveImage}
        '';
      };
    };

    # Pre-stage upgrade-target images: a rolling update pins an image the
    # data was written by, and converge/UpgradePayload only ever *select* (retag) —
    # never build/pull — on the failover path. So the target must already be resident.
    # This oneshot warms every briard.payload.stagedImages tarball into local podman
    # storage at boot, the warm-standby that (in prod) a `podman pull` of the published
    # digest does before the rollout. Idempotent; independent of which image serves.
    systemd.services.briard-payload-stage = lib.mkIf (havePayload && cfg.stagedImages != [ ]) {
      description = "Pre-stage upgrade-target payload images into local podman storage";
      wantedBy = [ "multi-user.target" ];
      after = [ "briard-payload-warm.service" ];
      path = [ pkgs.podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-payload-stage" ''
          set -eu
          ${lib.concatMapStringsSep "\n" (img: "podman load -i ${img}") cfg.stagedImages}
        '';
      };
    };

    # Converge-at-promotion: before the payload serves, reconcile this node's running
    # code to the code identity the data carries (imagePinFile, replicated). The data is
    # ground truth for "what code should run here" — same reconcile-from-DRBD pattern as the
    # reactor reading role. Ordered after the volume mount (must read the file) and required
    # by the payload, so on a mismatch the payload can't serve.
    #
    # It selects rather than switches: a local `podman tag`, same-version-safe and off the nix
    # lock. A node that cannot satisfy the pin — the image is not staged here — defers instead
    # of serving old code against new-format data. Empty/absent pin => proceed (no-op).
    #
    # It gates on the SERVICE identity only, never on the OS closure. A system closure is a
    # property of the node, not of the data, so refusing to serve over an OS mismatch would
    # defer a node for no data-safety reason — and would do it at promotion, i.e. during a
    # failover, which is the worst possible moment to withhold service.
    systemd.services.briard-converge = {
      description = "Gate the payload on code↔data identity";
      wantedBy = [ ];
      after = [ "briard-data.service" "briard-payload-warm.service" ];
      requires = [ "briard-data.service" ];
      path = [ pkgs.coreutils pkgs.podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-converge" ''
          set -eu

          # This gate never switches the OS generation: the host agent is the single owner
          # of os.switch, so there is no autonomous nix-profile action here to race a
          # host-managed op on the profile lock (the class, impossible by
          # construction). There is no /run/briard-maintenance defer marker
          # — with nothing autonomous to coordinate, there is nothing to hold off.

          # Payload image — the data-format identity: the
          # payload writes the service data, so a promoting node must run the image the data
          # was written by. Content-addressed, pre-staged on every node by the warm-load, so
          # this is a select (a local retag), never a build/pull on the failover path.
          # Re-point the serve tag, or refuse.
          #
          # This is the whole gate now. The whole-OS check that used to follow it went with
          # the .code-system pin: a system closure is a property of the node, not of
          # the data, so refusing to serve over it deferred a node for no data-safety reason
          # — at promotion, which is the worst possible moment to withhold service.
          pin=$(cat ${imagePinFile} 2>/dev/null || true)
          if [ -n "$pin" ]; then
            if podman image exists "$pin"; then
              podman tag "$pin" ${serveImage}
            else
              echo "briard-converge: pinned payload image $pin not staged; refusing to promote" >&2
              exit 1 # fail-safe: defer rather than serve stale code against new-format data
            fi
          fi
          exit 0 # the image matches the data (or nothing pinned) -> serve
        '';
      };
    };

    # Promoter-driven (runs only on the primary), but ordered after the warm-load so
    # a promotion can't race it into a cold load — on an already-warm survivor this
    # is instant, so it costs nothing at failover; on a node's first-ever boot it
    # waits for the one load that has to happen sometime anyway. Also gated on
    # briard-converge: the payload must not serve until the code matches the data.
    systemd.services.podman-briard-payload = lib.mkIf havePayload {
      wantedBy = lib.mkForce [ ];
      after = [ "briard-payload-warm.service" "briard-converge.service" ];
      wants = [ "briard-payload-warm.service" ];
      requires = [ "briard-converge.service" ];
    };

    # 3. vip — claim the service address and gratuitous-ARP it so the L2 segment
    #    learns its (new) home. BOTH the address and the device are agent-determined
    #    (net.configure writes VIP_ADDR + VIP_DEV to ${vipEnvPath}). Under the
    #    unified NIC layout eth1 is always the DRBD NIC and the VIP lives on
    #    eth2 — the installer sets VIP_DEV=eth2 even single-node (eth1 sits idle until
    #    a pairing addresses it), so a second anchor can join without a guest reboot.
    #    The baked eth1 default is only the fallback for agent-less harnesses (the
    #    lib.nix framework tests, whose lone service NIC is eth1) — the file overrides it.
    systemd.services.briard-vip = {
      description = "Briard service VIP";
      wantedBy = [ ];
      path = [ pkgs.iproute2 pkgs.iputils ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        Environment = [ "VIP_DEV=${vipDev}" "VIP_ADDR=${vipFallback}" ];
        EnvironmentFile = "-${vipEnvPath}";
        # Bring the service NIC up first (the framework brings the NIC up for the
        # nixosTests; a disk-image guest's NIC may still be down). Idempotent.
        ExecStartPre = "${pkgs.iproute2}/bin/ip link set dev $VIP_DEV up";
        ExecStart = "${pkgs.iproute2}/bin/ip addr add $VIP_ADDR dev $VIP_DEV";
        ExecStartPost = "-${vipArping}";
        ExecStop = "${pkgs.iproute2}/bin/ip addr del $VIP_ADDR dev $VIP_DEV";
      };
    };

    # 4. the front door — answer the VIP on :80 and terminate HTTPS on :443, forwarding to
    #    the payload. Woven into the
    #    promoter chain via briard-vip (wantedBy + partOf), NOT the drbd-reactor start-list —
    #    so it tracks the primary role (up on promote, down on demote) without touching the
    #    reactor snippet, leaving the six DRBD mechanism tests untouched (same trick as
    #    briard-converge). Cert/key live on the DRBD volume (${tlsDir}) so they replicate +
    #    survive failover; the proxy hot-reloads them, so a renewal is gap-free.
    #    wantedBy (not requires) => a missing cert never fails the VIP: :443 just doesn't
    #    answer until a cert exists, while :80 keeps serving — which is the *shipped* state of
    #    a free node, since a cert needs a domain.
    systemd.services.briard-reverse-proxy = {
      description = "Briard front door (serves the VIP on :80/:443)";
      wantedBy = [ "briard-vip.service" ];
      partOf = [ "briard-vip.service" ];
      after = [ "briard-vip.service" "podman-briard-payload.service" ];
      serviceConfig = {
        # No payload => no -backend: the front door then serves Briard's own page and answers
        # its own /healthz, which is what makes a node with nothing installed *ready* rather
        # than permanently unhealthy (the zombie state).
        ExecStart = "${pkgs.reverse-proxy}/bin/reverse-proxy"
          + " -http :80 -listen :443"
          + lib.optionalString havePayload
            " -backend http://127.0.0.1:${toString cfg.port} -backend-health ${cfg.healthPath}"
          + " -cert ${tlsDir}/fullchain.pem -key ${tlsDir}/key.pem";
        Restart = "on-failure";
        RestartSec = 2;
      };
    };

    # Lean, headless test image. The nixosTest / VM runner supplies the real boot
    # device + networking, so there is no bootloader/root device here.
    boot.loader.grub.enable = false;
    fileSystems."/" = {
      device = "/dev/vda";
      fsType = "ext4";
    };
    networking.firewall.enable = false;
  };
}
