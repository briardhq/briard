# A standalone bootable qcow2 of the guest image, for the agent's QEMU to boot
# via -drive. The framework-boot guest (configuration.nix) has no
# bootloader/root device; here we add grub + a partitioned root + virtio drivers
# and bake it into a disk with make-disk-image. This is the *bootstrap* artifact;
# field updates are incremental NixOS generation switches, not re-imaging.
#
# The deliberately-broken generation that used to live here (`.brokenSystem`: the payload
# container run with BRIARD_BROKEN=1) went with managed-upgrade.nix (c2-v). It was
# PAYLOAD-derived, which is what dated it: the OS upgrade it fed no longer touches the payload
# at all, and the lab rollback demo's broken generation breaks the FRONT DOOR instead — an ordinary delta on
# the shipped disk, fetched over the cache like any other target, needing no argument here.
#
# stageImages (default none) bakes extra payload image tarballs into the disk and warms
# them into podman at boot (briard.payload.stagedImages) — warm-standby upgrade targets a
# rolling update can pin without a pull on the failover path. Used by the
# fleet upgrade demo to pre-stage a v1 payload.
#
# stageSystemModule (default null) bakes a *second, distinct* guest system generation into
# the disk (the running system + this delta module) — a warm-standby whole-OS upgrade
# target. Its store path is exposed as the `.v1System` passthru so the OS rolling-update
# demo can pin it (`rollout -system`). Content-addressed => identical across the fleet, so
# a survivor can switch to the primary's exact closure (converge-at-promotion).
#
# rebootSystemModule (default null) bakes a THIRD generation, exposed as `.rebootSystem`, whose
# delta is chosen to make ActivationFor return reboot-only — so the reboot path has a
# target to aim at. A separate argument rather than a second entry in one list because the two
# differ in kind, not degree: the point of one is that it CAN be activated in band and of the
# other that it cannot, and a proof that mixed them up would still look like it passed. Costs
# almost nothing to bake — a kernel-params delta shares every other store path with the running
# generation.
#
# guestAgentEnv (default none) sets extra environment on the briard-guest-agent unit — used by
# the deadman test to bake a short BRIARD_DEADMAN so the reflex fires in seconds.
#
# payloadModule (default null) selects a service into the payload slot. The DEFAULT — none — is
# the shipped artifact: `install.sh` lays this disk down and the node comes up running no
# workload at all. Tests that need a payload to write checkable data pass the fixture
# (nixosTest/dummy-payload.nix), which is why there is a second disk build at all; a runtime install's
# runtime service install removes the need for it, and this argument with it.
#
# bakeTargets (default true) decides whether the upgrade-target generations above are also
# BAKED into the image. False means they are built and exposed as passthrus but the guest must
# FETCH them — which is what a field node does, and the only way to prove a fetched closure is
# usable. It is per-import because the two callers want opposite things: the lab
# demos exercise real delivery, while `boot-select` deliberately gives its guest no network at
# all so that a boot-selector proof cannot fail for a delivery reason.
#
# commonModules (default none) are folded into EVERY generation this file builds — the running
# system and each upgrade target alike. That is the point: the lab demos use it to point the
# guest at a reachable substituter, and a setting present in one generation but not another
# would widen the delta between them, which the reboot demo asserts is exactly one kernel
# parameter. (Distinct from mkGuest's own `extraModules`, which is per-generation and is what
# MAKES each target differ.)
{ nixpkgs, pkgs, overlay, stageImages ? [ ], stageSystemModule ? null
, rebootSystemModule ? null, guestAgentEnv ? { }, payloadModule ? null
, bakeTargets ? true, commonModules ? [ ]
  # The release id stamped into the guest's agent. Defaulted so every existing caller (the
  # lab fleet disks, the test variants) keeps building unchanged; flake.nix passes the real one.
, agentVersion ? "0.0.0-dev" }:
let
  lib = nixpkgs.lib;
  # The guest VM only ever runs `agent --guest`, so build the trimmed guest-only binary
  # Guest-only build: no host subsystems / net/http / TLS in the shipped guest closure.
  briardAgent = pkgs.callPackage ../agent/package.nix { tags = [ "guest" ]; version = agentVersion; };

  # Common bootable-guest modules, shared by the good and broken generations so the
  # broken one is a minimal, honest delta (only the payload env differs).
  bootModule =
    { config, lib, modulesPath, ... }:
    {
      imports = [ "${modulesPath}/profiles/qemu-guest.nix" ]; # virtio_blk/pci/console in initrd
      networking.hostName = "guest"; # DRBD .res on-block name (matches the driver's NODE)
      boot.loader.grub = {
        enable = lib.mkForce true;
        device = "/dev/vda";
        # The boot selector: the host says which generation this launch boots, and
        # says it OUTSIDE the disk. A reboot-path OS upgrade parks its target in the
        # `staging` system profile (os.stageboot) WITHOUT moving grub's default; the host
        # then passes `-smbios type=11,value=briard_boot=staging` for exactly the one launch
        # that should come up on it. Nothing on this disk is ever armed, so an OS-disk
        # snapshot taken before the reboot cannot re-run the bad generation when restored --
        # the reason `grub-reboot` (which writes into the guest's own grubenv) was rejected.
        #
        # Every step fails safe onto the existing default, i.e. onto the OLD system: no
        # smbios.mod, no type-11 structure, an unreadable string or a renamed submenu all
        # leave `default` untouched. A bug here costs an upgrade, never a boot.
        #
        # Mechanics, each verified against the pinned nixpkgs/grub rather than assumed:
        #   - grub reads the byte at the given offset as a string NUMBER (smbios.c). Type 11
        #     is OEM Strings: its only such byte is offset 4 (Count), which resolves to the
        #     last string -- the single one we pass.
        #   - install-grub.pl globs /nix/var/nix/profiles/system-profiles/* at run time and
        #     emits a submenu per profile, titled "<distro> - Profile '<name>'".
        #   - "title>0" is a submenu path: grub matches the title up to the '>' and then
        #     re-parses the remainder inside the submenu (menu.c), where 0 is the newest
        #     generation (profile generations are listed newest-first).
        #   - extraConfig lands AFTER `set default=` and BEFORE the menuentries, so this is
        #     an override through a supported option, not a patch.
        extraConfig = ''
          # Let grub speak on ttyS0, the guest's only console (boot.kernelParams already
          # points the kernel there). Until V3.17c2 grub had no decision to make and its
          # VGA-only output cost nothing; now that a launch can select a generation, a
          # bootloader that fails silently is a node that is simply "down" with no evidence.
          insmod serial
          serial --unit=0 --speed=115200
          terminal_output --append serial

          insmod smbios
          smbios --type 11 --get-string 4 --set briard_boot
          if [ "$briard_boot" = "briard_boot=staging" ]; then
            set default="${config.system.nixos.distroName} - Profile 'staging'>0"
          fi
        '';
      };
      fileSystems."/" = lib.mkForce {
        device = "/dev/disk/by-label/nixos";
        fsType = "ext4";
      };
      boot.kernelParams = [
        "console=ttyS0" # serial console for debugging
        "net.ifnames=0" # predictable eth0/eth1 (the tapped service NIC -> eth1, the VIP)
      ];
      # Forward the journal to ttyS0 so the captured serial log shows systemd +
      # drbd-reactor activity during an upgrade.
      services.journald.extraConfig = "ForwardToConsole=yes\nMaxLevelConsole=info";

      # drbd.conf includes the .res files the agent drops at runtime into a
      # writable /etc/drbd.d (the framework drbd-* tests bake these via lib.nix).
      environment.etc."drbd.conf".text = ''include "/etc/drbd.d/*.res";'';
      systemd.tmpfiles.rules = [
        "d /etc/drbd.d 0755 root root -"
        "d /run/briard 0755 root root -" # the deadman contact stamp + kmsg cursor live here
      ];

      # The in-guest control agent: opens the virtio-serial port and serves the
      # host's bring-up/observe/upgrade verbs. PATH carries the tools those verbs shell
      # out to. (reactor.pause/resume use `systemctl` on drbd-reactor.service; the
      # daemon itself runs from its own unit, so drbd-reactor isn't needed here.)
      systemd.services.briard-guest-agent = {
        description = "Briard in-guest control agent (serves the host over virtio-serial)";
        wantedBy = [ "multi-user.target" ];
        after = [ "systemd-tmpfiles-setup.service" ];
        path = [
          pkgs.drbd # drbdadm/drbdsetup, for the drbd.* verbs
          pkgs.drbd-reactor # drbd-reactorctl, for reactor.evict — the planned handover
          pkgs.systemd # systemctl, for payload.* / reactor.* / os.switch
          pkgs.nix # nix-env, for os.switch
          pkgs.coreutils # readlink, for os.system
          pkgs.btrfs-progs # btrfs, for data.snapshot/restore
          pkgs.iproute2 # ip, for net.configure (the system/DRBD NIC)
          pkgs.podman # podman, for payload.pin (retag the serve image —)
        ];
        # Restart=always (not on-failure): the guest agent serves ONE host connection
        # then Serve() returns nil on the clean EOF when the host disconnects (wire.go)
        # -- so `agent --guest` exits 0. The reconnect design (host.go) needs it
        # back on the port for the *next* host connection, which a genuine disconnect
        # (host-agent restart -> self-update; or a re-adopt) produces. `on-failure`
        # would NOT restart a clean exit, leaving the port dead and the new host's
        # handshake blocked. A virtio-serial read blocks (never EOF-loops) while no host
        # is connected, so a reopened port just waits -- no restart flapping. StartLimit
        # off so this critical channel never permanently gives up.
        startLimitIntervalSec = 0; # [Unit] section: never permanently give up on this channel
        serviceConfig = {
          ExecStart = "${briardAgent}/bin/briard-agent --guest";
          Restart = "always";
          RestartSec = 1;
        };
      };

      # The host-agent deadman as its OWN long-running service — decoupled from the
      # per-connection guest agent (which crash-loops while the host is down, so an in-process
      # timer would keep resetting). It watches the contact stamp the guest agent bumps and, once
      # the host agent is silent past T_deadman, reboots the guest — quorum-gated + graceful
      #. guestAgentEnv carries a short BRIARD_DEADMAN for the deadman test.
      systemd.services.briard-deadman = {
        description = "Briard host-agent deadman (reboots the guest if the host agent goes silent)";
        wantedBy = [ "multi-user.target" ];
        after = [ "systemd-tmpfiles-setup.service" ];
        path = [
          pkgs.drbd # drbdsetup, for the quorum gate
          pkgs.systemd # systemctl reboot
        ];
        environment = guestAgentEnv;
        serviceConfig = {
          ExecStart = "${briardAgent}/bin/briard-agent --deadman";
          Restart = "always";
          RestartSec = 2;
        };
      };
    };

  mkGuest =
    extraModules:
    lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        { nixpkgs.overlays = [ overlay ]; }
        ./configuration.nix
        bootModule
      ] ++ lib.optional (payloadModule != null) payloadModule ++ commonModules ++ extraModules;
    };

  # An optional distinct upgrade-target generation (the running system + a delta), staged
  # for the whole-OS rolling-update demo. Content-addressed => identical fleet-wide.
  v1System =
    if stageSystemModule == null then null else (mkGuest [ stageSystemModule ]).config.system.build.toplevel;

  # The reboot-only upgrade target. Same treatment, different promise: this one must
  # NOT be reachable by an in-band switch, which is what makes it a proof rather than a repeat.
  rebootSystem =
    if rebootSystemModule == null then null else (mkGuest [ rebootSystemModule ]).config.system.build.toplevel;

  # The good (running) generation, with any staged upgrade-target closures kept in its
  # closure so they're present on the disk for an offline `os.switch`, and any pre-staged
  # upgrade-target payload images baked in + warmed at boot.
  #
  # BAKING A TARGET IS THE OLD WAY, and it is being unwound. It existed because the
  # guest had no substituter, so the only way a closure could ever reach it was at image-build
  # time — the very gap the binary cache closes. Each baked target is a WHOLE SECOND OS CLOSURE in
  # the image, so this list is what made the fixture disk expensive, not the ~8 MB payload.
  # A test target belongs on a real (in-test) binary cache instead — cheaper, and a truer
  # rehearsal of how a field node receives every generation. `v1System`/`rebootSystem` are the
  # last two still baked, and go the same way.
  sys = mkGuest [
    {
      system.extraDependencies = lib.optionals bakeTargets (
        lib.optional (v1System != null) v1System
        ++ lib.optional (rebootSystem != null) rebootSystem);
      briard.payload.stagedImages = stageImages;
    }
  ];

  image = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
    inherit lib pkgs;
    config = sys.config;
    format = "qcow2";
    partitionTableType = "legacy";
    # NOT "auto". Auto sizes the disk to the closure plus a small margin, which left ~1.9 GB
    # free -- less than ONE Home Assistant. A service's image is pulled at RUNTIME into
    # /var/lib/containers on this root (images are cattle, warmed on every node; only service
    # DATA lives on the replicated volume), so `briard service install home-assistant` filled
    # the disk and died with ENOSPC 2 GB into the pull. The tests never saw it because they BAKE
    # the payload image into the image at build time, where "auto" grows to fit it -- so the
    # tested disk has room for exactly the image the test bakes and the shipped one has room for
    # nothing.
    #
    # 24 GiB is the headroom, not the footprint: qcow2 is sparse, so the published artifact and
    # the download are unchanged (~2.6 GB), and the host allocates only what the guest writes.
    # It buys room for a couple of real service images plus the second copy an image upgrade
    # holds, and the second system generation an OS upgrade stages.
    diskSize = 24576;
    label = "nixos";
  };
in
# Surface each upgrade target's store path alongside the image (passthru attrs on the
# derivation, via //, so `${guestDisk}/nixos.qcow2` and `nix build` still work), for a test to
# hand to os.stage / os.switch.
#
# `system` is the RUNNING generation's toplevel — the closure a field guest actually
# boots, and therefore the one the binary cache must serve. It is deliberately
# not `nixosConfigurations.guest`: that is the framework-boot variant (no bootloader, no
# briard-agent units), so a cache published from it would omit the agent and ship an
# initrd/etc no guest runs. Building this attr builds only the toplevel, not the qcow2.
image
// { system = sys.config.system.build.toplevel; }
// lib.optionalAttrs (v1System != null) { inherit v1System; }
// lib.optionalAttrs (rebootSystem != null) { inherit rebootSystem; }
