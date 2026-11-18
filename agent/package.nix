# The Briard Go binaries, built from the repo's one Go module: the host agent, the guest agent
# (serves the host over virtio-serial), the guest firmware the image bakes, and the Windows tray
# and setup. External deps stay minimal (CONTRIBUTING.md: new dependencies default to no): the
# module requires klauspost/compress, pion/mdns and golang.org/x/{net,sys}. vendorHash lives in
# ../vendor-hash.nix (with the regenerate recipe), imported by every Go package here, so a new
# dependency is one edit; it is module-wide, so build tags do not affect it (the vendor dir
# carries every dep; tags only change what links).
#
# subPackage: which main. `agent/cmd/briard-agent` is the host agent (and the `briard` CLI);
# `agent/cmd/briard-guest-agent` the in-guest agent, which the host pushes;
# `agent/cmd/briard-guest-firmware` the push protocol alone, the one binary the guest image
# bakes; `agent/cmd/briard-tray` / `agent/cmd/briard-setup` the Windows tray and setup. Each is
# its own main, so what each links is an import graph the arch tests fence, not a build tag.
#
# version: the release id, `<epoch>.<commit-hour>.<short-rev>` (e.g. `v3.26080614.92d4eee`, the
# hour YYMMDDHH in UTC), computed in flake.nix from `self`: derived from the tree, never from a
# build counter, so the same commit yields the same version and store path, which makes a
# released artifact re-derivable from its tag. The default is the dev sentinel. A dirty tree has
# no release id (flake.nix emits `v<epoch>.dirty`, which the publish script refuses): a build nobody
# can reproduce must not be publishable.
#
# CGO_ENABLED=0: the binary must be static. A cgo nixpkgs Go build links glibc with a /nix/store
# PT_INTERP, so it cannot exec on a host without /nix (the kernel reports the file itself as "not
# found"). The agent is the first thing a stranger runs (install.sh downloads it as the bootstrap
# that fetches and verifies everything else), so a store-linked agent breaks the free-local
# install on every non-Nix machine, invisibly to the nixosTests, whose hosts have that path.
# qemu-bundle.nix meets the same relocatability requirement by patchelf; pure Go simply needs no
# loader. Nothing here uses cgo (the pure-Go net resolver and os/user are fine; the agent shells
# out to iproute2/systemd).
# goos: "windows" cross-compiles a Windows program (briard-agent.exe, the tray, setup), a plain Go
# cross build, which is the whole reason the agent is pure Go. The ELF check below does not apply
# to a PE file.
# gui: a Windows GUI-subsystem program (the tray, setup), started without a console window.
# winres: a resource script beside the main (e.g. agent/cmd/briard-tray/briard-tray.rc: the icon
# and version info of agent/cmd/briard.rc, plus the manifest), compiled by mingw windres (the
# cross toolchain the Windows QEMU builds with; the host cpp preprocesses) into the .syso the Go
# linker takes by itself.
# ⚠️ No argument here may share a name with a package: callPackage fills it from pkgs over its
# default. Named `rc` (the Plan 9 shell), it would be set on every build that left it unset, and
# every one would run windres.
{ lib, buildGoModule, patchelf, pkgsCross, subPackage ? "agent/cmd/briard-agent", version ? "0.0.0-dev", goos ? null, gui ? false, winres ? null }:
let
  # The release id as Windows' four 16-bit version fields: v3.26102922.e073879 is 3.2610.2922.0
  # (the epoch, then the commit hour split in two); any other version, a dev build's, is 0.0.0.0.
  p = lib.splitString "." version;
  hour = builtins.elemAt p 1;
  field = s: toString (lib.toIntBase10 s); # a field may lead with zeros: 0105
  winVersion =
    if builtins.length p == 3 && lib.hasPrefix "v" (builtins.head p) && builtins.match "[0-9]{8}" hour != null
    then "${field (lib.removePrefix "v" (builtins.head p))},${field (builtins.substring 0 4 hour)},${field (builtins.substring 4 4 hour)},0"
    else "0,0,0,0";
in
buildGoModule {
  pname = baseNameOf subPackage;
  inherit version;
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ subPackage ];
  env.CGO_ENABLED = 0;
  # In preBuild rather than env: buildGoModule sets GOOS/GOARCH itself, over anything in env.
  preBuild = lib.optionalString (goos != null) "export GOOS=${goos} GOARCH=amd64\n"
    + lib.optionalString (winres != null) "(cd ${dirOf winres} && x86_64-w64-mingw32-windres --preprocessor=cpp -DBRIARD_VERSION=${version} -DBRIARD_FILEVERSION=${winVersion} -O coff -i ${baseNameOf winres} -o rsrc_windows_amd64.syso)\n";
  # A cross-compiled test binary cannot run here; the Windows tests run on a Windows machine.
  doCheck = goos == null;
  # The PE is not stripped by the fixup: binutils strip is an ELF tool, and the PE it leaves is
  # one Windows refuses to start ("not a valid Win32 application"; the unstripped build of the
  # same tree runs). Go `-s -w` would shed the symbol table; the size is not worth a second
  # ldflags path.
  dontStrip = goos != null;
  # Stamp the release id the agent reports as NodeStatus.AgentVersion and converges to on a
  # self-update. Overridable by a real release version at build time.
  ldflags = [ "-X" "briard.io/agent/host.buildVersion=${version}" ] ++ lib.optional gui "-H=windowsgui";

  # ...and prove it at build time. Setting CGO_ENABLED is an intention; an ELF interpreter is the
  # fact a stranger's kernel reads. A dependency that quietly re-enables cgo would otherwise ship
  # an agent that cannot start without this exact store path, a failure no NixOS test can see.
  # Checked on the built binary, so it cannot go vacuous.
  nativeBuildInputs = [ patchelf ] ++ lib.optional (winres != null) pkgsCross.ucrt64.buildPackages.binutils;
  postFixup = lib.optionalString (goos == null) ''
    if interp=$(patchelf --print-interpreter "$out/bin/briard-agent" 2>/dev/null) && [ -n "$interp" ]; then
      echo "briard-agent has an ELF interpreter ($interp) -- it must be STATIC, or it cannot exec" >&2
      echo "on a host without /nix. Something re-enabled cgo; see the CGO_ENABLED note above." >&2
      exit 1
    fi
  '';

  meta.description = "Briard host/guest agent";
}
