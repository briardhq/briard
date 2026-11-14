# A standalone bootable qcow2 of the guest image, for the agent QEMU to boot via -drive:
# configuration.nix (the framework-boot guest, no bootloader or root device) plus grub, a
# partitioned root and virtio drivers, baked by make-disk-image. The OS moves only by the host
# swapping this whole image for the next release (see bootModule), never by switching generations.
#
# stageImages (default none): extra app image tarballs baked in and loaded into podman at boot
# (briard.stagedImages); an image must be resident before a manifest is installed against it,
# since nothing on the failover path may pull. For rigs and lab disks with no registry.
# guestAgentEnv (default none): extra environment on the briard-deadman unit, e.g. the deadman
# test bakes a short BRIARD_DEADMAN so the reflex fires in seconds.
# commonModules (default none): folded into the one generation. A different OS is a different
# image, built with a different `commonModules` delta: the lab OS demos build each target as the
# product image plus one delta module, and a rig can bake a file in the same way.
{ nixpkgs, pkgs, overlay, stageImages ? [ ], guestAgentEnv ? { }
, commonModules ? [ ]
  # The build id stamped into the firmware and /etc/briard-release. nixosTest/outputs.nix passes
  # the guest build version (flake.nix guestVersion); callers that pass none (the lab disks) get
  # the dev default.
, agentVersion ? "0.0.0-dev" }:
let
  lib = nixpkgs.lib;
  # The one briard binary this image bakes: the push protocol, nothing else. The guest agent is
  # pushed by the host like the web units, so an edit to it does not move the image inputs hash
  # or republish a 400 MB guest chain (flake.nix guestInputPackages).
  briardFirmware = pkgs.callPackage ../agent/package.nix { subPackage = "agent/cmd/briard-guest-firmware"; version = agentVersion; };

  # The bootable-guest modules: what this image adds to configuration.nix (grub, the read-only
  # root, the state disk, the consoles and the guest units).
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
      # A placeholder: the agent sets the node id, which the DRBD .res on-blocks name, at every
      # bring-up (sys.hostname).
      networking.hostName = "guest";
      # An appliance image: one generation, no nix at runtime. The OS moves only by image swap,
      # so nothing here ever stages, switches or collects a closure, and `nix` itself is not
      # installed (taking the daemon, the store tooling and their closure out of every
      # household). The store paths are still a NixOS system; only the machinery to change them
      # is gone.
      nix.enable = false;
      boot.loader.grub = {
        enable = lib.mkForce true;
        device = "/dev/vda";
        # Grub speaks on ttyS0, the only console (boot.kernelParams points the kernel there too):
        # a bootloader that fails silently is a node simply "down" with no evidence. There is
        # exactly one generation to boot, so grub has no decision to make.
        extraConfig = ''
          insmod serial
          serial --unit=0 --speed=115200
          terminal_output --append serial
        '';
      };
      # A read-only OS: the guest writes only where the host has paid for it, never to its OS
      # disk. The root is a small tmpfs holding mountpoints, a few symlinks and the writable /etc
      # layer (machine-id, resolv.conf, localtime, LVM metadata backups: a few KB, measured); the
      # image partition is mounted read-only with only its store bound in; every other runtime
      # write lands on the state disk below. The cap turns an unplanned writer into ENOSPC inside
      # the guest, never a slow RAM leak; tmpfs costs only what is written, so the cap is a
      # ceiling, not a reservation.
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

      # The state disk: every byte the guest writes at runtime, in two classes on one filesystem.
      # Persistent, a closed, short list where adding to it is a design decision: podman storage
      # (the app images, content-addressed and digest-pinned by the quadlets; re-pulling gigabytes
      # after every restart would be pointless), the journal (the guest forensics; the host console
      # capture covers the boot, not the day) and the deadman backoff (which exists to survive the
      # reboots the deadman causes). Scratch, /var and /tmp, emptied at every boot: everything else
      # is re-derived from the host or the volume at bring-up, so nothing in it (the dressed
      # binaries, a pull's staged layers, systemd state) may outlive the boot that wrote it.
      #
      # The guest formats it on first boot when it finds no filesystem, so the host needs no
      # mkfs. Every guest has one (the install names it, STATE_DISK in agent/host/install.go, and
      # every rig that boots this image passes STATE_DISK), so a missing disk fails the boot
      # rather than finding somewhere else to write.
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
      # The boot wipe is a destructive act, so it is gated on a positive answer: the state disk
      # device is what is mounted there. A failed mount leaves a directory on the tmpfs root, the
      # check says no and the unit fails; it never removes anything it could not identify.
      # Removal is by name, of the previous boot directory renamed aside: a fresh `scratch` is
      # made empty, never emptied, and a power cut mid-removal leaves only `scratch.old`, which
      # the next boot removes first.
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
      # The build this image is, readable on the console: for the product image
      # `guest-build.<inputs hash>` (a function of the inputs, never of the commit, so an
      # unchanged image is the same build); a rig disk carries the rig version. The release id
      # (`vm.<date>.<inputs>`) is a channel fact the manifest names and the node record follows;
      # the image does not know which release it was published as, as an OCI image does not
      # know its tag.
      environment.etc."briard-release".text = agentVersion + "\n";
      boot.kernelParams = [
        "console=ttyS0" # serial console for debugging
        # The machine-id comes from the VM DMI product UUID, which the host derives from the node
        # name; systemd does not take it unasked (the id would be random). With no -uuid, qemu
        # gives an all-zero UUID, which is rejected, and systemd falls back to a random id.
        "systemd.machine_id=firmware"
        "net.ifnames=0" # predictable ethN in -device order (the service NIC, the VIP, is eth2)
      ];
      # Forward the journal to ttyS0 so the captured serial log shows systemd + drbd-reactor
      # activity during an upgrade, and bound it by size and by age, whichever comes first. It
      # lives on the state disk, so it outlives every relaunch (the point, for forensics) and each
      # boot appends a full boot; the journald default cap (10 % of the filesystem, up to 4 GB)
      # would let a node that reboots often fill the disk its container images share with logs
      # nobody reads. 128 MB keeps the last stretch of a busy node; seven days is the window a
      # support question is ever about. journald deletes whole archived files, never entries, so
      # MaxFileSec starts a new file daily: without it a quiet node can sit in one file for a month
      # and "seven days" means "seven days plus however long that file has been open".
      services.journald.extraConfig = ''
        ForwardToConsole=yes
        MaxLevelConsole=info
        SystemMaxUse=128M
        MaxRetentionSec=7day
        MaxFileSec=1day
      '';
      # No core dumps: a crash is logged to the journal (process, signal, stack summary) and its
      # core discarded, never written; a core is as large as the process, lands on a state disk
      # nobody sized for it, and nobody here reads one. Done through systemd-coredump, not by
      # disabling it: `systemd.coredump.enable = false` sets kernel.core_pattern to "core", and
      # the kernel then writes the core into the crashing process working directory, wherever.
      systemd.coredump.settings.Coredump = {
        Storage = "none";
        ProcessSizeMax = 0;
      };

      # The debug console: ttyS1, connected to nothing until someone opens it. The host gives
      # every guest a second serial port backed by the qemu `null` chardev (platform.serialArgs):
      # a port that exists, so the getty below can bind to it, on a device that discards writes
      # and never delivers a byte. `briard debug shell` swaps that backend for a unix socket over
      # QMP (chardev-change) and back on exit, so a normal node runs this getty against /dev/null
      # for its whole life, "open" only while a root operator on the host holds it open; every
      # launch starts the port null-backed again, so a relaunch disarms it too.
      #
      # Not a two-way ttyS0: ttyS0 is the capture (kernel, systemd and the whole journal) that
      # `briard logs` reads, usually the only account of a boot that went wrong; a shell there
      # would type into a log stream and put a login banner in the middle of the evidence.
      #
      # Autologin with no password anywhere grants nothing not already held: the only route to the
      # port is a socket in the QMP directory, which the agent creates 0700 root
      # (platform.secureQMPDir), on a host whose root already owns the guest disk, its qemu process
      # and the binary-push channel. A password would be a shared secret baked into a public image,
      # guarding a door its only possible holder is already through. Nor is it reachable from the
      # network or the cloud: QMP is a local unix socket, and nothing here rides the directive
      # plane.
      #
      # It is not a supported way to operate this appliance, so nothing advertises it (`briard
      # debug shell` is absent from the CLI help on purpose). The containment is not the lock but
      # that the guest is disposable: the OS moves by image swap and `briard rescue` rebuilds it
      # from the image, so anything hand-edited here is erased at the next update. Only the data
      # volume survives, and that is replicated and snapshotted.
      services.getty.autologinUser = "root";
      # Instantiated from the upstream template by a Wants= link, which adds no unit and overrides
      # nothing. Declaring `systemd.services."serial-getty@ttyS1"` would break it: NixOS renders a
      # named instance as its own unit file, shadowing `serial-getty@.service` for that instance
      # and taking the template ExecStart (the agetty invocation, --autologin included) with it.
      # The template carries `BindsTo=dev-%i.device`, so this is self-gating: launched without a
      # second serial port, /dev/ttyS1 never appears and the getty is never started (no failure,
      # no respawn).
      systemd.targets.getty.wants = [ "serial-getty@ttyS1.service" ];
      # The ttyS0 getty is masked because ttyS0 is a file: with autologin on, the template would
      # spawn a root shell against the capture, writing prompts into the log `briard logs` prints
      # and reading input that can never arrive. A login prompt in the log is not the way in.
      systemd.services."serial-getty@ttyS0".enable = false;

      # Exactly one NIC in this guest does DHCP, and it is not this one. The four are fixed and
      # only one faces a network we do not own: eth0 is the qemu SLIRP user-net (the WAN path for
      # OCI pulls), eth1 the system/DRBD NIC (this node IP, statically addressed by the agent),
      # eth3 the private guest<->host witness link (static), and eth2 carries the VIP, the one
      # address that belongs to the household LAN and the only one worth asking anybody for.
      #
      # The eth0 "DHCP server" is qemu itself, on a synthetic network whose addressing we hand it
      # (platform/qemu.go pins net/host/dns rather than leaning on its defaults), so a lease buys
      # a constant we already know. A general-purpose network manager suits a vanilla OS that must
      # cope with whatever it is plugged into; this guest is neither vanilla nor surprised by its
      # own NICs. No system dhcpcd also removes a defect rather than working around one: with one
      # running, the briard-vip per-interface invocation for eth2 never becomes an instance, it
      # forwards its argv to that master as a control command, and a master with eth2 in
      # denyInterfaces never runs DHCP for it, so the node silently refuses to be primary.
      networking.useDHCP = false;
      networking.interfaces.eth0.ipv4.addresses = [
        { address = "10.0.2.15"; prefixLength = 24; }
      ];
      networking.defaultGateway = {
        address = "10.0.2.2";
        interface = "eth0";
      };
      networking.nameservers = [ "10.0.2.3" ]; # SLIRP's resolver, forwarded to the host's

      # eth3, the private host<->guest link, is addressed by the agent, not baked here, and its
      # address is pure substrate: nothing dials it. The reboot gate answers at this node IP, the
      # host routes the VIP `via` that node IP, and both ends pin a permanent neighbour entry so
      # neither ARPs across the link (agent/platform/route.go, net.configure). It carries an
      # address at all for one measured reason: avahi joins the IPv4 mDNS group on an interface
      # only if it has a v4 address. Without one this NIC answers mDNS over IPv6 alone, the far end
      # is a stranger host that may have v6 off, and the name half of the private link breaks
      # silently: the household machine cannot find its own node while everything else works
      # (install-macvtap runs with v6 disabled precisely so nothing passes for a reason we do not
      # control).
      #
      # Not baked, although the host reads the reboot gate when the control channel is dead and an
      # agent-assigned address looks "reliably absent in the one failure it exists to serve". It
      # is not, because of `-no-reboot`: the host reads the gate only for a VM that is running but
      # mute, whose bring-up completed and so whose node IP the agent already set. A guest that
      # reboots itself with no agent to reconfigure it never reaches the gate: its unit ends and
      # the host relaunches the stopped unit (which runs bring-up) instead of asking a gate
      # (agent/host/guestrecover.go).

      # drbd.conf includes the .res files the agent drops at runtime, on tmpfs: the `.res` is
      # node-scoped, the host re-derives it at every bring-up (from cfg.Resource, which the mesh
      # cache durably holds even for a runtime pairing), and only a copy that outlives the agent
      # that wrote it can be stale. /etc/drbd.conf stays put: drbdadm looks for that one file at
      # a path we do not choose, and it is the pointer, not the state. (The framework drbd-*
      # tests declare both halves themselves, via lib.nix.)
      environment.etc."drbd.conf".text = ''include "/run/briard/drbd.d/*.res";'';
      systemd.tmpfiles.rules = [
        "d /run/briard 0755 root root -" # the deadman contact stamp + kmsg cursor live here too
        "d /run/briard/drbd.d 0755 root root -"
      ];

      # The in-guest control agent: opens the virtio-serial port and serves the host
      # bring-up/observe/upgrade verbs; PATH carries the tools those verbs shell out to.
      # (reactor.pause/resume use `systemctl` on drbd-reactor.service; the daemon runs from its
      # own unit, and only reactor.evict needs drbd-reactorctl here.)
      systemd.services.briard-guest-agent = {
        description = "Briard in-guest control agent (serves the host over virtio-serial)";
        wantedBy = [ "multi-user.target" ];
        after = [ "systemd-tmpfiles-setup.service" ];
        path = [
          pkgs.drbd # drbdadm/drbdsetup, for the drbd.* verbs
          pkgs.drbd-reactor # drbd-reactorctl, for reactor.evict — the planned handover
          pkgs.systemd # systemctl, for app.* / reactor.* / os.*
          pkgs.coreutils # readlink, for os.system
          pkgs.btrfs-progs # btrfs for data.member/restore, mkfs.btrfs for the one-time format
          # compsize, for storage.usage: an app's current data on disk, each extent once -- what du
          # cannot say on a compressed btrfs, where it reports the bytes before compression
          pkgs.compsize
          pkgs.iproute2 # ip, for net.configure (the system/DRBD NIC)
          pkgs.lvm2.bin # dmsetup, for the storage-seam telemetry
          pkgs.util-linux # blockdev, for storage.grow: the state disk's size as the kernel sees it
          pkgs.e2fsprogs # resize2fs, for storage.grow: the state disk's filesystem grown online
          # restic, for data.backup (the household repository, served by the host over REST). The
          # binary only: nixpkgs wraps it with rclone and openssh on PATH for backends we never
          # use, +115 MB of closure. Copying the wrapped-over binary fails closed: a nixpkgs that
          # stops wrapping has no `.restic-wrapped`, and the build stops. ~11 MB of download
          # (measured, zstd -19); stripping would save 0.7 MB of that.
          (pkgs.runCommand "restic-${pkgs.restic.version}" { } ''
            install -Dm755 ${pkgs.restic}/bin/.restic-wrapped $out/bin/restic
          '')
          # The module podman, not `pkgs.podman`, which would ship a second, differently-wrapped
          # copy of the runtime (configuration.nix explains).
          config.virtualisation.podman.package # podman, for the renderer + app.* verbs
        ];
        # Restart=always, not on-failure: the guest agent serves one host connection, then
        # ServeFrames (agent/guestfirmware/wire.go) returns nil on the clean EOF of a host
        # disconnect and `run --guest` exits 0. The reconnect design (host.go) needs it back on
        # the port for the next host connection, which a genuine disconnect (host-agent restart
        # -> self-update, or a re-adopt) produces; `on-failure` would not restart a clean exit,
        # leaving the port dead and the new host handshake blocked. StartLimit is off so this
        # critical channel never permanently gives up. A virtio-serial read does not simply block
        # while no host is connected: with the host end gone for good the reopened port returns
        # EOF at once, so Restart=always alone spins ~48x in 30s; both mains pause before exiting
        # on a clean EOF (hostAbsentPause, 5 s, in each main.go), making the restart loop a slow
        # poll.
        startLimitIntervalSec = 0; # [Unit] section: never permanently give up on this channel
        serviceConfig = {
          # Through the pivot (pivot.nix): the pushed agent when there is one, else the baked
          # firmware, the one binary this image carries, serving only the handshake, the three
          # push verbs and os.poweroff. This unit is the one an activation restarts; its start is
          # the verdict on the web units: trial verdict, then the port, then the one commit of
          # the whole pushed set, and only then READY (the mains). ⚠️ Deliberately no ExecStartPost:
          # a commit systemd ran after READY would run after the first host bring-up verbs too,
          # and those start units that exec the committed path. The agent commits itself
          # (agent/guestfirmware/bin.go BinCommit).
          Type = "notify";
          ExecStart = "${config.briard.pivot.exec}/bin/briard-bin-exec briard-guest-agent ${briardFirmware}/bin/briard-guest-firmware run --guest";
          Restart = "always";
          RestartSec = 1;
        };
      };

      # The host-agent deadman as its own long-running systemd unit, decoupled from the
      # per-connection guest agent (which crash-loops while the host is down, so an in-process timer
      # would keep resetting). It watches the contact stamp the guest agent bumps and, once the host
      # agent is silent past T_deadman, reboots the guest, gated and graceful. guestAgentEnv
      # carries a short BRIARD_DEADMAN for the deadman test.
      #
      # It also serves that gate to the host at this node IP (BRIARD_GATE_ADDR), the only way the
      # host rung can avoid power-cycling a node whose departure would cost a peer its quorum:
      # every other way of asking rides the channel whose death is the trigger. BRIARD_GATE_ADDR
      # is a port with no address, so the gate answers on whatever this node holds: the host
      # dials the node IP, agent-assigned and flock-scoped, which the image cannot know. Binding
      # wide is safe here as for no other listener: the gate reads nothing from a connection
      # (accept, write one line, close), so there is no parser to get wrong.
      #
      # ⚠️ Still a posture worth naming: the system subnet rides the LAN L2, so addressing does not
      # hide the gate. A stranger on the wire can read whether this node thinks a reboot is safe,
      # never set it (the verdict comes from the deadman evaluation, never the connection); a
      # flood makes the host read "unreachable", which it treats as allowed, so it removes the
      # guard rather than holding it shut.
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
        # ⚠️ It runs the pushed agent, not the firmware, and not through the picker: the picker
        # arm flag and `.ran` marker are keyed by binary name, so a second unit going through it
        # would consume the flag the trial belongs to. Naming the committed path directly means
        # it cannot start until the guest is dressed, which is correct and self-healing:
        # `Restart=always` with no start limit retries every 2 s from boot until the first commit
        # lands the binary, and the deadman is disarmed before the first host contact anyway
        # (deadman.Monitor: no contact yet this boot never fires), so it could do nothing sooner.
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
        # The denylisted module tree is an image fact: it pins the initrd to what this
        # qemu machine has (virtio disks, NICs, console) and drops what it cannot have. Test VMs
        # that build configuration.nix on their own hardware do not import it.
        ./modules.nix
        bootModule
      ] ++ commonModules ++ extraModules;
    };

  # The one generation this image boots, with any pre-staged app images baked in and warmed at
  # boot. No second OS generation is baked or fetched (see commonModules in the header).
  sys = mkGuest [
    {
      briard.stagedImages = stageImages;
    }
  ];

  image = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
    inherit lib pkgs;
    config = sys.config;
    # Do not bake the nixpkgs channel into the image. make-disk-image defaults this to `true`,
    # copying the whole nixpkgs source in so `<nixpkgs>` and `nix-env -iA nixos.x` work at the
    # console. Measured: 188 MB of source costing 512 MB of the shipped disk, because its 52,665
    # mostly-tiny .nix files each round up to a 4 KiB block.
    #
    # ⚠️ It is invisible to the closure: `system.build.toplevel` does not reference the channel,
    # so `nix path-info -S` correctly says 982 MB while the artifact a stranger downloads is
    # 1691 MB. Measuring the closure is not measuring the download; slim.nix cuts the closure and
    # does not touch this at all.
    #
    # Nothing in the guest could read it: `nix` is not installed (nix.enable = false, above) and
    # the OS moves only by image swap, which evaluates no expression. Its only use would be
    # console convenience on a box whose doctrine is dumb hands, and without nix there is no
    # package installation at the console anyway, so it costs nothing usable. Rescue tools, if
    # ever wanted, come back as a few named tools in environment.systemPackages: tens of MB, not
    # 512.
    copyChannel = false;
    format = "qcow2";
    partitionTableType = "legacy";
    # "auto": the closure plus the make-disk-image margin, nothing more, because the guest never
    # writes this disk (read-only partition; every runtime write lands on the state disk,
    # bootModule above). An app
    # image a test bakes in (stageImages) is a store path in the closure, so "auto" grows to fit
    # it, and `podman load` puts it on the state disk.
    diskSize = "auto";
    label = "nixos";
  };
in
# `system` is the image's toplevel, surfaced beside it (a passthru via //, so
# `${guestDisk}/nixos.qcow2` and `nix build` still work): the closure the guest manifest names
# and the host proves the booted guest against after an image swap. Deliberately not
# `nixosConfigurations.guest`, the framework-boot variant (no bootloader, no briard-agent units)
# that no field guest ever runs. Building this attr builds only the toplevel, not the qcow2.
image
// { system = sys.config.system.build.toplevel; }
