    # NOTHING TO POKE FOR THE NAME: the door re-reads the live file on its own tick and the
    # records follow the address within seconds ([B.152]). A unit to restart here was what the
    # name cost when a separate publisher held it.
# Briard VM unit image.
#
# The workload + DRBD 9 + drbd-reactor run *inside* this VM; the host agent runs
# outside it (the host/guest boundary, V0). drbd-reactor's promoter
# drives the ordered failover unit {DRBD-primary → data mount → services → VIP}
# on whichever node holds the DRBD primary role. The per-resource
# promoter rules are supplied per-deployment as a snippet the agent drops into /run/briard/drbd-reactor.d
# (the agent writes them in prod, V0; the harness writes them in tests),
# so this image stays generic and the daemon is idle until one appears.
{ config, pkgs, lib, ... }:
let
  btrfsRoot = "/var/lib/briard"; # the DRBD btrfs volume mount
  # ── THE STORAGE SEAM ([V3b.33]) ──────────────────────────────────────────────────────────────
  # The stack under the mount above is `dataDisk -> LUKS -> PV -> VG -> LV -> DRBD -> btrfs`, and
  # the VG exists for exactly one reason: an LV can have its dm table reloaded underneath a device
  # that is open, so the backing can be moved onto an encrypted PV -- OR OFF ONE -- with `pvmove`
  # while DRBD never closes its device. A bare disk has no table to reload, which is why the seam
  # had to exist before the encryption did.
  #
  # It is nearly free because a linear LV IS dm-linear -- one table line, the same target and
  # the same linear_map() a bare disk would have had. Everything LVM adds sits outside the data
  # path (a PV label, a 1 MiB metadata area, userspace, udev), and two of those additions are why
  # LVM beats raw `dmsetup` here: nothing has to rebuild a table at every boot, and "which backing
  # is this node on" lives ON THE DISK rather than in host config.
  #
  # ⚠️ NONE OF THOSE NAMES LIVE HERE ANY MORE ([V3b.33](d)). The device, the VG, the LV and the
  # encryption mode arrive in the spec the HOST writes to /run/briard/node-storage.json, because
  # storage policy is a node-scoped fact the host holds and pushes at bring-up (AGENTS §5) -- and
  # a unit whose only inputs are what it can read off the machine has nowhere for a decision to
  # arrive, which is why (c) could not build its Adiantum opt-in. This image contributes the unit
  # file; the pushed agent contributes every name and every choice.
  #
  # The one-time format marker and the snapshots directory left with it: both are the pushed
  # agent's now (agent/guestagent's dataFormatMarker and snapshotsDir), because the unit that
  # reads one and makes the other is Go rather than shell.
  tlsDir = "${btrfsRoot}/tls"; # cert/key on the DRBD volume (replicated, survive failover)
  # The VIP's address AND device are both agent-determined: net.configure writes VIP_ADDR +
  # VIP_DEV here, and briard-vip.service reads this file and NOTHING ELSE. Nothing is baked, so
  # there is nothing to fall back to and no address or NIC anyone has to attribute after the fact.
  #
  # The address used to be baked outright ("v0 fixed service VIP, not a knob"). That made the
  # product work on the one subnet our lab happens to use and **fail green** on every other:
  # the readiness probe runs in-guest, against an address the guest itself owns, so a node no
  # one in the house could reach still reported ready (V3.19). The LAN owns that value now.
  #
  # The DEVICE went the same way, and it took a field failure to earn it: a guest that reboots
  # while its host agent is absent has no /run (tmpfs), so briard-vip ran on the baked `eth1` --
  # the DRBD replication NIC -- claimed the service address there, and took a SECOND DHCP lease
  # doing it, because the client-id is derived from that NIC's own MAC ([V3b.16]). The baked
  # device was only ever the agent-less harnesses' fallback, and a fallback every test agrees
  # with is indistinguishable from a default nobody chose. Deleting it is safe for exactly one
  # reason, and that reason is the whole of [V3b.16a]: drbd-reactor no longer starts at boot, so
  # nothing can read this file before the agent has written it (see drbd-reactor.service below).
  #
  # The harnesses DECLARE their own device and address (nixosTest/lib.nix, and the driver-based
  # tests via VIP_DEV/VIP_ADDR), which turns an inherited assumption into a stated one.
  vipEnvPath = "/run/briard/vip.env";
  # The topology word node-storage writes beside vip.env ([B.145c]): `flock` or `alone`, and the
  # only thing a shell unit needs to know about the spec. PAIRED with the Go const
  # guestagent.topologyEnvPath and the two values its topologyEnv writes.
  topologyEnvPath = "/run/briard/topology.env";
  # ── THE IMAGE'S HALF OF A UNIT THE AGENT WRITES ([B.160]) ────────────────────────────────────
  #
  # THE IMAGE CONTRIBUTES THE CLOSURE; THE PUSHED AGENT CONTRIBUTES THE UNIT. Since [B.86j] the
  # image bakes exactly one binary and every other briard binary rides the host bundle -- but the
  # units that start them were left behind here, and a unit is mostly an ExecStart plus the
  # environment that binary needs, so the two moved on different cadences. Measured: a pushed
  # agent asked its older image for a unit that image did not define, and the node crash-looped 53
  # times with the household's app gone ([B.159](c)). The agent writes the units it owns into
  # /run/systemd/system at every start now -- the only place it can, since NixOS makes
  # /etc/systemd/system a read-only store path -- so a unit can never be older than the binary
  # that wrote it, and the whole defect class is unrepresentable rather than merely detectable.
  #
  # WHAT STAYS HERE IS THE PART A PUSHED BINARY CANNOT CARRY: the store paths. A unit's PATH is a
  # closure reference rather than a bare command, and an agent that guessed one would fail at
  # block-device time, which is the worst moment this product has. So the image publishes ONE
  # profile at a fixed path and the agent names that path: no manifest, no schema, nothing to
  # version between the two halves.
  #
  # WHOLE PACKAGES, NOT NAMED COMMANDS, and that is the point rather than laziness: a pushed agent
  # that starts calling `lvs` must not need a new image to do it. Naming commands would put the
  # skew back one level down. The list is the same one the units carried in their own `path =`, so
  # the move leaves the closure unchanged.
  #
  # ⚠️ AN OLDER IMAGE HAS NO PROFILE AT ALL, and that is the good failure: the agent stats this
  # directory before it renders and refuses to serve when it is missing, so its trial fails, the
  # committed agent comes back, and the node keeps running the release it had. Refusing the
  # upgrade is what this buys; stopping the host from OFFERING it is [B.159](e)'s floor, which is
  # still wanted for genuine on-disk migrations.
  guestTools = pkgs.buildEnv {
    name = "briard-guest-tools";
    paths = [
      pkgs.lvm2.bin # pvcreate/vgcreate/lvcreate/vgchange/vgs -- the `bin` output, already in this closure
      pkgs.cryptsetup
      pkgs.kmod # modprobe dm-mirror, which LVM cannot autoload here ([V3b.33](a))
      pkgs.drbd # drbdadm create-md / new-current-uuid, and drbdmeta for the lone-node probe
      pkgs.btrfs-progs # mkfs.btrfs, once, on the LV node storage just created ([B.145a])
      pkgs.coreutils # install, test, rm
      config.systemd.package # systemctl start drbd@<res>.target
    ];
    pathsToLink = [ "/bin" "/sbin" ];
  };
  # PAIRED with the Go const guestagent.defaultToolsBin, which is what an agent-written unit puts
  # on its PATH. Different languages, so no shared import; the agent-side comment names this back.
  toolsEtc = "briard/tools";
  # THE CHAIN, stated once: the seven promoter units in start order, the same list the host's
  # promoterUnits() hands drbd-reactor. The hold resets them, and the lone node's target (the
  # mkMerge tail of this file) carries them.
  chainMembers = [
    "briard-primary-storage.service"
    "briard-services.service"
    "briard-vip.service"
    "briard-reverse-proxy.service"
    "briard-dashboard.service"
  ];
  # A hold step with one body per topology ([B.145c]). The word decides; a missing word is
  # "node storage has not run on this boot", and an unknown one names itself rather than
  # guessing -- both are failures of the step, and what a failed step means is the unit's to say
  # (ExecCondition skips, ExecStartPre escalates).
  byTopology =
    name:
    { flock, alone }:
    pkgs.writeShellScript name ''
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
  # Where the agent drops drbd-reactor's promoter snippet. Tmpfs, and PAIRED with the Go const
  # guestagent.reactorPath -- different languages, so no shared import; the agent-side comment
  # names this file back.
  reactorSnippetDir = "/run/briard/drbd-reactor.d";
  # The FLOCK's service address, replicated with the data. Same shape, same place and same
  # write-authority as the TLS material: a small flock-scoped fact at the btrfs root, written only
  # by the node that holds the volume (only a Primary can mount it), read by whoever promotes next.
  #
  # It exists so a failover never has to ASK for the address. The MAC is *derived* -- every node
  # computes it from the flock id -- but an address is *acquired*, known only to whoever asked the
  # router, so it is the one piece that has to travel. Without it, a node that has never served
  # would have to run a full DHCP exchange at the exact moment a household's network is least
  # likely to be answering.
  vipAddrFile = "${btrfsRoot}/.vip-address";
  # The address this node ACTUALLY claimed, written by briard-vip once it has resolved one and
  # read by everything downstream (the gratuitous ARP, the mDNS name). Under DHCP the configured
  # value and the claimed value are not the same thing, and a name or an announcement must never
  # describe an address the node did not take -- the same ground-truth rule net.vip follows.
  vipLivePath = "/run/briard/vip.live";
  # `ip` wants the prefix, `arping` wants the bare address. Strip it here rather than carry
  # the address twice: two variables are two things that can disagree, and the one that
  # would silently win is the gratuitous ARP nobody is watching.
  # Announced from the LIVE file rather than the unit's environment. It runs as ExecStartPost of
  # the same unit that resolved the address, and whether systemd re-reads an EnvironmentFile
  # between an ExecStart and an ExecStartPost is exactly the kind of thing that must not be the
  # reason a gratuitous ARP names the wrong address. Sourcing it is one line and no assumption.
  vipArping = pkgs.writeShellScript "briard-vip-arping" ''
    set -eu
    . ${vipLivePath}
    exec ${pkgs.iputils}/bin/arping -A -c 1 -I "$VIP_DEV" "''${VIP_ADDR%%/*}"
  '';
  # The address-changed handler. ONE path for every cause -- a NAK, a lease yielded to a host
  # that ARP-claimed it, a router that repooled while the flock had no primary -- because "the
  # address changed" does not care why it changed.
  #
  # WHAT IT DELIBERATELY DOES NOT DO: restart briard-vip. That was the first shape, and it is
  # wrong twice over. (1) This hook runs as a descendant of briard-vip's own cgroup (dhcpcd was
  # started from its ExecStart), so restarting that unit KILLS THE PROCESS ASKING FOR THE
  # RESTART, mid-flight. (2) It would put an ordinary address change through the drbd-reactor
  # promoter chain -- a member whose failure trips OnFailure=drbd-demote-or-escalate -- which is
  # a failover-shaped risk taken for a job that needs no chain at all. The front door binds :80
  # on every interface, so it does not care what the address is; the only things that do are the
  # live file, the flock's store, the ARP announcement, and the name. All four are done HERE.
  #
  # A lease that EXPIRES needs nothing from this hook either: the interface then holds no
  # address, net.vip reports "" as ground truth, and the node reads not-ready by the same rule
  # that covers every other addressless data node. The honest signal already flows.
  # WHAT HANDS THE RESOURCE ON WHEN A CHAIN MEMBER GIVES UP ([V3b.5](c)), and it has to be a
  # STATE hook rather than a dependency, which is the whole finding.
  #
  # `Requires=` is JOB-level: systemd consults it when a stop or restart job is enqueued on the
  # depended-upon unit, and never against that unit's state (`transaction.c`, atom
  # UNIT_ATOM_PROPAGATE_STOP on UNIT_REQUIRED_BY). That is why the promoter target's default
  # `Requires=` on its members demoted this node on EVERY crash: `Restart=` enqueues its
  # auto-restart with job mode JOB_RESTART_DEPENDENCIES, which propagates a TRY_RESTART up to the
  # target, which stops drbd-promote@ (PartOf) -- measured, and it took the resource away from a
  # live peer in 2 of 5 door crashes. With `target-as = Wants` (agent/drbd/config.go) nothing
  # upstream reacts to a member at all, which is right for a crash and wrong for a member that
  # has genuinely given up.
  #
  # OnFailure= is the missing half: `unit.c` fires it on the transition INTO UNIT_FAILED, with no
  # reference to restart mode. Under `RestartMode=direct` a member never enters that state while
  # it is being auto-restarted, so this stays silent through the transient crashes and fires
  # exactly once -- when the start limit is exhausted and the unit really has stopped trying.
  #
  # It points at OUR hold unit rather than upstream's drbd-demote-or-escalate@ directly, and the
  # reason is ordering: reactor re-promotes ~2s after a demote completes, so the mask that says
  # "not me, for now" has to go on BEFORE the demote, not after it. briard-promotion-hold does
  # both in that order and carries the same FailureAction=reboot for a demote DRBD refuses.
  chainMemberFailure = {
    OnFailure = "briard-promotion-hold.service";
    OnFailureJobMode = "replace-irreversibly";
  };

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

    # Withdraw the address WE put on optimistically. dhcpcd removes the addresses it manages,
    # but the one applied at promotion from the flock's store is foreign to it -- so without
    # this the NIC quietly ends up holding two, and "the first address on the device" (which is
    # how net.vip reads ground truth) becomes a coin toss between the live one and a stale one.
    if [ -n "$cur" ]; then
      ${pkgs.iproute2}/bin/ip addr del "$cur" dev "''${interface}" || true
    fi
    ${pkgs.iproute2}/bin/ip addr replace "$addr" brd + dev "''${interface}"
    printf 'VIP_ADDR=%s\n' "$addr" >${vipLivePath}
    # Synced for the same reason as the promotion-time write ([V3.23]): this is the path where the
    # router hands us a DIFFERENT address, so it is the one where the previously stored value is
    # actively WRONG. Losing this write to the page cache leaves the flock remembering an address
    # it has already yielded. (vipLivePath above is /run -- tmpfs, nothing to sync.)
    { printf '%s\n' "$addr" >${vipAddrFile} && ${pkgs.coreutils}/bin/sync -f ${vipAddrFile}; } 2>/dev/null || true
    VIP_DEV="''${interface}" ${vipArping} || true
    # THE NAME NEEDS NOTHING FROM HERE. The door publishes it and re-reads this file on its own
    # tick, so the records follow the address within seconds with no unit to poke and no ordering
    # between the two to get wrong ([B.152]).
    exit 0
  '';

  # An EMPTY config for the VIP's dhcpcd instance, and the emptiness is the point — every setting
  # this instance has is on its command line below, where it can be read.
  #
  # It is not merely tidiness. The system's generated /etc/dhcpcd.conf now says
  # `denyinterfaces ... eth2`, which is right for the boot-time client and would refuse the very
  # interface this instance exists to lease. And it is the file a nixpkgs bump would add `duid`
  # to — the setting that would quietly give the two nodes of one flock different identities. An
  # instance that inherits neither cannot be surprised by either.
  dhcpcdConf = pkgs.writeText "briard-vip-dhcpcd.conf" "";

  # dhcpcd, scoped to the service NIC and to ONE job: hold the binding. It does not supply the
  # address (the store or the operator did) and it must not supply anything else. Every flag here
  # is load-bearing:
  #
  #   <dev> as the sole interface argument makes this a SEPARATE dhcpcd instance from the system
  #     one, with its own pidfile and control socket ("runs as a separate instance to other
  #     dhcpcd processes"). That is what lets the service NIC be denied at boot and leased at
  #     promotion, which is the whole point: a boot-time client would lease this NIC on the
  #     SECONDARY too, and after the flock MAC both nodes would ask one router for one lease.
  #   -G (nogateway): left at its default dhcpcd installs a default route from this lease, so
  #     PROMOTION would silently re-plumb the guest's WAN path (eth0, the SLIRP route out) and
  #     demotion would revert it. Routes are dhcpcd's own doing, not a hook's, so this flag is
  #     the only thing that stops it.
  #   -c <hook> replaces dhcpcd-run-hooks entirely for this instance, so the stock hooks never
  #     run and resolv.conf is never rewritten -- ours is the only script. -C resolv.conf is
  #     kept alongside as the belt to that braces: it is what still protects us if -c is ever
  #     dropped and the standard runner comes back.
  #   -L (noipv4ll) because a SERVICE address may not be invented by the machine that serves it.
  #     Left on, dhcpcd's answer to "no server answered" is to self-assign 169.254.x.x -- and
  #     measured, that is exactly what it did: the node then probed its own link-local address,
  #     passed (it owns it), and reported HEALTHY while nobody on the LAN could reach it. That is
  #     V3.19's own failure restored by its replacement, and a house whose router is briefly down
  #     at boot would have hit it. A VIP is the flock's address or it is nothing; a self-assigned
  #     one is worse than none because it looks like success.
  #   -I "01:<mac>" states the client-id OUTRIGHT: RFC 2132 type 1 (ethernet) + this NIC's
  #     address, which dhcpcd encodes as hex because the value is colon-separated. One flock then
  #     presents ONE identity, which is what makes a lease survive a failover -- and what stops
  #     dhcpcd's own shipped `duid` (a per-host DUID that, in the man page's words, "should not be
  #     copied to other hosts") from giving two nodes of one flock different leases if a nixpkgs
  #     bump ever restores it.
  #
  #     ⚠️ It was `-I ""` -- the documented way to ask for the hardware-address default -- and that
  #     was WRONG ON THE WIRE, which no amount of reading the command line could show. dhcpcd took
  #     the literal string "-h" as the client-id (dnsmasq recorded `00:2d:68`: type 0, then ASCII
  #     "-h"), which also consumed the -h option, so every node sent its SYSTEM hostname ("guest")
  #     and an identical client-id. Two different flocks on one LAN would then have fought over a
  #     single lease. Found by [B.78]'s router the first time anything looked at what we actually
  #     transmit. Never ask for a default when you can state the value.
  #   -h briard-<xxxxxx> is the FLOCK's name (option 12), taken from the low three bytes of the
  #     service NIC's own MAC -- which IS the flock id's derivative, read as ground truth off the
  #     interface rather than plumbed through as a second copy. It gives a household's router a
  #     recognisable client-list entry, and it makes a user-created static reservation survive
  #     failover, because name, MAC and client-id are all flock-scoped.
  #   --lastleaseextend keeps the address when no server answers, giving it up only to a host
  #     that ARP-claims it. It violates RFC 2131 3.7 knowingly and it is the right violation
  #     here: the case it covers is an unplanned failover with the router down, and the thing it
  #     still refuses to do is squat an address somebody actively wants.
  #   -r asks for the address we already claimed, so the replicated store wins over this node's
  #     own lease file instead of the two quietly drifting apart.
  dhcpcdRun = pkgs.writeShellScript "briard-vip-dhcpcd" ''
    set -eu
    dev="$1"; want="$2"; shift 2
    mac="$(cat /sys/class/net/"$dev"/address)"
    hex="''${mac//:/}"
    set -- -f ${dhcpcdConf} -c ${vipHook} -L \
      -G -C resolv.conf -I "01:$mac" -h "briard-''${hex: -6}" --lastleaseextend "$@"
    if [ -n "$want" ]; then
      set -- "$@" -r "''${want%%/*}"
    fi
    exec ${pkgs.dhcpcd}/sbin/dhcpcd "$@" "$dev"
  '';

  # Force a RENEW, on a ten-minute timer, INDEPENDENTLY of the lease term.
  #
  # WHY NOT A SHORTER LEASE. Asking for one (`-l`) looks like the obvious lever and is the wrong
  # one twice over. The term is the SERVER's to choose, so on a CPE that ignores a client's option
  # 51 the flag is a silent no-op and we would believe we had shortened something -- and where it
  # IS honoured it shortens the wrong thing, because a short lease makes --lastleaseextend, a
  # knowing RFC 2131 3.7 violation kept as an exceptional safety net, into the routine state for
  # the length of any router blip. We want the lease LONG and the renewal FREQUENT; nothing in RFC
  # 2131 requires a client to wait for T1, so the honest implementation is to renew early.
  #
  # WHAT IT BUYS is a shorter silence, not a firmer grip. A CPE reboot is the household's usual
  # fix for anything, it commonly loses the lease table, and a client sitting BOUND says nothing
  # at all until T1 -- at a router's usual 12-24h term, hours during which a fresh DISCOVER from
  # any device can be handed the address we still think is ours. Ten minutes bounds that window,
  # and on dnsmasq a renewal against a server that lost its table is ACKed, so the binding is
  # recreated rather than merely observed to be gone. It is also how we learn FAST in the case
  # where we lost it anyway: a NAK inside ten minutes drives the address-changed path, which is
  # what a briard.casa name depends on.
  #
  # ⚠️ THE GUARD IS THE POINT OF THIS SCRIPT, and it is not defensive programming. `dhcpcd -N`'s
  # documented behaviour with no instance running is "starts up as normal" -- and that is measured,
  # not inferred from the man page: run against an idle interface it forked a client and took an
  # address. On a STANDBY node, whose service NIC must have no LAN presence at all, that would put
  # a DHCP client on it and claim an address the flock's primary owns. The units below already
  # make it unreachable, since the timer lives and dies with briard-vip; this is what keeps it
  # safe under a hand-run `systemctl start` too.
  vipRenew = pkgs.writeShellScript "briard-vip-renew" ''
    set -eu
    pidfile="$(${pkgs.dhcpcd}/sbin/dhcpcd -P "$VIP_DEV")"
    if [ ! -s "$pidfile" ] || ! kill -0 "$(cat "$pidfile")" 2>/dev/null; then
      echo "briard-vip-renew: nothing is holding $VIP_DEV; declining to start a client" >&2
      exit 0
    fi
    exec ${pkgs.dhcpcd}/sbin/dhcpcd -N "$VIP_DEV"
  '';

  # Resolve the service address, claim it, and record what was actually claimed.
  #
  # THREE SOURCES, and the order is the design:
  #   1. VIP_ADDR -- an address the operator named. We set it, so we never ask about it.
  #   2. the replicated store -- the FLOCK's address, known even to a node that has never served.
  #      This is what keeps a failover off the household router entirely.
  #   3. DHCP, synchronously -- ONLY when both above are empty, which is the first promotion this
  #      flock has ever performed. That exception lands exactly where the existing doctrine
  #      already puts it: installing may need the network, running and failing over must not.
  #
  # The claim in cases 1 and 2 is OPTIMISTIC -- no ARP probe gate. A conflict at promotion time
  # was not caused by the promotion; it can only have arisen while the flock had no primary. So
  # detecting it is dhcpcd's job, afterwards, rather than a cost every failover pays to find
  # someone else's pre-existing condition. When it does find one it yields, the address changes,
  # and that is handled by the one address-changed path -- which does not care what caused it.
  vipUp = pkgs.writeShellScript "briard-vip-up" ''
    set -eu
    ${pkgs.iproute2}/bin/ip link set dev "$VIP_DEV" up
    mkdir -p "$(dirname ${vipLivePath})"

    configured="''${VIP_ADDR:-}"
    addr="$configured"
    if [ -z "$addr" ] && [ -r ${vipAddrFile} ]; then
      addr="$(cat ${vipAddrFile})"
    fi

    echo "briard-vip: dev=$VIP_DEV configured=''${configured:-<none>} stored=$([ -r ${vipAddrFile} ] && cat ${vipAddrFile} || echo '<none>')"
    # WHY there is no stored address, not merely that there isn't one. `stored=<none>` has three
    # very different causes -- the volume is not mounted, the volume is mounted and the file is
    # absent, or this flock genuinely never held an address -- and the line above cannot tell them
    # apart. That ambiguity is [V3.23]: the address is written on one boot
    # ("remembered ... for the flock") and read back as <none> on the next, and no log anywhere
    # says which of the three it was. One `findmnt` and one `ls` at the moment of the read settle
    # it, in the field as well as in a harness.
    if [ -z "$configured" ] && [ ! -r ${vipAddrFile} ]; then
      echo "briard-vip: no stored address at ${vipAddrFile} -- mount=$(${pkgs.util-linux}/bin/findmnt -no SOURCE,FSTYPE ${btrfsRoot} 2>/dev/null || echo '<NOT MOUNTED>') contents=[$(${pkgs.coreutils}/bin/ls -A ${btrfsRoot} 2>/dev/null | ${pkgs.coreutils}/bin/tr '\n' ' ')]"
    fi
    if [ -n "$configured" ]; then
      # An address the operator named. Claim it and hold NO lease: there is nothing to renew,
      # and asking a router about an address we were told to use would be asking permission for
      # something already decided. It is also what keeps dhcpcd away from the agent-less
      # harnesses, where this unit's NIC is eth1 -- the DRBD link, which must never lease.
      ${pkgs.iproute2}/bin/ip addr replace "$addr" brd + dev "$VIP_DEV"
    elif [ -n "$addr" ]; then
      # ⚠️ `brd +` IS LOAD-BEARING: it keeps the VIP CONTINUOUSLY PRESENT across the lease. dhcpcd
      # is about to be handed the SAME address, and if what we put on differs from what it wants
      # it does not update -- it DELETES and re-adds. `ip addr add X/24` leaves the broadcast
      # unset; dhcpcd wants X.X.X.255. Measured, one variable at a time, against a real dnsmasq
      # (lab/avahi-repro5-deladd.nix), counting RTM_DELADDR:
      #
      #   ip addr replace X/24                 -> 1 delete
      #   ip addr replace X/24 brd +           -> 0 deletes
      #   ...also with noprefixroute, or the full dhcpcd shape -> 0
      #
      # So the broadcast alone is the whole difference. A withdrawal here is no longer able to
      # wedge a name -- the responder claims names without probing ([B.152]) -- but an address
      # that blinks is still an address that is briefly not there, for the ARP cache of every
      # device mid-connection. V3.22 is the epoch record of what it used to cost.
      ${pkgs.iproute2}/bin/ip addr replace "$addr" brd + dev "$VIP_DEV"
      # A lease-holder here, never a gate: -b returns immediately, so nothing downstream of this
      # unit waits on a DHCP server. Failing to start it is not failing to serve -- the address
      # is already up, which is the entire point of applying before asking.
      ${dhcpcdRun} "$VIP_DEV" "$addr" -b || true
    else
      # Nothing to apply: this flock has never held an address. Ask, and wait for the answer.
      #
      # --waitip=4 MUST keep the `=`. The family is an OPTIONAL argument, so getopt_long only
      # takes it when it is attached; written `--waitip 4` the option gets no family at all and
      # the `4` becomes the next POSITIONAL, which for dhcpcd is an interface name (it says so:
      # `4: interface not found`). The option then means "wait for ANY family" -- and on a
      # dual-stack household router SLAAC completes in ~0.3s while IPv4 is still ARP-probing, so
      # dhcpcd daemonises satisfied by an IPv6 address, the IPv4-only read below finds nothing,
      # and this unit fails on a network where nothing is wrong. It cannot be caught by a test
      # topology that sends no RAs, because there "any family" and "IPv4" are the same behaviour
      # (V3.21, found on the first install onto a real home LAN).
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

    printf 'VIP_ADDR=%s\n' "$addr" >${vipLivePath}
    # Remember it FOR THE FLOCK, so the peer that promotes next does not have to ask. Only what
    # DHCP gave us: a configured address is already known to every node from its own config, so
    # storing it would blur what this file means -- "the address this flock ACQUIRED".
    #
    # Never fatal -- the address is claimed either way, and the agent-less harnesses run this unit
    # with no DRBD volume mounted at all -- but NOT silent. It was silenced, and that is precisely
    # why "the peer came back with no address" could not be told apart from "the store was never
    # written". Non-fatal is a decision about whether to stop; it is not a decision to say nothing.
    if [ -z "$configured" ]; then
      # ⚠️ THE SYNC IS THE POINT, not hygiene ([V3.23]). This file exists so that an UNPLANNED
      # failover -- the case where the household router is least likely to be answering -- can
      # re-claim the flock's address without asking anyone. An unplanned failover is, by
      # definition, usually a power cut. Writing it into the page cache and hoping means the one
      # event it exists for is the event that loses it: btrfs commits on its own schedule (30s by
      # default), and a guest killed inside that window comes back with the volume mounted, intact,
      # and the file simply absent -- which is exactly what `install-macvtap`'s router-down
      # assertion has been failing on, and why it "worked" by hand (a guest left running for a
      # minute has committed; a test that restarts immediately has not).
      #
      # `sync -f` (syncfs) rather than fsync-the-file: the file is NEW, so its directory entry has
      # to be durable too, and syncing the whole volume is free at this size and frequency -- one
      # small write per address acquisition, not per request.
      if printf '%s\n' "$addr" >${vipAddrFile} 2>/dev/null &&
         ${pkgs.coreutils}/bin/sync -f ${vipAddrFile} 2>/dev/null; then
        # The mount is named on the WRITE as well as the read ([V3.23]): "written here, read back
        # empty there" is only diagnosable if both lines say which `there` they meant.
        echo "briard-vip: remembered $addr for the flock at ${vipAddrFile} (mount=$(${pkgs.util-linux}/bin/findmnt -no SOURCE,FSTYPE ${btrfsRoot} 2>/dev/null || echo '<NOT MOUNTED>'))"
      else
        echo "briard-vip: WARNING could not write ${vipAddrFile} -- a peer promoting next will have to ask DHCP" >&2
      fi
    fi
  '';

  # Give the address back -- to the FLOCK, not to the pool.
  #
  # -x exits the lease holder WITHOUT releasing (-k is the one that releases). A release hands
  # the address back for the router to give away before the peer can claim it, which is the one
  # thing a floating service address must never do.
  vipDown = pkgs.writeShellScript "briard-vip-down" ''
    set -u
    # Captured BEFORE sourcing the live file, which sets VIP_ADDR to what we actually claimed.
    # The two are different questions: what we were configured with decides whether this NIC is
    # ours alone; what we claimed decides which address to withdraw.
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
    rm -f ${vipLivePath}
    # TAKE THE NIC DOWN WHEN IT IS OURS ALONE. The MAC on a dedicated service NIC is flock-scoped
    # and therefore shared with the PEER, and a Secondary holding it up teaches the switch the
    # wrong port for the VIP the moment it emits any frame at all (an IPv6 RS, an mDNS query) --
    # traffic for the service then goes to the node that is not serving. That is the [B.100]/[B.101]
    # class, and it is silent: nothing is down, the address is gone, and the packets still vanish.
    #
    # ⚠️ THE QUESTION IS THE DEVICE, NOT WHERE THE ADDRESS CAME FROM, and it took [V3b.26d] to see
    # that. This used to read `[ -z "$configured" ]` -- down only under DHCP -- on the reasoning
    # "static address => the NIC is shared => leave it up". That is a PROXY, and it stands for the
    # thing actually feared: the agent-less harnesses set VIP_DEV=eth1, the DRBD NIC, where a
    # link-down takes replication with it. On a SHIPPED node the proxy is simply false. A household
    # that sets BRIARD_VIP -- a documented, supported option (DESIGN §4) -- gets VIP_DEV=eth2, a
    # dedicated service NIC shared with nothing local, and every Secondary in that flock kept the
    # flock MAC up. The hazard the old comment described was live on the very configuration it
    # exempted.
    #
    # ⚠️ ARGUED, NOT MEASURED, and the distinction is not a formality. The reasoning above stands on
    # reading -- the proxy is false, and this makes the static path behave like the DHCP path on a
    # hazard the file already treats as real. What is NOT demonstrated is the hazard biting: a
    # Secondary teaching the switch a wrong port needs a RIGHT port to exist, so it takes two nodes,
    # and no test in the tree moves work between two installed nodes ([B.113]). [V3b.26d] tried to
    # catch it with one and produced an assertion that passed identically against fixed and unfixed
    # guests -- because a standby deletes the VIP address either way, and nobody answers ARP for an
    # address they do not hold. Do not read the install rigs' green as cover for this line.
    #
    # So: ask whether this NIC is the system/DRBD NIC. SYSTEM_DEV is written beside VIP_DEV by
    # net.configure, so a real node always knows; an agent-less harness never sets it and falls
    # through to the old DHCP-only rule, which is exactly the conservative answer for a NIC we
    # cannot identify. The `-z "$configured"` arm stays, so this strictly WIDENS when the NIC
    # comes down and can regress nothing that came down before.
    if [ -z "$configured" ] || { [ -n "''${SYSTEM_DEV:-}" ] && [ "$VIP_DEV" != "''${SYSTEM_DEV}" ]; }; then
      ${pkgs.iproute2}/bin/ip link set dev "$VIP_DEV" down || true
    fi
    exit 0
  '';

