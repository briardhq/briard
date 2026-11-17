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
  btrfsRoot = "/var/lib/briard"; # the DRBD btrfs volume mount
  # ── The storage seam ─────────────────────────────────────────────────────────────────────────
  # The stack under the mount above is `dataDisk -> LUKS -> PV -> VG -> LV -> DRBD -> btrfs`. The
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
  # those units with its own spellings (guestagent's tlsCertPath/tlsKeyPath and vipEnvPath). The
  # scripts below write and read the VIP's live/stored addresses, hence vipLivePath and vipAddrFile.
  # The topology word node-storage writes beside vip.env: `flock` or `alone`, the only thing a
  # shell unit needs to know about the spec. Paired with the Go const guestagent.topologyEnvPath
  # and the two values its topologyEnv writes.
  topologyEnvPath = "/run/briard/topology.env";
  # ── The image's half of a unit the agent writes ──────────────────────────────────────────────
  #
  # The image contributes the closure; the pushed agent contributes the unit. The image bakes
  # exactly one binary and every other briard binary rides the host bundle. A unit is mostly an
  # ExecStart plus its binary's environment, so a unit baked here would move on the image's
  # cadence while its binary moves on the host's: a pushed agent asking its older image for a unit
  # that image does not define crash-loops the node with the household's app gone. The agent
  # writes its units into /run/systemd/system at every start (the only place it can: NixOS makes
  # /etc/systemd/system a read-only store path), so a unit can never be older than the binary that
  # wrote it, and the defect class is unrepresentable rather than merely detectable.
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
      # And the named tools a unit's ExecStart points at directly, because systemd wants an
      # absolute path for the first word and the agent can only know a fixed one.
      config.briard.pivot.exec # briard-bin-exec, the frozen picker the web units go through
      vipArping
      vipDown
      vipRenew
      vipUp
    ]
    ++ holdSteps;
    pathsToLink = [ "/bin" "/sbin" ];
  };
  # Paired with the Go const guestagent.defaultToolsBin, which is what an agent-written unit puts
  # on its PATH. Different languages, so no shared import; the agent-side comment names this back.
  toolsEtc = "briard/tools";
  # ⚠️ The chain is not stated here. The units that name it are written by the pushed agent,
  # from the one Go definition both halves of the bundle read (shared/chain; the host reads it
  # in promoterUnits()), and the steps below take it as arguments instead of baking it.
  # A hold step with one body per topology. The word decides; a missing word means "node storage
  # has not run on this boot" and an unknown one names itself rather than guessing. Both fail the
  # step, and what that means is the unit's to say (ExecCondition skips, ExecStartPre escalates).
  # A named tool in the profile, because the agent-written unit can only name a fixed path. The
  # split is deliberate: the image keeps the dispatch and the commands (store paths, unchanged
  # by a release); the agent passes in the one thing only it knows, the chain's members, as `"$@"`.
  byTopology =
    name:
    { flock, alone }:
    pkgs.writeShellScriptBin name ''
      set -u
      if [ ! -r ${topologyEnvPath} ]; then
        echo "${name}: no ${topologyEnvPath}; node storage has not run on this boot" >&2
        exit 1
      fi
      . ${topologyEnvPath}
      case "''${BRIARD_TOPOLOGY:-}" in
        flock) ${flock} ;;
        alone) ${alone} ;;
        *)
          echo "${name}: ${topologyEnvPath} says '$BRIARD_TOPOLOGY', which is neither flock nor alone" >&2
          exit 1
          ;;
      esac
    '';
  # Where the agent drops drbd-reactor's promoter snippet. Tmpfs, and paired with the Go const
  # guestagent.reactorPath -- different languages, so no shared import; the agent-side comment
  # names this file back.
  reactorSnippetDir = "/run/briard/drbd-reactor.d";
  # The flock's service address, replicated with the data: like the TLS material, a small
  # flock-scoped fact at the btrfs root, written only by the node that holds the volume (only a
  # Primary can mount it) and read by whoever promotes next. It exists so a failover never has to
  # ask for the address. The MAC is *derived* (every node computes it from the flock id), but an
  # address is *acquired*, known only to whoever asked the router, so it is the one piece that has
  # to travel. Without it, a node that has never served would need a full DHCP exchange at the
  # moment a household's network is least likely to be answering.
  vipAddrFile = "${btrfsRoot}/.vip-address";
  # The address this node actually claimed, written by briard-vip once it has resolved one and
  # read by everything downstream (the gratuitous ARP, the mDNS name). Under DHCP the configured
  # and claimed values are not the same thing, and a name or announcement must never describe an
  # address the node did not take -- the same ground-truth rule net.vip follows.
  vipLivePath = "/run/briard/vip.live";
  # `ip` wants the prefix, `arping` the bare address: stripped here rather than carried twice,
  # because two variables can disagree and the one that would silently win is the gratuitous ARP
  # nobody is watching. Read from the live file, not the unit environment: this runs as
  # ExecStartPost of the unit that resolved the address, and whether systemd re-reads an
  # EnvironmentFile between ExecStart and ExecStartPost must not be the reason a gratuitous ARP
  # names the wrong address. Sourcing it is one line and no assumption.
  vipArping = pkgs.writeShellScriptBin "briard-vip-arping" ''
    set -eu
    . ${vipLivePath}
    exec ${pkgs.iputils}/bin/arping -A -c 1 -I "$VIP_DEV" "''${VIP_ADDR%%/*}"
  '';
  # ── The hold's steps, as image-side tools ────────────────────────────────────────────────────
  #
  # The agent writes the unit that runs them, so only what it cannot carry lives here: the
  # topology dispatch and the store paths. Steps that need the chain's members take them as
  # arguments, so the list does not live in this file.
  #
  # The order is the design: mask before demoting. Measured on a one-node DRBD resource,
  # drbd-reactor re-promotes about 2s after a demote completes, so a hold added after the demote
  # loses that race. Masking first refuses the promotion rather than undoing it: `systemctl start
  # drbd-services@r0.target` fails outright, drbd-promote@ never runs, DRBD's role never moves,
  # and no second mount/unmount cycle is paid. The mask is also the only lever that says "not me,
  # for now": everything else reactor offers is DRBD's own decision or a static handicap biasing
  # which node wins a race. `--runtime` puts it in /run, so a reboot clears it and a hold can
  # never outlive the boot that might have fixed its cause.
  holdSteps = [
    # ⚠️ Only a node that holds the resource may hand it on; without this guard the standby masks
    # itself on every boot. Measured: a member whose start job fails with result `dependency`
    # fires `OnFailure=` exactly like one that failed on its own, and on the node that loses the
    # promotion race every member does ("Multiple primaries not allowed by config" ->
    # drbd-promote@ fails -> "Dependency failed for ..." -> "Triggering OnFailure=
    # dependencies"). The loser would run the hold and mask itself for all of it: at 300s, a
    # standby that cannot take over for five minutes after a boot.
    #
    # ⚠️ On a lone node the guard is always met: no loser to protect and nobody else who could hold
    # the volume, so every member failure is this node's own to hold -- and to restart from, below.
    (byTopology "briard-hold-only-if-holding" {
      flock = "${pkgs.drbd}/bin/drbdadm role r0 | ${pkgs.gnugrep}/bin/grep -q '^Primary'";
      alone = "exit 0";
    })
    # A lone node needs no mask: nothing re-promotes during its hold, because it runs no reactor
    # and the restart step below is what starts its target again.
    (byTopology "briard-hold-refuse" {
      flock = "${config.systemd.package}/bin/systemctl mask --runtime drbd-services@r0.target && ${config.systemd.package}/bin/systemctl daemon-reload";
      alone = ":";
    })
    # Step 2 stops drbd-promote@, not the target, as a barrier rather than a preference.
    # Measured: `systemctl stop drbd-services@r0.target` returns as soon as the target is down --
    # `Stopped target` at 62.386, `Stopping briard-vip` at 62.388, the shim running at 62.412
    # while the volume was still mounted, exit 11, reboot. The members are `After=drbd-promote@`,
    # so they stop before it: waiting for the promote unit is the only spelling that waits for all
    # of them. Its ExecStop is the ordinary demote, so the confirm below only confirms. A lone
    # node has no promote unit, so its stop names every unit (the target, then the members in
    # reverse, passed in by the agent): a stop of several units returns when all are down.
    (byTopology "briard-hold-stop" {
      flock = "${config.systemd.package}/bin/systemctl stop drbd-promote@r0.service";
      alone = ''${config.systemd.package}/bin/systemctl stop "$@"'';
    })
    # The one escalation, the same cell in both topologies: a demote DRBD refused, or an unmount
    # something still holds open. Nothing else recovers a node stuck holding a volume it has
    # declared it cannot serve -- on a flock the peer cannot take over, alone the restart below
    # would mount on top of it. Whoever attempts the demote owns what happens when DRBD refuses
    # it: the unit's FailureAction=reboot.
    (byTopology "briard-hold-confirm" {
      flock = "${pkgs.drbd}/lib/drbd/scripts/drbd-service-shim.sh secondary-or-escalate r0";
      alone = "! ${pkgs.util-linux}/bin/mountpoint -q ${btrfsRoot} || ${pkgs.util-linux}/bin/umount ${btrfsRoot}";
    })
    # The release. The unit marks this and the resets `-`: a hold that cannot tidy up must still
    # end, because leaving the mask on is the one outcome worse than releasing early.
    (byTopology "briard-hold-release" {
      flock = "${pkgs.coreutils}/bin/rm -f /run/systemd/system/drbd-services@r0.target; ${config.systemd.package}/bin/systemctl daemon-reload";
      alone = ":";
    })
    # Alone, the restart is ours: no reactor re-promotes, so the hold starts the chain it stopped
    # -- hold-and-restart, forever, no reboot, as a flock does through its reactor. `--no-block`:
    # this runs inside the hold's own stop, and the start must not wait on it. The target is
    # `$1`, from the agent, for the same reason the stop list is.
    (byTopology "briard-hold-restart" {
      flock = ":";
      alone = ''${config.systemd.package}/bin/systemctl start --no-block "$1"'';
    })
  ];

  # The address-changed handler: one path for every cause (a NAK, a lease yielded to a host that
  # ARP-claimed it, a router that repooled while the flock had no primary), because "the address
  # changed" does not care why.
  #
  # It deliberately does not restart briard-vip, which would be wrong twice over: this hook runs
  # in briard-vip's own cgroup (dhcpcd was started from its ExecStart), so the restart would kill
  # the process asking for it, mid-flight; and it would put an ordinary address change through the
  # promoter chain -- a member whose failure trips OnFailure=briard-promotion-hold -- a
  # failover-shaped risk for a job that needs no chain. The reverse proxy binds :80 on every
  # interface and does not care what the address is; what does is the live file, the flock's
  # store, the ARP announcement and the name. The first three are done here; the name follows the
  # live file on its own (below). An expired lease needs nothing from this hook either: the
  # interface holds no address, net.vip reports "" as ground truth, and the node reads not-ready
  # by the rule that covers every other addressless data node.
  vipHook = pkgs.writeShellScript "briard-vip-dhcp-hook" ''
    set -u
    case "''${reason:-}" in
      # The reasons that mean "we hold an address". TIMEOUT is --lastleaseextend doing its job:
      # no server answered and we kept the lease, which is a non-event worth not reacting to.
      BOUND|RENEW|REBIND|REBOOT|TIMEOUT|STATIC) ;;
      *) exit 0 ;;
    esac
    [ -n "''${new_ip_address:-}" ] || exit 0
    addr="''${new_ip_address}/''${new_subnet_cidr:-24}"

    # A RENEW confirming what we already hold is the common case and must be silent -- otherwise
    # every lease period would re-announce and bounce the name for no reason.
    cur=""
    if [ -r ${vipLivePath} ]; then
      . ${vipLivePath}
      cur="''${VIP_ADDR:-}"
    fi
    [ "$addr" = "$cur" ] && exit 0

    # Withdraw the address we put on optimistically. dhcpcd removes the addresses it manages, but
    # the one applied at promotion from the flock's store is foreign to it: without this the NIC
    # holds two, and "the first address on the device" (how net.vip reads ground truth) becomes a
    # coin toss between the live one and a stale one.
    if [ -n "$cur" ]; then
      ${pkgs.iproute2}/bin/ip addr del "$cur" dev "''${interface}" || true
    fi
    ${pkgs.iproute2}/bin/ip addr replace "$addr" brd + dev "''${interface}"
    printf 'VIP_ADDR=%s\n' "$addr" >${vipLivePath}
    # Synced like the promotion-time write: here the router hands us a different address, so the
    # stored value is actively wrong, and losing this write to the page cache leaves the flock
    # remembering an address it has already yielded. (vipLivePath is tmpfs: nothing to sync.)
    { printf '%s\n' "$addr" >${vipAddrFile} && ${pkgs.coreutils}/bin/sync -f ${vipAddrFile}; } 2>/dev/null || true
    VIP_DEV="''${interface}" ${vipArping}/bin/briard-vip-arping || true
    # The name needs nothing from here: the reverse proxy publishes it and re-reads this file on
    # its own tick, so the records follow the address within seconds, with no unit to poke and no
    # ordering to get wrong.
    exit 0
  '';

  # An empty config for the VIP's dhcpcd instance, deliberately: every setting it has is on its
  # command line below, where it can be read. The system's generated /etc/dhcpcd.conf says
  # `denyinterfaces ... eth2`, right for the boot-time client but refusing the very interface this
  # instance exists to lease; and it is the file a nixpkgs bump would add `duid` to, quietly
  # giving the two nodes of one flock different identities. Inheriting neither, this instance
  # cannot be surprised by either.
  dhcpcdConf = pkgs.writeText "briard-vip-dhcpcd.conf" "";

  # dhcpcd, scoped to the service NIC and to one job: hold the binding. It does not supply the
  # address (the store or the operator did) and must not supply anything else. Every flag is
  # load-bearing:
  #
  #   <dev> as the sole interface argument makes this a separate instance from the system one,
  #     with its own pidfile and control socket ("runs as a separate instance to other dhcpcd
  #     processes"), so the service NIC can be denied at boot and leased at promotion: a boot-time
  #     client would lease it on the Secondary too, and with the flock MAC both nodes would ask
  #     one router for one lease.
  #   -G (nogateway): by default dhcpcd installs a default route from this lease, so promotion
  #     would silently re-plumb the guest's WAN path (eth0, the SLIRP route out) and demotion
  #     revert it. Routes are dhcpcd's own doing, not a hook's, so only this flag stops it.
  #   -c <hook> replaces dhcpcd-run-hooks for this instance: the stock hooks never run, resolv.conf
  #     is never rewritten, ours is the only script. -C resolv.conf is the belt to those braces,
  #     still protecting us if -c is ever dropped and the standard runner comes back.
  #   -L (noipv4ll): a service address may not be invented by the machine that serves it. Left
  #     on, dhcpcd answers "no server answered" by self-assigning 169.254.x.x -- measured: the node
  #     then probes its own link-local address, passes (it owns it), and reports HEALTHY while
  #     nobody on the LAN can reach it. A house whose router is briefly down at boot would hit it.
  #     A VIP is the flock's address or nothing; a self-assigned one is worse than none because it
  #     looks like success.
  #   -I "01:<mac>" states the client-id outright: RFC 2132 type 1 (ethernet) + the flock MAC,
  #     VIP_MAC from the agent (the NIC's own address where a harness sets none), which dhcpcd
  #     encodes as hex because the value is colon-separated. Stated, not read off the NIC: the
  #     identity is the flock's, never a property of the NIC carrying it, and it is the pair the
  #     host asks under where it holds the lease instead (a single-anchor substrate, VIP_LEASE=host
  #     below, agent/host/lease.go). One flock presents one identity, so a lease survives a
  #     failover, and dhcpcd's own shipped `duid` (a per-host DUID that, in the man page's words,
  #     "should not be copied to other hosts") cannot give two nodes of one flock different leases
  #     if a nixpkgs bump restores it.
  #     ⚠️ Never `-I ""`, the documented way to ask for the hardware-address default: it is wrong
  #     on the wire, invisibly on the command line. dhcpcd takes the literal "-h" as the client-id
  #     (dnsmasq records `00:2d:68`: type 0, then ASCII "-h"), consuming the -h option, so every
  #     node sends its system hostname ("guest") and an identical client-id, and two flocks on one
  #     LAN fight over a single lease. Never ask for a default when you can state the value.
  #   -h briard-<xxxxxx> is the flock name (option 12), from the low three bytes of the same
  #     flock MAC. It gives the router a recognisable client-list entry and makes a user-created
  #     static reservation survive failover, because name, MAC and client-id are all flock-scoped.
  #   --lastleaseextend keeps the address when no server answers, yielding it only to a host that
  #     ARP-claims it: a knowing RFC 2131 3.7 violation, right here because it covers an unplanned
  #     failover with the router down and still refuses to squat an address somebody wants.
  #   -r asks for the address we already claimed, so the replicated store wins over this node's
  #     own lease file instead of the two quietly drifting apart.
  #   -J sets the BROADCAST flag, so the server broadcasts its OFFER and ACK: the shape the host's
  #     own client sends where it holds the lease, so a router answers the flock one way whichever
  #     side asked. Harmless on every substrate.
  dhcpcdRun = pkgs.writeShellScript "briard-vip-dhcpcd" ''
    set -eu
    dev="$1"; want="$2"; shift 2
    mac="''${VIP_MAC:-$(cat /sys/class/net/"$dev"/address)}"
    hex="''${mac//:/}"
    set -- -f ${dhcpcdConf} -c ${vipHook} -L -J \
      -G -C resolv.conf -I "01:$mac" -h "briard-''${hex: -6}" --lastleaseextend "$@"
    if [ -n "$want" ]; then
      set -- "$@" -r "''${want%%/*}"
    fi
    exec ${pkgs.dhcpcd}/sbin/dhcpcd "$@" "$dev"
  '';

  # Force a RENEW on a ten-minute timer, independently of the lease term.
  #
  # Not a shorter lease: asking for one (`-l`) is the wrong lever twice over. The term is the
  # server's to choose, so on a CPE that ignores a client's option 51 the flag is a silent no-op
  # we would believe had worked; where honoured, a short lease turns --lastleaseextend (a knowing
  # RFC 2131 3.7 violation kept as an exceptional safety net) into the routine state for the length
  # of any router blip. We want the lease long and the renewal frequent, and nothing in RFC 2131
  # makes a client wait for T1.
  #
  # It buys a shorter silence, not a firmer grip. A CPE reboot, the household's usual fix for
  # anything, commonly loses the lease table, and a client sitting BOUND says nothing until T1 --
  # at a router's usual 12-24h term, hours in which a fresh DISCOVER from any device can be handed
  # the address we think is ours. Ten minutes bounds that window; on dnsmasq a renewal against a
  # server that lost its table is ACKed, so the binding is recreated, not merely found gone. And
  # where we lost it anyway, a NAK inside ten minutes drives the address-changed path that a
  # briard.casa name depends on.
  #
  # ⚠️ The guard is the point of this script. `dhcpcd -N`'s documented behaviour with no instance
  # running is "starts up as normal", and measured: against an idle interface it forked a client
  # and took an address. On a standby, whose service NIC must have no LAN presence, that would put
  # a DHCP client on it claiming an address the flock's primary owns. The agent-written units
  # (agent/guestagent/units.go) already make it unreachable, since the timer lives and dies with
  # briard-vip; the guard keeps a hand-run `systemctl start` safe too.
  vipRenew = pkgs.writeShellScriptBin "briard-vip-renew" ''
    set -eu
    pidfile="$(${pkgs.dhcpcd}/sbin/dhcpcd -P "$VIP_DEV")"
    if [ ! -s "$pidfile" ] || ! kill -0 "$(cat "$pidfile")" 2>/dev/null; then
      echo "briard-vip-renew: nothing is holding $VIP_DEV; declining to start a client" >&2
      exit 0
    fi
    exec ${pkgs.dhcpcd}/sbin/dhcpcd -N "$VIP_DEV"
  '';

  # Replies to the household leave the way their request came in. The guest's only default route
  # is the user-net WAN path (dhcpcd -G above): its own traffic must not depend on holding the
  # VIP. So a reply to a client off the VIP's subnet -- one routed in without NAT: a VPN on the
  # router, a subnet router, another VLAN of the same home -- would follow that default and be
  # lost. While the VIP is held, table 100 holds a default via the household's router on VIP_DEV,
  # and two kinds of traffic look it up:
  #
  #   - a connection that arrived on VIP_DEV. Its conntrack entry is marked on the way in and the
  #     mark copied onto its replies: in prerouting for a published port's reply, which leaves the
  #     pod with the pod's address and is rewritten to the VIP only after routing (so no rule on
  #     the source address can see it), and in a route-type output chain for the guest's own
  #     sockets, where a changed mark re-routes the packet;
  #   - all UDP. A wildcard-bound UDP socket's source is picked by the route, so only a route can
  #     make its reply leave from the VIP. The guest's own UDP is its DNS, to user-net's on-link
  #     resolver, which the rule before keeps where it is.
  #
  # `lookup main suppress_prefixlength 0` comes first: everything main routes more specifically
  # than its default (the VIP's own subnet, the pod networks, user-net's /24, multicast) keeps
  # that route. Only the default is replaced.
  #
  # The router is the host's own gateway on the LAN the VIP lives on (VIP_ROUTER, pushed by the
  # agent): one source for every way the address was set, since a hand-set VIP and a lease the
  # host holds have no lease here to read one from. None given, nothing is routed and the
  # household reaches the VIP from its own subnet only. The mark is one bit of our own, OR-ed in
  # and matched under its mask, so it composes with netavark's marks rather than replacing them.
  #
  # `up` re-installs from scratch, so it is idempotent; `down` is everything `up` does, undone.
  # Never fatal, never silent: the VIP serves its own subnet either way.
  vipReplyRoute = pkgs.writeShellScript "briard-vip-reply-route" ''
    set -u
    ip=${pkgs.iproute2}/bin/ip
    nft=${pkgs.nftables}/bin/nft
    mark=0x40000
    withdraw() {
      $nft delete table inet briard-vip 2>/dev/null
      for pref in 10790 10791 10792; do
        while $ip rule del pref $pref 2>/dev/null; do :; done
      done
      $ip route flush table 100 2>/dev/null
    }
    withdraw
    [ "$1" = up ] || exit 0
    if [ -z "''${VIP_ROUTER:-}" ]; then
      echo "briard-vip: no router named for $VIP_DEV; clients on other subnets get no reply"
      exit 0
    fi
    if ! $ip route replace default via "$VIP_ROUTER" dev "$VIP_DEV" table 100 ||
       ! $ip rule add pref 10790 lookup main suppress_prefixlength 0 ||
       ! $ip rule add pref 10791 fwmark $mark/$mark lookup 100 ||
       ! $ip rule add pref 10792 ipproto udp lookup 100 ||
       ! $nft -f - <<EOF
    table inet briard-vip {
      chain prerouting {
        type filter hook prerouting priority mangle; policy accept;
        iifname "$VIP_DEV" ct state new ct mark set ct mark | $mark
        iifname != "$VIP_DEV" ct mark & $mark == $mark meta mark set meta mark | $mark
      }
      chain output {
        type route hook output priority mangle; policy accept;
        ct mark & $mark == $mark meta mark set meta mark | $mark
      }
    }
    EOF
    then
      withdraw
      echo "briard-vip: WARNING could not route replies via $VIP_ROUTER on $VIP_DEV; clients on other subnets get no reply" >&2
      exit 0
    fi
    echo "briard-vip: replies to other subnets leave via $VIP_ROUTER on $VIP_DEV"
  '';

  # Resolve the service address, claim it, and record what was actually claimed.
  #
  # Three sources, and the order is the design:
  #   1. VIP_ADDR -- an address the operator named. We set it, so we never ask about it.
  #   2. the replicated store -- the flock's address, known even to a node that has never served.
  #      This keeps a failover off the household router entirely.
  #   3. DHCP, synchronously -- only when both above are empty, i.e. the first promotion this
  #      flock has ever performed, which is where the existing doctrine already puts the
  #      exception: installing may need the network, running and failing over must not.
  #
  # The claim in cases 1 and 2 is optimistic, with no ARP probe gate. A conflict at promotion
  # was not caused by the promotion; it can only have arisen while the flock had no primary. So
  # detecting it is dhcpcd's job, afterwards, not a cost every failover pays to find someone
  # else's pre-existing condition; when it finds one it yields, and the one address-changed path
  # handles the new address.
  vipUp = pkgs.writeShellScriptBin "briard-vip-up" ''
    set -eu
    ${pkgs.iproute2}/bin/ip link set dev "$VIP_DEV" up
    mkdir -p "$(dirname ${vipLivePath})"

    configured="''${VIP_ADDR:-}"
    # The host holds the lease on a single-anchor substrate, where this NIC's frames carry the
    # host's MAC: it asks under the flock's own and hands the answer down as VIP_ADDR. With none
    # handed down the router leased the flock nothing of its own -- the host has refused and said
    # so -- and a client started here would only be handed the host's own address. Claim nothing.
    if [ -z "$configured" ] && [ "''${VIP_LEASE:-}" = host ]; then
      echo "briard-vip: the host holds this address's lease and has none to give; claiming nothing on $VIP_DEV"
      exit 0
    fi
    addr="$configured"
    if [ -z "$addr" ] && [ -r ${vipAddrFile} ]; then
      addr="$(cat ${vipAddrFile})"
    fi

    echo "briard-vip: dev=$VIP_DEV configured=''${configured:-<none>} stored=$([ -r ${vipAddrFile} ] && cat ${vipAddrFile} || echo '<none>')"
    # Why there is no stored address, not merely that there is none. `stored=<none>` has three
    # causes the line above cannot tell apart -- the volume is not mounted, it is mounted and the
    # file is absent, or this flock never held an address -- and that ambiguity is the
    # lost-address bug: written on one boot ("remembered ... for the flock"), read back as <none>
    # on the next, with no log saying which. One `findmnt` and one `ls` at the moment of the read
    # settle it, in the field as well as in a harness.
    if [ -z "$configured" ] && [ ! -r ${vipAddrFile} ]; then
      echo "briard-vip: no stored address at ${vipAddrFile} -- mount=$(${pkgs.util-linux}/bin/findmnt -no SOURCE,FSTYPE ${btrfsRoot} 2>/dev/null || echo '<NOT MOUNTED>') contents=[$(${pkgs.coreutils}/bin/ls -A ${btrfsRoot} 2>/dev/null | ${pkgs.coreutils}/bin/tr '\n' ' ')]"
    fi
    if [ -n "$configured" ]; then
      # An address the operator named: claim it and hold no lease. There is nothing to renew,
      # and asking a router about it would be asking permission for something already decided.
      # This also keeps dhcpcd away from the agent-less harnesses, where this unit's NIC is eth1
      # -- the DRBD link, which must never lease.
      ${pkgs.iproute2}/bin/ip addr replace "$addr" brd + dev "$VIP_DEV"
    elif [ -n "$addr" ]; then
      # ⚠️ `brd +` is load-bearing: it keeps the VIP continuously present across the lease. dhcpcd
      # is about to be handed the same address, and if what we put on differs from what it wants
      # it does not update -- it deletes and re-adds. `ip addr add X/24` leaves the broadcast
      # unset; dhcpcd wants X.X.X.255. Measured, one variable at a time, against a real dnsmasq,
      # counting RTM_DELADDR:
      #
      #   ip addr replace X/24                 -> 1 delete
      #   ip addr replace X/24 brd +           -> 0 deletes
      #   ...also with noprefixroute, or the full dhcpcd shape -> 0
      #
      # The broadcast alone is the difference. A withdrawal cannot wedge a name (the reverse proxy
      # answers mDNS without probing), but an address that blinks is briefly not there for the ARP
      # cache of every device mid-connection.
      ${pkgs.iproute2}/bin/ip addr replace "$addr" brd + dev "$VIP_DEV"
      # A lease-holder here, never a gate: -b returns immediately, so nothing downstream waits on
      # a DHCP server. Failing to start it is not failing to serve -- the address is already up,
      # the point of applying before asking.
      ${dhcpcdRun} "$VIP_DEV" "$addr" -b || true
    else
      # Nothing to apply: this flock has never held an address. Ask, and wait for the answer.
      #
      # --waitip=4 must keep the `=`: the family is an optional argument, taken by getopt_long
      # only when attached. Written `--waitip 4`, the `4` becomes a positional, an interface name
      # to dhcpcd (`4: interface not found`), and the option means "wait for any family". On a
      # dual-stack household router SLAAC completes in ~0.3s while IPv4 is still ARP-probing, so
      # dhcpcd daemonises satisfied by IPv6, the IPv4-only read below finds nothing, and this unit
      # fails on a network where nothing is wrong. A test topology that sends no RAs cannot catch
      # it: there "any family" and "IPv4" behave the same.
      ${dhcpcdRun} "$VIP_DEV" "" --waitip=4
      # The field after `inet`, which is the same rule net.vip reads ground truth by -- never a
      # prefix match, which would accept an inet6 link-local and hand us an address to claim
      # that no one in the house can reach.
      addr="$(${pkgs.iproute2}/bin/ip -o -4 addr show dev "$VIP_DEV" scope global |
              ${pkgs.gnused}/bin/sed -n 's/^.* inet \([^ ]*\).*$/\1/p' | head -n1)"
      if [ -z "$addr" ]; then
        echo "briard-vip: no address configured, none stored, and DHCP yielded none on $VIP_DEV" >&2
        exit 1
      fi
    fi

    # Multicast leaves on the household NIC. Multicast is local by definition, so where it goes
    # out is where the LAN is, and apps ask exactly that to find their network: Home Assistant
    # picks the interface it announces and answers mDNS on, and the address of its own URL, by the
    # source address the kernel gives it for 224.0.0.251. Without this route that is the default
    # route NIC, the user-net WAN path nothing on the LAN can reach. No `src`: the kernel takes
    # the NIC address, so a renewed lease needs nothing here. Only multicast moves; the WAN path
    # stays.
    ${pkgs.iproute2}/bin/ip route replace 224.0.0.0/4 dev "$VIP_DEV"
    ${vipReplyRoute} up

    printf 'VIP_ADDR=%s\n' "$addr" >${vipLivePath}
    # Remember it for the flock, so the peer that promotes next need not ask. Only what DHCP gave
    # us: every node knows a configured address from its own config, and storing it would blur
    # what this file means -- "the address this flock acquired". Never fatal (the address is
    # claimed either way, and the agent-less harnesses run this unit with no DRBD volume mounted)
    # but never silent: silenced, "the peer came back with no address" cannot be told apart from
    # "the store was never written". Non-fatal decides whether to stop, not whether to say nothing.
    if [ -z "$configured" ]; then
      # ⚠️ The sync is the point, not hygiene. This file lets an unplanned failover -- when the
      # household router is least likely to answer, and usually a power cut -- re-claim the
      # flock address without asking anyone. Left in the page cache, it is lost in exactly that
      # event: btrfs commits on its own schedule (30s by default), and a guest killed inside that
      # window comes back with the volume mounted, intact, and the file absent. The router-down
      # assertion of `install-macvtap` catches this, and it can pass by hand because a guest left
      # running for a minute has committed while a test that restarts immediately has not.
      # `sync -f` (syncfs), not an fsync of the file: the file is new, so its directory entry must
      # be durable too, and syncing the volume is free at one small write per address acquisition.
      if printf '%s\n' "$addr" >${vipAddrFile} 2>/dev/null &&
         ${pkgs.coreutils}/bin/sync -f ${vipAddrFile} 2>/dev/null; then
        # The mount is named on the write as well as the read: "written here, read back
        # empty there" is only diagnosable if both lines say which `there` they meant.
        echo "briard-vip: remembered $addr for the flock at ${vipAddrFile} (mount=$(${pkgs.util-linux}/bin/findmnt -no SOURCE,FSTYPE ${btrfsRoot} 2>/dev/null || echo '<NOT MOUNTED>'))"
      else
        echo "briard-vip: WARNING could not write ${vipAddrFile} -- a peer promoting next will have to ask DHCP" >&2
      fi
    fi
  '';

  # Give the address back -- to the flock, not to the pool. -x exits the lease holder without
  # releasing (-k releases): a release hands the address to the router to give away before the
  # peer can claim it, the one thing a floating service address must never do.
  vipDown = pkgs.writeShellScriptBin "briard-vip-down" ''
    set -u
    # Captured before sourcing the live file, which sets VIP_ADDR to what we actually claimed.
    # Different questions: what we were configured with decides whether this NIC is ours alone;
    # what we claimed decides which address to withdraw.
    configured="''${VIP_ADDR:-}"
    live=""
    if [ -r ${vipLivePath} ]; then
      . ${vipLivePath}
      live="''${VIP_ADDR:-}"
    fi
    [ -n "$live" ] || live="$configured"
    ${pkgs.dhcpcd}/sbin/dhcpcd -x "$VIP_DEV" >/dev/null 2>&1 || true
    if [ -n "$live" ]; then
      ${pkgs.iproute2}/bin/ip addr del "$live" dev "$VIP_DEV" || true
    fi
    # The multicast route goes with the address it was added beside (briard-vip-up).
    ${pkgs.iproute2}/bin/ip route del 224.0.0.0/4 dev "$VIP_DEV" 2>/dev/null || true
    ${vipReplyRoute} down
    rm -f ${vipLivePath}
    # Take the NIC down when it is ours alone. The MAC on a dedicated service NIC is flock-scoped,
    # so shared with the peer, and a Secondary holding it up teaches the switch the wrong port for
    # the VIP the moment it emits any frame (an IPv6 RS, an mDNS query), and traffic for the
    # service address then goes to the node that is not serving. The wrong-port class is silent:
    # nothing is down, the address is gone, and the packets still vanish.
    #
    # ⚠️ The question is the device, not where the address came from. "Down only under DHCP"
    # ("static address => the NIC is shared => leave it up") is a proxy for the thing actually
    # feared: the agent-less harnesses set VIP_DEV=eth1, the DRBD NIC, where a link-down takes
    # replication with it. On a shipped node the proxy is false: a household that sets BRIARD_VIP
    # (documented, supported) gets VIP_DEV=eth2, a dedicated service NIC shared with nothing
    # local, and under that rule every Secondary in the flock would keep the flock MAC up.
    #
    # ⚠️ Argued, not measured, and the distinction is not a formality. This makes the static path
    # behave like the DHCP path on a hazard the file already treats as real; the hazard biting is
    # not demonstrated. A wrong port needs a right port to exist, so it takes two nodes, and no
    # test in the tree moves work between two installed nodes; a one-node assertion passes
    # identically against fixed and unfixed guests, because a standby deletes the VIP address
    # either way and nobody answers ARP for an address they do not hold. Do not read the install
    # rigs' green as cover for this line.
    #
    # So ask whether this NIC is the system/DRBD NIC. net.configure writes SYSTEM_DEV beside
    # VIP_DEV, so a real node always knows; an agent-less harness never sets it and falls through
    # to the DHCP-only rule, the conservative answer for a NIC we cannot identify. The
    # `-z "$configured"` arm keeps every NIC that held a DHCP address coming down.
    if [ -z "$configured" ] || { [ -n "''${SYSTEM_DEV:-}" ] && [ "$VIP_DEV" != "''${SYSTEM_DEV}" ]; }; then
      ${pkgs.iproute2}/bin/ip link set dev "$VIP_DEV" down || true
    fi
    exit 0
  '';

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
    # takeover's gratuitous ARP is explicit (vipArping crafts its own frames) and unaffected.
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
