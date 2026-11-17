# Briard VM unit image.
#
# The workload + DRBD 9 + drbd-reactor run *inside* this VM; the host agent runs outside it (the
# host/guest boundary). drbd-reactor's promoter drives the ordered failover unit {DRBD-primary →
# data mount → VIP → apps → reverse proxy → dashboard} on whichever node holds the DRBD primary
# role; a lone node runs no DRBD and the agent starts the same chain as a static target. The
# per-resource promoter rules arrive as a snippet in /run/briard/drbd-reactor.d (written by the
# agent in prod, by the harness in tests), so this image stays generic and the daemon is idle
# until one appears.
{ config, pkgs, lib, ... }:
let
  # ── The storage seam ─────────────────────────────────────────────────────────────────────────
  # The stack under the data volume's mount (/var/lib/briard) is `dataDisk -> LUKS -> PV -> VG -> LV -> DRBD -> btrfs`. The
  # VG exists for one reason: an LV can have its dm table reloaded under an open device, so
  # `pvmove` can move the backing onto an encrypted PV, or off one, while DRBD never closes its
  # device. A bare disk has no table to reload, so the seam had to exist before the encryption.
  # It is nearly free: a linear LV is dm-linear, one table line with the same target and
  # linear_map() a bare disk would have. Everything LVM adds sits outside the data path (a PV
  # label, a 1 MiB metadata area, userspace, udev), and it beats raw `dmsetup` because nothing
  # rebuilds a table at every boot and "which backing is this node on" lives on the disk, not in
  # host config.
  #
  # ⚠️ None of those names live here. The device, VG, LV and encryption mode arrive in the spec
  # the host writes to /run/briard/node-storage.json: storage policy is a node-scoped fact the
  # host holds and pushes at bring-up, and a unit whose only inputs are what it can read off the
  # machine has nowhere for a decision (the Adiantum opt-in, say) to arrive. The pushed agent
  # writes the unit and contributes every name and every choice. The one-time format and the
  # snapshots directory are the agent's too: the format runs in node storage, in the same branch
  # as the `lvcreate` that made the volume, with no marker (agent/guestagent/nodestorage.go), and
  # the snapshots directory is made at every promotion (agent/guestagent's snapshotsDir).
  # ⚠️ The TLS directory and the VIP's env file are not named here either: only units read them
  # (the reverse proxy's `-cert`/`-key`, briard-vip's EnvironmentFile), and the agent writes
  # those units with its own spellings (guestagent's tlsCertPath/tlsKeyPath and vipEnvPath), and
  # the scripts those units run are the agent's too (below).
  # ── The image's half of a unit the agent writes ──────────────────────────────────────────────
  #
  # The image contributes the closure; the pushed agent contributes the unit and the scripts it
  # runs. The image bakes exactly one binary and every other briard binary rides the host bundle.
  # A unit is mostly an ExecStart plus its binary's environment, so a unit baked here would move
  # on the image's cadence while its binary moves on the host's: a pushed agent asking its older
  # image for a unit that image does not define crash-loops the node with the household's app
  # gone. The agent writes its units into /run/systemd/system at every start (the only place it
  # can: NixOS makes /etc/systemd/system a read-only store path), so a unit can never be older
  # than the binary that wrote it, and the defect class is unrepresentable rather than merely
  # detectable. The shell those units run (the VIP's claim and release, the promotion hold) is
  # the same kind of thing and rides the same way: the agent embeds it and writes it beside the
  # units (agent/guestagent/scripts), so a change to it moves the agent, never this image.
  #
  # What stays here is what a pushed binary cannot carry: the store paths. A unit's PATH is a
  # closure reference, and an agent that guessed one would fail at block-device time, the worst
  # moment this product has. So the image publishes one profile at a fixed path and the agent
  # names that path: no manifest, no schema, nothing to version between the two halves. Whole
  # packages, deliberately not named commands: a pushed agent that starts calling `lvs` must not
  # need a new image, and naming commands would put the skew back one level down.
  #
  # ⚠️ An image that predates the profile has none at all, and that is the good failure: the agent
  # stats this directory before it renders and refuses to serve without it, so its trial fails,
  # the committed agent comes back, and the node keeps the release it had. This refuses the
  # upgrade; stopping the host from offering it is the release floor's job, still wanted for
  # genuine on-disk migrations.
  guestTools = pkgs.buildEnv {
    name = "briard-guest-tools";
    paths = [
      # The packages an agent-written unit's PATH resolves against.
      pkgs.lvm2.bin # pvcreate/vgcreate/lvcreate/vgchange/vgs -- the `bin` output, already in this closure
      pkgs.cryptsetup
      pkgs.kmod # modprobe dm-mirror, which LVM cannot autoload here
      pkgs.drbd # drbdadm create-md / new-current-uuid, and drbdmeta for the lone-node probe
      pkgs.btrfs-progs # mkfs.btrfs, once, on the LV node storage just created
      pkgs.coreutils # install, test, rm; sleep, for the hold
      pkgs.util-linux # mount/umount/mountpoint/findmnt, the primary-storage half
      pkgs.iproute2 # ip, for the VIP
      pkgs.iputils # arping, for the gratuitous announcement
      pkgs.dhcpcd # the VIP's lease holder
      pkgs.nftables # nft, the VIP's reply routing
      pkgs.bash # the interpreter the agent's scripts name, at this profile's fixed path
      config.systemd.package # systemctl, journalctl
      # ⚠️ The baseline NixOS gives every unit for free, restated because nothing gives it to a
      # runtime unit: nixpkgs defaults `systemd.services.<n>.path` to coreutils + findutils +
      # gnugrep + gnused + systemd and appends each unit's own list. Without them an
      # agent-written unit would quietly narrow PATH on the promotion path, the one place a
      # missing command costs a household its house; with them an agent-written unit gets a
      # strict superset of what a NixOS-declared unit gets.
      pkgs.findutils
      pkgs.gnugrep
      pkgs.gnused
      # The module's podman, not `pkgs.podman` -- naming the latter ships a second,
      # differently-wrapped copy of the runtime.
      config.virtualisation.podman.package
      # And the one named tool a unit's ExecStart points at directly: the pivot's picker is frozen
      # (it runs before any pushed binary is trusted), so it is the image's, not the agent's.
      config.briard.pivot.exec # briard-bin-exec, the frozen picker the web units go through
    ];
    # lib/drbd for upstream's drbd-service-shim.sh, which the hold runs by its absolute path.
    pathsToLink = [ "/bin" "/sbin" "/lib/drbd" ];
  };
  # Paired with the Go const guestagent.defaultToolsBin, which is what an agent-written unit puts
  # on its PATH. Different languages, so no shared import; the agent-side comment names this back.
  toolsEtc = "briard/tools";
  # Where the agent drops drbd-reactor's promoter snippet. Tmpfs, and paired with the Go const
  # guestagent.reactorPath -- different languages, so no shared import; the agent-side comment
  # names this file back.
  reactorSnippetDir = "/run/briard/drbd-reactor.d";

