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
  # The release id stamped into the guest's firmware. Defaulted so every existing caller (the
  # lab fleet disks, the test variants) keeps building unchanged; flake.nix passes the real one.
, agentVersion ? "0.0.0-dev" }:
let
  lib = nixpkgs.lib;
  # THE ONE BRIARD BINARY THIS IMAGE BAKES: the push protocol, and nothing else. The
  # guest AGENT is pushed by the host like the doors are, so an edit to it does not move this
  # image's inputs hash and does not republish a 400 MB guest chain (flake.nix guestInputPackages).
  briardFirmware = pkgs.callPackage ../agent/package.nix { subPackage = "agent/cmd/briard-guest-firmware"; version = agentVersion; };

  # Common bootable-guest modules, shared by the good and broken generations so the
  # broken one is a minimal, honest delta (only the service env differs).
  bootModule =
    { config, lib, modulesPath, ... }:
    let
      # A directory of the state disk bound into the tree, mounted in the initrd like the disk.
      stateBind = sub: after: {
        device = "/briard-state/${sub}";
        fsType = "none";
        options = [ "bind" ];
        depends = [ "/briard-state" ] ++ after;
        neededForBoot = true;
      };
    in
    {
      imports = [ "${modulesPath}/profiles/qemu-guest.nix" ]; # virtio_blk/pci/console in initrd
      networking.hostName = "guest"; # DRBD .res on-block name (matches the driver's NODE)
      # THE GUEST IS AN APPLIANCE IMAGE: one generation, no nix at runtime. The OS
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
      # A READ-ONLY OS: THE GUEST WRITES ONLY WHERE THE HOST HAS PAID FOR IT. Nothing the guest
      # runs writes to its OS disk. The root is a small tmpfs holding mountpoints, a few
      # symlinks and /etc's writable layer (machine-id, resolv.conf, localtime, LVM's metadata
      # backups -- a few KB, measured); the image's own partition is mounted read-only and only
      # its store is bound in. Every other runtime write lands on the state disk below. The cap
      # turns a writer nobody planned for into ENOSPC inside the guest, never a slow RAM leak --
      # tmpfs costs only what is written, so the cap is a ceiling and not a reservation.
      fileSystems."/" = lib.mkForce {
        device = "none";
        fsType = "tmpfs";
        options = [ "mode=0755" "size=16M" ];
      };
      # `noload`: a read-only mount must not replay a journal it cannot write back.
      fileSystems."/nix/.ro-disk" = {
        device = "/dev/disk/by-label/nixos";
        fsType = "ext4";
        options = [ "ro" "noload" ];
        neededForBoot = true;
      };
      fileSystems."/nix/store" = {
        device = "/nix/.ro-disk/nix/store";
        fsType = "none";
        options = [ "bind" "ro" ];
        depends = [ "/nix/.ro-disk" ];
      };
      # /etc is an overlay of the generation's own image with its writable layer on the tmpfs root,
      # so there is no activation script rewriting it at every boot; users come from userborn.
      boot.initrd.systemd.enable = true;
      system.etc.overlay.enable = true;
      services.userborn.enable = true;

      # THE STATE DISK: every byte the guest writes at runtime, in two classes on one filesystem.
      # PERSISTENT, a CLOSED and short list where adding to it is a design decision: podman's
      # storage (the service images, content-addressed and digest-pinned by the quadlets --
      # re-pulling gigabytes after every restart would be pointless), the journal (the guest's own
      # forensics; the host's console capture covers the boot, not the day) and the deadman's
      # backoff (which exists precisely to survive the reboots the deadman itself causes).
      # SCRATCH, /var and /tmp, emptied at every boot: everything else the guest holds is
      # re-derived from the host or the volume at bring-up, so nothing in it may outlive the boot
      # that wrote it -- the dressed binaries, a pull's staged layers, systemd's own state.
      #
      # Formatted by the guest on first boot when it finds no filesystem, so the host needs no
      # mkfs. Every guest has one: install.sh creates it and every rig that boots this image passes
      # STATE_DISK, so a missing disk fails the boot rather than finding somewhere else to write.
      fileSystems."/briard-state" = {
        device = "/dev/disk/by-id/virtio-briard-state";
        fsType = "ext4";
        autoFormat = true;
        neededForBoot = true;
      };
      fileSystems."/var" = stateBind "scratch/var" [ ];
      fileSystems."/tmp" = stateBind "scratch/tmp" [ ];
      fileSystems."/var/lib/containers" = stateBind "containers" [ "/var" ];
      fileSystems."/var/log/journal" = stateBind "journal" [ "/var" ];
      fileSystems."/var/lib/briard-deadman" = stateBind "deadman" [ "/var" ];
      # THE BOOT WIPE, and it is a destructive act, so it is gated on a POSITIVE answer: the
      # state disk's device is what is mounted there. A failed mount leaves a directory on the
      # tmpfs root, the check says no, and the unit fails -- it never removes anything it could
      # not identify. Removal is BY NAME, of the previous boot's directory renamed aside: a
      # fresh `scratch` is made empty, never emptied, and a power cut mid-removal leaves only
      # `scratch.old`, which the next boot removes first.
      boot.initrd.systemd.extraBin.findmnt = "${pkgs.util-linux}/bin/findmnt";
      boot.initrd.systemd.extraBin.chattr = "${pkgs.e2fsprogs.bin}/bin/chattr";
      boot.initrd.systemd.services.briard-state-scratch = {
        description = "Briard: a fresh scratch directory on the state disk";
        requiredBy = [ "initrd-fs.target" ];
        before = [ "initrd-fs.target" "sysroot-var.mount" "sysroot-tmp.mount" ];
        after = [ "sysroot-briard\\x2dstate.mount" ];
        requires = [ "sysroot-briard\\x2dstate.mount" ];
        unitConfig.DefaultDependencies = false;
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          StandardOutput = "journal+console";
          StandardError = "journal+console";
        };
        script = ''
          set -eu
          s=/sysroot/briard-state
          dev=$(readlink -f /dev/disk/by-id/virtio-briard-state)
          src=$(findmnt -n -o SOURCE --mountpoint "$s")
          if [ -z "$dev" ] || [ "$src" != "$dev" ]; then
            echo "briard-state-scratch: $s is not the state disk (mounted: '$src', disk: '$dev') -- not wiping" >&2
            exit 1
          fi
          # Immutable flags are cleared first: NixOS makes /var/empty `chattr +i`, which `rm`
          # alone cannot remove -- and a wipe that fails stops every reboot in emergency mode.
          discard() {
            [ -e "$1" ] || return 0
            chattr -R -f -i "$1" || true
            rm -rf "$1"
          }
          discard "$s/scratch.old"
          if [ -e "$s/scratch" ]; then mv "$s/scratch" "$s/scratch.old"; fi
          discard "$s/scratch.old"
          mkdir -p "$s/scratch/var" "$s/scratch/tmp" "$s/containers" "$s/journal" "$s/deadman"
          chmod 1777 "$s/scratch/tmp"
        '';
      };
      # A weekly fstrim: the state disk is attached discard=unmap, so what the
      # guest deletes is given back to the host file only once something TRIMs it. Weekly is the
      # usual cadence; podman's churn is bursty and a sweep bounds the footprint at a week's peak.
      services.fstrim.enable = true;
      # The BUILD this image is, readable on the console. It is the version the
      # image was built with: for the product image that is `guest-build.<inputs hash>` -- a
      # function of the image's inputs, never of the commit, so an unchanged image is the same
      # build -- and a rig's disk carries the rig's version. The RELEASE id (`vm.<date>.<inputs>`) is a
      # channel fact: the manifest names it and the node's record follows it; the image itself
      # does not know which release it was published as, the way an OCI image does not know its tag.
      environment.etc."briard-release".text = agentVersion + "\n";
      boot.kernelParams = [
        "console=ttyS0" # serial console for debugging
        # The machine-id comes from the VM's DMI product UUID, which the host derives from the
        # node name: systemd no longer does that on its own (it fell back to a random
        # id on the first rig run), so it is asked to. With no -uuid (a rig that predates it)
        # qemu's all-zero UUID is rejected and systemd falls back to random, as before.
        "systemd.machine_id=firmware"
        "net.ifnames=0" # predictable eth0/eth1 (the tapped service NIC -> eth1, the VIP)
      ];
      # Forward the journal to ttyS0 so the captured serial log shows systemd +
      # drbd-reactor activity during an upgrade.
      #
      # AND BOUND IT, by size and by age, whichever comes first. The journal lives on the state
      # disk (above), so it outlives every relaunch -- the point, for forensics -- and each boot
      # appends a full boot's worth; journald's default cap (10 % of the filesystem, up to 4 GB)
      # would let a node that reboots often fill the disk its container images share with boot
      # logs nobody reads. 128 MB keeps the last stretch of a busy node; seven days is the window
      # a support question is ever about. journald deletes whole archived FILES, never entries,
      # so MaxFileSec starts a new file daily -- without it a quiet node can sit in one file for
      # a month and "seven days" means "seven days plus however long that file has been open".
      services.journald.extraConfig = ''
        ForwardToConsole=yes
        MaxLevelConsole=info
        SystemMaxUse=128M
        MaxRetentionSec=7day
        MaxFileSec=1day
      '';

      # THE DEBUG CONSOLE: ttyS1, and it is connected to nothing until someone opens it.
      #
      # The host gives every guest a SECOND serial port whose backend is qemu's `null` chardev
      # (platform.serialArgs) -- a port that exists, so the getty below can bind to it, wired to
      # a device that discards writes and never delivers a byte. `briard debug shell` swaps that
      # backend for a unix socket over QMP (chardev-change) and swaps it back on exit, so a
      # normal node runs this getty against /dev/null for its whole life and is "open" only
      # while a root operator on the host is holding it open. A relaunch disarms it too, by
      # construction: every launch starts the port null-backed again.
      #
      # WHY ttyS1 RATHER THAN MAKING ttyS0 TWO-WAY. ttyS0 is the capture -- kernel, systemd and
      # (just above) the whole journal -- which `briard logs` reads and which is usually the only
      # account of a boot that went wrong. A shell sharing that wire would be typing into a log
      # stream and would put a login banner in the middle of the evidence.
      #
      # AUTOLOGIN, WITH NO PASSWORD ANYWHERE, and it grants nothing that was not already held.
      # The only route to this port is a socket in the QMP directory, which the agent creates
      # 0700 root (platform.secureQMPDir), on a host whose root already owns the guest's disk,
      # its qemu process and the binary-push channel. A password here would be a shared secret
      # baked into a public image, guarding a door its only possible holder is already through.
      # It is also not network-reachable and not reachable by the cloud: QMP is a local unix
      # socket, and nothing about this rides the directive plane.
      #
      # WHAT IT IS NOT is a supported way to operate this appliance, which is why nothing
      # advertises it -- `briard debug shell` is absent from the CLI's help on purpose. The
      # containment is not the lock, it is that the guest is DISPOSABLE: the OS moves by
      # image swap and `briard rescue` rebuilds it from the image, so anything hand-edited in
      # this root is erased at the next update. Only the data volume survives, and that is
      # replicated and snapshotted.
      services.getty.autologinUser = "root";
      # Instantiated from the upstream template by a Wants= link rather than by declaring
      # `systemd.services."serial-getty@ttyS1"`, WHICH WOULD BREAK IT: NixOS renders a named
      # instance as its own unit file, shadowing `serial-getty@.service` for that instance and
      # taking the template's ExecStart (the agetty invocation, including the --autologin above)
      # with it. A Wants= link adds no unit and overrides nothing.
      #
      # The upstream template carries `BindsTo=dev-%i.device`, so this is self-gating: on a guest
      # whose host is too old to give it a second serial port, /dev/ttyS1 never appears, the
      # device unit is never active, and the getty is simply never started -- no failure, no
      # respawn. That is the case that makes the link safe to ship before every agent has the
      # host half.
      systemd.targets.getty.wants = [ "serial-getty@ttyS1.service" ];
      # ttyS0's getty is masked because ttyS0 is a FILE. With autologin on, the template would
      # otherwise spawn a root shell against the capture -- writing prompts into the log that
      # `briard logs` prints, reading input that can never arrive. Nothing was using it: the
      # login prompt it used to print into the log was never the way into this guest.
      systemd.services."serial-getty@ttyS0".enable = false;

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
      # It also removes a defect rather than working around one: with a system dhcpcd
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
      # off -- so the private link's name half would break silently, the household's own machine unable to
      # find its own node while everything else works (install-macvtap runs with v6
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

      # drbd.conf includes the .res files the agent drops at runtime. TMPFS: the
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
          pkgs.lvm2.bin # dmsetup, for the storage-seam telemetry
          # The MODULE's podman, not `pkgs.podman` — naming the latter ships a second,
          # differently-wrapped copy of the runtime (configuration.nix explains).
          config.virtualisation.podman.package # podman, for the renderer + service.* verbs
        ];
        # Restart=always (not on-failure): the guest agent serves ONE host connection
        # then Serve() returns nil on the clean EOF when the host disconnects (wire.go)
        # -- so `run --guest` exits 0. The reconnect design (host.go) needs it
        # back on the port for the *next* host connection, which a genuine disconnect
        # (host-agent restart -> self-update; or a re-adopt) produces. `on-failure`
        # would NOT restart a clean exit, leaving the port dead and the new host's
        # handshake blocked. StartLimit off so this critical channel never permanently gives up.
        #
        # This comment used to claim a virtio-serial read BLOCKS while no host is connected, so a
        # reopened port just waits and there is no flapping. That is true only of a BRIEF gap: with
        # the host end gone for good, the reopened port returns EOF on the first read and the exit
        # is immediate, so Restart=always spun ~48x in 30s. The agent now pauses before
        # exiting on a clean EOF (hostAbsentPause in main.go) -- the restart policy here is
        # unchanged, and correct; what was wrong was the assumption that made it free.
        startLimitIntervalSec = 0; # [Unit] section: never permanently give up on this channel
        serviceConfig = {
          # THROUGH THE PIVOT (pivot.nix): the agent the host pushed
          # when there is one, else the baked firmware -- which is the ONE binary this image
          # carries, and serves only the handshake, the three push verbs and os.poweroff. This
          # unit is the one an activation restarts, and its start is the verdict on the doors:
          # trial verdict, then the port, then the ONE commit of the whole pushed set, and only
          # then READY (the mains). ⚠️ NO ExecStartPost, and its absence is deliberate:
          # a commit systemd ran after READY ran after the host's first bring-up verbs too, and
          # those start units that exec the committed path. It is the agent's own job now.
          Type = "notify";
          ExecStart = "${config.briard.pivot.exec}/bin/briard-bin-exec briard-guest-agent ${briardFirmware}/bin/briard-guest-firmware run --guest";
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
      # image can know. Binding wide is safe here in a way it would not be for any
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
        startLimitIntervalSec = 0; # [Unit] section: it retries until the first dress lands the binary
        path = [
          pkgs.drbd # drbdsetup, for the reboot gate
          pkgs.systemd # systemctl reboot
        ];
        environment = { BRIARD_GATE_ADDR = ":7790"; } // guestAgentEnv;
        # ⚠️ IT RUNS THE PUSHED AGENT, NOT THE FIRMWARE, and NOT through the picker:
        # the picker's arm flag and `.ran` marker are keyed by BINARY NAME, so a second unit
        # going through it would consume the flag the trial belongs to. The committed path is
        # named directly, which means this unit cannot start until the guest has been dressed --
        # correct, and self-healing: `Restart=always` with no start limit retries every 2 s from
        # boot until the first commit lands the binary, and the deadman is disarmed before the
        # host's first contact anyway (deadman.Monitor: no contact yet this boot never fires), so
        # there is nothing it could have done in the meantime.
        serviceConfig = {
          ExecStart = "${config.briard.pivot.binDir}/briard-guest-agent run --deadman";
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
        # The denylisted module tree is an IMAGE fact: it pins the initrd to what this
        # qemu machine has (virtio disks, NICs, console) and drops what it cannot have. Test VMs
        # that build configuration.nix on their own hardware do not import it.
        ./modules.nix
        bootModule
      ] ++ commonModules ++ extraModules;
    };

  # The one generation this image boots, with any pre-staged service images baked in and
  # warmed at boot. There is no second OS generation any more, baked or fetched: a different OS
  # is a different IMAGE, built by importing this file with a different
  # `commonModules` delta.
  sys = mkGuest [
    {
      briard.stagedImages = stageImages;
    }
  ];

  image = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
    inherit lib pkgs;
    config = sys.config;
    # DO NOT BAKE THE NIXPKGS CHANNEL INTO THE IMAGE. make-disk-image defaults this to `true`,
    # which copies the whole nixpkgs source tree in so that `<nixpkgs>` and `nix-env -iA nixos.x`
    # work for a human at the console. Measured: 188 MB of source costing **512 MB of the
    # shipped disk**, because it is 52,665 mostly-tiny .nix files and every one of them rounds up
    # to a 4 KiB block.
    #
    # ⚠️ IT IS INVISIBLE TO THE CLOSURE. The channel is not referenced by `system.build.toplevel`,
    # so `nix path-info -S` on the system says 982 MB and is RIGHT while the artifact a stranger
    # downloads is 1691 MB. Measuring the closure is not measuring the download; the slimming pass cut the
    # closure by 631 MB and did not touch this at all.
    #
    # Nothing in the product reads it -- the agent updates by handing `nix-env --set` an explicit
    # store path and running switch-to-configuration, which never evaluates an expression. Its only
    # purpose was console convenience, and the slimming had already disabled most of that without meaning
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
    # "auto": the closure plus make-disk-image's margin, and nothing more, because the guest never
    # writes this disk -- its partition is mounted read-only and every runtime write lands on the
    # state disk (bootModule above). A service image a test bakes in (stageImages) is a store
    # path in the closure, so "auto" grows to fit it, and `podman load` puts it on the state disk.
    diskSize = "auto";
    label = "nixos";
  };
in
# `system` is the image's toplevel, surfaced beside it (a passthru via //, so
# `${guestDisk}/nixos.qcow2` and `nix build` still work): what the guest manifest names as the
# closure this image boots, and what the host proves the booted guest against after an image
# swap. It is deliberately not `nixosConfigurations.guest`: that is the
# framework-boot variant (no bootloader, no briard-agent units), a closure no field guest ever
# runs. Building this attr builds only the toplevel, not the qcow2.
image
// { system = sys.config.system.build.toplevel; }
