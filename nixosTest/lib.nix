# Shared scaffolding for the nixosTests that boot guest nodes: each test supplies its topology (a
# node list) and testScript; this builds the resource config and the per-node NixOS module.
#
# A node is { name; id; diskless ? false; }. Node-id N lives at 10.0.0.<N+1>, matching the
# framework's 10.0.0.<nodeNumber> on eth1 (the private DRBD subnet; the VIP stays on 192.168.1.100).
{ pkgs, guestModule }:

let
  inherit (pkgs.lib) concatMapStringsSep mkIf;
  inherit (pkgs) lib;

  # The real renderer, as a helper binary: the package app-install.nix is handed too, so the
  # sidecars a fixture is installed from come from the same code the agent runs.
  quadletRender = pkgs.callPackage ./quadlet-render-pkg.nix { };

  # The promoter's ordered units: the same on every node, whatever is installed. `briard-apps`
  # converges the node to the manifests on the volume once it is mounted, so the apps are not
  # members. The reverse proxy, which also publishes the mDNS names, is: on a node with no `.casa`
  # domain those names are the only way to reach an app and it is the only thing answering them,
  # so a node that cannot publish or serve should hand the resource on rather than look healthy
  # while reaching nobody. A copy of shared/chain.Members (host.promoterUnits in production); Nix
  # cannot import Go, so internal/arch TestRigPromoterSnippetMatchesTheChain holds the two lists
  # equal, member for member and in order.
  promoterSnippet =
    let
      units = [
        "briard-primary-storage.service"
        "briard-vip.service"
        "briard-apps.service"
        "briard-reverse-proxy.service"
        "briard-dashboard.service"
      ];
    in
    ''
      [[promoter]]
      [promoter.resources.r0]
      adjust-resource-on-start = false
      target-as = "Wants"
      start = [ ${concatMapStringsSep ", " (u: ''"${u}"'') units} ]
    '';

  # R0 over the node list, with the production safety config. Per-node volume form so a witness
  # can be `disk none`; new-current-uuid then needs the volume id (r0/0), which
  # briard-node-storage passes.
  #
  # A diskful node's backing is `/dev/mapper/briardvol-data`, its metadata on `briardvol-metadata`,
  # in the product and every rig: the two LVs briard-node-storage builds from the host's spec,
  # restated because a `.res` is text and cannot import Go's `drbd.DataDevice` / `drbd.MetaDevice`.
  mkResource =
    nodes:
    let
      onBlock = n: ''
        on ${n.name} {
          node-id ${toString n.id};
          address 10.0.0.${toString (n.id + 1)}:7789;
          volume 0 {
            device /dev/drbd0;
            ${
              if n.diskless or false then
                "disk none;"
              else
                "disk /dev/mapper/briardvol-data; meta-disk /dev/mapper/briardvol-metadata;"
            }
          }
        }'';
    in
    ''
      resource r0 {
        net { protocol C; }
        options {
          auto-promote                  no;
          quorum                        majority;
          on-no-quorum                  io-error;
          on-suspended-primary-outdated force-secondary;
        }
        ${concatMapStringsSep "\n  " onBlock nodes}
        connection-mesh { hosts ${concatMapStringsSep " " (n: n.name) nodes}; }
      }
    '';

  # A test-only unit that prewarms catalogued fixtures: assert each image is resident in local
  # podman storage and lay down the manifest the harness will install from. That is exactly the
  # part a test legitimately stands in for, the bytes a host agent would have fetched and warmed
  # before anything promoted; rendering, unit files and the static promoter chain are the
  # product's (`briard-apps` renders from the volume at promotion). Nothing is baked into the
  # guest image at build time (`oci-containers`), a mechanism no user has, so tests riding this
  # prove the path everyone runs.
  #
  # The host agent's orchestration cannot run here (the node is the guest, with no host on the
  # other end), so `install_fixture` below does the Primary-only half by hand; fetch, verify and
  # health-gate are unit-tested in agent/host/app_test.go. Everything under it is real: the
  # product's renderer and binary, reading the product's volume layout.
  #
  # No registry, measured rather than assumed (fixture-app.nix has the three facts): each fixture
  # ships the podman store its archive was loaded into, and mkNode attaches it as a read-only
  # additional image store, so no node runs a `podman load` at boot. One unit stages N fixtures,
  # because the coordination is plural and a harness that stages only one can only prove the
  # singular case. Each lands under /run/briard/fixtures/<app name>; /run/briard/fixture is a
  # symlink to the first, the path every single-app test and helper default reads.
  fixturesInstall = fixtures: config: {
    description = "Prewarm ${lib.concatMapStringsSep " + " (f: f.name) fixtures} onto this node (test harness)";
    wantedBy = [ "multi-user.target" ];
    # Before the promoter can be started by hand: a converge that runs before the image is
    # resident would have to pull it, and there is no registry on a hermetic node.
    before = [ "drbd-reactor.service" ];
    path = [
      config.virtualisation.podman.package # the guest's podman, never a second copy
      quadletRender
      pkgs.coreutils
    ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
    };
    script = ''
      set -eu
      rm -rf /run/briard/fixtures /run/briard/fixture
      mkdir -p /run/briard/fixtures
      # The pod pool, which on a real node arrives over net.configure from the host that drew it
      # (agent/subnet); with no host here it is seeded, as the VIP device and address are. Without
      # it a private app cannot be rendered, and converge says so rather than quietly using the
      # guest namespace. A fixed octet, not a draw: the draw exists to miss the addressing of a
      # household, and a hermetic rig has none to miss.
      printf '10.12.99\n' >/run/briard/pod.subnet
    ''
    + lib.concatMapStrings (fixture: ''
      # The manifest's digest must resolve locally, which is what Pull=never requires. Measured:
      # podman records a RepoDigest for a loaded archive, and resolves it from an additional store.
      podman image exists "$(cat ${fixture}/ref)"

      # The manifest, plus the renderer sidecars (`dataroot`, `subdirs`, `units`, `identity`;
      # install_fixture reads the first two, fixture_units the third), so a test never restates
      # the layout the product decides. No unit file is copied: converge writes those itself.
      mkdir -p /run/briard/fixtures/${fixture.appName}
      quadlet-render ${fixture}/manifest.json /run/briard/fixtures/${fixture.appName}
      cp ${fixture}/manifest.json /run/briard/fixtures/${fixture.appName}/manifest.json
      # The image ref the manifest pins, so a test can assert warmth by digest rather than by a
      # tag (a tag can match an image the manifest does not name).
      cp ${fixture}/ref /run/briard/fixtures/${fixture.appName}/ref

      # Every other published version is warmed too, only its manifest kept aside: a test that
      # upgrades installs a different manifest under the same name, which is what a version
      # change is. The product also warms up front (an upgrade must not pull on the promotion
      # path), so the ordering a test exercises is the real one.
      ${lib.concatMapStrings (label: ''
        podman image exists "$(cat ${fixture}/variants/${label}/ref)"
        mkdir -p /run/briard/fixtures/${fixture.appName}/variants/${label}
        cp ${fixture}/variants/${label}/manifest.json /run/briard/fixtures/${fixture.appName}/variants/${label}/manifest.json
        cp ${fixture}/variants/${label}/ref /run/briard/fixtures/${fixture.appName}/variants/${label}/ref
      '') (fixture.variantLabels or [ ])}
    '') fixtures
    + ''
      ln -sfn /run/briard/fixtures/${(lib.head fixtures).appName} /run/briard/fixture
    '';
  };

  # testScript helpers for a node built with `fixture`. Prepend to a test's script.
  #
  # install_fixture stands in for the one half of the install path a hermetic harness cannot run,
  # the Primary-only orchestration of the host agent: it writes the manifest to the replicated
  # volume (what `app.provision` records as the app's identity), creates its storage, then hands
  # over to the product — `briard-guest-agent --converge`, the same code drbd-reactor runs at every
  # promotion. It must run after a node has promoted, since only a Primary has the volume mounted:
  # the exact constraint the product's install path lives under, and the reason converge exists.
  #
  # One subvolume per tier the app mounts, plain subdirectories in it, never nested: `btrfs
  # subvolume delete` refuses on a subvolume containing them, so data.restore would break. Podman
  # does not create a missing bind-mount source (measured — the container dies with `statfs
  # …/app: no such file or directory`), so the provision step is required, not defensive. The
  # data root and the directories, in full, come from the renderer sidecars, never restated here.
  fixtureHelpers = ''
    def fixture_dir(app=None):
        """Where a staged fixture lives. `app` names one when the node carries several;
        the default is the symlink to the first, which is what a single-app node
        has always read."""
        return "/run/briard/fixture" if app is None else f"/run/briard/fixtures/{app}"

    def install_fixture(m, variant=None, app=None):
        """Install the fixture -- or, with `variant`, a DIFFERENT VERSION of it under the same
        name, which is what an upgrade is. Both go through the product's own converge."""
        d = fixture_dir(app)
        dataroot = m.succeed(f"cat {d}/dataroot").strip()
        m.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || (mkdir -p $(dirname {dataroot}) && btrfs subvolume create {dataroot})")
        for sub in m.succeed(f"cat {d}/subdirs").split():
            area = sub.rsplit("/", 1)[0]
            m.succeed(f"btrfs subvolume show {area} >/dev/null 2>&1 || (mkdir -p $(dirname {area}) && btrfs subvolume create {area})")
            m.succeed(f"mkdir -p {sub}")
        # The app name is the last element of its data root, taken from the renderer.
        name = dataroot.rstrip("/").split("/")[-1]
        src = f"{d}/manifest.json" if variant is None \
            else f"{d}/variants/{variant}/manifest.json"
        m.succeed("mkdir -p /var/lib/briard/.apps")
        m.succeed(f"cp {src} /var/lib/briard/.apps/{name}.json")
        m.succeed("sync")
        # The product converge, by the entry point briard-apps.service uses. On a version change
        # it also bounces the container: converge restarts what it has not started with exactly
        # these bytes.
        m.succeed("briard-guest-agent --converge")
        return dataroot

    def fixture_units(m, app=None):
        """The app's units the RENDERER produced -- never a list restated here. They are NOT
        promoter chain members: briard-apps starts them, which is what keeps a
        crashed container from demoting the node."""
        units = m.succeed(f"cat {fixture_dir(app)}/units").split()
        assert units, f"the renderer produced no app units: {units}"
        return units

    def name_the_flock(m, name="brave-elf"):
        """Give the node a flock name the way the AGENT does -- net.mdnsname writes exactly this
        file, and converge composes the per-app names from it. Without it a node has
        no name to route on: the app is installed and reachable on its port, but nothing
        answers for it at the reverse proxy, which is the honest state of a node whose flock has
        never been named."""
        m.succeed("mkdir -p /run/briard")
        m.succeed(f"printf 'FLOCK_NAME={name}\\n' >/run/briard/mdns.env")

    def routed_host(m, app):
        """The name the reverse proxy routes `app` on, read from the table the PRODUCT wrote --
        never a name restated here, which would assert our own arithmetic rather than the node's."""
        import json
        table = json.loads(m.succeed("cat /run/briard/routes.json"))
        for s in table["apps"]:
            if s["name"] == app:
                assert s.get("hosts"), f"{app} is routed but unnamed: {s}"
                return s["hosts"][0]
        raise AssertionError(f"{app} is not in the node's routing table: {table}")
  '';

  # A test node: the unit image + a backing disk (unless diskless) + its private DRBD address +
  # the resource. `promoter` adds the promoter snippet; tests provision DRBD, then start
  # drbd-reactor by hand, as the product does (the agent arms the promoter at bring-up), so nothing
  # here forces it off. Divergence between the disk-image guest and these nodes is where the
  # guest-parity bugs lived.
  mkNode =
    {
      resource,
      diskless ? false,
      promoter ? true,
      # false = a node that runs no DRBD: the spec names the data LV as the device, node-storage
      # writes no .res and attaches nothing, and `systemctl start briard-chain.target` starts the
      # chain instead of drbd-reactor. The default keeps every existing rig on DRBD, even a
      # one-node flock the product never writes (its StorageSpec runs DRBD only with two diskful
      # members), because the DRBD mechanism rigs are about the mechanism.
      replicated ? true,
      # The block device the data tier is built on. The framework's empty disk by default; a rig
      # that injects media faults names a device-mapper target it stacked on top of it.
      tierDevice ? "/dev/vdb",
      # A catalogued fixture (nixosTest/fixture-app.nix) prewarmed onto the node at boot; the
      # test installs it onto the volume with install_fixture once something has promoted. The
      # only way a test node gets a workload (the image has no build-time app slot), and the way
      # a shipped node gets one: a container put on a node by a mechanism no user has proves a
      # path nobody runs.
      fixture ? null,
      # Several catalogued apps on one node (`fixture` is the one-app spelling), each staged
      # under its own name, so a test can prove what the plural coordination does — that
      # installing or upgrading one app leaves the others alone.
      fixtures ? [ ],
      # The household router the host would push as VIP_ROUTER, which replies to clients on other
      # subnets leave through. null = none, as for an agent with no LAN: these rigs have no router,
      # so only a rig that stands one up names one.
      vipRouter ? null,
    }:
    let
      allFixtures = lib.optional (fixture != null) fixture ++ fixtures;
      # The harness's VIP, stated once (why the harness states one at all: "The framework
      # declares its own VIP" below). It reaches the node only as the file the agent would have
      # written (vip.env, the tmpfiles rule below): briard-vip reads it as its EnvironmentFile and
      # the reverse proxy for the address its mDNS answers carry. One value, so they cannot
      # disagree.
      vipDev = "eth1";
      vipAddr = "192.168.1.100/24";
      vipEnvFile = pkgs.writeText "vip.env" ("VIP_DEV=${vipDev}\nVIP_ADDR=${vipAddr}\n"
        + lib.optionalString (vipRouter != null) "VIP_ROUTER=${vipRouter}\n");

      # ── The harness as the host's stand-in, for storage ───────────────────────────────────
      # Storage policy is the host's, pushed at bring-up as /run/briard/node-storage.json and
      # carried out by briard-node-storage.service. With no host, the harness writes the spec and
      # starts the unit -- never a hand-rolled `modprobe drbd; create-md --force; drbd@r0.target;
      # new-current-uuid; touch data.format`, a harness re-implementing the product: that hides
      # bugs like the encrypted-blank-device one from every hermetic test (a `--force` cannot meet
      # the probe the product runs).
      #
      # freshInit is false here and set per node by the script below: every machine in a rig
      # shares one module, and a spec that seeded on all of them would have three nodes declare
      # themselves UpToDate.
      storageSpecFile = pkgs.writeText "node-storage.json" (
        builtins.toJSON {
          tiers = lib.optional (!diskless) {
            name = "data";
            device = tierDevice; # the framework's emptyDiskImages disk by default, as the product's is
            vg = "briardvol";
            lv = "data";
            metaLV = "metadata";
            mode = "auto";
          };
          resource = {
            name = "r0";
            device = if replicated then "/dev/drbd0" else "/dev/mapper/briardvol-data";
            inherit replicated;
            maxPeers = 4; # the product constant (agent/drbd MaxPeers): baked into the metadata
            config = resource;
            inherit diskless;
            freshInit = false;
          };
        }
      );
      # A command rather than a testScript helper, so a rig needs no preamble to reach it: every
      # test already runs shell on its nodes, and some do not import fixtureHelpers.
      #   briard-test-storage            bring this node up as a joiner (never seeds)
      #   briard-test-storage --seed     ...as the seed of a new flock
      #   briard-test-storage --mode off ...formatted in the clear, whatever the CPU can do
      #   briard-test-storage --alone    ...running no DRBD (the spec names the data LV), or
      #                       --flock    ...replicated -- either overrides mkNode's `replicated`,
      #                                  which is how a rig drives a transition: the host
      #                                  would record the mesh and reboot the guest; the harness
      #                                  rewrites the spec and re-runs node-storage
      #   briard-test-storage --alone --disable  ...with the one-shot convert-disable intent an
      #                                  unpair asserts, the only thing that lets a node found
      #                                  alone with DRBD metadata wipe it rather than refuse
      testStorage = pkgs.writeShellScriptBin "briard-test-storage" ''
        set -eu
        seed=false
        mode=auto
        replicated=${lib.boolToString replicated}
        convert=""
        while [ $# -gt 0 ]; do
          case "$1" in
            --seed) seed=true ;;
            --mode) shift; mode="$1" ;;
            --alone) replicated=false ;;
            --flock) replicated=true ;;
            --disable) convert=disable ;;
            *) echo "briard-test-storage: unknown argument $1" >&2; exit 2 ;;
          esac
          shift
        done
        # The unit before the start: briard-node-storage.service is written by the agent stand-in,
        # ordered before multi-user.target, but the backdoor shell of the framework is not, so a
        # rig running this the moment it can would race the render. Starting the oneshot is the
        # barrier: it blocks until the render finishes, then is a no-op (RemainAfterExit).
        ${pkgs.systemd}/bin/systemctl start briard-test-write-units.service
        mkdir -p /run/briard
        rm -f /run/briard/node-storage.json
        ${pkgs.jq}/bin/jq --argjson s "$seed" --arg m "$mode" --argjson r "$replicated" --arg c "$convert" \
          '.resource.freshInit = $s | (.tiers[]?).mode = $m
           | .resource.replicated = $r
           | .resource.device = (if $r then "/dev/drbd0" else "/dev/mapper/briardvol-data" end)
           | if $c == "" then del(.resource.convert) else .resource.convert = $c end' \
          ${storageSpecFile} >/run/briard/node-storage.json
        exec ${pkgs.systemd}/bin/systemctl start briard-node-storage.service
      '';
    in
    { config, ... }:
    {
      # How long a node refuses to promote after one of its chain members gave up. A field node
      # runs the agent's own 300s (agent/guestagent/units.go defaultHoldSecs); this option exists
      # only because the contract rigs drive the whole hold lifecycle and cannot wait five
      # minutes. Declared here, not in the product module, because the unit is the agent's and an
      # option the image does not read would be a lie. It sets the renderer's environment, below.
      imports = [
        guestModule
        {
          options.briard.promotionHoldSecs = lib.mkOption {
            type = lib.types.ints.positive;
            default = 300;
            description = "Seconds a node refuses promotion after a promoter chain member exhausts its start limit.";
          };
        }
      ];
      # No host pushes the guest's binaries into a nixosTest machine: the
      # image bakes only the firmware, so link the pushed set in as if a host had dressed it.
      briard.pivot.preDressed = {
        briard-reverse-proxy = "${pkgs.reverse-proxy}/bin/reverse-proxy";
        briard-dashboard = "${pkgs.dashboard}/bin/dashboard";
        briard-guest-agent = "${config.briard.agentPackage}/bin/briard-guest-agent";
      };
      # ── The harness as the host's stand-in, for units ─────────────────────────────────────
      # The image does not define the units the agent owns; the pushed agent writes them into
      # /run/systemd/system first thing in `run --guest`. These machines run no `run --guest` (no
      # host on the other end of a control channel), so without this every rig that starts one
      # would fail on a unit that does not exist.
      #
      # It runs the product renderer rather than declaring the units here, the rule the storage
      # stand-in above follows: a harness that re-implements the thing under test cannot notice
      # the product changing it. The harness supplies only the trigger the host would have.
      # After the tmpfiles that link preDressed into the binDir of the pivot, since the binary
      # this unit runs is one of them.
      systemd.services.briard-test-write-units = {
        description = "Harness stand-in for the host's dress: render the agent's own units";
        wantedBy = [ "multi-user.target" ];
        after = [ "systemd-tmpfiles-setup.service" ];
        before = [ "multi-user.target" ];
        path = [ pkgs.systemd ]; # systemctl daemon-reload
        environment.BRIARD_PROMOTION_HOLD_SECS = toString config.briard.promotionHoldSecs;
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          ExecStart = "${config.briard.pivot.binDir}/briard-guest-agent --write-units";
        };
      };
      # ── The harness as the host's stand-in, for the inbound channel ───────────────────────
      # As for the unit render above. In the product the inbound listener runs inside the
      # long-running agent (the logic belongs where everything it reasons about lives), and these
      # machines run no `run --guest`, so nothing would bind the socket: a container's notify
      # would meet ECONNREFUSED, start anyway (`|| true`), and every rig would pass while proving
      # nothing about the channel. It runs the product listener rather than faking a socket, for
      # the same reason as the render.
      systemd.services.briard-test-inbound = {
        description = "Harness stand-in for the agent's own inbound listener";
        wantedBy = [ "multi-user.target" ];
        after = [ "briard-test-write-units.service" ];
        serviceConfig = {
          ExecStart = "${config.briard.pivot.binDir}/briard-guest-agent run --inbound-listen";
          # The listener owns the socket for the machine's whole life and a restart rebinds it.
          # Nothing here is a promoter chain member, so a failure costs the channel, never the node.
          Restart = "always";
          RestartSec = 1;
        };
      };
      virtualisation.emptyDiskImages = mkIf (!diskless) [ 256 ];
      networking.interfaces.eth1.ipv4.addresses = [
        {
          address = "10.0.0.${toString config.virtualisation.test.nodeNumber}";
          prefixLength = 24;
        }
      ];
      # Test-only: curl probes the VIP; with a fixture, install_fixture runs btrfs and converge
      # from a test shell (in the product they run from the PATH of the guest agent unit and of
      # briard-apps). lvm2/cryptsetup are the seam's tooling for a rig that inspects the stack it
      # built (`dmsetup deps`, `cryptsetup isLuks`, `pvs`) from outside any unit; the product
      # reaches them through the image's tool profile, briard-node-storage's whole PATH. Free in
      # size: both are already in this guest's closure.
      environment.systemPackages = [ pkgs.curl pkgs.lvm2.bin pkgs.cryptsetup testStorage ]
        ++ lib.optionals (allFixtures != [ ]) [ pkgs.btrfs-progs config.briard.agentPackage ];
      # The framework declares its own VIP. The guest image bakes none (unset means DHCP, and a
      # nixosTest's synthetic L2 has no DHCP server), so the harness states the address it will
      # curl rather than inheriting one: a baked default every test agrees with hides a defect,
      # because nothing disagrees with it.
      #
      # eth1, not eth2: these agent-less guests have one NIC for it, which also carries their
      # private DRBD address. That is why vip.env here carries no SYSTEM_DEV: without one,
      # briard-vip-down takes the NIC down only when the address came from DHCP, which this static
      # one never does.
      #
      # ⚠️ Stated once, in the tmpfiles symlink below: briard-vip is written by the agent at
      # runtime, so there is no unit here to put an override on, and the product requires the file
      # as an EnvironmentFile anyway.
      # /etc/drbd.conf is the one file drbdadm looks for at a path we do not choose, so the harness
      # states it here (the shipped image states the identical glob in disk-image.nix, which these
      # nodes do not import).
      environment.etc."drbd.conf".text = ''include "/run/briard/drbd.d/*.res";'';
      # The agent-written files the harness states (vip.env, the reactor snippet) are on tmpfs
      # under /run/briard in the product, so not environment.etc: tmpfiles symlinks put store files
      # at the exact paths the agent would write, keeping the harness's own configuration on the
      # product's layout (drbd-reactor, `drbd-reactorctl evict` included, reads a symlinked snippet
      # identically). Matching paths are the point: the divergence between these nodes and the
      # disk-image guest is where the guest-parity bugs lived.
      systemd.tmpfiles.rules = [
        # The VIP configuration where the agent would write it. briard-vip requires it as its
        # EnvironmentFile (without it the unit fails); the reverse proxy reads it for the address
        # its mDNS answers carry.
        "d /run/briard 0755 root root -"
        "L+ /run/briard/vip.env - - - - ${vipEnvFile}"
        # ⚠️ No `.res` here: briard-node-storage writes it, as in the product, and writing through
        # a tmpfiles symlink to a store file fails with `read-only file system`. Only the
        # directory is declared; its contents are the unit's, from the spec above.
        "d /run/briard/drbd.d 0755 root root -"
      ]
      # The snippet is static: the chain names briard-apps, and what the node runs comes from the
      # volume at promotion, so no test's workload choice can change it.
      ++ lib.optionals promoter [
        "d /run/briard/drbd-reactor.d 0755 root root -"
        "L+ /run/briard/drbd-reactor.d/briard.toml - - - - ${pkgs.writeText "briard.toml" promoterSnippet}"
      ];
      systemd.services.briard-test-fixture-install =
        mkIf (allFixtures != [ ]) (fixturesInstall allFixtures config);
      # Each fixture's prebuilt podman store (fixture-app.nix), attached as a read-only disk and
      # mounted as an additional image store: images resident from first boot, no load on the
      # node. mkAfter keeps these behind the framework's drives so the data tier stays /dev/vdb;
      # the mount names its disk by serial for the same reason.
      virtualisation.qemu.drives = lib.mkAfter (
        lib.imap0 (i: f: {
          name = "fixture${toString i}";
          file = "${f}/store.erofs";
          driveExtraOpts = {
            readonly = "on";
            format = "raw";
          };
          deviceExtraOpts.serial = "fixture${toString i}";
        }) allFixtures
      );
      virtualisation.fileSystems = lib.listToAttrs (
        lib.imap0 (
          i: _:
          lib.nameValuePair "/var/lib/containers/fixtures/${toString i}" {
            device = "/dev/disk/by-id/virtio-fixture${toString i}";
            fsType = "erofs";
            options = [ "ro" ];
          }
        ) allFixtures
      );
      virtualisation.containers.storage.settings.storage.options.additionalimagestores = lib.imap0 (
        i: _: "/var/lib/containers/fixtures/${toString i}"
      ) allFixtures;
    };

  # The advert of HA, as HA's own zeroconf resolves it: one JSON object, or exit 1 if none resolved.
  haAdvertProbe = pkgs.writeText "ha-advert-probe.py" ''
    """Browse _home-assistant._tcp with HA's own zeroconf; print the record as JSON."""
    import json
    import sys
    import time

    from zeroconf import ServiceBrowser, Zeroconf

    found = {}


    class Listener:
        def add_service(self, zc, type_, name):
            info = zc.get_service_info(type_, name, timeout=5000)
            if info:
                found[name] = info

        def update_service(self, *a):
            pass

        def remove_service(self, *a):
            pass


    zc = Zeroconf()
    ServiceBrowser(zc, "_home-assistant._tcp.local.", Listener())
    for _ in range(45):
        if found:
            break
        time.sleep(1)
    zc.close()
    if not found:
        sys.exit(1)
    info = next(iter(found.values()))
    print(json.dumps({
        "addresses": sorted(info.parsed_addresses()),
        "port": info.port,
        "txt": {k.decode(): (v.decode() if v is not None else None) for k, v in (info.properties or {}).items()},
    }))
  '';
in
{
  inherit
    mkResource
    mkNode
    fixturesInstall
    fixtureHelpers
    # Two rigs ask what HA advertises: one where HA is installed after promotion, one where it
    # starts inside a promotion.
    haAdvertProbe
    # Exported because app-install writes its own reactor snippet from the testScript and takes
    # the chain from here: a rig whose list is short never starts the missing members. One
    # definition: mkNode bakes it, app-install installs it, internal/arch holds it to shared/chain.
    promoterSnippet
    ;
}
