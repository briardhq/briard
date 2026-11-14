{
  description = "Briard — managed, highly-available home automation";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  # The app catalog, its own repository: the version files and the list a node reads, and the
  # image hashes the test fixtures pull by. The tests read it from here (outputs.nix, `.#catalog-test`);
  # a node never does -- it fetches the published copy from get.briard.io. Moving this pin is
  # housekeeping: whoever tests what is about to ship overrides it with the catalog's `main`.
  inputs.catalog = {
    url = "github:briardhq/catalog";
    flake = false;
  };

  outputs = { self, nixpkgs, catalog }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      # X86_64 pkgs with our overlay (overlays.default below), shared by the tests.
      pkgsX = import nixpkgs {
        system = "x86_64-linux";
        overlays = [ self.overlays.default ];
      };
      # The release id, DERIVED FROM THE TREE. Shape `<epoch>.<commit-hour>.<short-rev>`,
      # e.g. `v3.26080614.92d4eee`: the epoch carries the only high-level semantics we have (this
      # is an epoch scheme, not semver); the commit hour is YYMMDDHH in UTC, quasi-incremental and
      # readable at a glance (two commits in one hour share it, which is fine); the short rev makes
      # it unique AND survives a history rewrite, which a commit *count* would not.
      #
      # YYMMDDHH, NOT YYYYMMDD. Installed nodes order releases on this field alone, as a number, so
      # with a date alone two releases in one day would be one release to them, and a same-day fix
      # would wait for midnight. Dropping the century keeps the field eight digits, which every
      # shipped agent parses, and every YYMMDDHH id (26xxxxxx) is above every YYYYMMDD one
      # (20xxxxxx), so an agent stamped with a date-only id takes it as newer. It wraps in 2100.
      #
      # Deliberately NOT a build counter: the same commit must yield the same version, hence the
      # same store path, or a released artifact could never be re-derived from its tag — which is
      # exactly the release pipeline's acceptance test.
      #
      # A DIRTY TREE HAS NO REV, so it stamps `v3.dirty`, which the publish script refuses: a build
      # nobody can reproduce must not be publishable. Dev builds keep working — they are just
      # labelled as what they are, which is what the startup banner then reports.
      agentVersion =
        if self ? shortRev
        then "v3.${builtins.substring 2 8 self.lastModifiedDate}.${self.shortRev}"
        else "v3.dirty";
      # THE GUEST IMAGE'S VERSION IS A FUNCTION OF ITS INPUTS, NOT OF THE COMMIT, AND
      # ITS INPUTS ARE THE PUSH PROTOCOL. `publish-release.sh stage` asks whether the
      # live channel already serves an image with THESE inputs and re-stages the 400 MB guest
      # chain only when they changed; what makes that condition honest is that the image bakes
      # ONE binary, briard-guest-firmware, whose graph is the protocol and nothing else. An edit
      # to the pushed agent -- DRBD, apps, converge, telemetry, the deadman -- does not reach
      # this list, because it does not reach the image.
      #
      # The inputs are the paths that reach the image: the image recipe, the packages built into
      # it, the Go packages the FIRMWARE links (listed by name; internal/arch asserts the list
      # against `go list -deps ./agent/cmd/briard-guest-firmware`, so a new import cannot silently
      # fall outside the hash), the module files and the nixpkgs pin. A toolchain bump still
      # counts, correctly: go.mod/go.sum/vendor-hash and nixpkgs can change the bytes the
      # firmware runs. `builtins.path` copies each into the store, and a store path's name IS its
      # content hash -- test files excluded, since they never reach the image.
      guestInputPackages = [
        "agent/cmd/briard-guest-firmware" "agent/guestfirmware" "shared/sdnotify"
      ];
      guestInputDirs = [ "guest-image" ] ++ guestInputPackages; # every other briard binary is pushed, not baked
      noTests = path: type: !(lib.hasSuffix "_test.go" (baseNameOf path)) && baseNameOf path != "testdata";
      inputPath = p: toString (builtins.path { path = ./. + "/${p}"; name = builtins.replaceStrings [ "/" ] [ "-" ] p; filter = noTests; });
      guestInputs = builtins.hashString "sha256" (lib.concatStringsSep "\n" (
        map inputPath guestInputDirs
        ++ map (f: toString (builtins.path { path = ./. + "/${f}"; name = baseNameOf f; })) [ "go.mod" "go.sum" "vendor-hash.nix" "agent/package.nix" ]
        ++ [ (nixpkgs.rev or "dirty") ]
      ));
      guestVersion = "guest-build." + builtins.substring 0 12 guestInputs;
      tests = import ./nixosTest/outputs.nix {
        inherit nixpkgs agentVersion guestVersion guestInputs catalog;
        pkgs = pkgsX;
        overlay = self.overlays.default;
      };
      # The cloud-coupled tests + the cloud operator apps, layered in ONLY when the private
      # half's outputs file (the path below) is present. This tree never carries it: the private
      # sibling imports it from its own flake, with this flake as an input, so here `private` is
      # always the empty set and the private tests and apps do not exist rather than break.
      private =
        if builtins.pathExists ./nixosTest/outputs-private.nix
        then import ./nixosTest/outputs-private.nix { pkgs = pkgsX; }
        else { tags = { }; apps = { }; };
      lib = nixpkgs.lib;
      # Tags merge member-wise, not attr-wise: a tag on both sides must gain members rather than
      # be replaced, while a tag on only one side is kept as it is.
      #
      # Wrapped in `run-here` at the point the merged set is formed, which matters: `allTests`,
      # `.#tests.<name>` and `.#test-manifest` all derive from `tags`, so marking the tests here
      # is what makes every name for a test the derivation a build produces. Marking a later copy
      # would leave one name pointing at derivations nothing builds -- an invalidation that
      # deletes the wrong paths, which is the exact shape of stale green the tier exists to refuse.
      tags = import ./nixosTest/run-here.nix { inherit lib; } (tests.tags // lib.mapAttrs
        (n: g: (tests.tags.${n} or { }) // g)
        private.tags);
      # Every tag but `debug` and `edge` is a real test group; flatten them into the full set. Derived
      # rather than hand-listed so a tag appearing or vanishing with the private half needs no
      # edit here, and a new tag cannot be silently forgotten.
      allTests = lib.foldl' (a: b: a // b) { } (lib.attrValues (removeAttrs tags [ "debug" "edge" ]));
      # The lab rig's flake outputs (fleet module + containers, the demo/soak guest
      # disks, the self-hosted runner), layered in ONLY when the lab outputs file (the path below)
      # is present. The lab runs on our own box and never in a user's home, so the whole of it is
      # private: this tree never carries it (the private sibling imports it from its own flake),
      # so here `labOutputs` is empty and this flake has no lab outputs rather than a broken
      # reference.
      labOutputs =
        if builtins.pathExists ./lab/outputs.nix
        then import ./lab/outputs.nix {
          inherit nixpkgs self lib;
          pkgs = pkgsX;
          artifacts = tests.artifacts;
        }
        else { };
      # Spliced whole at the top level, except the two that merge onto public counterparts.
      labTopLevel = removeAttrs labOutputs [ "nixosConfigurations" "artifacts" ];
    in
    labTopLevel //
    {
      # In-repo packages not in nixpkgs, surfaced as an overlay so
      # the unit image can reference them as pkgs.<name>.
      overlays.default = final: prev: {
        # nixpkgs builds drbd-utils but installs none of its systemd units
        # (systemd isn't a real build input, so configure can't detect the unit
        # dir). Enable them so drbd-reactor's promoter drives the *stock*
        # drbd-promote@ / drbd-services@ + bring-up (drbd@.target) units. The installed units
        # call the drbd-service-shim by its store path; drbd-reactor bakes the FHS
        # /lib/drbd/scripts path, which its package (guest-image/pkgs/drbd-reactor) rewrites
        # to the store path, so no /lib/drbd symlink exists or is needed.
        drbd = prev.drbd.overrideAttrs (old: {
          configureFlags = (old.configureFlags or [ ]) ++ [ "--with-initscripttype=systemd" ];
          installFlags = (old.installFlags or [ ]) ++ [
            "systemdunitdir=/lib/systemd/system"
            "systemdpresetdir=/lib/systemd/system-preset"
          ];
        });
        drbd-reactor = final.callPackage ./guest-image/pkgs/drbd-reactor { };
        dummy-app = final.callPackage ./nixosTest/dummy-app/package.nix { };
        reverse-proxy = final.callPackage ./reverse-proxy/package.nix { }; # reverse proxy at the VIP
        dashboard = final.callPackage ./dashboard/package.nix { }; # the household dashboard behind the reverse proxy
        # The upgrade-pair fixture: {from = 2025.11.0 (schema 51); to =
        # 2025.12.0 (schema 53)}, straddling the recorder v52 `unit_class` migration.
        home-assistant-upgrade-pair = final.callPackage ./guest-image/pkgs/home-assistant-image-pair { };
        # The broker, in the two versions the upgrade tests switch between.
        mosquitto-upgrade-pair = final.callPackage ./guest-image/pkgs/mosquitto-image { };
      };

      packages = forAllSystems (pkgs: {
        drbd-reactor = pkgs.callPackage ./guest-image/pkgs/drbd-reactor { };
        dummy-app = pkgs.callPackage ./nixosTest/dummy-app/package.nix { };
        # The upgrade-pair, surfaced flat + by name so the FOD sha256 can be
        # filled with `nix build .#home-assistant-image-2025_11` (see the pair's
        # refresh procedure) and each end can be built directly.
        home-assistant-image-2025_11 = (pkgs.callPackage ./guest-image/pkgs/home-assistant-image-pair { }).from;
        home-assistant-image-2025_12 = (pkgs.callPackage ./guest-image/pkgs/home-assistant-image-pair { }).to;
        # The mosquitto pair, surfaced by name for the same reason: each end builds directly so
        # its FOD sha256 can be filled in (see the pin refresh procedure).
        mosquitto-image-2_1_1 = (pkgs.callPackage ./guest-image/pkgs/mosquitto-image { }).from;
        mosquitto-image-2_1_2 = (pkgs.callPackage ./guest-image/pkgs/mosquitto-image { }).to;
        # The nixpkgs-pinned Postgres, surfaced by name so the private sibling's soak (a host
        # script — not a nixosTest) provisions an ephemeral PG from the SAME pin its cloud-store
        # tests use (real PG in the soak).
        postgresql = pkgs.postgresql;
        # The release pipeline's Authenticode signer (scripts/ssign.nix).
        ssign = pkgs.callPackage ./scripts/ssign.nix { };
        # The catalog check (catalog/package.nix): catalog/catalog_test.go over the `catalog` input.
        catalog-test = pkgs.callPackage ./catalog/package.nix { inherit catalog; };
      });

      # The guest VM image: NixOS + DRBD 9 + drbd-reactor, and NO workload -- an app is
      # installed at runtime from a signed manifest, so there is nothing to select here. This is
      # configuration.nix alone, the framework-boot guest (no bootloader); the shipped disk is
      # guest-image/disk-image.nix. The lab hosts join these when the lab outputs file is
      # present (never in this tree).
      nixosConfigurations = {
        guest = nixpkgs.lib.nixosSystem {
          system = "x86_64-linux";
          modules = [
            { nixpkgs.overlays = [ self.overlays.default ]; }
            ./guest-image/configuration.nix
          ];
        };
      } // (labOutputs.nixosConfigurations or { });

      # Hermetic nixosTest mechanism tests (nixosTest/outputs.nix). ONE test is a build:
      #   nix build .#tests.drbd-fence -L
      # `nix flake check` stays light: it evaluates the flake and the config closures, but
      # boots no VM test. These are Tier-1 hermetic tests, NOT the private sibling's soak fleet
      # (never `nix build`).
      #
      # ⚠️ THERE IS DELIBERATELY NO `.#all`, AND NO `.#drbd`. An aggregate (a linkFarm) would
      # start every member at nix's default `--max-jobs` — ~40 nixosTests × 2–3
      # nested QEMU nodes at once, which does not fail slowly, it takes the machine down. The
      # aggregate cannot guard itself (its builder runs after its members are realised) and a
      # flake cannot read the machine at eval time, so the answer is that a group is not
      # something you can type at nix. A GROUP IS STILL A THING YOU CAN RUN — see `test-manifest`
      # below and CONTRIBUTING's "run a whole tag" recipe, which spells the cap out loud instead
      # of hiding it behind an attribute that looks free.
      tests = allTests // (tags.debug or { }) // (tags.edge or { });

      # What the tier IS, as data: `tests.tsv` (name, tags, curated, nodes, memory) and
      # `tags/<name>` member lists, including `tags/all` — the curated set.
      # Cheap to build (text files; it takes no test as an input, and names no test's path),
      # which is the whole point. The private sibling's tier scheduler reads it to enumerate and
      # cost the suite.
      test-manifest = import ./nixosTest/manifest.nix { pkgs = pkgsX; inherit tags; };

      # The cloud operator apps — they drive the cloud datastore, so they arrive with the private
      # half and are absent from this tree (`private.apps` is empty here). x86_64-linux only,
      # like the tests.
      apps.x86_64-linux = private.apps;
      # The product build artifacts, plus the demo guest disks when the lab outputs file is
      # present (never in this tree; they exist only to feed the lab fleet and its demos).
      artifacts = tests.artifacts // (labOutputs.artifacts or { });

      devShells = forAllSystems (pkgs: {
        # Dev shell: the Go toolchain + LSP/lint. Harness deps (qemu,
        # drbd-utils, nixosTest plumbing) are added as needed.
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools # goimports, etc.
            go-tools # staticcheck
            restic # the backup store's tests drive the real client against it
          ];
          # Install the tracked git hooks on shell entry. Symlink (not copy)
          # so edits to scripts/hooks/ take effect without re-running install.
          # Skip if .git is absent (e.g. a nix build of the source tree).
          shellHook = ''
            if [ -d .git/hooks ] && [ ! -L .git/hooks/pre-commit ]; then
              ln -sf ../../scripts/hooks/pre-commit .git/hooks/pre-commit
            fi
          '';
        };
      });
    };
}
