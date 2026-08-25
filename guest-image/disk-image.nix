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
  # The guest VM only ever runs `briard run --guest`, so build the trimmed guest-only binary
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

      # EXACTLY ONE NIC IN THIS GUEST DOES DHCP, AND IT IS NOT THIS ONE.
      #
      # The four are fixed and only one faces a network we do not own: eth0 is qemu's SLIRP
      # user-net (the WAN path for OCI pulls), eth1 is the DRBD link (private, statically
      # addressed by the agent, unaddressed at all on a single node), eth3 is the private
      # guest<->host witness link (static), and eth2 carries the VIP -- the one address that
      # belongs to the household's LAN, and the only one worth asking anybody for.
      #
      # eth0's "DHCP server" is qemu's own, on a synthetic network whose addressing we hand to
      # qemu ourselves (platform/qemu.go pins net/host/dns rather than leaning on its defaults),
      # so leasing it buys a constant we already know. A general-purpose network manager makes
      # sense for a vanilla OS that must cope with whatever it is plugged into; this guest is
      # neither vanilla nor surprised by its own NICs.
      #
      # It also removes a defect rather than working around one ([B.78]): with a system dhcpcd
      # running, briard-vip's per-interface invocation for eth2 never became an instance -- it
      # forwarded its argv to that master as a control command, and the master had eth2 in
      # denyInterfaces (which we had put there), so DHCP silently never ran and the node refused
      # to be primary. With no master, there is nothing to be hijacked by.
      networking.useDHCP = false;
      networking.interfaces.eth0.ipv4.addresses = [
        { address = "10.0.2.15"; prefixLength = 24; }
      ];
      networking.defaultGateway = {
        address = "10.0.2.2";
        interface = "eth0";
      };
      networking.nameservers = [ "10.0.2.3" ]; # SLIRP's resolver, forwarded to the host's

      # eth3 -- the private host<->guest link -- is addressed by the AGENT, not baked here, and its
      # address is pure SUBSTRATE: nothing dials it. The reboot gate answers at this node's node IP,
      # the host routes the VIP `via` that node IP, and both ends pin a permanent neighbour entry so
      # neither has to ARP across the link (agent/platform/route.go, net.configure).
      #
      # It carries an address at all for one measured reason: avahi joins the IPv4 mDNS group on an
      # interface only if that interface HAS a v4 address. Without one this NIC answers mDNS over
      # IPv6 alone, and the far end of that conversation is a stranger's host which may have v6
      # off -- so [V3b.19]'s name half would break silently, the household's own machine unable to
      # find its own node while everything else works ([V3b.26b]; install-macvtap runs with v6
      # disabled precisely so nothing can pass for a reason we do not control).
      #
      # NOT BAKED, and the objection to that is answered rather than dropped. The address used to be baked
      # precisely because the host reads the reboot gate when the CONTROL CHANNEL IS DEAD, and an
      # agent-assigned address looked like it would be "reliably absent in the one failure it
      # exists to serve". It is not, and `-no-reboot` is why: the gate is consulted only on rung 3,
      # a VM that is RUNNING but mute, and a running VM is one whose bring-up completed and
      # therefore one whose node IP the agent already set. The case the old comment feared -- a
      # guest that reboots itself with no agent to reconfigure it -- does not reach this code,
      # because with `-no-reboot` that guest's unit ENDS and the host's rung 2 relaunches it
      # (which runs bring-up) instead of asking a gate.

      # drbd.conf includes the .res files the agent drops at runtime. TMPFS since [V3b.16b]: the
      # `.res` is node-scoped, the host re-derives it at every bring-up (from cfg.Resource, which
      # the mesh cache now durably holds even for a runtime pairing), and a copy that outlives the
      # agent that wrote it is the only kind that can be stale. /etc/drbd.conf itself stays put --
      # drbdadm looks for that one file at a path we do not choose, and it is the POINTER, not the
      # state. (The framework drbd-* tests declare both halves themselves, via lib.nix.)
      environment.etc."drbd.conf".text = ''include "/run/briard/drbd.d/*.res";'';
      systemd.tmpfiles.rules = [
        "d /run/briard 0755 root root -" # the deadman contact stamp + kmsg cursor live here too
        "d /run/briard/drbd.d 0755 root root -"
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
          # The MODULE's podman, not `pkgs.podman` — naming the latter ships a second,
          # differently-wrapped copy of the runtime (configuration.nix explains; [B.5]).
          config.virtualisation.podman.package # podman, for payload.pin (retag the serve image —)
        ];
        # Restart=always (not on-failure): the guest agent serves ONE host connection
        # then Serve() returns nil on the clean EOF when the host disconnects (wire.go)
        # -- so `briard run --guest` exits 0. The reconnect design (host.go) needs it
        # back on the port for the *next* host connection, which a genuine disconnect
        # (host-agent restart -> self-update; or a re-adopt) produces. `on-failure`
        # would NOT restart a clean exit, leaving the port dead and the new host's
        # handshake blocked. StartLimit off so this critical channel never permanently gives up.
        #
        # This comment used to claim a virtio-serial read BLOCKS while no host is connected, so a
        # reopened port just waits and there is no flapping. That is true only of a BRIEF gap: with
        # the host end gone for good, the reopened port returns EOF on the first read and the exit
        # is immediate, so Restart=always spun ~48x in 30s ([B.35]). The agent now pauses before
        # exiting on a clean EOF (hostAbsentPause in main.go) -- the restart policy here is
        # unchanged, and correct; what was wrong was the assumption that made it free.
        startLimitIntervalSec = 0; # [Unit] section: never permanently give up on this channel
        serviceConfig = {
          ExecStart = "${briardAgent}/bin/briard-agent run --guest";
          Restart = "always";
          RestartSec = 1;
        };
      };

      # The host-agent deadman as its OWN long-running service — decoupled from the
      # per-connection guest agent (which crash-loops while the host is down, so an in-process
      # timer would keep resetting). It watches the contact stamp the guest agent bumps and, once
      # the host agent is silent past T_deadman, reboots the guest — gated + graceful
      #. guestAgentEnv carries a short BRIARD_DEADMAN for the deadman test.
      #
      # It also SERVES that gate to the host on the private link (BRIARD_GATE_ADDR), which is the
      # only reason the host's own rung can avoid power-cycling a node whose departure would cost
      # a peer its quorum: every other way of asking rides the channel whose death is the trigger.
      # BRIARD_GATE_ADDR is a PORT with no address: the gate answers on whatever this node holds,
      # because its address is now the node IP -- agent-assigned, flock-scoped, and not a thing the
      # image can know (DESIGN §4). Binding wide is safe here in a way it would not be for any
      # other listener: the gate READS NOTHING from a connection (accept, write one line, close),
      # so there is no request to parse and no parser to get wrong.
      #
      # ⚠️ It is still a posture change worth naming: the gate used to be unreachable from the LAN
      # by addressing alone, and the system subnet rides the LAN's L2. What a stranger on the wire
      # gains is the ability to READ whether this node currently thinks a reboot is safe. They
      # cannot set it -- the verdict comes from the deadman's own evaluation, never from the
      # connection -- and flooding the listener makes the host read "unreachable", which it treats
      # as ALLOWED, so a flood removes the guard rather than holding it shut.
      systemd.services.briard-deadman = {
        description = "Briard host-agent deadman (reboots the guest if the host agent goes silent)";
        wantedBy = [ "multi-user.target" ];
        after = [ "systemd-tmpfiles-setup.service" "network-online.target" ];
        wants = [ "network-online.target" ];
        path = [
          pkgs.drbd # drbdsetup, for the reboot gate
          pkgs.systemd # systemctl reboot
        ];
        environment = { BRIARD_GATE_ADDR = ":7790"; } // guestAgentEnv;
        serviceConfig = {
          ExecStart = "${briardAgent}/bin/briard-agent run --deadman";
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
    # DO NOT BAKE THE NIXPKGS CHANNEL INTO THE IMAGE. make-disk-image defaults this to `true`,
    # which copies the whole nixpkgs source tree in so that `<nixpkgs>` and `nix-env -iA nixos.x`
    # work for a human at the console. Measured [B.5]: 188 MB of source costing **512 MB of the
    # shipped disk**, because it is 52,665 mostly-tiny .nix files and every one of them rounds up
    # to a 4 KiB block.
    #
    # ⚠️ IT IS INVISIBLE TO THE CLOSURE. The channel is not referenced by `system.build.toplevel`,
    # so `nix path-info -S` on the system says 982 MB and is RIGHT while the artifact a stranger
    # downloads is 1691 MB. Measuring the closure is not measuring the download; B.5 slimmed the
    # closure by 631 MB and did not touch this at all.
    #
    # Nothing in the product reads it -- the agent updates by handing `nix-env --set` an explicit
    # store path and running switch-to-configuration, which never evaluates an expression. Its only
    # purpose was console convenience, and [B.5] had already disabled most of that without meaning
    # to: `setNixPath = false` leaves the guest with no `nix-path`, nix 2.34's compiled-in default
    # for it is EMPTY (checked, not assumed), so `<nixpkgs>` already resolved to nothing and
    # `nix-shell -p` / `nix-build '<nixpkgs>'` already failed. The one surviving user was
    # `nix-env -iA nixos.<attr>`, which reads ~/.nix-defexpr directly. Half a gigabyte for one
    # command on a box whose whole doctrine is dumb hands.
    #
    # WHAT THIS COSTS: OFFLINE package installation at the console. Networked rescue still works
    # through flakes by full URL (`nix shell github:NixOS/nixpkgs/nixos-26.05#tcpdump`); guest WAN
    # is a standing product requirement, and a node with no network is one you are reaching through
    # the host anyway. If offline rescue is ever wanted, buy it back as a few named tools in
    # environment.systemPackages -- tens of MB, not 512.
    copyChannel = false;
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
    # THE SIZE POLICY (measured 2026-08-06 on a real install, not guessed). A node running Home
    # Assistant uses 5.1 GB of this disk: 2.4 GB OS closure + 2.7 GB for the service image in
    # podman storage + ~25 MB logs. The number that matters is not that, though — it is what an
    # UPGRADE needs on top, because self-undoing updates are the product:
    #   +2.7 GB   a service image upgrade holds the new image beside the old one
    #   +0.5-1.5  an OS upgrade stages a second system generation (incremental; the store shares)
    # So 8 GiB (5.1 used, 2.9 free) installs fine and then cannot upgrade the thing it installed —
    # the failure would land exactly where a rollback is supposed to save you. 16 GiB leaves ~11 GB
    # free: both upgrades at once, with room for a second service.
    #
    # It is HEADROOM, NOT FOOTPRINT: qcow2 is sparse, so the published artifact and the download
    # are unchanged (2.56 GB actual) and the host allocates only what the guest writes. The host
    # side of this policy is the report card's free-space gate (a thin disk still has to be backed
    # by something) and the thick-allocated data volume in install.sh.
    diskSize = 16384;
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