in
{
  # What this appliance does not carry. Imported here, not in the callers, so the same slimming
  # applies to the shipped disk and to every nixosTest that boots this module: a fatter test guest
  # would prove nothing about the one strangers install. modules.nix (the denylisted module tree)
  # is deliberately not imported here: it pins the initrd to the virtio set the image boots with,
  # and the nixosTests that build this module mount their store over virtiofs (measured: three
  # nodes panicked in the initrd). It joins in disk-image.nix, where the machine is known.
  imports = [ ./slim.nix ./pivot.nix ];

  # The guest agent build a test node is pre-dressed with. No unit in the shipped image names it:
  # the image bakes the firmware alone, and every unit that needs the agent reaches it at its
  # committed path under the pivot's binDir, where the host's dress put it. A nixosTest machine
  # has no host, so nixosTest/lib.nix links this build in as if one had dressed it
  # (briard.pivot.preDressed), and those tests converge with the product's own code rather than a
  # harness stand-in. An option rather than a bare callPackage, so a caller wanting a versioned
  # build, or a stand-in, hands one in instead of getting a second agent derivation.
  options.briard.agentPackage = lib.mkOption {
    type = lib.types.package;
    default = pkgs.callPackage ../agent/package.nix { subPackage = "agent/cmd/briard-guest-agent"; };
    defaultText = lib.literalExpression "the briard-guest-agent build";
    description = "The briard-guest-agent build a test node is pre-dressed with, and that its shells invoke.";
  };

  # Image tarballs baked into this disk's closure and loaded into podman at boot. A node fact,
  # not an app one: an image has to be resident before anything renders against it, because
  # nothing on the failover path may pull. Used by the fleet's upgrade demo to pre-stage the
  # target of a rotation. Empty by default — the shipped image carries no workload.
  options.briard.stagedImages = lib.mkOption {
    type = lib.types.listOf lib.types.package;
    default = [ ];
    description = "Image tarballs pre-staged into local podman storage at boot.";
  };

  config = lib.mkMerge [ {
    system.stateVersion = "26.05";

    # No substituters and no baked cache key: the guest has no nix (disk-image.nix,
    # `nix.enable = false`), so nothing in it ever fetches a store path. An OS release reaches a
    # node as a whole signed image over the guest chain, verified by the release keyring the host
    # holds; the image is what a release is.

    # Answer the ACPI power button. systemd-logind listens for it, and nothing else here starts
    # it: it ships with no [Install] section and only a dbus alias, so on a box with no logins
    # nothing touches login1 and it stays down. The guest then ignores QEMU's `system_powerdown`
    # outright (measured, 60 s of silence), and every clean shutdown the host can ask for (an OS
    # upgrade's reboot leg, a host reboot, a UPS event) degrades to killing QEMU: a power cut, to a
    # machine whose whole job is not losing data. logind is already in the closure, and its
    # default HandlePowerKey=poweroff is what we want.
    systemd.services.systemd-logind.wantedBy = [ "multi-user.target" ];

    # DRBD 9: the out-of-tree module (the one with quorum), built against the default
    # 6.18 LTS kernel and loaded at boot. /proc/drbd then reports a 9.x module.
    #
    # Pinned ahead of nixpkgs, for good: nixos-26.05 ships 9.2.16 for the channel's life,
    # nixpkgs does not track the 9.2 maintenance line, and unstable has jumped to the 9.3
    # feature branch, so no channel bump is coming. Three 9.2 releases are worth having:
    #
    #   9.2.17  DRBD asks the kernel for BLK_FEAT_STABLE_WRITES again, after kernel-side
    #           changes silently dropped it. DRBD needs it structurally: the same page feeds
    #           the local write and the network send, so a page rewritten under writeback
    #           makes the replicas diverge. Our backing disk is virtio-blk, which does not
    #           advertise it, and btrfs sits directly on /dev/drbd0, so 9.2.16 on 6.18
    #           replicates without the guarantee. This one alone justifies the pin.
    #   9.2.18  Quorum arithmetic: a connected peer that is diskless *unintentionally* (a
    #           detached disk -- an anchor whose drive died, or the attach/detach window our
    #           pairing drives) was counted as a storage voter holding data: a wrong count in
    #           calc_quorum, on the failover path, for the failure this product exists to
    #           survive. Also: `drbdadm attach` now waits for UUID negotiation and reports a
    #           stale/diverged attach instead of returning 0 with the device quietly diskless.
    #   9.2.19  Holds a primary-loss survivor at Consistent until the post-loss reconcile
    #           settles, gap-free, so it cannot declare itself authoritative before learning
    #           whether a peer holds a write it is missing. Plus the silent-divergence family
    #           of fixes.
    #
    # Deliberately not 9.3.x, though it is the same `overrideAttrs` away: 9.3 is where new
    # functionality lands, and it makes resync-without-replication the default -- a rewrite of
    # the exact path our heal invariants and failover timings measure. It also brings variable
    # bitmap granularity, and 9.2 refuses to attach metadata written with any granularity but
    # 4k: a trapdoor for a product whose OS (and so its DRBD module) can roll backwards.
    #
    # Refresh: bump `version`, then `nix-prefetch-url --type sha256 <the url>` and
    # `nix hash convert --hash-algo sha256 --to sri <base32>`.
    boot.extraModulePackages = [
      (config.boot.kernelPackages.drbd.overrideAttrs (_: {
        version = "9.2.19";
        src = pkgs.fetchurl {
          url = "https://pkg.linbit.com//downloads/drbd/9/drbd-9.2.19.tar.gz";
          hash = "sha256-bhmvViC/m03IgP/g6kBHkCLJx6D8PKXK7oa9Uy57XcE=";
        };
      }))
    ];
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

    # drbd-reactor's promoter snippet lives on tmpfs, agent-written like everything else
    # node-scoped. The host re-derives it from cfg.Promoter at every bring-up, so persisting it
    # would buy nothing and cost the one thing that matters: a persisted snippet outlives the
    # agent that wrote it, and a reactor started before bring-up would promote against
    # configuration nobody had just restated. One lifetime for every fact the agent writes means
    # stale configuration cannot exist. It is also a second, nearly free backstop to the
    # arm-by-agent gate below, failing independently of it: a reactor with no snippet is idle
    # even if something starts it.
    #
    # Pointed at /run rather than symlinked into /etc: drbd-reactor takes the directory as config,
    # so this is a one-line edit, not a link somebody has to keep correct. The agent-less
    # harnesses declare their own snippet into the same directory (nixosTest/lib.nix).
    environment.etc."drbd-reactor.toml".text = ''
      snippets = "${reactorSnippetDir}"
    '';

    # The tool profile an agent-written unit puts on its PATH (`guestTools` above says why it
    # exists). A fixed path in /etc rather than a store path the agent would have to be told: the
    # two halves share a name and nothing else, so they need no handshake to agree.
    environment.etc.${toolsEtc}.source = guestTools;
    systemd.tmpfiles.rules = [
      "d /run/briard 0755 root root -"
      "d ${reactorSnippetDir} 0755 root root -"
      # DRBD's own state dir. Without it every attach logs
      #   lk_bdev_save(/var/lib/drbd/drbd-minor-0.lkbd) failed: No such file or directory
      # which is harmless (it caches the backing device's last known size) but sits directly on
      # top of the real error when an attach is being diagnosed. Upstream ships this dir
      # in its package; nixpkgs' drbd does not create it.
      "d /var/lib/drbd 0700 root root -"
    ];

    # This node's name is set by the agent at every bring-up (sys.hostname) and nothing in this
    # image restores it. The baked hostname is "guest" (disk-image.nix) and syscall.Sethostname
    # does not survive a reboot, so a `.res` that survived one would have a rebooted guest run as
    # "guest" against `on briard-node-<id>`: a reactor promoting into the mismatch fails
    # drbd@<res>, a failed promote is never retried, and the node parks quorate but never Primary,
    # with no VIP and no address. So the two facts share one lifetime: the `.res` is ephemeral and
    # the name with it. Both are node-scoped, the host re-derives both at every bring-up
    # (cfg.Node, cfg.Resource), and the agent-armed promoter means nothing can promote before that
    # bring-up. Two facts with one lifetime cannot disagree, and there is no third copy on disk to
    # be right or wrong about, which is what a restored-at-boot file always is.

    # The promoter is armed by the agent, never by boot. Failover is entirely drbd-reactor's
    # (nothing here promotes or demotes); the agent decides when it may start: once the host has
    # told this guest who it is, where its NICs are, and what its promoter chain is. On a flock,
    # guestagent's BringUp ends in `systemctl start drbd-reactor.service` (reactor.resume restarts
    # it after a maintenance pause); nothing else starts it. Not only for an absent agent: a
    # boot-started reactor races the agent's reconnect -> net.configure -> vip.env, and a node
    # that loses the race claims the VIP on the DRBD NIC, under a second DHCP identity, with mDNS
    # dead and the probe blind.
    #
    # There is no deadlock to design around, which makes it nearly free: bring-up gates on
    # quorate, not Primary (host.go), so the agent never waits for a promotion, and quorum does
    # not need the reactor because the agent attaches DRBD itself (storage.node). So promotion may
    # safely wait for the agent, and does.
    #
    # The cost: a permanently absent agent (/opt/briard wiped, the unit masked, an incompatible
    # binary after an upgrade) is a total outage rather than a degraded-but-serving node.
    # Accepted: the lost race above is what degraded-but-serving looks like, and a node that
    # plainly does not serve is more honest and repairable than one up in a way nobody can see or
    # fix. Bring-up is also the one place that arms the promoter, so an agent SIGKILLed inside a
    # maintenance bracket (whose resume lived only on the dead process's stack) re-arms it by
    # restarting; and the agent-less state is inert, so the deadman's reboot cycle stops churning
    # DRBD and the household's DHCP server on every pass.
    systemd.services.drbd-reactor = {
      description = "drbd-reactor — DRBD failover orchestrator";
      wantedBy = [ ];
      after = [ "network.target" ];
      path = [ pkgs.drbd pkgs.systemd ]; # drbdsetup/drbdadm + systemd management
      serviceConfig = {
        Type = "notify";
        ExecStart = "${pkgs.drbd-reactor}/bin/drbd-reactor";
        Restart = "on-failure";
        # Defuse the promote-vs-stop deadlock on every stop, not just the one the agent drives.
        # drbd-reactor writes itself an ordering drop-in putting drbd-services@r0.target Before=
        # this daemon; on the way down that reverses, so systemd stops the daemon first. A stopping
        # reactor re-emits its exists-events and fires one last `systemctl start
        # drbd-services@r0.target`; systemd refuses it (destructive: a shutdown is already
        # queued); the reactor reads that as a failed start and answers with `systemctl stop
        # drbd-services@r0.target` -- a job it then waits for, which the ordering sequences behind
        # its own stop. Neither can proceed: 90s TimeoutStopSec, then SIGKILL. Measured from the
        # guest's console during `briard rescue`: the shutdown began 1s after os.poweroff,
        # deadlocked at +11.5s, and finished at +101s.
        #
        # Removing the drop-in first is drbd-reactor's own sanctioned defusal; `reactor.pause`
        # (guestagent.go, verbReactorPause) is a bare stop and relies on it being done here. It is
        # race-free because the promoter only (re)writes the file in Promoter::new, i.e. on the
        # next start, so nothing re-arms it while we are stopping. nixosTest/guest-rescue.nix
        # guards it on the shipped artifact (no unit may hold the shutdown).
        #
        # On ExecStop rather than in the verb, deliberately: this stop happens on paths no agent
        # verb touches -- the deadman's `systemctl reboot`, a user rebooting the host (the guest
        # unit's ExecStop -> ACPI -> this same shutdown), a guest that reboots itself. systemd runs
        # ExecStop= before it signals the process, so the ordering is gone before the reactor's
        # last gasp. Both commands are `-` prefixed: a failure here must never turn a stop into a
        # failed stop, and `rm -f` on an absent file is a no-op (a reactor that never promoted
        # wrote no drop-in).
        #
        # Safe on stops we do not initiate (and we are not the only ones who stop it: a user
        # rebooting their own machine asks nobody), because the drop-in's only legitimate function
        # is a start ordering, its sole effect on the stop side is the deadlock, and drbd-reactor
        # recreates it in `Promoter::new` before the next start. No stop exists for which removing
        # it is wrong, whoever initiated it -- the only kind of argument worth having, since who
        # stops us is not ours to control.
        #
        # Measured rather than assumed, a crash does not get it: systemd runs ExecStop= on an
        # explicit stop, a restart and a shutdown transaction, but not when the main process
        # crashes on its own (that path runs ExecStopPost= only). Fine twice over: a dead reactor
        # has no last gasp to sequence, and `Restart=on-failure` restarts it through
        # `Promoter::new`, which rewrites the drop-in anyway. So the one shape that could loop
        # `daemon-reload` -- crash, restart, crash -- cannot, since the crash half never gets here.
        #
        # The `daemon-reload` is not optional: removing the file leaves the `Before=` in systemd's
        # in-memory graph, so the deadlock would survive its own defusal.
        #
        # Paired with the same path in nixosTest/maintenance-contract.nix, deliberately written out
        # rather than shared, for the single resource (r0): a test that imported the constant could
        # not notice the product changing it.
        ExecStop = [
          "-${pkgs.coreutils}/bin/rm -f /run/systemd/system/drbd-services@r0.target.d/reactor-50-before.conf"
          "-${config.systemd.package}/bin/systemctl daemon-reload"
        ];
      };
    };

    # Stock DRBD systemd integration: drbd-reactor's promoter drives drbd-utils' own
    # drbd-promote@ / drbd-services@ units and the drbd@.target bring-up chain -- the tested
    # upstream units, not hand-rolled ones (the overlay patches nixpkgs' drbd to install them; see
    # flake.nix). NixOS loads them but starts nothing until the agent (or, in tests, the harness)
    # writes a .res and fires drbd@<res>.target; promotion is then drbd-reactor's.
    systemd.packages = [ pkgs.drbd ];
    services.udev.packages = [ pkgs.drbd ]; # DRBD udev rules (/dev/drbd/by-res symlinks + perms)

    # Upstream's escalation cannot write its own reason down on NixOS. reactor still attaches
    # drbd-demote-or-escalate@ as `OnFailure=` on drbd-promote@ -- the promotion-failure path, not
    # ours (a member that gives up goes through briard-promotion-hold). That unit ends in
    # `ExecStopPost=-/bin/journalctl --sync`, a path that does not exist here (measured: "Unable
    # to locate executable '/bin/journalctl'"). The `-` makes it ignored, not fatal, so the unit
    # works but loses the one thing it does before rebooting the node: flushing the explanation
    # to disk. Reset the list and give it the real path.
    systemd.services."drbd-demote-or-escalate@r0" = {
      overrideStrategy = "asDropin";
      serviceConfig.ExecStopPost = [
        ""
        "-${config.systemd.package}/bin/journalctl --sync"
      ];
    };

    # ── The promoter chain is not defined here ───────────────────────────────────────────────
    #
    # Every unit the pushed binaries are started by -- briard-node-storage, the five chain
    # members, the VIP's renewal pair, the hold that hands the resource on, and the lone node's
    # target -- is written by the guest agent into /run/systemd/system at its own start
    # (agent/guestagent/units.go carries the unit texts and their reasoning). The image owns what
    # is above: the tool profile those units name, and the hold's per-topology steps, which take
    # the chain's members as arguments. The whole chain, not only the units that exec a pushed
    # binary, because the member list can have only one owner: the hold enumerates the members to
    # stop and reset, the target folds the ordering onto them, and briard-vip sits mid-list, so
    # baking any of them would put the list on both sides of the seam with nothing checking they
    # agree -- the image-versus-agent skew the agent-written units remove, at one remove.
    #
    # What stays baked, each for its own reason: briard-guest-agent and briard-deadman
    # (disk-image.nix) supervise the agent's arrival, so it cannot write them -- the
    # frozen-by-necessity line, drawn inside the guest; drbd-reactor and upstream's drbd-* units
    # are not ours; and briard-stage below is a build-time fact about this image
    # (`config.briard.stagedImages`), which no pushed binary knows.

    # Podman belongs to the guest OS, not to any app: it is the runtime an app is installed into,
    # by the renderer, at runtime. No declared container and no `virtualisation.oci-containers`
    # here -- a workload is not a build-time fact about this image.
    virtualisation.podman.enable = true;

    # Pre-stage image tarballs into local podman storage at boot: images that must already be
    # resident because nothing on the failover path may pull. A runtime-installed app renders
    # `Pull=never` against a digest, so its image has to be here before it is installed.
    # Idempotent, runs on every node (a standby is where a cold pull would hurt), and independent
    # of what is installed. The IDs it loaded go to /run/briard/staged-images, and image.remove
    # keeps any image named there: an image this OS carries is reloaded at every boot, so removing
    # it frees nothing for long, and on a node with no registry it is the only copy a later
    # install of that version can use.
    systemd.services.briard-stage = lib.mkIf (config.briard.stagedImages != [ ]) {
      description = "Pre-stage app images into local podman storage";
      wantedBy = [ "multi-user.target" ];
      path = [ config.virtualisation.podman.package ]; # the module's podman, not a second copy
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-stage" ''
          set -eu
          mkdir -p /run/briard
          : > /run/briard/staged-images.tmp
          for img in ${lib.concatStringsSep " " config.briard.stagedImages}; do
            # The load is its own command, so a failed one fails the unit: inside the list of the
            # `for`, a $(…) would lose its status and the unit would report success with nothing
            # staged.
            loaded=$(podman load -i "$img")
            names=$(printf '%s\n' "$loaded" | sed -n 's/^Loaded image[^:]*: //p' | tr ',' ' ')
            [ -n "$names" ] || { echo "briard-stage: $img loaded no image" >&2; exit 1; }
            for name in $names; do
              podman image inspect --format '{{.Id}}' "$name" >> /run/briard/staged-images.tmp
            done
          done
          mv /run/briard/staged-images.tmp /run/briard/staged-images
        '';
      };
    };

    # Lean, headless test image. The nixosTest / VM runner supplies the real boot
    # device + networking, so there is no bootloader here and the root device below is a
    # placeholder (disk-image.nix overrides it with its own).
    boot.loader.grub.enable = false;
    # Deliberately no console here: the guests built from this module alone are nixosTest nodes,
    # whose console the framework already captures. The bootable image adds its own
    # (disk-image.nix: console=ttyS0 + grub on serial + journald ForwardToConsole), because there
    # the host's `-serial file:` is the only way to see inside. Setting it in both places would
    # duplicate the kernel param and imply this module owns a decision it does not.
    fileSystems."/" = {
      device = "/dev/vda";
      fsType = "ext4";
    };
    networking.firewall.enable = false;

    # dhcpcd runs on every interface by default, and this guest has interfaces that must never
    # ask a stranger's router for anything. Measured: left at the default, a node puts two extra
    # DHCP clients on the household's router, one on the DRBD replication link -- a private
    # point-to-point path between anchors with no business holding a LAN address, whose address
    # the agent sets explicitly (net.configure) when a pairing happens.
    #
    # Kept: eth0 only (qemu's SLIRP user-net, the guest's WAN path for OCI pulls). Denied: eth1
    # (DRBD) and eth3 (the private guest<->host witness link), both statically addressed by the
    # agent and invisible to the LAN by design, and eth2, the service NIC. eth2 becomes the VIP,
    # but it cannot be leased at boot: a boot-time client would lease it on the Secondary too (a
    # per-node "VIP" on a node that is not serving), and its MAC is flock-scoped, so both nodes
    # would ask one router for one lease at once. briard-vip draws the lease at promotion, the one
    # moment exactly one node holds this identity, as its own single-interface dhcpcd instance,
    # which is what lets denying it here and leasing it there coexist.
    networking.dhcpcd.denyInterfaces = [ "eth1" "eth2" "eth3" ];

    # Answer ARP only on the interface that holds the address (and source kernel ARP probes from
    # the outgoing interface's own address). Both NICs can share one L2 (a household that wires
    # both ports into one switch), and Linux's weak-host default (arp_ignore=0) then lets the
    # service NIC answer for the DRBD address: the peer caches the wrong MAC, replication silently
    # transits eth2 while it holds the VIP address, and when a VIP teardown strips that address,
    # source validation turns the flow into a silent one-way blackhole -- far longer than DRBD's
    # 500ms ping deadline, i.e. a split-brain with no failure anywhere. Every address here is
    # hand-placed on the NIC that owns its traffic (net.configure, briard-vip's lease, the witness
    # link), so weak-host ARP adds nothing and only the cross-NIC ambiguity goes. The VIP
    # takeover's gratuitous ARP is explicit (briard-vip-arping crafts its own frames) and unaffected.
    boot.kernel.sysctl = {
      "net.ipv4.conf.all.arp_ignore" = 1;
      "net.ipv4.conf.default.arp_ignore" = 1;
      "net.ipv4.conf.all.arp_announce" = 2;
      "net.ipv4.conf.default.arp_announce" = 2;
      # Swap here is zram (below), where bringing a page back is a decompression, not a disk read:
      # 100 tells reclaim that parking an anonymous page costs the same as dropping a cache page,
      # instead of the disk-swap default of 60 that protects anonymous memory at the cache's expense.
      "vm.swappiness" = 100;
    };

    # Compressed swap in RAM. The guest's memory is fixed at boot with only the OOM killer above
    # it; zram turns memory an app touched once and never again into a fraction of its size, so a
    # burst lands there instead of on the killer. Page cache is never swapped, so zram holds only
    # anonymous memory -- demand the guest's size has to cover -- which is why its usage rides the
    # resources verb. zstd and a cap of half the RAM (uncompressed) are the module's defaults.
    zramSwap.enable = true;

  }
  ];
}
