# Test outputs, factored out of flake.nix to keep the top-level flake lean. Given
# the overlaid pkgs (so the nodes get our drbd/drbd-reactor), nixpkgs, and the
# overlay, returns { tags; artifacts; }.
#
# These are the Tier-1 **hermetic nixosTests** (the `integration` pair drives a *nested* guest). They boot throwaway QEMU VMs,
# assert convergence, and vanish — no external state. They are NOT the lab/ soak fleet
# (Tier 2, driven by cmd/soak, never `nix build`).
#
# `tags` groups the tests so flake.nix can expose one buildable aggregate per tag:
#   nix build .#drbd | .#upgrade | .#ha | .#integration   (a slice)
#   nix build .#all                                        (every test — the nightly)
#   nix build .#tests.<name> -L                            (one test, e.g. drbd-fence)
# `nix flake check` stays light — it evaluates the flake + builds the config closures,
# but boots no VM tests; use a tag for that.
#
# this is the PUBLIC half. Every test that builds a `cloud/` package lives in
# `outputs-private.nix`, which flake.nix merges in tag-wise when it is present — so this
# file must stay buildable with `cloud/` and `lab/` absent from the tree.
{ nixpkgs, pkgs, overlay
  # The release id stamped into the shipped agent + guest disk. Defaulted so a direct
  # import (no flake) still evaluates; flake.nix passes the real one, derived from `self`.
, agentVersion ? "0.0.0-dev" }:
let
  # The guest as SHIPPED: an empty payload slot. Only zero-service.nix boots this —
  # every other framework test selects the fixture, because it needs a payload that writes
  # checkable data.
  shippedGuestModule = ../guest-image/configuration.nix;
  guestModule = ./dummy-guest.nix; # configuration.nix + the dummy fixture in the slot
  haGuestModule = ../guest-image/ha-guest.nix; # same guest, HA in the payload slot
  haUpgradeGuestModule = ../guest-image/ha-upgrade-guest.nix; # HA on the from-image, to warm-staged

  # HA in the slot: real Home Assistant boots and serves at the VIP, its recorder
  # SQLite + .storage landing on the DRBD subvolume.
  haPayload = import ./ha-payload.nix { inherit pkgs; guestModule = haGuestModule; };

  # Kill the primary → HA fails over with config intact at the same VIP.
  haFailover = import ./ha-failover.nix { inherit pkgs; guestModule = haGuestModule; };

  # A real HA upgrade (2025.11.0 → 2025.12.0) carrying a real recorder schema
  # migration (v52 unit_class) through the pipeline, its data intact.
  haUpgrade = import ./ha-upgrade.nix { inherit pkgs; guestModule = haUpgradeGuestModule; };

  # The forced-failure half — a real regressed migration (briard_canary) trips
  # the S1 health-gate (entrygate-eval judges HA's real config-entry states) and the
  # {code + data} rollback restores HA to `from` with its recorder history intact.
  entrygateEval = pkgs.callPackage ./entrygate-eval-pkg.nix { };
  haUpgradeRollback = import ./ha-upgrade-rollback.nix {
    inherit pkgs entrygateEval;
    guestModule = haUpgradeGuestModule;
  };

  # Off-site encrypted `.storage` backup — the sacred config sealed client-side
  # (age) to an off-box target and restored byte-faithfully (the cheap DR half).
  briardBackup = pkgs.callPackage ./briard-backup-pkg.nix { };
  haBackup = import ./ha-backup.nix {
    inherit pkgs briardBackup;
    guestModule = haGuestModule;
  };

  # THE SHIPPED ARTIFACT: the bootable disk `install.sh` lays down, running no service. This
  # is what `.#artifacts.guest-disk` publishes and what the agent-driven bring-up tests boot,
  # so the shape a stranger installs is the shape CI exercises.
  guestDisk = import ../guest-image/disk-image.nix { inherit nixpkgs pkgs overlay agentVersion; };

  # There is no fixture-payload disk image. Tests that need a payload run on `guestModule`
  # (configuration.nix + dummy-payload.nix), which supplies it as a module — only a test booting
  # a prebuilt qcow2 would need one baked into an image, and none do.

  # Boot-select's disk: shipped + one extra baked generation, no payload. `.v1System`'s delta from
  # the running system is a single /etc file, so both grub entries boot the identical kernel and
  # the only thing that can distinguish them is which menu entry was chosen — which is the whole
  # proof. Rides `debug`, so this image is not a nightly cost.
  bootSelectGuestDisk = import ../guest-image/disk-image.nix {
    inherit nixpkgs pkgs overlay;
    stageSystemModule = { environment.etc."briard-generation".text = "v1"; };
  };
  driverPkg = pkgs.callPackage ./driver/package.nix { };
  agentPkg = pkgs.callPackage ../agent/package.nix { version = agentVersion; }; # the product agent binary (host + --guest)

  # The macvtap fd-passing launch wrapper, installed +x (a bare store source file is 0444,
  # which systemd-run can't exec). Shared by every agent/driver-launched integration test that runs
  # on the macvtap substrate (the default).
  netWrap = pkgs.runCommand "briard-net-wrap" { } ''
    install -Dm0755 ${../scripts/briard-net-wrap.sh} $out/bin/briard-net-wrap
  '';
  agentBringup = import ./agent-bringup.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; };
  agentReadopt = import ./agent-readopt.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; }; # restart transparent to guest

  # The host-agent deadman on a lone node must HOLD, never self-outage. Needs a guest with
  # a SHORT T_deadman so the reflex fires in seconds (baked into the guest-agent unit's env).
  deadmanGuestDisk = import ../guest-image/disk-image.nix {
    inherit nixpkgs pkgs overlay;
    guestAgentEnv = {
      BRIARD_DEADMAN = "8s";
      BRIARD_DEADMAN_JITTER = "1s";
      BRIARD_DEADMAN_TICK = "2s";
    };
  };
  agentDeadman = import ./agent-deadman.nix {
    inherit pkgs netWrap;
    agent = agentPkg;
    guestDisk = deadmanGuestDisk;
  };

  # The host-agent self-update PIVOT — the frozen, agent-independent commit/revert
  # mechanism (Type=notify gate). Hermetic (a single VM, no nested guest): the stub stands in
  # for the trial binary that decides whether to signal READY. Rides `.#all`.
  selfupdateStub = pkgs.callPackage ./selfupdate-stub.nix { };
  agentSelfupdate = import ./agent-selfupdate.nix {
    inherit pkgs;
    stub = selfupdateStub;
  };

  # The closure a guest must FETCH: the SHIPPED guest plus one marker file, so the delta is a
  # handful of paths — the shape of a real incremental release rather than a whole second system.
  # Testing delivery against the artifact a stranger installs is the point; what the node happens
  # to be serving has nothing to do with it.
  #
  # It is baked nowhere, which is the entire point: staging something already on the disk would be
  # satisfied from the local store and prove nothing.
  #
  # Taking `.v1System` off a disk-image import builds only that toplevel, never the qcow2: the
  # variant's `image` attr is simply not forced, so this costs no second disk build.
  stagedSystem =
    (import ../guest-image/disk-image.nix {
      inherit nixpkgs pkgs overlay;
      stageSystemModule = {
        environment.etc."briard-staged".text = "staged";
      };
    }).v1System;
  osStage = import ./os-stage.nix {
    inherit pkgs netWrap stagedSystem guestDisk;
    driver = driverPkg;
  };

  # The boot selector. Unlike os-stage this needs a second generation that IS on
  # the disk — the question is which of two bootable entries grub picks, not how bytes get
  # there — so it takes a disk that bakes one, and no payload.
  bootSelect = import ./boot-select.nix {
    inherit pkgs;
    guestDisk = bootSelectGuestDisk;
    stagingSystem = bootSelectGuestDisk.v1System;
    driver = driverPkg;
  };

  # The maintenance-mode contract suite — the FULL pause/poke/resume contract
  # (non-destructive, promoter-inert-while-paused, mount survives, clean resume). Nightly.
  # HERMETIC since V3.17e4: it drives the lifecycle from the test shell on a lib.nix node
  # instead of through the agent's verbs on a nested guest — see the file's header.
  maintenanceContract = import ./maintenance-contract.nix { inherit pkgs guestModule; };

  # The isolated, deterministic promote-vs-stop deadlock gate (promote, then
  # time a BARE maintenance pause -- no upgrade / snapshot / btrfs race). Reds at ~90s on the
  # deadlock (drbd-services@r0.target Before=drbd-reactor.service serializes the dying reactor's
  # own promote behind its stop), greens in ms once the drop-in is removed first. Debug tag; NOT
  # nightly. Hermetic same reason as the contract suite above.
  reactorPauseDeadlock = import ./reactor-pause-deadlock.nix { inherit pkgs guestModule; };

  # Upgrade + TLS-serving mechanism tests. Hermetic nixosTests on the
  # dummy guest (like the DRBD net), but each boots a multi-node cluster with
  # pre-staged images — converge-payload alone runs ~6 min.
  # the SHIPPED shape — an empty payload slot. The only test that boots the guest as
  # the downloadable artifact is built, and so the one that would catch the slot's optionality
  # regressing back into a baked-in workload.
  zeroService = import ./zero-service.nix { inherit pkgs; guestModule = shippedGuestModule; };

  # Put a service ONTO the shipped (zero-service) node at runtime — a real digest-pinned
  # TLS pull from a registry the test runs, our real renderer's quadlet output, and the promoter
  # chain it emits driving the pod up behind the front door.
  quadletRenderPkg = pkgs.callPackage ./quadlet-render-pkg.nix { };
  serviceInstall = import ./service-install.nix {
    inherit pkgs;
    guestModule = shippedGuestModule;
    quadletRender = quadletRenderPkg;
  };
  # The failure half — upgrade to a broken manifest, gate trips, {code+data} rollback to the
  # prior service. Reuses the whole hermetic scaffolding above (registry, CA, renderer, DRBD).
  serviceInstallBroken = import ./service-install.nix {
    inherit pkgs;
    guestModule = shippedGuestModule;
    quadletRender = quadletRenderPkg;
    broken = true;
  };

  convergeAtPromotion = import ./converge-at-promotion.nix { inherit pkgs guestModule; };
  convergePayload = import ./converge-payload.nix { inherit pkgs guestModule; };
  rollingUpdate = import ./rolling-update.nix { inherit pkgs guestModule; };
  tlsServing = import ./tls-serving.nix { inherit pkgs guestModule; };

  # The relocatable qemu bundle (runs on a host with no /nix/store) + its proof.
  qemuBundle = import ./qemu-bundle.nix { inherit pkgs; };

  # The machine report card on a real host (the free-local install gate).
  reportCard = import ./report-card.nix { inherit pkgs; agent = agentPkg; };

  # The bridge FALLBACK substrate -- a pure DELTA over installMacvtap: the NIC enslave,
  # the host-IP move, and a clean abort at that irreversible step. Nothing mode-independent lives
  # here, so it is a clean delete whenever the fallback goes.
  installBridge = import ./install-bridge.nix {
    inherit pkgs guestDisk;
    agent = agentPkg;
    qemuBundle = qemuBundle.bundle;
  };

  # On the DEFAULT macvtap substrate: the whole free-local `curl | sh` install
  # -> green, proven by an OFF-BOX LAN client reaching the payload at the VIP, using the bundled
  # qemu (no distro qemu) + a nested guest; plus the cattle/pet reinstall. It carries that
  # mode-independent chain here from the bridge test, so the default substrate carries it.
  installMacvtap = import ./install-macvtap.nix {
    inherit pkgs guestDisk;
    agent = agentPkg;
    qemuBundle = qemuBundle.bundle;
  };
