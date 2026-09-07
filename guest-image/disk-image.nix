# A standalone bootable qcow2 of the guest image, for the agent's QEMU to boot
# via -drive. The framework-boot guest (configuration.nix) has no
# bootloader/root device; here we add grub + a partitioned root + virtio drivers
# and bake it into a disk with make-disk-image. This is the *bootstrap* artifact;
# field updates are incremental NixOS generation switches, not re-imaging.
#
# The deliberately-broken generation that used to live here (`.brokenSystem`: the service
# container run with BRIARD_BROKEN=1) went with managed-upgrade.nix (c2-v). It was
# PAYLOAD-derived, which is what dated it: the OS upgrade it fed no longer touches the payload
# at all, and the lab rollback demo's broken generation breaks the FRONT DOOR instead — an ordinary delta on
# the shipped disk, fetched over the cache like any other target, needing no argument here.
#
# stageImages (default none) bakes extra service image tarballs into the disk and loads them
# into podman at boot (briard.stagedImages) — an image has to be resident before a manifest is
# installed against it, since nothing on the failover path may pull. Used by the fleet upgrade
# demo to pre-stage the target of a manifest rotation.
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
{ nixpkgs, pkgs, overlay, stageImages ? [ ], guestAgentEnv ? { }
, commonModules ? [ ]
  # The release id stamped into the guest's agent. Defaulted so every existing caller (the
  # lab fleet disks, the test variants) keeps building unchanged; flake.nix passes the real one.
, agentVersion ? "0.0.0-dev" }:
let
  lib = nixpkgs.lib;
  # The guest VM only ever runs `briard run --guest`, so build the trimmed guest-only binary
  # Guest-only build: no host subsystems / net/http / TLS in the shipped guest closure.
  briardAgent = pkgs.callPackage ../agent/package.nix { tags = [ "guest" ]; version = agentVersion; };

  # Common bootable-guest modules, shared by the good and broken generations so the
  # broken one is a minimal, honest delta (only the service env differs).
  bootModule =
    { config, lib, modulesPath, ... }:
    {
      imports = [ "${modulesPath}/profiles/qemu-guest.nix" ]; # virtio_blk/pci/console in initrd
      networking.hostName = "guest"; # DRBD .res on-block name (matches the driver's NODE)
      # THE GUEST IS AN APPLIANCE IMAGE ([B.86h]): one generation, no nix at runtime. The OS
      # moves only by the host swapping this image for the next release's, so nothing in here
      # ever stages, switches or collects a closure -- `nix` itself is not installed, which
      # also takes the daemon, the store tooling and their closure out of every household.
      # The store paths are still a NixOS system; only the machinery to CHANGE them is gone.
      nix.enable = false;
      boot.loader.grub = {
        enable = lib.mkForce true;
        device = "/dev/vda";
        # Let grub speak on ttyS0, the guest's only console (boot.kernelParams already points
        # the kernel there): a bootloader that fails silently is a node that is simply "down"
        # with no evidence. There is exactly one generation to boot, so grub has no decision
        # to make -- the boot selector that once lived here went with the closure path.
        extraConfig = ''
          insmod serial
          serial --unit=0 --speed=115200
          terminal_output --append serial
        '';
      };
      fileSystems."/" = lib.mkForce {
        device = "/dev/disk/by-label/nixos";
        fsType = "ext4";
      };
      # THE STATE DISK ([B.86g]). The OS disk is disposable -- the host discards it at will, a
      # rescue rebuilds it, [B.86h] swaps it for a new image -- so the guest keeps what a restart
      # must not cost on a separate node-local disk the host attaches by serial. The list is
      # CLOSED and short, and adding to it is a design decision: podman's storage (the service
      # images, content-addressed and digest-pinned by the quadlets -- re-pulling gigabytes after
      # every restart would be pointless), the journal (the guest's own forensics; the host's
      # console capture covers the boot, not the day) and the deadman's backoff (which exists
      # precisely to survive the reboots the deadman itself causes). Everything else the guest
      # holds is re-derived from the host or the volume at bring-up.
      #
      # Formatted by the guest on first boot when it finds no filesystem, so the host needs no
      # mkfs. `nofail`: a rig that predates the disk boots exactly as before, with the three
      # paths on the OS disk; a production node always has it (install.sh creates it). The
      # three paths are BIND-MOUNTED by one unit rather than listed in fstab, because a bind in
      # fstab whose source is absent is a failed mount unit on every disk-less rig, while a
      # unit conditioned on the disk being mounted is simply skipped. (A rig that bakes service
      # images into /var/lib/containers AND attaches a state disk would hide them under the
      # bind; none does, and the shipped image bakes nothing.)
      fileSystems."/var/lib/briard-state" = {
        device = "/dev/disk/by-id/virtio-briard-state";
        fsType = "ext4";
        autoFormat = true;
        # 5s: on a node without the disk (a rig that predates it) this is what local-fs.target
        # waits before the mount gives up and the layout unit below is skipped.
        options = [ "nofail" "x-systemd.device-timeout=5s" ];
      };
      systemd.services.briard-state-layout = {
        description = "Briard: put the guest's persistent paths on its state disk";
        wantedBy = [ "local-fs.target" ];
        after = [ "var-lib-briard\\x2dstate.mount" ];
        requires = [ "var-lib-briard\\x2dstate.mount" ];
        # Before the journal is flushed to /var/log/journal and before anything that uses
        # podman or the deadman's state can start.
        before = [ "local-fs.target" "systemd-journal-flush.service" "shutdown.target" ];
        conflicts = [ "shutdown.target" ];
        unitConfig = {
          ConditionPathIsMountPoint = "/var/lib/briard-state";
          # An EARLY-BOOT unit, and this line is load-bearing: a service with default
          # dependencies is After=sysinit.target, sysinit is After=local-fs.target, and this
          # unit is Before=local-fs.target -- an ordering cycle, which systemd breaks by deleting
          # a job. Measured on the first rig run: the deleted job was systemd-tmpfiles-setup,
          # avahi's runtime directory was never created, the mDNS chain member failed, the
          # promotion failed, and the front door never answered. Nothing pointed at this unit.
          DefaultDependencies = false;
        };
        path = [ pkgs.coreutils pkgs.util-linux ];
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
        };
        script = ''
          set -eu
          for p in containers journal deadman; do
            mkdir -p "/var/lib/briard-state/$p"
          done
          mkdir -p /var/lib/containers /var/log/journal /var/lib/briard-deadman
          mount --bind /var/lib/briard-state/containers /var/lib/containers
          mount --bind /var/lib/briard-state/journal /var/log/journal
          mount --bind /var/lib/briard-state/deadman /var/lib/briard-deadman
        '';
      };
      # A weekly fstrim: the state disk (and the overlay) are attached discard=unmap, so what the
      # guest deletes is given back to the host file only once something TRIMs it. Weekly is the
      # usual cadence; podman's churn is bursty and a sweep bounds the footprint at a week's peak.
      services.fstrim.enable = true;
      # The BUILD this image is, readable on the console. Since [B.86i] it is the version the
      # image was built with: for the product image that is `guest-build.<inputs hash>` -- a
      # function of the image's inputs, never of the commit, so an unchanged image is the same
      # build -- and a rig's disk carries the rig's version. The RELEASE id (`guest.<date>.<rev>`) is a
      # channel fact: the manifest names it and the node's record follows it; the image itself
      # does not know which release it was published as, the way an OCI image does not know its tag.
      environment.etc."briard-release".text = agentVersion + "\n";
      boot.kernelParams = [
        "console=ttyS0" # serial console for debugging
        # The machine-id comes from the VM's DMI product UUID, which the host derives from the
        # node name ([B.86g]): systemd no longer does that on its own (it fell back to a random
        # id on the first rig run), so it is asked to. With no -uuid (a rig that predates it)
        # qemu's all-zero UUID is rejected and systemd falls back to random, as before.
        "systemd.machine_id=firmware"
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
          pkgs.systemd # systemctl, for service.* / reactor.* / os.switch
          pkgs.coreutils # readlink, for os.system
          pkgs.btrfs-progs # btrfs for data.snapshot/restore, mkfs.btrfs for the one-time format
          pkgs.iproute2 # ip, for net.configure (the system/DRBD NIC)
          # The MODULE's podman, not `pkgs.podman` — naming the latter ships a second,
          # differently-wrapped copy of the runtime (configuration.nix explains; [B.5]).
          config.virtualisation.podman.package # podman, for the renderer + service.* verbs
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
          # THROUGH THE PIVOT ([B.86j], pivot.nix): the binary the host pushed when there is one,
          # else the baked firmware. READY at listen (main.go), and the commit only after it --
          # with --release, because the guest agent is the LAST binary an activation restarts,
          # so its commit is what makes the handshake report the new bundle.
          Type = "notify";
          ExecStart = "${config.briard.pivot.exec} briard-guest-agent ${briardAgent}/bin/briard-agent run --guest";
          ExecStartPost = "${config.briard.pivot.commit} briard-guest-agent --release";
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
      ] ++ commonModules ++ extraModules;
    };

  # The one generation this image boots, with any pre-staged service images baked in and
  # warmed at boot. There is no second OS generation any more, baked or fetched: a different OS
  # is a different IMAGE ([B.86h]), built by importing this file with a different
  # `commonModules` delta.
  sys = mkGuest [
    {
      briard.stagedImages = stageImages;
      # One agent derivation for all three units that need it (briard-guest-agent,
      # briard-deadman, and briard-services' converge). configuration.nix defaults this to an
      # UNVERSIONED guest build for the nixosTests; letting that default stand here would put a
      # second agent in the shipped image's closure.
      briard.agentPackage = briardAgent;
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
    # the service image into the image at build time, where "auto" grows to fit it -- so the
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
# `system` is the image's toplevel, surfaced beside it (a passthru via //, so
# `${guestDisk}/nixos.qcow2` and `nix build` still work): what the guest manifest names as the
# closure this image boots, and what the host proves the booted guest against after an image
# swap ([B.86d]/[B.86h]). It is deliberately not `nixosConfigurations.guest`: that is the
# framework-boot variant (no bootloader, no briard-agent units), a closure no field guest ever
# runs. Building this attr builds only the toplevel, not the qcow2.
image
// { system = sys.config.system.build.toplevel; }
