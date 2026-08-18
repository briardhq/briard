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
  # AND NOTHING IS BAKED HERE EITHER, as of step 3. This was kept for one more step as the
  # agent-less harnesses' fallback; that made it the last place our lab's subnet sat in the
  # product image, and a fallback that every test agrees with is indistinguishable from a
  # default nobody chose -- which is the shape of the original defect. Empty means DHCP: the
  # router tells us the answer, inside its own pool, instead of us guessing at someone's house.
  #
  # The harnesses now DECLARE their address (nixosTest/lib.nix, and the driver-based tests via
  # VIP_ADDR), which turns an inherited assumption into a stated one.
  vipFallback = "";
  vipDev = "eth1";
  vipEnvPath = "/run/briard/vip.env";
  # The FLOCK's service address, replicated with the data. Same shape, same place and same
  # write-authority as imagePinFile: a small flock-scoped fact at the btrfs root, written only by
  # the node that holds the volume (only a Primary can mount it), read by whoever promotes next.
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
  # The FLOCK's visible name, handed down by the agent (net.mdnsname) and NOT baked: it is pet
  # identity arriving at a cattle image, which is the distinction V3.19 was found for.
  # How long the publisher waits for avahi to confirm the name before it gives up and lets systemd
  # restart it. This is the BACKSTOP for a case we have not seen, not the working path: the settle
  # wait below is what keeps us out of the one failure we measured. See vipPublish.
  mdnsEstablishSecs = 15;

  # How long the published address must sit still before we publish it, and how long we are willing
  # to wait for that. avahi wedges permanently if the address moves inside its probe window, which
  # is under a second (V3.22, measured 0/6 vs 6/6).
  mdnsSettleSecs = 2;
  mdnsSettleMaxSecs = 20;

  # The publisher's stdout, as a fifo rather than a pipe, so the reader stays in the main shell and
  # the publisher's pid remains killable. See vipPublish.
  mdnsFifoPath = "/run/briard/mdns.fifo";

  mdnsEnvPath = "/run/briard/mdns.env";
  # The name avahi ACTUALLY established, which the host reads back over net.mdnspublished.
  mdnsPublishedPath = "/run/briard/mdns.published";

  # Publish the VIP under `briard-<flock name>.local`.
  #
  # The name is FLOCK-scoped, and that is a correction rather than a detail (V3.20). It used to be
  # `briard-$(hostname).local` -- node-scoped -- while the address it resolves to is the VIP, which
  # is flock-scoped and moves. So on failover the name changed identity while the thing it pointed
  # at did not: not merely unfriendly, incoherent. FLOCK_NAME has no such problem, because the
  # whole flock has one.
  #
  # The address still comes from VIP_ADDR with its prefix stripped -- the same single source the
  # VIP itself is claimed from, so the name can never point somewhere the address is not.
  #
  # ⚠️ WHY THIS IS NOT `exec`, AND WHY stdbuf. avahi-publish prints `Established under name 'X'`,
  # and on a collision `Name collision, picking new name 'X'` -- it renames itself and tells no
  # one. That output is the ONLY place the truth about the published name appears, so this reads
  # it and records it. Two traps, both silent if got wrong:
  #   - `exec` would replace this shell and leave nothing to read the output.
  #   - stdout to a PIPE is full-buffered by libc, so without `stdbuf -oL` the line would sit in a
  #     4 KiB buffer for the entire lifetime of a long-running publisher and arrive only at exit.
  #     The read-back would then be empty forever, while everything looked fine -- a vacuous green
  #     of exactly the kind V3.19 was.
  # The process still holds the record for as long as it runs (avahi withdraws on exit), so the
  # unit's lifetime is the record's lifetime.
  #
  # ⚠️ WHY A FIFO AND A PID RATHER THAN A PIPELINE, which is what this was until V3.22. As
  # `avahi-publish | while read`, the loop is the RIGHT side of a pipe and therefore a subshell,
  # and the publisher's pid is not knowable from inside it. That is fatal to the deadline below:
  # breaking out of the loop does not end the pipeline, because the shell waits for EVERY member
  # to exit and a hung avahi-publish never does -- so the script blocked forever, one line short
  # of `exit 1`, having already printed that it was giving up. **Measured, not reasoned**: the
  # first version of this fix logged `did not establish a name within 15s` at exactly the deadline
  # and then never restarted, which is a more embarrassing version of the very bug it fixes -- a
  # failure detected, announced, and not acted on. Reading from a fifo keeps the loop in the MAIN
  # shell, so `$pub` exists, the trap can kill it, and `exit 1` is reachable.
  #   - `pipefail` mattered in the pipeline form and is kept for the same reason it was added: a
  #     publisher that dies must not be read as a clean exit.
  vipPublish = pkgs.writeShellScript "briard-vip-publish" ''
    set -euo pipefail
    : >${mdnsPublishedPath}
    # ---- WAIT FOR THE ADDRESS TO STOP MOVING BEFORE PUBLISHING -----------------------------
    # THE ROOT CAUSE, measured rather than guessed (V3.22). avahi wedges its entry group -- for
    # good, silently, neither established nor refused -- if the address it is publishing is
    # withdrawn and re-added while the group is still probing. Reproduced in isolation:
    #
    #   del+add 0.5s after avahi-publish starts   -> established 0/6
    #   same, but wait for the address to settle  -> established 6/6
    #   del+add at 0.1s -> wedged; at 1.0s+       -> fine
    #
    # So the vulnerable window is under a second, and it is exactly the window we were aiming at:
    # briard-vip applies the address optimistically, finishes, this unit starts immediately, and
    # dhcpcd -- started with -b, so it returns before it has a lease -- re-applies the SAME address
    # ~0.5s later as del+add. Dead centre. That is why it hit on the first install onto a real
    # household LAN rather than being the rare event a retry is meant for.
    #
    # It watches the PUBLISHED ADDRESS specifically, not the whole interface: IPv6 churn (SLAAC on
    # a dual-stack router adds records for ~9s) never wedged it in the reproduction, so waiting for
    # that too would delay every promotion for nothing.
    dev="''${VIP_DEV:-${vipDev}}"
    want="''${VIP_ADDR%%/*}"
    # Nothing to wait for if we have no address: the publish below will fail on its own terms,
    # and stalling the full 20s first would turn a broken state into a slow broken state.
    stable=0; waited=0
    [ -n "$want" ] || waited=${toString mdnsSettleMaxSecs}
    while [ "$stable" -lt ${toString mdnsSettleSecs} ] && [ "$waited" -lt ${toString mdnsSettleMaxSecs} ]; do
      if ${pkgs.iproute2}/bin/ip -o -4 addr show dev "$dev" 2>/dev/null |
         ${pkgs.gnugrep}/bin/grep -qF " $want/"; then
        stable=$((stable+1))
      else
        stable=0
      fi
      ${pkgs.coreutils}/bin/sleep 1
      waited=$((waited+1))
    done
    # Not fatal when it never settles: publishing anyway is strictly better than not publishing,
    # and the establishment deadline below is exactly the backstop for that case.
    if [ "$stable" -lt ${toString mdnsSettleSecs} ]; then
      echo "briard-mdns: $want never held still on $dev for ${toString mdnsSettleSecs}s (waited ''${waited}s); publishing anyway" >&2
    fi

    rm -f ${mdnsFifoPath}
    ${pkgs.coreutils}/bin/mkfifo -m 0600 ${mdnsFifoPath}
    ${pkgs.coreutils}/bin/stdbuf -oL ${pkgs.avahi}/bin/avahi-publish -a -R \
      "briard-''${FLOCK_NAME}.local" "''${VIP_ADDR%%/*}" >${mdnsFifoPath} 2>&1 &
    pub=$!
    # Killing the publisher is what makes the deadline REAL rather than merely announced: a hung
    # avahi-publish outlives this script otherwise, and systemd would be waiting on a process that
    # is doing nothing. `|| true` because it is normal for it to be gone already.
    trap 'kill "$pub" 2>/dev/null || true; rm -f ${mdnsFifoPath}' EXIT
    while :; do
      # A DEADLINE, BUT ONLY UNTIL THE NAME IS ESTABLISHED. avahi-publish can hang forever with
      # its entry group never confirmed and never refused -- measured in the field (V3.22): the
      # publisher started 400ms before dhcpcd re-applied the address, avahi logged the interface
      # "no longer relevant" and back, and the group was never established again. The process
      # stayed up, printed nothing, exited never; the unit was `active` and the name resolved
      # nowhere for five minutes, ending only when an unrelated agent restart cycled the unit.
      # Restart=on-failure cannot help a process that does not fail. So: if nothing arrives
      # before the deadline, treat the silence as the failure it is and fall through to the
      # exit-1 tail below, which is the same path a refusal already takes.
      #
      # The timeout applies ONLY while unestablished -- after that avahi-publish is legitimately
      # silent for the whole life of the record, and a deadline on every read would kill a
      # perfectly healthy publisher on a quiet LAN. Establishment itself is sub-second in
      # practice (0.9s on both field boots), so ${toString mdnsEstablishSecs}s is slack, not a race.
      if [ -s ${mdnsPublishedPath} ]; then
        IFS= read -r line || break
      else
        IFS= read -r -t ${toString mdnsEstablishSecs} line || {
          echo "briard-mdns: avahi did not establish a name within ${toString mdnsEstablishSecs}s -- giving up so systemd retries" >&2
          break
        }
      fi
      printf '%s\n' "$line"
      case "$line" in
        "Established under name '"*|"Name collision, picking new name '"*)
          # `...name 'briard-brave-elf.local'.` -> `brave-elf`. Recorded BARE, so the host is
          # handed the name and not a label it would have to unwrap the same way twice.
          n="''${line#*\'}"; n="''${n%%\'*}"; n="''${n%.local}"; n="''${n#briard-}"
          printf '%s\n' "$n" >${mdnsPublishedPath}
          ;;
      esac
    done <${mdnsFifoPath}
    # REACHING HERE IS ALWAYS A FAILURE, and saying so is the point. avahi-publish holds the record
    # only while it runs, so if it has returned, the name is gone -- yet it exits 0 even when the
    # daemon refused it, which systemd logged for two days as `briard-mdns.service: Deactivated
    # successfully` while nothing at all was published. A publisher that no-ops quietly is the same
    # disease as a name that resolves nowhere. Exiting non-zero turns it into a restart and a
    # journal line somebody can find.
    if [ -s ${mdnsPublishedPath} ]; then
      echo "briard-mdns: the publisher exited; the name is no longer published" >&2
    else
      echo "briard-mdns: avahi never established a name (refused, exited first, or never answered)" >&2
    fi
    exit 1
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
    # try-restart, not restart: republish the name only where a name is already published. On a
    # node that is not currently serving there is nothing to correct.
    ${pkgs.systemd}/bin/systemctl try-restart briard-mdns.service || true
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
      # ⚠️ `brd +` IS LOAD-BEARING, and it is what stops V3.22 at the source rather than dodging
      # it. dhcpcd is about to be handed the SAME address by the lease. If what we put on differs
      # from what it wants, it does not update -- it DELETES and re-adds, and that withdrawal
      # inside avahi's probe window is what wedged the mDNS name permanently. `ip addr add X/24`
      # leaves the broadcast unset; dhcpcd wants X.X.X.255. Measured, one variable at a time,
      # against a real dnsmasq (lab/avahi-repro5-deladd.nix), counting RTM_DELADDR:
      #
      #   ip addr replace X/24                 -> 1 delete   (today's behaviour)
      #   ip addr replace X/24 brd +           -> 0 deletes
      #   ...also with noprefixroute, or the full dhcpcd shape -> 0
      #
      # So the broadcast alone is the whole difference, and with it dhcpcd updates in place:
      # avahi never sees the address leave, and the wedge has nothing to trigger on. The settle
      # wait in vipPublish stays as well -- a lease that comes back DIFFERENT is a real del+add
      # that no flag can remove.
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
    # Take the NIC DOWN, but only where the address came from DHCP -- which is exactly where the
    # MAC is flock-scoped and therefore shared with the peer. A Secondary holding that MAC up
    # teaches the switch the wrong port for the VIP the moment it emits any frame at all (an
    # IPv6 RS, an mDNS query), and traffic for the service goes to the node that is not serving.
    #
    # The condition is not timidity, it is the hazard: with a configured address this unit also
    # runs in the agent-less harnesses, where VIP_DEV is eth1 -- the DRBD NIC -- and a link-down
    # there would take replication with it. Static address => the NIC is shared => leave it up.
    if [ -z "$configured" ]; then
      ${pkgs.iproute2}/bin/ip link set dev "$VIP_DEV" down || true
    fi
    exit 0
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
  # What this appliance does not carry ([B.5]). Imported here rather than folded into the callers so
  # that the SAME slimming applies to the shipped disk and to every nixosTest that boots this
  # module -- a guest the tests exercise fatter than the one strangers install would prove nothing
  # about the one strangers install.
  imports = [ ./slim.nix ];

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
      # "no payload baked", not "no service installed": this is a BUILD-time slot, and a service
      # installed at RUNTIME never sets it. Conflating the two is what let the front door announce
      # an empty node while one was serving beside it.
      description = "OCI image ref the payload container runs (name:tag); null = no payload baked into this image.";
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
    # paths (drbd/drbd-reactor/reverse-proxy/briard-agent/podman+crun) + this guest's
    # own `toplevel` come from cache.briard.io at 100 — measured 140 MB of a 982 MB
    # closure, the closure-*diff* effect, not a re-image. (Both numbers moved with
    # [B.5]: the closure fell 1611 → 982 MB, while OUR share rose 74 → 140 MB,
    # because slimming crun took podman off the public cache with it. A guest fetches
    # far less overall and slightly more of it from us — stated here because the
    # second half of that trade is the one nobody would notice.) Our cache
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

    # drbd-reactor daemon: runs from boot watching DRBD, idle until a promoter
    # snippet for some resource is dropped into /etc/drbd-reactor.d.
    environment.etc."drbd-reactor.toml".text = ''
      snippets = "/etc/drbd-reactor.d"
    '';
    systemd.tmpfiles.rules = [
      "d /etc/drbd-reactor.d 0755 root root -"
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

    # THIS NODE'S NAME, restored before anything can act on it.
    #
    # The guest's baked hostname is "guest" (disk-image.nix) and the agent renames it at every
    # bring-up with syscall.Sethostname -- which does NOT survive a reboot. The DRBD `.res` naming
    # that same node DOES: it is written to /etc/drbd.d and stays there. So after a guest reboot
    # the persistent config says `on briard-node-<id>` while the running system is called "guest",
    # drbd-reactor starts from boot, promotes into the mismatch, drbd@<res> fails, and the failed
    # promote job is never retried -- the node parks quorate but never Primary, with no VIP, no
    # dhcpcd and no address. Found on the L0 runner (V3.20); invisible before it, because the
    # baked hostname and the node name were THE SAME LITERAL and the boot-time promote matched by
    # luck.
    #
    # The fix is to give the two facts the same LIFETIME, not merely the same moment: sys.hostname
    # persists the name beside the .res, and this restores it Before drbd-reactor. Ordering only
    # against drbd-reactor is sufficient -- drbd@ and drbd-promote@ are started BY it, so they
    # inherit the ordering.
    #
    # Conditioned on the file: a guest that has never been told who it is (first boot, and every
    # agent-less harness) keeps the baked name rather than failing a unit over it.
    systemd.services.briard-identity = {
      description = "Briard node identity (restore this node's hostname)";
      wantedBy = [ "multi-user.target" ];
      before = [ "drbd-reactor.service" ];
      unitConfig.ConditionPathExists = "/etc/briard/node-id";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.nettools}/bin/hostname -F /etc/briard/node-id";
      };
    };

    systemd.services.drbd-reactor = {
      description = "drbd-reactor — DRBD failover orchestrator";
      wantedBy = [ "multi-user.target" ];
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
        # nothing re-arms it while we are stopping. The deadlock and this defusal are gated by
        # nixosTest/reactor-pause-deadlock.nix.
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
        # PAIRED with the same path in nixosTest/{reactor-pause-deadlock,maintenance-contract}.nix,
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
      # `config.virtualisation.podman.package`, NEVER `pkgs.podman`, here and at every other podman
      # call site in this image. They are not the same derivation: the NixOS module wraps podman
      # with its own helper/binary paths (`/run/wrappers` for the setuid shadow, systemd for
      # container healthchecks), so naming `pkgs.podman` in a unit does not reuse the podman that
      # is on the node's PATH -- it ships a SECOND, differently-wrapped copy. Measured at 57 MB of
      # pure duplication ([B.5]), and worse than the size: two runtimes on one node, of which the
      # unit-local one is the one missing the wrappers.
      path = [ config.virtualisation.podman.package ];
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
      path = [ config.virtualisation.podman.package ]; # the module's podman, not a second copy
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
      path = [ pkgs.coreutils config.virtualisation.podman.package ]; # not a second podman
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
        # Resolve-claim-record: static address, else the flock's replicated one, else DHCP.
        # It brings the NIC up itself (the framework does that for the nixosTests; a disk-image
        # guest's NIC may still be down) -- idempotent, and it has to happen before DHCP can ask.
        ExecStart = "${vipUp}";
        ExecStartPost = "-${vipArping}";
        ExecStop = "${vipDown}";
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
    #     Bound to briard-vip exactly like the front door (wantedBy + partOf), which buys three
    #     things: the name appears only when this node actually holds the VIP, it points at the
    #     VIP rather than at whatever else the guest is addressed on, and on a pair only the
    #     PRIMARY publishes — so the two nodes of ONE flock never collide with each other. Two
    #     DIFFERENT flocks in one house still can, and avahi resolves that by renaming one of them
    #     silently, which is why the published name is read back rather than assumed.
    systemd.services.briard-mdns = {
      description = "Briard mDNS name for the VIP";
      wantedBy = [ "briard-vip.service" ];
      partOf = [ "briard-vip.service" ];
      after = [ "briard-vip.service" "avahi-daemon.service" ];
      requires = [ "avahi-daemon.service" ];
      serviceConfig = {
        # The address briard-vip ACTUALLY claimed, so the name cannot drift from it. Three
        # sources, last wins: baked fallback, the agent-written config, and the live file that
        # records what was really taken -- which under DHCP is the only one that knows.
        #
        # FLOCK_NAME comes from the agent (net.mdnsname) and has NO baked fallback on purpose: a
        # node with no minted name must publish nothing rather than publish a guess, and
        # `briard-.local` is worse than silence. ConditionPathExists enforces that -- the unit
        # stays inactive rather than failing in a restart loop, because "this node has no name" is
        # a legitimate state (every agent-less harness) and not an error.
        Environment = "VIP_ADDR=${vipFallback}";
        EnvironmentFile = [ "-${vipEnvPath}" "-${vipLivePath}" "-${mdnsEnvPath}" ];
        # avahi-publish holds the record for as long as it runs and withdraws it on exit, so the
        # unit's lifetime IS the record's lifetime -- no cleanup path to get wrong on demotion.
        ExecStart = "${vipPublish}";
        Restart = "on-failure";
        RestartSec = 2;
      };
      unitConfig.ConditionPathExists = mdnsEnvPath;
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
    # from the outgoing interface's own address). Linux's default weak-host ARP (arp_ignore=0)
    # answers "who-has <DRBD address>" from the SERVICE NIC too whenever both NICs share an L2
    # -- which they do on any household that wires both ports into one switch, and on the lab
    # bridge, where it was measured splitting a flock (2026-08-18): the peer's post-reboot
    # re-ARP cached the service NIC's MAC, replication silently transited eth2 for as long as
    # eth2 held the VIP address, and the eviction's teardown then turned the flow into a
    # silent per-source blackhole (an address-less interface under any rp_filter>=1 refuses
    # every source that does not route back out of it) -- 37s of one-way partition against
    # DRBD's 500ms ping-timeout, i.e. a split-brain with no failure anywhere.
    #
    # Every address here is hand-placed on the NIC that owns its traffic (the agent's
    # net.configure, briard-vip's lease, the witness link), so there is nothing for weak-host
    # ARP to add -- only the cross-NIC ambiguity to remove. The VIP takeover's gratuitous ARP
    # is explicit (vipArping crafts its own frames) and unaffected by either setting.
    boot.kernel.sysctl = {
      "net.ipv4.conf.all.arp_ignore" = 1;
      "net.ipv4.conf.default.arp_ignore" = 1;
      "net.ipv4.conf.all.arp_announce" = 2;
      "net.ipv4.conf.default.arp_announce" = 2;
    };

    # mDNS, so the node has a NAME and not just an address (V3.19d). Responder only -- the guest
    # answers for the one name it publishes and browses for nothing.
    #
    # ⚠️ THE NAME NEVER PUBLISHED, from 2026-08-07 until this fix, and it took two wrong guesses
    # and a real console to find out why. briard-mdns died every single time with
    #
    #     Failed to create entry group: Not permitted
    #
    # and the error names the failing call precisely: ENTRY GROUP, i.e. the `EntryGroupNew` D-Bus
    # method, which avahi refuses outright when `disable-user-service-publishing=yes` -- the NixOS
    # default, since `publish.userServices` defaults to false. Nothing about the address is even
    # reached. avahi-daemon.conf(5) describes that setting as blocking "user applications
    # publishing SERVICES", which is what sent the first fix at `publish.addresses` instead: with
    # addresses enabled the daemon was demonstrably registering records of its own
    # (`Registering new address record for 192.168.1.119 on eth2.IPv4`) while briard-mdns kept
    # failing at the step before. Both are needed -- and in this module they collapse anyway,
    # since `publish-addresses = userServices || addresses`.
    #
    # Nothing noticed for two days because nothing had ever RESOLVED the name: the V3.19 assertion
    # stopped at "the unit is configured", which was true and worthless. A name believed present
    # and published nowhere -- V3.19's own failure shape, inside V3.19's own fix.
    #
    # The fear behind it was real and is handled by INTERFACE instead. Avahi's default is to
    # publish an A record for every address on every interface under its own hostname; measured on
    # the real machine that produced V3.19, `giouli-desktop.local` resolved to `172.18.0.1` -- a
    # **Docker bridge**, not the LAN address. This guest has more ways to get that wrong than a
    # desktop: eth0 is qemu's SLIRP net (10.0.2.15) and eth3 the private guest<->host witness
    # link, and an address from either is a name that resolves and then goes nowhere. Both are
    # DENIED, so avahi never sees them. eth1/eth2 stay allowed because which one carries the VIP
    # is not fixed -- production puts it on eth2, the agent-less harnesses use the baked eth1
    # default -- and pinning one here would break the other.
    #
    # What the daemon may now auto-publish is `briard-node-<id>.local`, the node id, which is a
    # name no household is ever given; the name they ARE given is published explicitly by
    # briard-mdns, is flock-scoped, and carries the VIP and nothing else.
    services.avahi = {
      enable = true;
      denyInterfaces = [ "eth0" "eth3" ];
      publish.enable = true;
      publish.userServices = true; # THE gate: without it EntryGroupNew is refused and nothing publishes
      publish.addresses = true;
      publish.workstation = false; # no _workstation._tcp browsing bait
      publish.hinfo = false; # no CPU/OS disclosure on a household LAN
      nssmdns4 = false; # nothing in the guest resolves .local names; it only answers
    };
  };
}