in
{
  # The hermetic mechanism tests, grouped into tags. flake.nix turns each group into
  # a buildable aggregate (`.#drbd` … `.#all`) + a flat `.#tests.<name>`.
  tags = {
    # The DRBD failover net (7 topologies): bring-up, promote, failover, fence,
    # witness, and minority-refuses-to-promote (quorum). The fast mechanism core.
    drbd = {
      guest-drbd9 = import ./guest-drbd9.nix { inherit pkgs guestModule; };
      drbd-replicate = import ./drbd-replicate.nix { inherit pkgs guestModule; };
      drbd-promote = import ./drbd-promote.nix { inherit pkgs guestModule; };
      drbd-failover = import ./drbd-failover.nix { inherit pkgs guestModule; };
      reactor-evict = import ./reactor-evict.nix { inherit pkgs guestModule; }; # V3.17c2-iv-6: a PLANNED handover
      drbd-fence = import ./drbd-fence.nix { inherit pkgs guestModule; };
      drbd-witness = import ./drbd-witness.nix { inherit pkgs guestModule; };
      drbd-witness-loss = import ./drbd-witness-loss.nix { inherit pkgs guestModule; };
      single-node-promoter = import ./single-node-promoter.nix { inherit pkgs guestModule; }; # peer-less mode
      runtime-join = import ./runtime-join.nix { inherit pkgs guestModule; }; # grow single-node -> 3-node mesh at runtime
      drbd-loopback-path = import ./drbd-loopback-path.nix { inherit pkgs guestModule; }; # loopback-path gating experiment
      # The maintenance-mode contract (pause → poke → resume). It sat in `integration`
      # while it drove the agent's verbs over a nested guest's channel; V3.17e4 made it
      # hermetic, so it belongs with the other one-node promoter mechanisms. Nightly either way.
      maintenance-contract = maintenanceContract;
      # `witness-proxy` + `witness-tap-link` join this tag from outputs-private.nix —
      # both build the cloud witness. The three tests above mention the cloud only in prose.
    };

    # OS/payload rolling upgrade + TLS-serving (multi-node, staged images).
    upgrade = {
      converge-at-promotion = convergeAtPromotion;
      converge-payload = convergePayload;
      rolling-update = rollingUpdate;
      tls-serving = tlsServing;
    };

    # Real Home Assistant in the payload slot (2.4 GB image boot).
    ha = {
      ha-payload = haPayload;
      ha-failover = haFailover;
      ha-upgrade = haUpgrade; # real recorder schema migration through the upgrade
      ha-upgrade-rollback = haUpgradeRollback; # real regression trips the gate → {code+data} rollback
      ha-backup = haBackup; # off-site encrypted .storage backup + restore
    };

    # Agent-in-the-loop: the agent drives a real guest under *nested*
    # QEMU over the virtio-serial channel. Needs nested KVM.
    integration = {
      agent-bringup = agentBringup;
      agent-readopt = agentReadopt; # an agent restart re-adopts the running guest
      agent-deadman = agentDeadman; # a lone node holds (never self-outages) when its agent dies
      os-stage = osStage; # a closure the guest does NOT have, fetched over a cache
    };

    # The frozen host-agent self-update pivot (Type=notify commit/revert gate).
    # Hermetic — one VM, no nested guest, so it rides `.#all`.
    selfupdate = {
      agent-selfupdate = agentSelfupdate;
    };

    # The free-local install path. qemu-bundle proves the relocatable qemu bundle runs
    # with /nix/store masked (the stock-host condition). Hermetic — one VM, rides `.#all`.
    install = {
      zero-service = zeroService; # what a fresh install actually gives you — no service
      service-install = serviceInstall; # and putting something on it at runtime
      service-install-broken = serviceInstallBroken; # broken upgrade -> gate trips -> {code+data} rollback
      qemu-bundle = qemuBundle.test;
      report-card = reportCard;
      install-macvtap = installMacvtap; # DEFAULT substrate: curl|sh -> green, off-box VIP reach, cattle/pet reinstall
      install-bridge = installBridge; # the bridge FALLBACK delta: NIC enslave + host-IP move
    };

    # Debug harnesses — deliberately EXCLUDED from allTests / the nightly `.#all` (flake.nix
    # merges this into the flat `.#tests.*` only). Run by hand in a repro loop.
    debug = {
      # The boot selector, proven live -- but NOT in the nightly, because it cannot
      # be made reliable here. The sequence needs the guest stopped CLEANLY between launches,
      # and a nested L2 guest completes no shutdown by any trigger, so the harness
      # power-cuts it moments after it rewrote its own bootloader -- and the next boot then
      # sometimes never comes up at all. That failure is the honest consequence of a power cut
      # at the worst possible moment, which is precisely what the product avoids and this
      # harness cannot. Promote it back to `integration` when the non-nested rig exists.
      boot-select = bootSelect;
      reactor-pause-deadlock = reactorPauseDeadlock;
      # Spike: can a service be installed at runtime as a podman pod (quadlet), and does
      # that pod work as a promoter chain member? Answers a design question; promoted or deleted
      # once the design settles.
      quadlet-spike = import ./quadlet-spike.nix { inherit pkgs; guestModule = shippedGuestModule; };
      # — **THIS TEST FAILS TODAY, AND THAT IS ITS JOB.** Act 1 (crash the primary, the
      # survivor promotes) passes and is the control; act 2 (the survivor then restarts while its
      # peer is still absent) does not, because a diskless witness can KEEP quorum but never GRANT
      # it. It sits in `debug` rather than `drbd` precisely because it is red: the nightly must
      # stay a statement about what works. Run it by hand to see the gap in VANILLA DRBD — no
      # agent, no nesting, config baked into the image — and when is decided, this is its
      # acceptance test.
      drbd-survivor-restart = import ./drbd-survivor-restart.nix { inherit pkgs guestModule; };
      # `netbird-selfhost`, `witness-proxy-gray` and `witness-proxy-fence` join this tag from
      # outputs-private.nix.
    };
  };

  # Build artifacts (NOT tests): the product agent binary, the bootable guest qcow2,
  # and the nested-test driver. Consumed by the fleet (flake fleetArgs) + demo scripts,
  # so they stay reachable by name (`.#artifacts.<name>`).
  artifacts = {
    guest-disk = guestDisk;
    agent = agentPkg;
    driver = driverPkg;
    qemu-bundle = qemuBundle.bundle; # the relocatable qemu the free-local installer ships
    net-wrap = netWrap; # macvtap launch wrapper; the fleet runs on it too
  };
}
