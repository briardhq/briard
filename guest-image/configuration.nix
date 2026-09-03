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
  # The installer's one-time "this volume is brand new" marker (agent/guestagent's
  # dataFormatMarker). A /run path is a two-sided contract with the agent, restated here the way
  # mdnsEnvPath and vipEnvPath are, and the Go side owns it.
  dataFormatMarker = "/run/briard/data.format";
  snapDir = "${btrfsRoot}/.snapshots"; # pre-upgrade snapshots, siblings of the data subvolume
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
  # The FLOCK's visible name, handed down by the agent (net.mdnsname) and NOT baked: it is pet
  # identity arriving at a cattle image, which is the distinction V3.19 was found for.
  # How long a publisher waits for avahi to confirm what it published before it gives up and lets
  # systemd restart it. Shared by both publishers, and for the flock name it is still the BACKSTOP
  # rather than the working path -- the settle wait below is what keeps that one out of the failure
  # V3.22 measured. For the SERVICE records it is load-bearing: they are created while converge is
  # starting containers, so podman's bridge and veths appear inside their probe window and avahi
  # wedges the group ([V3b.30](a), measured on two runs of one closure). See vipPublish, settled.
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
  # The front door's routing table (shared/routes.Path), written by converge. BOTH the proxy and
  # the service-name publisher read this one file -- the publisher with jq, so that there is no
  # pre-flattened second copy for anyone to read while it is stale. Restated here rather than
  # shared, the same way mdnsEnvPath and vipEnvPath are: a /run path is a two-sided contract with
  # the agent, and the Go side owns it.
  routesPath = "/run/briard/routes.json";
  # How often the service-name publisher notices that the routing table changed. Lag, not a race:
  # nothing is waiting on a name to appear within a deadline, and an install prints the name it
  # just created from the agent's own knowledge, not from avahi.
  mdnsWatchSecs = 2;

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
    dev="$VIP_DEV"
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

  # The PER-SERVICE mDNS names ([B.48]): one `briard-<flock>-<service>.local` A record per routed
  # service, all pointing at the VIP, so that the front door's Host-based routing has names to be
  # reached by. The list is written by converge (shared/routes.HostsPath) from the same composed
  # table the proxy routes on -- so a name that is published but not routed, or routed but not
  # published, is not expressible.
  #
  # ⚠️ A SEPARATE PUBLISHER FROM briard-mdns, deliberately, and this is the containment that made
  # per-service names acceptable at all. The flock's own name is the node's one canonical contact
  # address, and V3.22 is what a wedged avahi publisher costs: five minutes of a name resolving
  # nowhere while the unit read `active`. Folding N records into that process would put the
  # household's most important name behind a publisher that now churns on every install; split, a
  # service-name publisher that wedges takes only the service names with it, and briard-mdns keeps
  # its settle-wait untouched. The split is NOT a licence to skip the rest of what V3.22 bought:
  # this publisher wedged the same way the moment anything asserted its records ([V3b.30](a)), so
  # it now carries its own establishment deadline and read-back (settled, below). What stays
  # unshared is the settle-wait, which is about the VIP moving and belongs where the VIP is
  # resolved.
  #
  # ⚠️ IT WATCHES THE FILE RATHER THAN BEING RESTARTED. An install writes the table while this is
  # already running, and a rename rewrites it too, so the alternative was a `systemctl restart`
  # from inside converge -- which is both a cross-component call and, worse, a no-op exactly when
  # it is needed most: before the first install this unit may be inactive, and `try-restart` on an
  # inactive unit publishes nothing while reporting success. Polling the mtime has no such state.
  # ${toString mdnsWatchSecs}s of lag on a name is nothing; a name that never appears is not.
  #
  # A NODE WITH NOTHING TO PUBLISH STILL RUNS. It holds no records and watches: that is the shipped
  # zero-service state, and it is what makes the first install's names appear without anyone
  # having to start anything.
  mdnsServicesPublish = pkgs.writeShellScript "briard-mdns-services-publish" ''
    set -uo pipefail
    addr="''${VIP_ADDR%%/*}"
    if [ -z "$addr" ]; then
      echo "briard-mdns-services: no VIP address; nothing to point the service names at" >&2
      exit 1
    fi
    pids=""
    stamp=""
    # avahi withdraws a record when its publisher exits, so killing these IS the withdrawal --
    # both on a re-publish and on the demote that stops this unit.
    withdraw() {
      [ -n "$pids" ] || return 0
      kill $pids 2>/dev/null || true
      wait $pids 2>/dev/null || true
      pids=""
    }
    # THE READ-BACK, which is what makes "publishing" a claim rather than a hope ([V3b.30](a)).
    #
    # ⚠️ MEASURED 2026-09-01, on THREE RUNS OF A BYTE-IDENTICAL CLOSURE (nixosTest/mosquitto):
    # twice no record established at all -- with the avahi-publish processes alive, silent, and
    # exiting never -- while the flock's own name, published three seconds earlier by the other
    # publisher, resolved fine and a manual publish issued at that moment established instantly.
    # The daemon is healthy and only the existing groups are wedged, so nothing but NEW groups
    # clears them. It is V3.22's wedge one level down, and the window is structural rather than
    # unlucky: converge rewrites the routing table and starts the containers at the same instant,
    # podman creates a bridge and a veth, and avahi re-probes every group when an interface
    # appears.
    #
    # ⚠️ IT HAS TO BE THE PUBLISHER'S OWN `Established under name`, and the cheaper check that is
    # NOT good enough was measured too: asking the local daemon to resolve the name comes back
    # SUCCESSFUL for a group that is merely registered, probing or wedged -- the record is in its
    # registry either way -- so a read-back through avahi-resolve passes while nothing on the LAN
    # can see the name. That is the same vacuous green in a new place. The line the client prints
    # is the daemon telling it the group reached ESTABLISHED, and it is the only signal that means
    # what we need it to mean.
    #
    # PER-PUBLISHER FILES, not vipPublish's fifo: a fifo needs a reader in the main shell and the
    # pid bookkeeping that goes with it, which is affordable for one publisher and not for N. A
    # file on tmpfs that stdbuf keeps line-fresh is the same signal with none of that, and it
    # doubles as the diagnostic to print when the deadline passes.
    #
    # Failure exits rather than warns, for the reason vipPublish exits: a restart IS the cure, and
    # `Restart=on-failure` is already there. A publisher that no-ops quietly is the same disease as
    # a name that resolves nowhere.
    outDir=/run/briard/mdns-services
    settled() {
      [ "$k" -gt 0 ] || return 0
      waited=0
      while [ "$waited" -lt ${toString mdnsEstablishSecs} ]; do
        ok=1
        for f in "$outDir"/*.log; do
          ${pkgs.gnugrep}/bin/grep -q "Established under name" "$f" 2>/dev/null || ok=0
        done
        [ "$ok" = 1 ] && return 0
        ${pkgs.coreutils}/bin/sleep 1
        waited=$((waited+1))
      done
      return 1
    }
    trap 'withdraw' EXIT
    while :; do
      now=$(${pkgs.coreutils}/bin/stat -c %Y ${routesPath} 2>/dev/null || echo none)
      if [ "$now" != "$stamp" ]; then
        stamp="$now"
        withdraw
        n=0
        m=0
        k=0
        rm -rf "$outDir"; mkdir -p "$outDir"
        # An ABSENT file is the state before this node has ever converged -- at boot, or on a node
        # that has not promoted. Not an error and not worth a shell diagnostic every time: it means
        # nothing is routed, which is exactly what publishing zero names says.
        #
        # THE NAMES ARE READ, NEVER COMPOSED. Every rule for building one lives in
        # shared/routes.HostName, so a second name form (`*.casa`, [V3b.14]) is a change in one Go
        # function rather than in a Go function and a shell format string that must agree.
        if [ -r ${routesPath} ]; then
          while IFS= read -r host; do
            [ -n "$host" ] || continue
            # -R: allow this name to coexist with other records for the same address. NOT unique,
            # which is what makes a duplicate silently shadow rather than fail (measured 2026-08-23)
            # -- the reason these names are flock-scoped and not bare `homeassistant.local`.
            # Output to its own file, line-buffered, because settled reads it: see settled.
            k=$((k+1))
            ${pkgs.coreutils}/bin/stdbuf -oL ${pkgs.avahi}/bin/avahi-publish -a -R "$host" "$addr" >"$outDir/$k.log" 2>&1 &
            pids="$pids $!"
            n=$((n+1))
          done < <(${pkgs.jq}/bin/jq -r '.services[]?.hosts[]? // empty' ${routesPath} 2>/dev/null)
          # THE SERVICE RECORDS ([V3b.30](a)). Same file, same withdrawal, one more loop -- the
          # difference from the names above is who does the looking: a name is what someone types,
          # a service record is what an appliance BROWSES for. Tasmota- and ESPHome-class firmware
          # finds its broker by asking for `_mqtt._tcp` and nothing else.
          #
          # -H POINTS THE SRV RECORD AT THE SERVICE'S OWN NAME -- the A record published in the
          # loop above, which resolves to the VIP. Never at the guest's own hostname, which is
          # what avahi-publish would use by default: that name is node-scoped, so every device on
          # the LAN would be sent to the machine that has just stopped being Primary. Both halves
          # come out of this one process, so a service record cannot outlive the name it targets.
          #
          # READ AS TSV, NEVER COMPOSED, for the same reason the host names are: the instance
          # label is built once, in shared/routes.InstanceName. jq's @tsv is also what makes the
          # field split safe -- it escapes an embedded tab or newline rather than emitting one,
          # and shared/routes refuses them upstream of that.
          while IFS="$(${pkgs.coreutils}/bin/printf '\t')" read -r host name type port; do
            [ -n "$host" ] && [ -n "$name" ] || continue
            # Output kept and line-buffered, for the reason the address loop above gives.
            k=$((k+1))
            ${pkgs.coreutils}/bin/stdbuf -oL ${pkgs.avahi}/bin/avahi-publish -s -H "$host" "$name" "$type" "$port" >"$outDir/$k.log" 2>&1 &
            pids="$pids $!"
            m=$((m+1))
          done < <(${pkgs.jq}/bin/jq -r '.services[]? | (.hosts[0]? // empty) as $h | .announce[]? | [$h, .name, .type, .port] | @tsv' ${routesPath} 2>/dev/null)
        fi
        echo "briard-mdns-services: publishing $n service name(s) and $m service record(s) at $addr" >&2
        if ! settled; then
          echo "briard-mdns-services: avahi did not establish all $k record(s) within ${toString mdnsEstablishSecs}s -- giving up so systemd retries with fresh entry groups" >&2
          ${pkgs.gnugrep}/bin/grep -H "" "$outDir"/*.log >&2 || true
          exit 1
        fi
      fi
      ${pkgs.coreutils}/bin/sleep ${toString mdnsWatchSecs}
    done
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
  # Upstream's unit is built for this (its own comment says "to be used as OnFailure unit in
  # drbd-promote@.service"): it `Conflicts=drbd-promote@%i.service`, so starting it demotes, and
  # it carries `FailureAction=reboot` for the case where `drbdsetup secondary` is REFUSED --
  # something still holding the device open. That escalation is proportionate however the demote
  # was triggered: DRBD is single-primary, so a node stuck Primary after declaring it cannot serve
  # blocks its peer from taking over, and nothing else recovers from that.
  #
  # `r0` is written out for v0's single resource, exactly as drbd-reactor.service's ExecStop is.
  chainMemberFailure = {
    OnFailure = "drbd-demote-or-escalate@r0.service";
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
  imports = [ ./slim.nix ];

  # The agent binary this guest runs. It is an option rather than a callPackage here because
  # disk-image.nix already builds a VERSIONED one for briard-guest-agent + briard-deadman, and a
  # second instantiation with different arguments would put TWO agent derivations in the shipped
  # image's closure. Setting it there means the same store path serves all three units.
  #
  # It exists at all because briard-services (converge-at-promotion, [V3b.3](f)) is defined HERE:
  # it is a promoter chain member, so it has to live in the same module as briard-data and
  # briard-vip — naming a unit the guest does not define fails the whole ordered chain. The
  # default is what makes the nixosTests work unchanged: a test node gets a guest-tagged agent
  # without every test having to hand one in, and converges with the product's own code rather
  # than a harness stand-in.
  options.briard.agentPackage = lib.mkOption {
    type = lib.types.package;
    default = pkgs.callPackage ../agent/package.nix { tags = [ "guest" ]; };
    defaultText = lib.literalExpression "the guest-tagged briard-agent";
    description = "The briard-agent build this guest's units invoke (guest-tagged).";
  };

  # Image tarballs baked into this disk's closure and loaded into podman at boot. A NODE fact,
  # not a service one: an image has to be RESIDENT before anything renders against it, because
  # nothing on the failover path may pull. Used by the fleet's upgrade demo to pre-stage the
  # target of a rotation. Empty by default — the shipped image carries no workload.
  options.briard.stagedImages = lib.mkOption {
    type = lib.types.listOf lib.types.package;
    default = [ ];
    description = "Image tarballs pre-staged into local podman storage at boot.";
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
    # need the reactor either, because the agent attaches DRBD itself (drbd.provision + drbd.up).
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

    # THE ESCALATION MUST NOT RACE THE TEARDOWN IT TRIGGERS ([V3b.5](c)). Upstream ships
    # drbd-demote-or-escalate@ for use as `OnFailure=` on drbd-promote@ -- a context where the
    # promotion never completed, so nothing is holding the volume and `drbdsetup secondary`
    # succeeds immediately. We attach it to the chain MEMBERS instead (chainMemberFailure), where
    # the members ARE running and have to come down first, and nothing in the shipped unit orders
    # it after them. Measured without this drop-in: the script ran 0.1s after the target began
    # stopping, called `drbdsetup secondary` while /var/lib/briard was still mounted, got
    # `exit code=11`, and FailureAction=reboot rebooted a node whose demote then completed on its
    # own 0.9s later during shutdown. A spurious reboot, from a race rather than a stuck volume.
    #
    # Ordering after drbd-promote@ is the semantically right one rather than merely the latest:
    # that unit's own ExecStop is the NORMAL demote path, and it stops after every member. So the
    # escalation now runs only once the ordinary route has had its turn -- by which point
    # `drbdsetup secondary` either returns 0 ("already secondary anyways", per the shim) or the
    # resource really is stuck Primary, which is the only case worth rebooting for.
    systemd.services."drbd-demote-or-escalate@r0" = {
      overrideStrategy = "asDropin";
      after = [ "drbd-promote@r0.service" "briard-data.service" ];
    };

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
        # THE SAME BUDGET AS EVERY OTHER CHAIN MEMBER ([B.125](b)). A oneshot may carry
        # Restart=on-failure -- only `always`/`on-success` are refused for this Type, and a
        # oneshot that exits cleanly is never restarted -- so the policy is uniform across the
        # chain rather than "the simple ones retry and the oneshots get exactly one attempt",
        # which is what the absence of a directive used to mean and nobody had decided.
        Restart = "on-failure";
        RestartSec = 2;
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "briard-data-up" ''
          set -eu
          mkdir -p ${btrfsRoot}
          # FORMAT ONLY WHEN THE INSTALLER SAID SO ([B.126]). This used to be
          # `blkid /dev/drbd0 || mkfs.btrfs -f` -- a forced format behind a probe that cannot tell
          # "this device is blank" from "this device could not be read", since blkid is non-zero
          # for both. A transient read failure on a healthy volume would have reformatted the
          # household's replicated data, and this unit runs at EVERY promotion, forever.
          #
          # The decision now belongs to bring-up, which alone knows this is the seed of a NEW flock
          # on a disk create-md just claimed (FreshInit && CreatedMetadata). The ACT stays here
          # because only a Primary can be formatted and nothing in the agent may promote
          # (architectural invariant 2). ⚠️ The marker is on TMPFS, and that is the property rather
          # than a detail: it cannot outlive the boot that created the volume, so no later boot,
          # promotion or failover can ever find it.
          #
          # -f is right HERE, where the installer has just claimed the disk for a brand-new flock
          # and overwriting a previous life is the intent. The bug was never the flag; it was a
          # destructive operation on a path that runs forever.
          # MOUNT GUARDED, because this unit may now be RETRIED ([B.125](b)): mounting an already
          # mounted path stacks a second mount rather than failing, so the retry has to ask.
          # ⚠️ QUOTED, AND THAT IS A SAFETY PROPERTY RATHER THAN STYLE. `[ -e $X ]` with an empty X
          # is a ONE-argument test on the string "-e", which is always TRUE -- so a lost
          # interpolation here does not mean "never format", it means "format on EVERY promotion",
          # on every node. MEASURED, by doing it: an editing slip emptied this path, drbd-failover
          # went red, and the survivor had reformatted the replicated volume mid-failover. Quoted,
          # the same slip yields `[ -e "" ]`, which is false -- the mount then fails and the node
          # demotes, which is the direction this whole item exists to move things in.
          if [ -e "${dataFormatMarker}" ]; then
            rm -f "${dataFormatMarker}"   # consume FIRST: a format that fails must not be retried
            mkfs.btrfs -f /dev/drbd0
          fi
          # An unformatted volume fails here, which fails the chain and demotes the node -- loud
          # and recoverable, which "silently reformatted" is not. Same direction create-md takes:
          # refuse rather than overwrite.
          # MOUNT GUARDED, because this unit may now be RETRIED ([B.125](b)): mounting an already
          # mounted path stacks a second mount rather than failing, so a retry has to ask first.
          ${pkgs.util-linux}/bin/mountpoint -q ${btrfsRoot} || mount /dev/drbd0 ${btrfsRoot}
          # First use: the snapshots dir, sibling of whatever service subvolumes get created
          # later. It replicates with the volume, so it survives failover. Idempotent. A
          # service's own data subvolume is created when that service is INSTALLED (the guest's
          # renderer makes it), so a node nobody has given a workload to mounts an empty volume
          # — which is the honest state of one.
          mkdir -p ${snapDir}
        '';
        ExecStop = "${pkgs.util-linux}/bin/umount ${btrfsRoot}";
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
    #    defined unconditionally for the same reason briard-data is: naming a unit the guest does
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
      after = [ "briard-data.service" ];
      requires = [ "briard-data.service" ];
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
        ExecStart = "${config.briard.agentPackage}/bin/briard-agent --converge";
        ExecStop = "${config.briard.agentPackage}/bin/briard-agent --converge-stop";
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
    #     A PROMOTER CHAIN MEMBER since [B.125], where it used to be bound to briard-vip the way
    #     the front door is (wantedBy + partOf). The three things that binding bought are all
    #     still true, and the chain states them more strongly: the name appears only when this
    #     node actually holds the VIP, it points at the VIP rather than at whatever else the guest
    #     is addressed on, and on a pair only the PRIMARY publishes — so the two nodes of ONE
    #     flock never collide with each other. What membership ADDS is that a node which cannot
    #     publish hands the resource on instead of serving addresses nobody can reach. Two
    #     DIFFERENT flocks in one house still can, and avahi resolves that by renaming one of them
    #     silently, which is why the published name is read back rather than assumed.
    # Ten-minute lease renewal, for as long as this node holds the VIP.
    #
    # NOT A CHAIN MEMBER, deliberately and for [V3b.5c]'s reason: a renewal that fails must never
    # be able to demote a serving node. Nothing Requires it, its failure propagates nowhere, and
    # the real consequence of a renewal going wrong is a NAK, which dhcpcd's own hook handles as
    # an address change rather than as a unit failure.
    #
    # wantedBy + partOf briard-vip is the binding briard-mdns's comment above describes: the timer
    # starts when the node takes the VIP and stops when it gives it up, so it cannot tick on a
    # standby. `wantedBy` is a WEAK reference on purpose -- a timer that will not start must not
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

    systemd.services.briard-mdns = {
      description = "Briard mDNS name for the VIP";
      # A PROMOTER CHAIN MEMBER ([B.125]): reactor writes PartOf=drbd-services@<res>.target and
      # Requires=/After= briard-vip, so the START binding wantedBy used to express is the chain's
      # now. avahi-daemon is NOT a member, so its ordering stays stated here.
      #
      # ⚠️ partOf briard-vip STAYS, for RESTART propagation rather than for start ([B.125](b)).
      # briard-vip may now be retried, and its ExecStop withdraws the address and can take the NIC
      # down -- which is the V3.22 wedge trigger for a publisher that keeps running across it: an
      # established entry group whose interface goes away and returns is exactly what was "never
      # established again" in the field. Carrying these two with a VIP restart means they
      # re-establish cleanly instead of holding a record through the churn.
      partOf = [ "briard-vip.service" ];
      after = [ "briard-vip.service" "avahi-daemon.service" ];
      requires = [ "avahi-daemon.service" ];
      serviceConfig = {
        # The address briard-vip ACTUALLY claimed, so the name cannot drift from it. Two sources,
        # last wins: the agent-written config, and the live file that records what was really
        # taken -- which under DHCP is the only one that knows. Both are REQUIRED ([V3b.16a]): this
        # unit can only run downstream of a briard-vip that ran, which can only run downstream of
        # an agent that configured it, so an absent file is a broken node and not a bare one.
        #
        # FLOCK_NAME comes from the agent (net.mdnsname) and has NO baked fallback on purpose: a
        # node with no minted name must publish nothing rather than publish a guess, and
        # `briard-.local` is worse than silence. ConditionPathExists enforces that, and it is the
        # one file here whose absence is NOT a fault: it tracks whether the flock has a minted name
        # (FLOCK_NAME="" -> net.mdnsname deliberately writes nothing), never whether an agent is
        # present. So the unit stays inactive rather than failing in a restart loop.
        EnvironmentFile = [ vipEnvPath vipLivePath mdnsEnvPath ];
        # avahi-publish holds the record for as long as it runs and withdraws it on exit, so the
        # unit's lifetime IS the record's lifetime -- no cleanup path to get wrong on demotion.
        ExecStart = "${vipPublish}";
        Restart = "on-failure";
        # A TRANSIENT CRASH MUST NOT MOVE THE RESOURCE ([V3b.5](c)). Without this, the
        # auto-restart's stop job deactivates drbd-reactor's target -- which unmounts the data
        # volume and demotes the node on ONE crash, measured, with a peer taking the resource
        # about half the time. `direct` restarts through activating instead of failed, so
        # dependents are not notified of the temporary failure. It is also what lets the
        # StartLimit below finally accumulate: the unit is no longer torn down and started
        # fresh on every cycle, so a member that genuinely gives up still reaches `failed`
        # and still hands the resource on -- which is what this budget always claimed to do.
        # NOT on briard-data/services/vip: for those, failure really does mean this node
        # must not hold the volume.
        RestartMode = "direct";
        RestartSec = 2;
      };
      unitConfig = chainMemberFailure // {
        ConditionPathExists = mdnsEnvPath;
        # THE ESCALATION BUDGET, and it is ours alone ([B.125]). drbd-reactor starts the chain once
        # and never watches it again -- it has no retry counter -- so what turns a broken unit into
        # a failover is systemd reaching the failed state.
        #
        # ⚠️ THE BUDGET DOES NOT CURRENTLY DO WHAT THE REST OF THIS COMMENT DESCRIBES, measured
        # 2026-09-02 ([V3b.5](c)). The reading below is that `Restart=` POSTPONES the failover
        # until the start limit is exceeded, and every number here is tuned on it. It does not: the
        # target `Requires=` this unit, so the FIRST crash already stops the target, demotes the
        # resource and unmounts the data volume -- and the rebuild starts this unit fresh, so the
        # counter reads `restart counter is at 1` on every cycle and the limit is never reached.
        # The numbers are left exactly as they were rather than re-tuned around a mechanism that
        # is not running; what needs fixing is the propagation, not the budget. Everything below
        # is the ORIGINAL rationale, kept because it is what the numbers mean once (c) lands.
        #
        # THE SEMANTICS ARE BACK TO FRONT FROM THE OBVIOUS READING, so tune them deliberately:
        # with Burst held constant, a LARGER interval is STRICTER -- it demands fewer than Burst
        # starts across a wider window. It is also what decides whether the limit fires at all,
        # since it only ever trips when Burst x failure-cycle < Interval. That is exactly why the
        # 5-in-10s DEFAULT never fired here: a cycle of RestartSec plus the establishment deadline
        # is 17-37s, so a 10s window never held more than one start and this unit would have
        # restarted forever without escalating.
        #
        # 300/5 IS A JUDGEMENT, NOT A MEASUREMENT, and it is the same on all three `simple` chain
        # members deliberately -- one number to reason about until something gives us a reason for
        # more. It buys ~85s of trying for a publisher and ~10s for the front door, whose cycle is
        # ~2s; that asymmetry is known and accepted rather than overlooked. [B.125](b) holds what
        # would justify changing it: how long a healthy publisher takes to establish on a cold
        # household LAN, and how long the door can lose :80 to its own previous instance.
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
      };
    };

    # 3b. the per-service mDNS names -- the other half of Host-based routing ([B.48]). See
    #     mdnsServicesPublish for why this is a second publisher rather than more work inside
    #     briard-mdns, and why it watches its input instead of being restarted.
    #
    #     A chain member beside briard-mdns since [B.125]: the records point at the VIP, so they
    #     must exist only where the VIP does and vanish when it moves. Ordered after briard-mdns,
    #     not because it depends on it, but because the address-settle wait lives there and
    #     publishing into avahi's probe window is what wedged it (V3.22) -- letting the flock's
    #     name establish first means these start on an address that has already held still. Under
    #     the chain that ordering is reactor's Requires=/After= on the previous member, so it is
    #     no longer stated twice.
    #
    #     NO ConditionPathExists on the hosts file: an empty or absent list is a node with nothing
    #     routed, which is the shipped state, and the watcher's whole job is to be already running
    #     when the first install writes one.
    systemd.services.briard-mdns-services = {
      description = "Briard mDNS names for the routed services";
      # A chain member too ([B.125]); see briard-mdns above, including why partOf briard-vip stays
      # for restart propagation. Reactor orders it after briard-mdns, the previous member.
      partOf = [ "briard-vip.service" ];
      after = [ "briard-vip.service" "briard-mdns.service" "avahi-daemon.service" ];
      requires = [ "avahi-daemon.service" ];
      serviceConfig = {
        # The address briard-vip ACTUALLY claimed, same two files and same last-wins order as
        # briard-mdns: a service name must never point at an address this node did not take.
        EnvironmentFile = [ vipEnvPath vipLivePath ];
        ExecStart = "${mdnsServicesPublish}";
        Restart = "on-failure";
        # A TRANSIENT CRASH MUST NOT MOVE THE RESOURCE ([V3b.5](c)). Without this, the
        # auto-restart's stop job deactivates drbd-reactor's target -- which unmounts the data
        # volume and demotes the node on ONE crash, measured, with a peer taking the resource
        # about half the time. `direct` restarts through activating instead of failed, so
        # dependents are not notified of the temporary failure. It is also what lets the
        # StartLimit below finally accumulate: the unit is no longer torn down and started
        # fresh on every cycle, so a member that genuinely gives up still reaches `failed`
        # and still hands the resource on -- which is what this budget always claimed to do.
        # NOT on briard-data/services/vip: for those, failure really does mean this node
        # must not hold the volume.
        RestartMode = "direct";
        RestartSec = 2;
      };
      # THE SAME GUARD briard-mdns CARRIES, and for the same reason: a node with no minted flock
      # name has no per-service names either (they are composed from it), so there is nothing to
      # publish and the unit must stay inactive rather than fail in a restart loop. Measured
      # 2026-08-31 without it: 56 restarts in one test run, on a rig that had no name and no VIP
      # env -- noise that would have hidden a real failure of this unit.
      unitConfig = chainMemberFailure // {
        ConditionPathExists = mdnsEnvPath;
        # The same budget, for the same reason -- see briard-mdns above ([B.125]).
        StartLimitIntervalSec = 300;
        StartLimitBurst = 5;
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
        ExecStart = "${pkgs.reverse-proxy}/bin/reverse-proxy"
          + " -http :80 -listen :443"
          + " -cert ${tlsDir}/fullchain.pem -key ${tlsDir}/key.pem";
        Restart = "on-failure";
        # A TRANSIENT CRASH MUST NOT MOVE THE RESOURCE ([V3b.5](c)). Without this, the
        # auto-restart's stop job deactivates drbd-reactor's target -- which unmounts the data
        # volume and demotes the node on ONE crash, measured, with a peer taking the resource
        # about half the time. `direct` restarts through activating instead of failed, so
        # dependents are not notified of the temporary failure. It is also what lets the
        # StartLimit below finally accumulate: the unit is no longer torn down and started
        # fresh on every cycle, so a member that genuinely gives up still reaches `failed`
        # and still hands the resource on -- which is what this budget always claimed to do.
        # NOT on briard-data/services/vip: for those, failure really does mean this node
        # must not hold the volume.
        RestartMode = "direct";
        RestartSec = 2;
      };
      # The same budget as the publishers, deliberately identical -- see briard-mdns above for the
      # semantics and why the number is a judgement ([B.125]). It matters more here than the shape
      # suggests: with no StartLimit at all systemd's 5-in-10s default applies, and at RestartSec=2
      # that IS reachable, so a door would hand the resource on after ~10s of trying. That is eager
      # for one whose likeliest transient is losing the race for :80 to its own previous instance
      # during a failover, and it is the asymmetry [B.125](b) holds open.
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
    # measured chain: briard-farm docs/V3.md [B.101].
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
    # **Docker bridge**, not the LAN address. eth0 is qemu's SLIRP net (10.0.2.15), an address that
    # would be a name resolving to nowhere.
    #
    # ⚠️ AN ALLOW-LIST, NOT A DENY-LIST, AND THAT IS A FIX RATHER THAN A PREFERENCE ([V3b.30](a)).
    # Denying eth0 kept the guest's OWN interfaces honest but said nothing about the ones podman
    # creates, and those are the ones that broke it. MEASURED across ten runs of
    # nixosTest/mosquitto: converge writes the routing table and starts the containers in the same
    # instant, podman brings up its bridge and a veth, avahi logs `New relevant interface
    # veth0.IPv4 for mDNS` and RE-PROBES every entry group -- and a group caught in that window
    # wedges for good, neither established nor refused. It happened on about half of runs; it took
    # the per-service names down with it every time, silently, INCLUDING AFTER the publisher had
    # already logged `Established under name` (so a one-shot read-back cannot see it, and a restart
    # lands in the same window). A household hits exactly this on every install and every
    # promotion that starts a service.
    #
    # So the household's NICs are named and everything else is invisible to avahi, which removes
    # the trigger rather than reacting to it -- and it is the right shape anyway: a pod-internal
    # bridge has no business carrying the household's names, and a browse from the guest used to
    # show its own records arriving on `podman1` and `veth0`.
    #
    # eth1/eth2 are BOTH here because which one carries the VIP is not fixed -- production puts it
    # on eth2, the agent-less harnesses use the baked eth1 default -- and pinning one would break
    # the other. The list is the guest's whole NIC set minus eth0, so a future NIC needs a line
    # here; that is the cost of an allow-list, and it is paid in the one place a reader looks.
    #
    # ⚠️ ONE RE-PROBE REMAINS, and it is harmless where the old one was not: under BRIDGE mode the
    # guest MAKES eth2 itself, as a macvlan child, after avahi has started (platform/qemu.go). That
    # interface IS allowed, so avahi re-probes when it appears -- but it appears during network
    # setup, before anything has a routing table to publish from, rather than in the middle of a
    # promotion. The wedge needed a new interface DURING a publish.
    #
    # ⚠️ eth3, THE PRIVATE HOST<->GUEST LINK, IS ALLOWED, and it is the one interface whose
    # inclusion is a decision rather than a default ([V3b.19]). The host running this guest is the
    # ONE machine on the LAN that cannot hear it: macvtap isolates a parent NIC from its own
    # children, and a switch does not reflect a frame to the port it came from -- so the guest's
    # multicast reaches every machine in the house except the one it lives in. eth3 is a plain tap
    # (platform/qemu.go keeps it NetBridge even under macvtap, deliberately), so it is the only
    # path by which the household's own machine can resolve the household's own name.
    #
    # The auto-address fear above does not reach the name a household is GIVEN. briard-mdns
    # publishes an EXPLICIT address -- the VIP, stripped from the same VIP_ADDR the node claimed --
    # and an explicit `avahi-publish -a` record is interface-independent, so what eth3 carries is
    # the VIP and never 10.11.9.2. That distinction is what makes allowing eth3 safe rather than
    # merely useful: the private address is TRANSPORT and must never become identity. It is
    # node-scoped, while the VIP and the name are flock-scoped and survive a failover it does not.
    # Reaching that VIP from the host is the route the agent maintains (platform/route.go).
    #
    # What the daemon may auto-publish is `briard-node-<id>.local`, the node id -- on eth3 that
    # resolves to 10.11.9.2, which is true, node-scoped, and a name no household is ever given, on a
    # two-host point-to-point wire. The name they ARE given is published explicitly by briard-mdns,
    # is flock-scoped, and carries the VIP and nothing else.
    #
    # Responder-only stands: `nssmdns4 = false` below, so nothing in this guest RESOLVES a .local
    # name through libc. The guest's two host-facing dependencies (the witness forwarder, the
    # deadman gate) are fixed addresses precisely because they must work when everything else is
    # dead, and a service that wants discovery (Home Assistant's zeroconf) does its own multicast
    # rather than going through NSS.
    services.avahi = {
      enable = true;
      # The household's NICs and nothing else -- not eth0, and not the interfaces podman creates.
      # eth3 is here on purpose ([V3b.19]); the exclusions are the point ([V3b.30](a)). See above.
      allowInterfaces = [ "eth1" "eth2" "eth3" ];
      publish.enable = true;
      publish.userServices = true; # THE gate: without it EntryGroupNew is refused and nothing publishes
      publish.addresses = true;
      publish.workstation = false; # no _workstation._tcp browsing bait
      publish.hinfo = false; # no CPU/OS disclosure on a household LAN
      nssmdns4 = false; # nothing in the guest resolves .local names; it only answers
    };
  };
}
