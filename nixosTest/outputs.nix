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
  # THE GUEST AS SHIPPED, and now the only one the framework tests boot ([V3b.3](e2)). There used
  # to be two: this one, which zero-service alone used, and a `dummy-guest` that baked the fixture
  # into a build-time payload slot for everything else. The slot is deleted, so a test that needs a
  # workload INSTALLS one -- the same way a shipped node gets one -- and the guest under test is
  # the guest that ships.
  guestModule = ../guest-image/configuration.nix;
  # The dummy fixture as a CATALOGUED service: a digest-pinned manifest plus the tarball it names.
  # This is how a test gets a workload onto a node ([V3b.3](e2) deleted the build-time slot that
  # was the alternative) — the same fixture, delivered the way a shipped node actually gets one.
  fixture = import ./fixture-service.nix { inherit pkgs; };
  # HOME ASSISTANT AS A CATALOGUED SERVICE ([V3b.3](e2)). HA used to reach a guest through the
  # build-time payload slot, which is how nothing else could: the slot is gone, and HA is now
  # exactly what a user's service is -- a signed manifest naming a digest-pinned image, installed
  # onto the volume after promotion. Only the manifest's fields differ from the dummy's: /config,
  # :8123, and /manifest.json as the readiness path (HA has no /healthz, and `/` redirects into
  # onboarding).
  hassFixture = import ./fixture-service.nix {
    inherit pkgs;
    name = "home-assistant";
    mount = "/config";
    port = 8123;
    healthPath = "/manifest.json";
    version = "2026.7.1";
    imageFile = pkgs.home-assistant-image;
    imageName = "ghcr.io/home-assistant/home-assistant";
  };
  # The upgrade PAIR, as two published versions of one service: 2025.11.0 (recorder schema 51)
  # with 2025.12.0 (schema 53) as a variant. Installing the variant IS the upgrade -- same name,
  # new manifest, converge bounces the container onto it -- which is the shape a household's HA
  # update has, where the old test drove an image re-pin no user could produce.
  hassPairFixture = import ./fixture-service.nix {
    inherit pkgs;
    name = "home-assistant";
    mount = "/config";
    port = 8123;
    healthPath = "/manifest.json";
    version = "2025.11.0";
    imageFile = pkgs.home-assistant-upgrade-pair.from;
    imageName = "ghcr.io/home-assistant/home-assistant";
    variants.to = {
      version = "2025.12.0";
      imageFile = pkgs.home-assistant-upgrade-pair.to;
      imageName = "ghcr.io/home-assistant/home-assistant";
    };
  };

  # HA installed as a service: real Home Assistant boots and serves, its recorder
  # SQLite + .storage landing on the DRBD subvolume.
  hassPayload = import ./hass-payload.nix { inherit pkgs guestModule; fixture = hassFixture; };

  # Kill the primary → HA fails over with config intact at the same VIP.
  hassFailover = import ./hass-failover.nix { inherit pkgs guestModule; fixture = hassFixture; };

  # A real HA upgrade (2025.11.0 → 2025.12.0) carrying a real recorder schema
  # migration (v52 unit_class) through the pipeline, its data intact.
  hassUpgrade = import ./hass-upgrade.nix { inherit pkgs guestModule; fixture = hassPairFixture; };

  # The forced-failure half — a real regressed migration (briard_canary) trips
  # the S1 health-gate (entrygate-eval judges HA's real config-entry states) and the
  # {code + data} rollback restores HA to `from` with its recorder history intact.
  entrygateEval = pkgs.callPackage ./entrygate-eval-pkg.nix { };
  hassUpgradeRollback = import ./hass-upgrade-rollback.nix {
    inherit pkgs entrygateEval;
    inherit guestModule;
    fixture = hassPairFixture;
  };

  # Off-site encrypted `.storage` backup — the sacred config sealed client-side
  # (age) to an off-box target and restored byte-faithfully (the cheap DR half).
  briardBackup = pkgs.callPackage ./briard-backup-pkg.nix { };
  hassBackup = import ./hass-backup.nix {
    inherit pkgs briardBackup;
    inherit guestModule;
    fixture = hassFixture;
  };

  # THE SHIPPED ARTIFACT: the bootable disk `install.sh` lays down, running no service. This
  # is what `.#artifacts.guest-disk` publishes and what the agent-driven bring-up tests boot,
  # so the shape a stranger installs is the shape CI exercises.
  guestDisk = import ../guest-image/disk-image.nix { inherit nixpkgs pkgs overlay agentVersion; };

  # Boot-select's disk: shipped + one extra baked generation. `.v1System`'s delta from
  # the running system is a single /etc file, so both grub entries boot the identical kernel and
  # the only thing that can distinguish them is which menu entry was chosen — which is the whole
  # proof. Rides `debug`, so this image is not a nightly cost.
  bootSelectGuestDisk = import ../guest-image/disk-image.nix {
    inherit nixpkgs pkgs overlay agentVersion;
    stageSystemModule = { environment.etc."briard-generation".text = "v1"; };
  };
  driverPkg = pkgs.callPackage ./driver/package.nix { };
  agentPkg = pkgs.callPackage ../agent/package.nix { version = agentVersion; }; # the product agent binary (host + run --guest)

  # The macvtap fd-passing launch wrapper, installed +x (a bare store source file is 0444,
  # which systemd-run can't exec). Shared by every agent/driver-launched integration test that runs
  # on the macvtap substrate (the default).
  netWrap = pkgs.runCommand "briard-net-wrap" { } ''
    install -Dm0755 ${../scripts/briard-net-wrap.sh} $out/bin/briard-net-wrap
  '';
  agentBringup = import ./agent-bringup.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; };
  agentReadopt = import ./agent-readopt.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; }; # restart transparent to guest
  agentRecover = import ./agent-recover.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; }; # host restarts a wedged guest
  agentWatchdog = import ./agent-watchdog.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; }; # V3.32: init restarts a wedged AGENT
  guestRescue = import ./guest-rescue.nix { inherit pkgs guestDisk netWrap; agent = agentPkg; }; # B.10: rebuild the guest from its image, keep the data

  # The host-agent deadman on a lone node must HOLD, never self-outage. Needs a guest with
  # a SHORT T_deadman so the reflex fires in seconds (baked into the guest-agent unit's env).
  deadmanGuestDisk = import ../guest-image/disk-image.nix {
    inherit nixpkgs pkgs overlay agentVersion;
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
      inherit nixpkgs pkgs overlay agentVersion;
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
  # there — so it takes a disk that bakes one, and no service.
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
  maintenanceContract = import ./maintenance-contract.nix { inherit pkgs fixture guestModule; };

  # The isolated, deterministic promote-vs-stop deadlock gate (promote, then
  # time a BARE maintenance pause -- no upgrade / snapshot / btrfs race). Reds at ~90s on the
  # deadlock (drbd-services@r0.target Before=drbd-reactor.service serializes the dying reactor's
  # own promote behind its stop), greens in ms once the drop-in is removed first. Debug tag; NOT
  # nightly. Hermetic same reason as the contract suite above.
  reactorPauseDeadlock = import ./reactor-pause-deadlock.nix { inherit pkgs fixture guestModule; };

  # ⚠️ A HERMETIC NODE DOES NOT REPRODUCE THE SHUTDOWN DEADLOCK, measured while chasing [B.85]:
  # a lib.nix node converged the same way — DRBD Primary, volume mounted, service serving, VIP up,
  # the reactor's `Before=` drop-in present — powered itself off in 1.5s while the SHIPPED guest
  # sat in that deadlock for 90s. The throwaway harness that showed this is not kept: it was green
  # against a broken product, which is the most expensive kind of test to leave lying around.
  # The gate lives on the real artifact instead (guest-rescue asserts the agent route was taken
  # and that no unit held the guest's shutdown), and reactor-pause-deadlock stays the isolated
  # red/green for the deadlock itself.

  # A whole cluster running NOTHING -- the state every node is in until something is installed.
  # Every other test here now boots the same guest, so what this one still owns is the assertion
  # that a node with no services is a WORKING node: volume mounted, chain complete, VIP up, front
  # door answering for itself.
  zeroService = import ./zero-service.nix { inherit pkgs guestModule; };

  # Put a service ONTO the shipped (zero-service) node at runtime — a real digest-pinned
  # TLS pull from a registry the test runs, our real renderer's quadlet output, and the promoter
  # chain it emits driving the pod up behind the front door.
  quadletRenderPkg = pkgs.callPackage ./quadlet-render-pkg.nix { };
  serviceInstall = import ./service-install.nix {
    inherit pkgs;
    inherit guestModule;
    quadletRender = quadletRenderPkg;
  };
  # The failure half — upgrade to a broken manifest, gate trips, {code+data} rollback to the
  # prior service. Reuses the whole hermetic scaffolding above (registry, CA, renderer, DRBD).
  serviceInstallBroken = import ./service-install.nix {
    inherit pkgs;
    inherit guestModule;
    quadletRender = quadletRenderPkg;
    broken = true;
  };

  tlsServing = import ./tls-serving.nix { inherit pkgs guestModule; };

  # The relocatable qemu bundle (runs on a host with no /nix/store) + its proof.
  qemuBundle = import ./qemu-bundle.nix { inherit pkgs; };
  qemuBundleWindows = import ./qemu-bundle-windows.nix { inherit pkgs; };

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
  # -> green, proven by an OFF-BOX LAN client reaching the service at the VIP, using the bundled
  # qemu (no distro qemu) + a nested guest; plus the cattle/pet reinstall. It carries that
  # mode-independent chain here from the bridge test, so the default substrate carries it.
  installMacvtap = import ./install-macvtap.nix {
    # selfupdateStub serves the signed release channel over HTTP so the install runs the REAL
    # network path (fetch + verify + expand) rather than the BRIARD_ARTIFACTS escape hatch.
    inherit pkgs guestDisk selfupdateStub;
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
      drbd-promote = import ./drbd-promote.nix { inherit pkgs fixture guestModule; };
      drbd-failover = import ./drbd-failover.nix { inherit pkgs fixture guestModule; };
      reactor-evict = import ./reactor-evict.nix { inherit pkgs fixture guestModule; }; # V3.17c2-iv-6: a PLANNED handover
      drbd-fence = import ./drbd-fence.nix { inherit pkgs fixture guestModule; };
      drbd-witness = import ./drbd-witness.nix { inherit pkgs fixture guestModule; };
      drbd-witness-loss = import ./drbd-witness-loss.nix { inherit pkgs fixture guestModule; };
      single-node-promoter = import ./single-node-promoter.nix { inherit pkgs fixture guestModule; }; # peer-less mode
      runtime-join = import ./runtime-join.nix { inherit pkgs fixture guestModule; }; # grow single-node -> 3-node mesh at runtime
      drbd-loopback-path = import ./drbd-loopback-path.nix { inherit pkgs guestModule; }; # loopback-path gating experiment
      # The maintenance-mode contract (pause → poke → resume). It sat in `integration`
      # while it drove the agent's verbs over a nested guest's channel; V3.17e4 made it
      # hermetic, so it belongs with the other one-node promoter mechanisms. Nightly either way.
      maintenance-contract = maintenanceContract;
      # `witness-proxy` + `witness-tap-link` join this tag from outputs-private.nix —
      # both build the cloud witness. The three tests above mention the cloud only in prose.
    };

    # Version-change + TLS-serving mechanisms. `service-install` is the version change now
    # ([V3b.3](e2)): the three tests that used to live here drove the baked slot's image re-pin,
    # which no longer exists. Their subjects did not go with them — `service-install`'s upgrade
    # half is the in-place version change, its broken half is the health-gated rollback, and its
    # reboot half is converge-at-promotion (a node that renders from the volume, having been told
    # nothing).
    upgrade = {
      tls-serving = tlsServing;
    };

    # Real Home Assistant as an INSTALLED SERVICE (2.4 GB image boot).
    ha = {
      hass-payload = hassPayload;
      hass-failover = hassFailover;
      hass-upgrade = hassUpgrade; # real recorder schema migration through the upgrade
      hass-upgrade-rollback = hassUpgradeRollback; # real regression trips the gate → {code+data} rollback
      hass-backup = hassBackup; # off-site encrypted .storage backup + restore
    };

    # Agent-in-the-loop: the agent drives a real guest under *nested*
    # QEMU over the virtio-serial channel. Needs nested KVM.
    integration = {
      agent-bringup = agentBringup;
      agent-readopt = agentReadopt; # an agent restart re-adopts the running guest
      agent-deadman = agentDeadman; # a lone node holds (never self-outages) when its agent dies
      # The mirror of agent-deadman: there the host goes silent and the guest reboots itself;
      # here the GUEST goes silent and the host restarts its VM. Minutes long by construction --
      # it measures the wait, because a ladder that acts too soon is worse than one that waits.
      agent-recover = agentRecover;
      # The third of the trio, and the rung the other two cannot reach: there the GUEST is what
      # fails, here the AGENT is -- alive, so nothing in the product can see it, which is why
      # this one is init's job rather than ours.
      agent-watchdog = agentWatchdog;
      guest-rescue = guestRescue;
      os-stage = osStage; # a closure the guest does NOT have, fetched over a cache
      # The boot selector. Promoted back from `debug` once the premise that demoted it turned
      # out to be false: an L2 guest under nesting DOES complete a clean shutdown, by BOTH
      # triggers. This sequence needs the guest stopped cleanly between launches, and it now is
      # -- so the power cut that hung the next boot about half the time no longer happens, and
      # the test ASSERTS the clean stop rather than tolerating its absence.
      boot-select = bootSelect;
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
      reactor-pause-deadlock = reactorPauseDeadlock;
      # Spike: can a service be installed at runtime as a podman pod (quadlet), and does
      # that pod work as a promoter chain member? Answers a design question; promoted or deleted
      # once the design settles.
      quadlet-spike = import ./quadlet-spike.nix { inherit pkgs guestModule; };
      # — **THIS TEST FAILS TODAY, AND THAT IS ITS JOB.** Act 1 (crash the primary, the
      # survivor promotes) passes and is the control; act 2 (the survivor then restarts while its
      # peer is still absent) does not, because a diskless witness can KEEP quorum but never GRANT
      # it. It sits in `debug` rather than `drbd` precisely because it is red: the nightly must
      # stay a statement about what works. Run it by hand to see the gap in VANILLA DRBD — no
      # agent, no nesting, config baked into the image — and when is decided, this is its
      # acceptance test.
      drbd-survivor-restart = import ./drbd-survivor-restart.nix { inherit pkgs fixture guestModule; };
      # — **ALSO RED BY DESIGN** ([B.100]), and the mirror image of the one above. That is the GAIN
      # side of the tiebreaker guard: a restarted survivor has no runtime `quorum[NOW]`, so it can
      # never gain quorum from a diskless node. This is the KEEP side: a broken link BETWEEN the
      # two anchors leaves `quorum[NOW]` true on BOTH, so both keep quorum, both may write, and the
      # household's data forks into a permanent StandAlone. It is the one fault shape
      # `block()`/`crash()` cannot express, because nothing leaves the cluster — only the edge
      # between the two data nodes fails, and the witness stays reachable from both.
      drbd-link-split = import ./drbd-link-split.nix { inherit pkgs guestModule; };
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
    # The Windows arm of the same thing, repackaged from pinned upstream bytes because building it
    # ourselves is blocked upstream (qemu-bundle-windows.nix says where and why). Its proof is the
    # tier-4 Windows rig, not CI -- a Linux runner cannot run it.
    qemu-bundle-windows = qemuBundleWindows.bundle;
    net-wrap = netWrap; # macvtap launch wrapper; the fleet runs on it too
  };
}