in
{
  # What this appliance does not carry ([B.5]). Imported here rather than folded into the callers so
  # that the SAME slimming applies to the shipped disk and to every nixosTest that boots this
  # module -- a guest the tests exercise fatter than the one strangers install would prove nothing
  # about the one strangers install.
  # modules.nix (the denylisted module tree, [B.136]) is NOT imported here: it pins the initrd to
  # the virtio set the IMAGE boots with, and the nixosTests that build this module into test VMs
  # mount their store over virtiofs (measured: three nodes panicked in the initrd). It joins in
  # disk-image.nix, where the machine is known.
  imports = [ ./slim.nix ./pivot.nix ];

  # THE GUEST AGENT BUILD A TEST NODE IS PRE-DRESSED WITH ([B.139]). No unit in the SHIPPED image
  # names it: the image bakes the firmware alone, and every unit that needs the agent reaches it
  # at its committed path under the pivot's binDir, where the host's dress put it. A nixosTest
  # machine has no host to dress it, so nixosTest/lib.nix links this build in as if one had
  # (briard.pivot.preDressed) -- which is what makes those tests converge with the product's own
  # code rather than a harness stand-in.
  #
  # It stays an option rather than a bare callPackage so a caller that wants a VERSIONED build,
  # or a stand-in, hands one in instead of getting a second agent derivation alongside the first.
  options.briard.agentPackage = lib.mkOption {
    type = lib.types.package;
    default = pkgs.callPackage ../agent/package.nix { subPackage = "agent/cmd/briard-guest-agent"; };
    defaultText = lib.literalExpression "the briard-guest-agent build";
    description = "The briard-guest-agent build a test node is pre-dressed with, and that its shells invoke.";
  };

  # Image tarballs baked into this disk's closure and loaded into podman at boot. A NODE fact,
  # not a service one: an image has to be RESIDENT before anything renders against it, because
  # nothing on the failover path may pull. Used by the fleet's upgrade demo to pre-stage the
  # target of a rotation. Empty by default — the shipped image carries no workload.
  # HOW LONG A NODE REFUSES TO PROMOTE AFTER ONE OF ITS CHAIN MEMBERS GAVE UP ([V3b.5](c)).
  #
  # 300s is a judgement, not a measurement, and the trade is legible: a fault that TRAVELS with the
  # replicated volume (a bad routes table, a corrupt cert, a name that collides LAN-wide) is hit
  # identically by whoever promotes next, so the resource ping-pongs with a period of roughly this
  # value -- five minutes is about twelve hand-overs an hour, each costing a mount cycle and a
  # service restart. Longer is calmer and sidelines a recovered node for longer. A node-local fault
  # (memory pressure, a failing disk, a leaked process holding :80) does not ping-pong at all: the
  # peer simply keeps serving and this node's hold expires against a resource it cannot have.
  #
  # It is an OPTION rather than a constant so the contract test can drive the full lifecycle in
  # seconds. Nothing else varies it; a node in the field runs the default.
  options.briard.promotionHoldSecs = lib.mkOption {
    type = lib.types.ints.positive;
    default = 300;
    description = "Seconds a node refuses promotion after a promoter chain member exhausts its start limit.";
  };

  options.briard.stagedImages = lib.mkOption {
    type = lib.types.listOf lib.types.package;
    default = [ ];
    description = "Image tarballs pre-staged into local podman storage at boot.";
  };

  config = lib.mkMerge [ {
    system.stateVersion = "26.05";

    # No substituters and no baked cache key ([B.86i]): the guest has no nix (disk-image.nix,
    # `nix.enable = false`), so nothing in it ever fetches a store path. An OS release reaches a
    # node as a whole signed IMAGE over the guest chain, verified by the release keyring the HOST
    # holds; the image is what a release IS, and cache.briard.io and its narinfo key retired with
    # the closure path they served.

    # Answer the ACPI power button. systemd-logind is what listens for it, and
    # nothing on this appliance was starting it: it ships with no [Install] section and only
    # a dbus alias, so on a box with no logins nothing ever touches login1 and it stays down.
    # The guest therefore ignored QEMU's `system_powerdown` outright — measured, 60 s of
    # silence — which meant every clean shutdown the host can ask for (an OS upgrade's reboot
    # leg, a host reboot, a UPS event) degraded to killing QEMU: a power cut, to a machine
    # whose whole job is not losing data. logind is already in the closure, so this starts it
    # rather than adding anything, and its default HandlePowerKey=poweroff is what we want.
    systemd.services.systemd-logind.wantedBy = [ "multi-user.target" ];

    # DRBD 9: the out-of-tree module (the one with quorum), built against the default
    # 6.18 LTS kernel and loaded at boot. /proc/drbd then reports a 9.x module.
    #
    # PINNED AHEAD OF NIXPKGS, and it will stay pinned. nixos-26.05 ships 9.2.16 and
    # will keep shipping it for the channel's life: nixpkgs does not track the 9.2
    # maintenance line, unstable has already jumped to the 9.3 feature branch, so no
    # channel bump is coming. Three releases of the 9.2 line are worth having here:
    #
    #   9.2.17  DRBD asks the kernel for BLK_FEAT_STABLE_WRITES again. Kernel-side
    #           changes had silently dropped the flag, and DRBD needs it structurally:
    #           the same page feeds the local write AND the network send, so a page
    #           rewritten under writeback makes the two replicas diverge. Our backing
    #           disk is virtio-blk, which does not advertise it, and btrfs sits
    #           directly on /dev/drbd0 -- so 9.2.16 on 6.18 replicates without the
    #           guarantee. This one alone justifies the pin.
    #   9.2.18  Quorum arithmetic: a CONNECTED peer that is diskless *unintentionally*
    #           (a detached disk -- an anchor whose drive died, or the attach/detach
    #           window our pairing drives) was counted as a storage voter holding data.
    #           Wrong count, in calc_quorum, on the failover path, for the failure this
    #           product exists to survive. Also: `drbdadm attach` now waits for UUID
    #           negotiation and REPORTS a stale/diverged attach instead of returning 0
    #           with the device quietly diskless.
    #   9.2.19  Holds a primary-loss survivor at Consistent until the post-loss
    #           reconcile settles, gap-free, so it cannot declare itself authoritative
    #           before learning whether a peer holds a write it is missing. Plus the
    #           silent-divergence family LINBIT shipped two weeks early in April.
    #
    # NOT 9.3.x, though it is the same `overrideAttrs` away: 9.2 is the maintenance
    # branch, 9.3 is where new functionality lands, and 9.3 makes resync-without-
    # replication the default -- a rewrite of the exact path our heal invariants and
    # failover timings measure. 9.3 also brings variable bitmap granularity, and 9.2
    # REFUSES to attach metadata written with any granularity but 4k -- a trapdoor for
    # a product whose OS (and so whose DRBD module) can roll backwards.
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

    # drbd-reactor's promoter snippet lives on TMPFS, and is agent-written like everything else
    # node-scoped ([V3b.16b]). The host re-derives it from cfg.Promoter at every bring-up, so
    # persisting it bought nothing and cost the one thing that matters: a snippet on the overlay
    # outlived the agent that wrote it, which is what let a boot-started reactor promote against
    # configuration nobody had just restated ([V3b.16]). One lifetime for every fact the agent
    # writes means stale configuration cannot exist.
    #
    # A second, nearly-free backstop to [V3b.16a]'s gate, and the reason this is worth doing rather
    # than merely tidy: a reactor with no snippet is IDLE even if something starts it. The two
    # mechanisms fail independently.
    #
    # POINTED AT /run rather than symlinked into /etc: drbd-reactor takes the directory as config,
    # so this is a one-line edit and not a link somebody has to keep correct. The agent-less
    # harnesses declare their own snippet into the same directory (nixosTest/lib.nix).
    environment.etc."drbd-reactor.toml".text = ''
      snippets = "${reactorSnippetDir}"
    '';

    # THE TOOL PROFILE AN AGENT-WRITTEN UNIT PUTS ON ITS PATH ([B.160], `guestTools` above says
    # why it exists at all). A fixed path in /etc rather than a store path the agent would have to
    # be told: the whole point is that the two halves share a NAME and nothing else, so an agent
    # and an image that both speak B.160 need no handshake to agree.
    environment.etc.${toolsEtc}.source = guestTools;
    systemd.tmpfiles.rules = [
      "d /run/briard 0755 root root -"
      "d ${reactorSnippetDir} 0755 root root -"
      # DRBD's own state dir. Without it every attach logs
      #   lk_bdev_save(/var/lib/drbd/drbd-minor-0.lkbd) failed: No such file or directory
      # which is harmless (it caches the backing device's last known size) but sat directly on top
      # of the real error while V3.22 was being read, and cost time twice. Upstream ships this dir
      # in its package; nixpkgs' drbd does not create it.
      "d /var/lib/drbd 0700 root root -"
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

    # THIS NODE'S NAME is set by the agent at every bring-up (sys.hostname) and NOTHING in this
    # image restores it. A `briard-identity` oneshot used to, reading it back from
    # /etc/briard/node-id before drbd-reactor could act; [V3b.16b] deleted the unit and the file.
    # The reasoning is kept because it is a shape rather than one unit's story.
    #
    # What it solved (V3.20): the baked hostname is "guest" (disk-image.nix) and
    # syscall.Sethostname does not survive a reboot, while the `.res` naming this node did. So a
    # rebooted guest ran as "guest" against a persistent config saying `on briard-node-<id>`, the
    # boot-started reactor promoted into the mismatch, drbd@<res> failed, and a failed promote is
    # never retried -- the node parked quorate but never Primary, with no VIP and no address.
    # Invisible before V3.20 because the baked hostname and the node name were the SAME LITERAL.
    #
    # The fix then was to give the two facts one lifetime by making the NAME persistent. This is the
    # same principle read the other way: make the `.res` EPHEMERAL and the name with it. Both are
    # node-scoped, the host re-derives both at every bring-up (cfg.Node, cfg.Resource), and
    # [V3b.16a] means nothing can promote before that bring-up has happened. Two facts with one
    # lifetime cannot disagree -- and this way there is no third copy on disk to be right or wrong
    # about, which is what a restored-at-boot file always is.

    # THE PROMOTER IS ARMED BY THE AGENT, NEVER BY BOOT ([V3b.16a]). Failover is still entirely
    # drbd-reactor's (AGENTS §4.2 is untouched: nothing here promotes or demotes) -- what changed
    # is WHEN the orchestrator is allowed to start, and the answer is "once the host has told this
    # guest who it is, where its NICs are, and what its promoter chain is". guestagent's BringUp
    # ends in `systemctl start drbd-reactor.service`, and that is now the only thing that starts it.
    #
    # WHY, and it is not the agent-absence case alone. The promoter snippet lives on the PERSISTENT
    # overlay, so on a first install there is no snippet and the ordering held by construction --
    # but on every reboot afterwards the snippet is already on disk and a boot-started reactor
    # RACES the agent's reconnect -> net.configure -> vip.env. The agent usually won, which is why
    # this looked like an agent-absence bug when a stranger's node finally lost the race ([V3b.16]:
    # the VIP claimed on the DRBD NIC, under a second DHCP identity, mDNS dead, probe blind).
    #
    # THERE IS NO DEADLOCK TO DESIGN AROUND, which is what makes it nearly free: bring-up gates on
    # QUORATE, not Primary (host.go), so the agent never waits for a promotion; and quorum does not
    # need the reactor either, because the agent attaches DRBD itself (storage.node, [V3b.33](d)).
    # So promotion may safely wait for the agent, and does.
    #
    # THE COST, stated rather than buried: a permanently absent agent -- /opt/briard wiped, the unit
    # masked, an incompatible binary after an upgrade -- is now a TOTAL outage rather than a
    # degraded-but-serving node. Accepted: [V3b.16] is the field evidence for what
    # degraded-but-serving actually looked like, and a node that plainly does not serve is more
    # honest and more repairable than one that is up in a way nobody can see or fix.
    #
    # It also makes bring-up the one place that arms the promoter, so an agent SIGKILLed inside a
    # maintenance bracket -- whose resume existed only on the dead process's stack ([V3b.15]) --
    # re-arms it by restarting. And it makes the agent-less state inert, so the deadman's reboot
    # cycle stops churning DRBD and the household's DHCP server on every pass.
    systemd.services.drbd-reactor = {
      description = "drbd-reactor — DRBD failover orchestrator";
      wantedBy = [ ];
      after = [ "network.target" ];
      path = [ pkgs.drbd pkgs.systemd ]; # drbdsetup/drbdadm + systemd management
      serviceConfig = {
        Type = "notify";
        ExecStart = "${pkgs.drbd-reactor}/bin/drbd-reactor";
        Restart = "on-failure";
        # DEFUSE THE PROMOTE-VS-STOP DEADLOCK ON EVERY STOP, not just on the one the agent
        # drives. drbd-reactor writes itself an ordering drop-in saying drbd-services@r0.target
        # comes Before= this daemon; on the way DOWN that reverses, so systemd stops the daemon
        # first and the target after. A stopping reactor re-emits its exists-events and fires one
        # last `systemctl start drbd-services@r0.target`, systemd refuses it (destructive: a
        # shutdown is already queued), the reactor reads the refusal as a failed start and answers
        # with `systemctl stop drbd-services@r0.target` -- a job it then WAITS for, and which the
        # ordering above sequences behind its own stop. Neither can proceed: 90s TimeoutStopSec,
        # then SIGKILL.
        #
        # MEASURED, from the guest's console during `briard rescue` ([B.85]): the shutdown began
        # 1s after os.poweroff, deadlocked at +11.5s, and finished at +101s. It was read as "the
        # guest agent ignores os.poweroff" because the host has no console on its guest and the
        # two 90s constants -- this TimeoutStopSec and the host's shutdownGrace -- expired
        # together, so the ACPI fallback appeared to do the work the SIGKILL had just done.
        #
        # Removing the drop-in first is drbd-reactor's own sanctioned defusal: it is what
        # `reactor.pause` does (guestagent.go, verbReactorPause), and it is race-free because the
        # promoter only (re)writes the file in Promoter::new -- i.e. on the next START -- so
        # nothing re-arms it while we are stopping. The deadlock is [B.28]; the defusal is guarded
        # on the shipped artifact by nixosTest/guest-rescue.nix (no unit may hold the shutdown).
        #
        # ON ExecStop RATHER THAN IN THE VERB, and that is the point: this stop happens on paths
        # no agent verb touches -- the deadman's `systemctl reboot`, a user rebooting the host
        # (the guest unit's ExecStop -> ACPI -> this same shutdown), a guest that reboots itself.
        # systemd runs ExecStop= before it signals the process, so the ordering is gone before the
        # reactor's last gasp. Both commands are `-` prefixed: a failure here must never turn a
        # stop into a failed stop, and `rm -f` on an absent file is already a no-op (a reactor
        # that never promoted wrote no drop-in).
        #
        # WHY IT IS SAFE TO FIRE ON STOPS WE DO NOT INITIATE -- the obvious objection, since this
        # now runs on every stop rather than on the one an agent verb drives. NOT because we are
        # the only ones who stop it: we are not, and that is the whole point (a user rebooting
        # their own machine is a stop nobody asked us about). It is because the drop-in's only
        # legitimate function is a START ordering -- drbd-services@r0.target before this daemon --
        # while on the stop side its sole effect is the deadlock, and drbd-reactor recreates it in
        # `Promoter::new` before the next start. So there is no stop for which removing it is
        # wrong, whoever initiated it. An argument that does not depend on who is stopping us is
        # the only kind worth having here, because that is not ours to control.
        #
        # WHAT DOES NOT GET IT, measured rather than assumed: a CRASH. systemd runs ExecStop= on
        # an explicit stop, on a restart and in a shutdown transaction, but NOT when the main
        # process dies on its own (that path runs ExecStopPost= only). Which is fine twice over --
        # a dead reactor has no last gasp to sequence, and `Restart=on-failure` then restarts it
        # through `Promoter::new`, which rewrites the drop-in anyway. It also means the one shape
        # that could have made a `daemon-reload` loop -- crash, restart, crash -- cannot, because
        # the crash half never reaches this.
        #
        # The `daemon-reload` is not optional: removing the file leaves the `Before=` in systemd's
        # in-memory graph, so the deadlock would survive its own defusal.
        #
        # PAIRED with the same path in nixosTest/maintenance-contract.nix,
        # deliberately written out rather than shared, for v0's single resource (r0): a test that
        # imported the constant could not notice the product changing it.
        ExecStop = [
          "-${pkgs.coreutils}/bin/rm -f /run/systemd/system/drbd-services@r0.target.d/reactor-50-before.conf"
          "-${config.systemd.package}/bin/systemctl daemon-reload"
        ];
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

    # UPSTREAM'S ESCALATION CANNOT WRITE ITS OWN REASON DOWN ON NIXOS. reactor still attaches
    # drbd-demote-or-escalate@ as `OnFailure=` on drbd-promote@ -- the PROMOTION-failure path, which
    # is not ours (a member that gives up goes through briard-promotion-hold instead). That unit
    # ends in `ExecStopPost=-/bin/journalctl --sync`, a path that does not exist here: measured,
    # "Unable to locate executable '/bin/journalctl'". The `-` prefix means it is ignored rather
    # than fatal, so the unit works -- it just loses the one thing it does before rebooting the
    # node, which is flushing the explanation to disk. Reset the list and give it the real path.
    systemd.services."drbd-demote-or-escalate@r0" = {
      overrideStrategy = "asDropin";
      serviceConfig.ExecStopPost = [
        ""
        "-${config.systemd.package}/bin/journalctl --sync"
      ];
    };

    # THE HAND-OVER, AND THE REFUSAL TO TAKE IT STRAIGHT BACK ([V3b.5](c)). One unit owns the whole
    # sequence because the ORDER is the design: mask BEFORE demoting.
    #
    # Measured on a lone node: drbd-reactor re-promotes about 2s after a demote completes. So a
    # hold bolted on AFTER the demote loses that race, and the tidy-looking alternative -- leave
    # the members' OnFailure pointing at upstream's drbd-demote-or-escalate@ and hang a hold off
    # its success -- cannot work. Masking first means the promotion is REFUSED rather than
    # undone: `systemctl start drbd-services@r0.target` fails outright, drbd-promote@ never runs,
    # DRBD's role never moves, and there is no second mount/unmount cycle to pay for.
    #
    # THE MASK IS THE ONLY LEVER THAT SAYS "NOT ME, FOR NOW". Everything else reactor offers is
    # either DRBD's own decision (may_promote, quorum) or a static handicap biasing WHICH node wins
    # a race -- preferred-nodes, sleep-before-promote-factor, fencing-promote-delay, the automatic
    # disk-state sleep. None of them is a local, temporary refusal. `drbd-reactorctl evict` uses
    # exactly this mask, and `--keep-masked` is exactly this hold without the timer.
    #
    # WHY THE ESCALATION LIVES HERE rather than in upstream's unit: whoever attempts the demote has
    # to own what happens when DRBD refuses it. `secondary-or-escalate` exits non-zero when
    # `drbdsetup secondary` is refused -- something still holds the device open -- and this unit's
    # FailureAction=reboot is the answer, for the reason upstream gives: DRBD is single-primary, so
    # a node stuck Primary after declaring it cannot serve blocks its peer from taking over, and
    # nothing else recovers from that. Note it runs AFTER the target stop above, so by then the
    # ordinary demote (drbd-promote@'s own ExecStop) has already had its turn and this is a
    # confirmation, not a race -- the shim returns 0 for "already secondary anyways".
    #
    # `--runtime` puts the mask in /run, so a reboot clears it: a hold can never outlive the boot
    # that might have fixed its cause.
    #
    # RELEASE CLEARS THE START LIMIT TOO, and that is not housekeeping. Without the reset, the
    # member is still inside its StartLimitIntervalSec window when the hold ends, so the next
    # promotion starts a member systemd immediately refuses -- and the hold length would be
    # silently pinned to that window. Two numbers that must agree are two numbers that can drift;
    # this makes promotionHoldSecs a free parameter instead.
    systemd.services.briard-promotion-hold = {
      description = "Briard: hand the resource on, and refuse to take it back for a while";
      wantedBy = [ ];
      serviceConfig = {
        Type = "simple";
        # ⚠️ ONLY A NODE THAT HOLDS THE RESOURCE MAY HAND IT ON, and this guard is not defensive
        # programming -- without it the STANDBY masks itself on every boot. Measured: a member whose
        # start job fails with result `dependency` fires `OnFailure=` exactly like one that failed
        # on its own, and on the node that loses the promotion race every member does
        # ("Multiple primaries not allowed by config" -> drbd-promote@ fails -> "Dependency failed
        # for ... briard-reverse-proxy" -> "Triggering OnFailure= dependencies"). So the loser ran
        # this unit and masked itself for the whole hold, which at the shipped 300s is a standby
        # that cannot take over for five minutes after a boot -- and if the primary died in that
        # window, nobody would serve.
        #
        # It was harmless right up until it wasn't: the loser fired OnFailure into upstream's
        # drbd-demote-or-escalate@ too, which demoted an already-Secondary node and exited 0
        # ("already secondary anyways", per the shim). The same trigger only became dangerous when
        # the action grew a five-minute mask.
        #
        # ExecCondition rather than ExecStartPre: 1..254 SKIPS the unit and does NOT mark it
        # failed, so a Secondary quietly declines instead of failing into FailureAction=reboot.
        #
        # ⚠️ AND NOTHING ABOUT DRESS TRIALS HERE, deliberately ([B.138]). A dress restarts the
        # front door and the dashboard onto pushed binaries to see whether they really start, so
        # it can provoke exactly the failure this unit answers -- but a single failed start never
        # reaches here: an auto-restart under `RestartMode=direct` skips failed/inactive and skips
        # `OnFailure=`, for a start that failed as much as for a crash while running. What reaches
        # here is a member with no restart LEFT, the start limit spent. A trial costs up to three
        # of the doors' five starts, so the trial CLEARS the counter before it begins
        # (`systemctl reset-failed`, agent/guestfirmware/bin.go) and the arithmetic cannot reach the
        # limit. An earlier cut made this unit's ExecCondition conditional on a
        # "trial-in-progress" flag instead; the owner removed it (2026-09-08): the upgrade path is
        # already intricate, and a rule that makes the demote hook itself conditional -- with a
        # flag lifetime to get wrong -- buys a rarely-exercised branch where a budget reset with
        # plain semantics does the same job.
        #
        # ⚠️ AND ON A LONE NODE THE GUARD IS ALWAYS MET ([B.145c]): there is no loser to protect
        # and nobody else who could be holding the volume, so every member failure is this
        # node's own to hold -- and to restart from, below.
        ExecCondition = "${byTopology "briard-hold-only-if-holding" {
          flock = "${pkgs.drbd}/bin/drbdadm role r0 | ${pkgs.gnugrep}/bin/grep -q '^Primary'";
          alone = "exit 0";
        }}";
        # 1. refuse promotion, 2. stop the chain (this IS the demote), 3. confirm we really are
        #    Secondary or escalate. Each is its own ExecStartPre so a failure names its own step,
        #    and each has its lone-node body ([B.145c]) -- the same three steps by symmetry, with
        #    the volume standing where the resource stands.
        #
        # STEP 2 STOPS drbd-promote@, NOT THE TARGET, and that is a barrier rather than a
        # preference. Measured: `systemctl stop drbd-services@r0.target` returns as soon as the
        # TARGET is down -- `Stopped target` at 62.386, `Stopping briard-vip` at 62.388, the shim
        # running at 62.412 while the volume was still mounted, exit 11, reboot. The members are
        # `After=drbd-promote@`, so on the way down they stop BEFORE it: waiting for the promote
        # unit is the only spelling that waits for all of them. Its own ExecStop is also the
        # ordinary demote, so step 3 is a confirmation (the shim returns 0 for "already
        # secondary anyways") rather than the thing doing the work. The lone node has no promote
        # unit to wait on, so its stop NAMES every member, in reverse: a stop of several units
        # returns when all of them are down.
        ExecStartPre = [
          # A lone node needs no mask: nothing re-promotes during its hold, because the release
          # below is the only thing that starts its target.
          "${byTopology "briard-hold-refuse" {
            flock = "${config.systemd.package}/bin/systemctl mask --runtime drbd-services@r0.target && ${config.systemd.package}/bin/systemctl daemon-reload";
            alone = ":";
          }}"
          "-${byTopology "briard-hold-stop" {
            flock = "${config.systemd.package}/bin/systemctl stop drbd-promote@r0.service";
            alone = "${config.systemd.package}/bin/systemctl stop briard-chain.target ${lib.concatStringsSep " " (lib.reverseList chainMembers)}";
          }}"
          # The ONE escalation, the same cell in both topologies: a demote DRBD refused, or an
          # unmount something still holds open. A node stuck holding a volume it has declared it
          # cannot serve is the one thing nothing else recovers from -- on a flock because the
          # peer cannot take over, alone because the restart below would mount on top of it.
          "${byTopology "briard-hold-confirm" {
            flock = "${pkgs.drbd}/lib/drbd/scripts/drbd-service-shim.sh secondary-or-escalate r0";
            alone = "! ${pkgs.util-linux}/bin/mountpoint -q ${btrfsRoot} || ${pkgs.util-linux}/bin/umount ${btrfsRoot}";
          }}"
        ];
        ExecStart = "${pkgs.coreutils}/bin/sleep ${toString config.briard.promotionHoldSecs}";
        # The release. `-` on the unmask and the resets: a hold that cannot tidy up must still end,
        # because leaving the mask on is the one outcome worse than releasing early.
        ExecStopPost = [
          "-${byTopology "briard-hold-release" {
            flock = "${pkgs.coreutils}/bin/rm -f /run/systemd/system/drbd-services@r0.target; ${config.systemd.package}/bin/systemctl daemon-reload";
            alone = ":";
          }}"
          "-${config.systemd.package}/bin/systemctl reset-failed ${lib.concatStringsSep " " chainMembers}"
          # Alone, the restart is ours ([B.145c]): no reactor re-promotes, so the hold starts the
          # chain it stopped -- hold-and-restart, forever, and no reboot, which is what a flock
          # does through its reactor. `--no-block`, because this runs inside the hold's own stop
          # and the start must not wait on it.
          "-${byTopology "briard-hold-restart" {
            flock = ":";
            alone = "${config.systemd.package}/bin/systemctl start --no-block briard-chain.target";
          }}"
          # LAST, and the reason is FailureAction=reboot above: a node that reboots because it
          # could not release the resource must leave the reason on disk first, and the journal
          # is otherwise still in RAM when the reboot happens.
          "-${config.systemd.package}/bin/journalctl --sync"
        ];
      };
      unitConfig = {
        # The stuck-Primary escalation. Reachable only through a demote DRBD refused, never through
        # an ordinary member failure -- those end in a clean stop and a sleep.
        FailureAction = "reboot";
      };
    };


    # The ordered failover unit. Each piece has wantedBy = [] so it never starts on
    # its own — drbd-reactor starts them, in this order, only after it has promoted
    # the resource, and stops them in reverse on demote. So they run on the primary
    # and nowhere else.

    # 0. node storage — NOT DEFINED HERE ANY MORE ([B.160]). The pushed agent writes
    # briard-node-storage.service into /run/systemd/system at every start, from the closure
    # `guestTools` publishes above (agent/guestagent/units.go carries the unit and the reasoning
    # that used to sit here). It is the first unit to move because it is the one that measurably
    # broke: it is a chain member of nothing, `wantedBy = [ ]`, started by the host's storage.node
    # verb alone -- so its whole dependency graph is the host's timing, and moving it moves
    # nothing else.

    # 1. primary storage — format on first use, mount the replicated volume ([V3b.33](d)). The
    # FILESYSTEM half: node storage did the block work on every node, and this runs only where the
    # volume is actually mounted, which is the one node that promoted. The cut between the two is
    # by SCOPE, and it is the one the product already had.
    systemd.services.briard-primary-storage = {
      description = "Briard primary storage (the replicated volume, mounted on the primary)";
      wantedBy = [ ];
      # The tools the pushed agent shells out to: mount/umount/mountpoint (util-linux),
      # mkfs.btrfs, and mkdir/rm. A unit's default PATH is minimal, and the agent names these by
      # command rather than by store path -- so the unit is where they are resolved.
      path = [ pkgs.util-linux pkgs.btrfs-progs pkgs.coreutils ];
      serviceConfig = {
        # THE SAME BUDGET AS EVERY OTHER CHAIN MEMBER ([B.125](b)). A oneshot may carry
        # Restart=on-failure -- only `always`/`on-success` are refused for this Type, and a
        # oneshot that exits cleanly is never restarted -- so the policy is uniform across the
        # chain rather than "the simple ones retry and the oneshots get exactly one attempt",
        # which is what the absence of a directive used to mean and nobody had decided.
        Restart = "on-failure";
        RestartSec = 2;
        Type = "oneshot";
        RemainAfterExit = true;
        # THROUGH THE PIVOT'S binDir DIRECTLY ([B.86j], [B.138]), never through the picker, whose
        # trial flag is keyed by the binary's NAME. This was the last inline-shell unit in the
        # chain; storage is code we expect to change, so it belongs on the side that moves with
        # the host bundle rather than frozen in this image. ⚠️ It follows that the MOUNT now
        # depends on the pushed bundle -- an exposure that already existed (briard-services is a
        # chain member running the pushed agent, and a guest with no good bundle cannot serve
        # anything), but a decision rather than a side effect.
        ExecStart = "${config.briard.pivot.binDir}/briard-guest-agent --primary-storage";
        ExecStop = "${config.briard.pivot.binDir}/briard-guest-agent --primary-storage-stop";
      };
      unitConfig = chainMemberFailure // {
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
      };
    };

    # Podman belongs to the guest OS, not to any service: it is the runtime a service will be
    # installed INTO, by the renderer, at runtime ([V3b.3](f)). There is no declared container
    # here and there is no `virtualisation.oci-containers` — a workload is not a build-time fact
    # about this image any more ([V3b.3](e2)).
    virtualisation.podman.enable = true;

    # Pre-stage image tarballs into local podman storage at boot: images that must already be
    # RESIDENT because nothing on the failover path may pull. A runtime-installed service renders
    # `Pull=never` against a digest, so its image has to be here before it is installed.
    # Idempotent, runs on EVERY node (a standby is where a cold pull would hurt), and independent
    # of what is installed.
    systemd.services.briard-stage = lib.mkIf (config.briard.stagedImages != [ ]) {
      description = "Pre-stage service images into local podman storage";
      wantedBy = [ "multi-user.target" ];
      path = [ config.virtualisation.podman.package ]; # the module's podman, not a second copy
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-stage" ''
          set -eu
          ${lib.concatMapStringsSep "\n" (img: "podman load -i ${img}") config.briard.stagedImages}
        '';
      };
    };

    # 2. services — CONVERGE-AT-PROMOTION ([V3b.3](f)). Once the volume is mounted, read every
    #    manifest under its `.services/`, render, warm and start them. This node makes itself
    #    match the VOLUME, so what a node was told — or whether it was even up when the install
    #    ran — stops deciding what the household gets after a failover.
    #
    #    IT IS A CHAIN MEMBER, AND STATICALLY SO. The chain is what drbd-reactor promotes WITH,
    #    but the volume is only readable AFTER promotion — so the start-list cannot name the
    #    services themselves, and goes back to being constant: `data -> services -> vip` on every
    #    data node. A constant chain is what made converge-at-promotion possible for the baked
    #    payload slot; this generalises the trick to N runtime-installed services. The unit is
    #    defined unconditionally for the same reason briard-primary-storage is: naming a unit the guest does
    #    not define fails the WHOLE ordered chain.
    #
    #    ITS FAILURE IS LOUD, BY POSITION. A promoter fails the whole promotion if a member
    #    fails, and the VIP comes after this — so a node that cannot converge never takes the
    #    service address, and a primary with no address is already reported unhealthy. That is
    #    deliberate: built as a side-effect that shrugs, converge would put fallible work (render,
    #    possibly a pull) on the promotion path and leave the silent-healthy hole exactly as
    #    dangerous. Same shape, and the same reason, as the deleted `briard-converge`'s refusal:
    #    a gate that shrugs is not a gate.
    #
    #    THE SERVICE UNITS THEMSELVES ARE NOT MEMBERS, which is what makes "a service error
    #    alerts but never demotes" mechanically true — drbd-reactor never sees them, so a crashed
    #    container cannot deactivate the target. The consequences are handled where they land: a
    #    crash is the container unit's own Restart= (agent/quadlet), and the STOP is ExecStop
    #    below, because reverse-order chain unwinding would otherwise leave containers running on
    #    a volume about to be unmounted.
    systemd.services.briard-services = {
      description = "Briard services, converged from the replicated volume at promotion";
      wantedBy = [ ];
      after = [ "briard-primary-storage.service" ];
      requires = [ "briard-primary-storage.service" ];
      path = [
        pkgs.coreutils # ls/mkdir/rm, for reading the volume and owning the quadlet dir
        pkgs.systemd # systemctl daemon-reload + start/stop of the rendered units
        # The MODULE's podman, not `pkgs.podman` — naming the latter ships a second,
        # differently-wrapped copy of the runtime ([B.5]).
        config.virtualisation.podman.package
      ];
      serviceConfig = {
        # THE SAME BUDGET AS EVERY OTHER CHAIN MEMBER ([B.125](b)). A oneshot may carry
        # Restart=on-failure -- only `always`/`on-success` are refused for this Type, and a
        # oneshot that exits cleanly is never restarted -- so the policy is uniform across the
        # chain rather than "the simple ones retry and the oneshots get exactly one attempt",
        # which is what the absence of a directive used to mean and nobody had decided.
        Restart = "on-failure";
        RestartSec = 2;
        Type = "oneshot";
        RemainAfterExit = true;
        # THE PUSHED AGENT, BY ITS COMMITTED PATH ([B.139]) -- not the firmware, which serves the
        # push protocol alone, and not through the picker, whose arm flag is keyed by binary name
        # and belongs to the agent's own unit. This member runs only at promotion, and the host
        # dresses the guest before rejoin, so the binary is always there by the time drbd-reactor
        # reaches this rung.
        ExecStart = "${config.briard.pivot.binDir}/briard-guest-agent --converge";
        ExecStop = "${config.briard.pivot.binDir}/briard-guest-agent --converge-stop";
      };
      unitConfig = chainMemberFailure // {
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
      };
    };

    # 3. vip — claim the service address and gratuitous-ARP it so the L2 segment
    #    learns its (new) home. BOTH the address and the device are agent-determined
    #    (net.configure writes VIP_ADDR + VIP_DEV to ${vipEnvPath}). Under the
    #    unified NIC layout eth1 is always the DRBD NIC and the VIP lives on
    #    eth2 — the installer sets VIP_DEV=eth2 even single-node (eth1 sits idle until
    #    a pairing addresses it), so a second anchor can join without a guest reboot.
    #
    #    THE FILE IS REQUIRED, not optional, and there is no baked device or address behind it
    #    ([V3b.16a]). It can only be missing if something started this unit that the agent did not
    #    configure — which the promoter gate makes impossible, since drbd-reactor itself is
    #    agent-started. So "no VIP configuration" is now an error rather than a guess, and the one
    #    guess it used to make claimed the service address on the replication NIC ([V3b.16]).
    systemd.services.briard-vip = {
      description = "Briard service VIP";
      wantedBy = [ ];
      path = [ pkgs.iproute2 pkgs.iputils ];
      serviceConfig = {
        # THE SAME BUDGET AS EVERY OTHER CHAIN MEMBER ([B.125](b)). A oneshot may carry
        # Restart=on-failure -- only `always`/`on-success` are refused for this Type, and a
        # oneshot that exits cleanly is never restarted -- so the policy is uniform across the
        # chain rather than "the simple ones retry and the oneshots get exactly one attempt",
        # which is what the absence of a directive used to mean and nobody had decided.
        Restart = "on-failure";
        RestartSec = 2;
        Type = "oneshot";
        RemainAfterExit = true;
        EnvironmentFile = vipEnvPath;
        # Resolve-claim-record: static address, else the flock's replicated one, else DHCP.
        # It brings the NIC up itself (the framework does that for the nixosTests; a disk-image
        # guest's NIC may still be down) -- idempotent, and it has to happen before DHCP can ask.
        ExecStart = "${vipUp}";
        ExecStartPost = "-${vipArping}";
        ExecStop = "${vipDown}";
      };
      unitConfig = chainMemberFailure // {
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
      };
    };

    # 3b. the NAME — publish `briard-<flock name>.local` for the VIP over mDNS, so the address a
    #     user is given is true on every LAN instead of only on ours. The README could previously
    #     only quote an IP, which is exactly the kind of claim that is wrong in someone else's
    #     house.
    #
    #     FLOCK-scoped, and this was a CORRECTION (V3.20): it published `briard-$(hostname).local`,
    #     a node-scoped name, pointing at the VIP, which is flock-scoped and moves. On failover the
    #     name changed identity while the thing it resolved to did not. The flock has exactly one
    #     name, so the mismatch is gone by construction.
    #
    #     SINGLE-label, deliberately, and this was MEASURED rather than assumed (V3.19d): on a
    #     stock Ubuntu 24.04 client, `<name>.briard.local` publishes fine and then **does not
    #     resolve** — `mdns4_minimal`, the resolver in Debian/Ubuntu's nsswitch, handles exactly
    #     one label before `.local`. A hierarchy would have shipped a name nothing on the LAN could
    #     look up. The `briard-` prefix keeps N flocks distinguishable on one LAN.
    #
    #     ⚠️ IT DOES NOT MATCH THE DHCP HOSTNAME, and that is deliberate (V3.20). Option 12 stays
    #     `briard-<mac tail>`, derived in-guest from the NIC's own address: changing a hostname
    #     mid-lease is a change whose effect on an arbitrary household's DHCP server nobody can
    #     predict — a second client-list entry, or a buggy server moving the address — and a
    #     RENAME MUST NEVER RISK THE ADDRESS. The router's list and the mDNS name therefore differ,
    #     which costs one line of installer wording and buys an identity that is safe to change.
    #
    #     PUBLISHED BY THE FRONT DOOR ([B.152]), which is a promoter chain member, so the name
    #     appears only when this node actually holds the VIP, points at the VIP rather than at
    #     whatever else the guest is addressed on, and is claimed by the PRIMARY alone — the two
    #     nodes of ONE flock never collide with each other. Membership is also what makes a node
    #     that cannot publish hand the resource on rather than serve addresses nobody can reach:
    #     a household with no [V3c.4] `*.casa` name has `.local` and nothing else, so a name that
    #     does not resolve is a service that cannot be reached.
    #
    #     ⚠️ TWO DIFFERENT FLOCKS DRAWING THE SAME WORD PAIR BOTH ANSWER IT, at different
    #     addresses, and neither renames: the responder claims names and does not probe, which is
    #     what removes the entire class of wedged-entry-group failures ([B.152] weighs the trade).
    #     The odds are the word list's — one pair in 178,928 — and the household-visible symptom
    #     is a name that resolves to whichever answer arrives first. `briard-<flock>` is what keeps
    #     that at a collision between flocks rather than between a flock and an HAOS box.
    # Ten-minute lease renewal, for as long as this node holds the VIP.
    #
    # NOT A CHAIN MEMBER, deliberately and for [V3b.5c]'s reason: a renewal that fails must never
    # be able to demote a serving node. Nothing Requires it, its failure propagates nowhere, and
    # the real consequence of a renewal going wrong is a NAK, which dhcpcd's own hook handles as
    # an address change rather than as a unit failure.
    #
    # wantedBy + partOf briard-vip: the timer starts when the node takes the VIP and stops when it
    # gives it up, so it cannot tick on a standby. `wantedBy` is a WEAK reference on purpose -- a timer that will not start must not
    # keep the VIP from coming up.
    systemd.timers.briard-vip-renew = {
      description = "Renew the Briard VIP's DHCP lease every ten minutes";
      wantedBy = [ "briard-vip.service" ];
      partOf = [ "briard-vip.service" ];
      timerConfig = {
        # First one ten minutes after the VIP is taken (the lease is fresh at promotion, so an
        # immediate renewal would be pure noise), then every ten minutes after each run.
        OnActiveSec = "10min";
        OnUnitActiveSec = "10min";
        AccuracySec = "30s";
      };
    };
    systemd.services.briard-vip-renew = {
      description = "Renew the Briard VIP's DHCP lease";
      after = [ "briard-vip.service" ];
      serviceConfig = {
        Type = "oneshot";
        # VIP_DEV, the same source briard-vip itself reads: the renewer must act on the interface
        # that was actually claimed, not on a second opinion about which one that is.
        EnvironmentFile = vipEnvPath;
        ExecStart = "${vipRenew}";
      };
    };

    # 4. the front door — answer the VIP on :80 and terminate HTTPS on :443.
    #
    #    A PROMOTER CHAIN MEMBER since [B.125], where it used to ride briard-vip (wantedBy +
    #    partOf) and stay out of the reactor start-list. The reason it moved is the reason the
    #    mDNS publishers did: every name they claim resolves to the VIP, and this is the only
    #    thing that answers there, so a node with no door serves nothing while reporting healthy.
    #    It still tracks the primary role exactly as before -- reactor writes PartOf=<target> --
    #    and it is ordered BEFORE the publishers so a node claims names only once the door that
    #    serves them has started.
    #
    #    Cert/key live on the DRBD volume (${tlsDir}) so they replicate + survive failover; the
    #    proxy hot-reloads them, so a renewal is gap-free. ⚠️ A MISSING CERT IS STILL NOT A
    #    FAILURE, and that property is the door's own, not the old wantedBy's: :443 simply does
    #    not answer until a cert exists while :80 keeps serving, which is the *shipped* state of a
    #    free node, since a cert needs a domain. Membership would be wrong if the door failed on
    #    it -- it does not.
    systemd.services.briard-reverse-proxy = {
      description = "Briard front door (serves the VIP on :80/:443)";
      after = [ "briard-vip.service" "briard-services.service" ];
      serviceConfig = {
        # NO -backend, and no -routes either: the front door has no single backend at all as of
        # [B.48], and the table it does route on has a compiled-in default (shared/routes.Path)
        # that this unit deliberately does not restate. Naming the path here would put the same
        # /run path in two places with nothing checking they agree -- and it is not a knob a node
        # ever varies, unlike the cert paths, which live on the replicated volume this module
        # defines.
        #
        # ORDERED AFTER briard-services, which is what makes the table exist before the door reads
        # it: converge writes it as part of the same promotion, one chain member earlier. The door
        # reloads the file on mtime anyway, so an install that lands later needs nothing from
        # systemd -- this ordering only spares a freshly-promoted node from a few seconds of
        # serving its own page over services it already runs.
        # THROUGH THE PIVOT ([B.86j], [B.138], pivot.nix): the picker runs the copy the host pushed,
        # and nothing else -- the image bakes no door. READY means "listening" (reverse-proxy says it
        # after both binds), which is what a trial agent reads as its verdict on the pushed copy;
        # the commit is the agent unit's, for the whole set.
        Type = "notify";
        ExecStart = "${config.briard.pivot.exec} briard-reverse-proxy -"
          + " -http :80 -listen :443"
          + " -cert ${tlsDir}/fullchain.pem -key ${tlsDir}/key.pem"
          # Every name the table does not route -- the bare IP, the node's own name -- goes to the
          # dashboard ([V3b.31b]); the door has no page of its own.
          + " -fallback http://127.0.0.1:8087";
        Restart = "on-failure";
        # A TRANSIENT CRASH MUST NOT MOVE THE RESOURCE ([V3b.5](c)). Without this, the
        # auto-restart's stop job deactivates drbd-reactor's target -- which unmounts the data
        # volume and demotes the node on ONE crash, measured, with a peer taking the resource
        # about half the time. `direct` restarts through activating instead of failed, so
        # dependents are not notified of the temporary failure. It is also what lets the
        # StartLimit below finally accumulate: the unit is no longer torn down and started
        # fresh on every cycle, so a member that genuinely gives up still reaches `failed`
        # and still hands the resource on -- which is what this budget always claimed to do.
        # NOT on briard-primary-storage/services/vip: for those, failure really does mean this node
        # must not hold the volume.
        RestartMode = "direct";
        RestartSec = 2;
        # A staged copy that execs but never says READY must fail inside the trial agent's watch
        # ([B.138]); the real one says READY at listen within milliseconds, so 10 s is generous.
        TimeoutStartSec = 10;
      };
      # A RESTART BUDGET, and the number is a judgement rather than a measurement ([B.125]):
      # five starts in five minutes, after which the member gives up and the resource moves. It matters more here than the shape
      # suggests: with no StartLimit at all systemd's 5-in-10s default applies, and at RestartSec=2
      # that IS reachable, so a door would hand the resource on after ~10s of trying. That is eager
      # for one whose likeliest transient is losing the race for :80 to its own previous instance
      # during a failover, and it is the asymmetry [B.125](b) holds open.
      unitConfig = chainMemberFailure // {
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
      };
    };

    # THE HOUSEHOLD DASHBOARD ([V3b.31b]): loopback only, behind the door, which forwards every
    # name it does not route here. A chain member for the reason the door is one -- its device
    # registry lives on the volume, and only the primary has it -- under the same [V3b.5](c)
    # settings: RestartMode=direct so a transient crash is a restart in place, the hold on giving
    # up. It reads the routing table converge wrote and Home Assistant's control token, both on
    # /run; it writes only under /var/lib/briard/dashboard.
    systemd.services.briard-dashboard = {
      description = "Briard household dashboard (behind the front door)";
      after = [ "briard-primary-storage.service" "briard-services.service" ];
      serviceConfig = {
        # THROUGH THE PIVOT ([B.138], pivot.nix): the copy the host pushed, and nothing else -- the
        # image bakes no dashboard. Type=notify, READY at listen, so a trial agent reads this
        # unit's start as its verdict on the pushed copy.
        Type = "notify";
        ExecStart = "${config.briard.pivot.exec} briard-dashboard - -listen 127.0.0.1:8087";
        Restart = "on-failure";
        RestartMode = "direct";
        RestartSec = 2;
        # A staged copy that execs but never says READY must fail inside the trial agent's watch
        # ([B.138]); the real one says READY at listen within milliseconds, so 10 s is generous.
        TimeoutStartSec = 10;
      };
      unitConfig = chainMemberFailure // {
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
      };
    };

    # Lean, headless test image. The nixosTest / VM runner supplies the real boot
    # device + networking, so there is no bootloader/root device here.
    boot.loader.grub.enable = false;
    # NO console here, deliberately: the guests built from this module alone are nixosTest NODES,
    # whose console the framework already captures. The BOOTABLE image adds its own
    # (disk-image.nix: console=ttyS0 + grub on serial + journald ForwardToConsole), because there
    # the host's `-serial file:` is the only way to see inside. Setting it in both places would
    # duplicate the kernel param and imply this module owns a decision it does not.
    fileSystems."/" = {
      device = "/dev/vda";
      fsType = "ext4";
    };
    networking.firewall.enable = false;

    # dhcpcd runs on every interface by default, and this guest has interfaces that must never
    # ask a stranger's router for anything. Measured on the machine that produced V3.19: a node
    # put TWO extra DHCP clients on the household's router, one of them on the DRBD replication
    # link -- a private point-to-point path between anchors that has no business holding a LAN
    # address, and whose address the agent sets explicitly (net.configure) when a pairing happens.
    #
    # Kept: eth0 only (qemu's SLIRP user-net, the guest's WAN path for OCI pulls).
    # Denied: eth1 (DRBD) and eth3 (the private guest<->host witness link) -- both statically
    # addressed by the agent, both invisible to the LAN by design -- and eth2, the service NIC.
    #
    # eth2's denial REVERSES what this list said when it was written ("the service NIC, whose
    # lease becomes the VIP"). It does become the VIP, but it cannot be leased at BOOT:
    #   - a boot-time client leases it on the SECONDARY too, so the "VIP" would be a per-node
    #     address sitting on a node that is not serving; and
    #   - the service NIC's MAC is flock-scoped (V3.19b), so both nodes would be asking one
    #     router for one lease from two machines at once.
    # The lease is drawn at PROMOTION instead, by briard-vip, which is the one moment exactly one
    # node holds this identity. A single-interface dhcpcd there runs as its own instance, which
    # is what makes denying it here and leasing it there coexist rather than fight.
    networking.dhcpcd.denyInterfaces = [ "eth1" "eth2" "eth3" ];

    # Answer ARP only on the interface that HOLDS the address (and source kernel ARP probes
    # from the outgoing interface's own address). Both NICs can share one L2 -- a household
    # that wires both ports into one switch -- and Linux's weak-host default (arp_ignore=0)
    # then lets the SERVICE NIC answer for the DRBD address: the peer caches the wrong MAC,
    # replication silently transits eth2 while it holds the VIP address, and the moment a VIP
    # teardown strips that address, source validation turns the flow into a silent one-way
    # blackhole -- far longer than DRBD's 500ms ping deadline, i.e. a split-brain with no
    # failure anywhere. Every address here is hand-placed on the NIC that owns its traffic
    # (the agent's net.configure, briard-vip's lease, the witness link), so weak-host ARP adds
    # nothing and only the cross-NIC ambiguity is removed. The VIP takeover's gratuitous ARP
    # is explicit (vipArping crafts its own frames) and unaffected by either setting. The
    # measured chain: farm docs/V3.md [B.101].
    boot.kernel.sysctl = {
      "net.ipv4.conf.all.arp_ignore" = 1;
      "net.ipv4.conf.default.arp_ignore" = 1;
      "net.ipv4.conf.all.arp_announce" = 2;
      "net.ipv4.conf.default.arp_announce" = 2;
    };

  }

  # THE LONE NODE'S TARGET ([B.145c]): the promoter chain with no promoter. A home with one
  # diskful member runs no DRBD, so nothing generates drbd-services@r0.target for it; this static
  # target carries the IDENTICAL member list in the identical order, with the same Wants/After the
  # reactor writes onto its target and the same PartOf + Requires/After-the-previous it writes onto
  # each member -- so a lone node and a flock run one chain, and the members cannot tell which
  # target started them. A lone node's bring-up ends in `systemctl start` of this (the guest's
  # chain.start verb) where a flock's ends in starting drbd-reactor; `wantedBy = [ ]` so nothing
  # else can. Folded onto the members here rather than written into each unit, so the list is
  # stated once (chainMembers, the same seven the hold above resets).
  {
    systemd.targets.briard-chain = {
      description = "Briard: the promoter chain, on a node that runs no promoter";
      wantedBy = [ ];
      wants = chainMembers;
      after = chainMembers;
    };
    systemd.services = lib.listToAttrs (
      lib.imap0 (
        i: unit:
        lib.nameValuePair (lib.removeSuffix ".service" unit) {
          partOf = [ "briard-chain.target" ];
          requires = lib.optional (i > 0) (lib.elemAt chainMembers (i - 1));
          after = lib.optional (i > 0) (lib.elemAt chainMembers (i - 1));
        }
      ) chainMembers
    );
  }
  ];
}
