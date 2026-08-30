{
  description = "Briard — managed, highly-available home automation";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      # X86_64 pkgs with our overlay (drbd/drbd-reactor/dummy), shared by the tests.
      pkgsX = import nixpkgs {
        system = "x86_64-linux";
        overlays = [ self.overlays.default ];
      };
      # The release id, DERIVED FROM THE TREE. Shape `<epoch>.<commit-date>.<short-rev>`,
      # e.g. `v3.20260806.92d4eee`: the epoch carries the only high-level semantics we have (this
      # is an epoch scheme, not semver); the commit date is quasi-incremental and readable at a
      # glance (same-day collisions are expected and fine); the short rev makes it unique AND
      # survives a history rewrite, which a commit *count* would not.
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
        then "v3.${builtins.substring 0 8 self.lastModifiedDate}.${self.shortRev}"
        else "v3.dirty";
      tests = import ./nixosTest/outputs.nix {
        inherit nixpkgs agentVersion;
        pkgs = pkgsX;
        overlay = self.overlays.default;
      };
      # The cloud-coupled tests + the cloud operator apps, layered in ONLY when the
      # private half is present. The public tree simply does not carry
      # `outputs-private.nix`, so `.#store`, `.#witness-*` and `nix run .#regen-schema` cease
      # to exist rather than break — the whole point of the delete-and-build assertion.
      private =
        if builtins.pathExists ./nixosTest/outputs-private.nix
        then import ./nixosTest/outputs-private.nix { pkgs = pkgsX; }
        else { tags = { }; apps = { }; };
      lib = nixpkgs.lib;
      # Tags merge member-wise, not attr-wise: `drbd` and `debug` exist on both sides and must
      # gain members rather than be replaced, while `store` appears only on the private side.
      tags = tests.tags // lib.mapAttrs
        (n: g: (tests.tags.${n} or { }) // g)
        private.tags;
      # Every tag but `debug` is a real test group; flatten them into the full set. Derived
      # rather than hand-listed so a tag appearing or vanishing with the private half needs no
      # edit here (it also drops the old union's risk of silently forgetting a new tag).
      testTags = removeAttrs tags [ "debug" ];
      allTests = lib.foldl' (a: b: a // b) { } (lib.attrValues testTags);
      mkTag = name: group:
        pkgsX.linkFarm "briard-tests-${name}"
          (lib.mapAttrsToList (n: p: { name = n; path = p; }) group);
      # The lab rig's flake outputs (fleet module + containers, the demo/soak guest
      # disks, the self-hosted runner), layered in ONLY when `lab/` is present. `lab/` runs on
      # our L0 box and never in a user's home, so OSS's property line puts the whole of it
      # private; the public tree then has no `nixosModules` / `fleetContainers` / `fleet-demo*`
      # rather than a broken reference.
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
    # One `.#<tag>` aggregate per test group, generated from the merged tags so a group that
    # arrives or departs with the private half (`.#store`) needs no edit below. Spliced
    # first, so anything named explicitly in the main attrset would win — nothing does.
    lib.mapAttrs mkTag testTags //
    labTopLevel //
    {
      # In-repo packages not in nixpkgs, surfaced as an overlay so
      # the unit image can reference them as pkgs.<name>.
      overlays.default = final: prev: {
        # nixpkgs builds drbd-utils but installs none of its systemd units
        # (systemd isn't a real build input, so configure can't detect the unit
        # dir). Enable them so drbd-reactor's promoter drives the *stock*
        # drbd-promote@ / drbd-services@ + bring-up (drbd@.target) units
        #. The units bake an absolute /lib/drbd/scripts shim path (so does
        # drbd-reactor), which the unit image provides via a tmpfiles symlink.
        drbd = prev.drbd.overrideAttrs (old: {
          configureFlags = (old.configureFlags or [ ]) ++ [ "--with-initscripttype=systemd" ];
          installFlags = (old.installFlags or [ ]) ++ [
            "systemdunitdir=/lib/systemd/system"
            "systemdpresetdir=/lib/systemd/system-preset"
          ];
        });
        drbd-reactor = final.callPackage ./guest-image/pkgs/drbd-reactor { };
        dummy-service = final.callPackage ./nixosTest/dummy-service/package.nix { };
        reverse-proxy = final.callPackage ./reverse-proxy/package.nix { }; # front door at the VIP
        # HA as a digest-pinned upstream OCI image — the real service.
        home-assistant-image = final.callPackage ./guest-image/pkgs/home-assistant-image { };
        # The upgrade-pair fixture: {from = 2025.11.0 (schema 51); to =
        # 2025.12.0 (schema 53)}, straddling the recorder v52 `unit_class` migration.
        home-assistant-upgrade-pair = final.callPackage ./guest-image/pkgs/home-assistant-image-pair { };
        # The broker, in the two versions the [V3b.4] upgrade tests switch between.
        mosquitto-upgrade-pair = final.callPackage ./guest-image/pkgs/mosquitto-image { };
      };

      packages = forAllSystems (pkgs: {
        drbd-reactor = pkgs.callPackage ./guest-image/pkgs/drbd-reactor { };
        dummy-service = pkgs.callPackage ./nixosTest/dummy-service/package.nix { };
        home-assistant-image = pkgs.callPackage ./guest-image/pkgs/home-assistant-image { };
        # The upgrade-pair, surfaced flat + by name so the FOD sha256 can be
        # filled with `nix build .#home-assistant-image-2025_11` (see the pair's
        # refresh procedure) and each end can be built directly.
        home-assistant-image-2025_11 = (pkgs.callPackage ./guest-image/pkgs/home-assistant-image-pair { }).from;
        home-assistant-image-2025_12 = (pkgs.callPackage ./guest-image/pkgs/home-assistant-image-pair { }).to;
        # The mosquitto pair, surfaced by name for the same reason: each end builds directly so
        # its FOD sha256 can be filled in (see the pin refresh procedure).
        mosquitto-image-2_1_1 = (pkgs.callPackage ./guest-image/pkgs/mosquitto-image { }).from;
        mosquitto-image-2_1_2 = (pkgs.callPackage ./guest-image/pkgs/mosquitto-image { }).to;
        # The nixpkgs-pinned Postgres, surfaced by name so the lab soak (soak-demo.sh, a
        # host script — not a nixosTest) provisions an ephemeral PG from the SAME pin the
        # store nixosTest + schema.sql drift-gate use (real PG in the soak).
        postgresql = pkgs.postgresql;
      });

      # The guest VM image: NixOS + DRBD 9 + drbd-reactor, and NO workload -- a service is
      # installed at runtime from a signed manifest, so there is nothing to select here and no
      # `guest-ha` variant any more ([V3b.3](e2)). The lab's `fleet-demo*` / `runner-host`
      # hosts join these when `lab/` is present.
      nixosConfigurations = {
        guest = nixpkgs.lib.nixosSystem {
          system = "x86_64-linux";
          modules = [
            { nixpkgs.overlays = [ self.overlays.default ]; }
            ./guest-image/configuration.nix
          ];
        };
      } // (labOutputs.nixosConfigurations or { });

      # Hermetic nixosTest mechanism tests (nixosTest/outputs.nix), exposed as tags you
      # build on demand:
      #   nix build .#drbd | .#upgrade | .#ha | .#integration   (a slice)
      #   nix build .#all                                        (every test — the nightly)
      #   nix build .#tests.<name> -L                            (one test, e.g. drbd-fence)
      # `nix flake check` stays light: it evaluates the flake + builds the config
      # closures, but boots no VM tests — use a tag for that. These are Tier-1 hermetic
      # tests, NOT the lab/ soak fleet (cmd/soak, never `nix build`).
      # The per-tag aggregates themselves are generated above the attrset; `.#store`
      # is there only when the private half is.
      all = mkTag "all" allTests;
      # Flat `.#tests.<name>` includes debug harnesses (reactor-pause-deadlock); `.#all`
      # above deliberately does not, so the nightly never runs them.
      tests = allTests // (tags.debug or { });

      # The cloud operator apps (`nix run .#regen-schema` / `.#backup-store` / `.#migrate-store`
      # / `.#provision-tenant`) — all four drive the cloud datastore, so they arrive with the private
      # half and are absent from the public tree. x86_64-linux only, like the tests.
      apps.x86_64-linux = private.apps;
      # The product build artifacts, plus the demo guest disks when `lab/` is present
      # (they exist only to feed the lab fleet and its demos).
      artifacts = tests.artifacts // (labOutputs.artifacts or { });

      devShells = forAllSystems (pkgs: {
        # V0 dev shell: the Go toolchain + LSP/lint. Harness deps (qemu,
        # drbd-utils, nixosTest plumbing) are added as needed.
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools # goimports, etc.
            go-tools # staticcheck
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
