# The Briard agent binary, built from the repo's Go module. External deps are
# kept minimal (CONTRIBUTING.md: new dependencies default to no); currently lego (ACME DNS-01) + its tree,
# vendored via vendorHash. Three mains share the module: the host agent, the guest agent that
# serves the host over the virtio-serial channel, and the guest FIRMWARE the image bakes.
#
# vendorHash lives in ../vendor-hash.nix, which every Go package here imports -- it used to be
# copied into eight files kept in step by comments, so adding one dependency meant editing eight
# of them in lockstep. That file carries the regenerate recipe. It is module-wide, so it is
# unaffected by build tags (the vendor dir carries every dep; tags only change what links).
#
# subPackage: which main to build. `agent/cmd/briard-agent` is the HOST agent (and the `briard`
# CLI); `agent/cmd/briard-guest-agent` is the in-guest agent, which the host PUSHES; and
# `agent/cmd/briard-guest-firmware` is the push protocol alone, the ONE binary the guest
# image bakes. Each is its own main, so what each links is an import graph the arch tests fence
# rather than a build tag.
#
# version: the release id stamped into the binary. Shape `<epoch>.<commit-hour>.<short-rev>`
# — e.g. `v3.26080614.92d4eee`, the hour YYMMDDHH in UTC — computed in flake.nix from `self`, i.e. DERIVED FROM THE TREE and
# never from a build counter: the same commit yields the same version, hence the same store path,
# which is what makes a released artifact re-derivable from its tag. The default below is the dev
# sentinel. A dirty tree cannot produce a release id at all (flake.nix emits `v3.dirty`, which the
# publish script refuses), because a build nobody can reproduce must not be publishable.
#
# CGO_ENABLED=0 -- the binary MUST be static. A default (cgo) nixpkgs Go build links against
# glibc with a /nix/store PT_INTERP, so it cannot exec on a host that has no /nix: the kernel
# fails to find the interpreter and reports the file itself as "not found". The agent is the
# FIRST thing a stranger runs (install.sh downloads it as the bootstrap that fetches and
# verifies everything else), so a store-linked agent breaks the free-local install on every
# non-Nix machine -- invisible to the nixosTests, which run on hosts that do have that path.
# This is the same relocatability requirement qemu-bundle.nix solves by patchelf; the agent is
# pure Go, so it can simply not need a loader. Nothing here uses cgo (net's pure-Go resolver
# and os/user's pure-Go path are both fine -- the agent shells out to iproute2/systemd).
# goos: "windows" cross-compiles the Windows host agent (briard-agent.exe) -- a plain Go cross build,
# which is the whole reason the agent is pure Go. The ELF check below does not apply to a PE file.
# gui: a Windows GUI-subsystem program (the tray), which Windows starts without a console window.
# rc: a Windows resource script beside the main (its icon, its manifest), compiled by mingw's windres
# into the .syso the Go linker takes by itself -- the same cross toolchain the Windows QEMU builds
# with. The host's cpp is its preprocessor.
{ lib, buildGoModule, patchelf, pkgsCross, subPackage ? "agent/cmd/briard-agent", version ? "0.0.0-dev", goos ? null, gui ? false, rc ? null }:
buildGoModule {
  pname = baseNameOf subPackage;
  inherit version;
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ subPackage ];
  env.CGO_ENABLED = 0;
  # In preBuild rather than env: buildGoModule sets GOOS/GOARCH itself, over anything in env.
  preBuild = lib.optionalString (goos != null) "export GOOS=${goos} GOARCH=amd64\n"
    + lib.optionalString (rc != null) "(cd ${dirOf rc} && x86_64-w64-mingw32-windres --preprocessor=cpp -O coff -i ${baseNameOf rc} -o rsrc_windows_amd64.syso)\n";
  # A cross-compiled test binary cannot run here; the Windows tests run on a Windows machine.
  doCheck = goos == null;
  # The PE is NOT stripped by the fixup: binutils' strip is an ELF tool, and run over the PE Go
  # emits it leaves a file Windows refuses to start ("not a valid Win32 application" -- measured
  # on the rig, with the unstripped build of the same tree running beside it). Go's own
  # `-s -w` would be the way to shed the symbol table; the size is not worth a second ldflags path.
  dontStrip = goos != null;
  # Stamp the release id the agent reports as NodeStatus.AgentVersion and converges to on a
  # self-update. Overridable by a real release version at build time.
  ldflags = [ "-X" "briard.io/agent/host.buildVersion=${version}" ] ++ lib.optional gui "-H=windowsgui";

  # ...and PROVE it, at build time. Setting CGO_ENABLED is an intention; an ELF interpreter is a
  # fact, and it is the fact a stranger's kernel reads. A future dependency that quietly re-enables
  # cgo would otherwise ship an agent that cannot start on any machine without this exact store
  # path -- the failure we already shipped once, and one no NixOS test can see. This fails the
  # build instead: it is checked on the built binary, so it cannot go vacuous.
  nativeBuildInputs = [ patchelf ] ++ lib.optional (rc != null) pkgsCross.ucrt64.buildPackages.binutils;
  postFixup = lib.optionalString (goos == null) ''
    if interp=$(patchelf --print-interpreter "$out/bin/briard-agent" 2>/dev/null) && [ -n "$interp" ]; then
      echo "briard-agent has an ELF interpreter ($interp) -- it must be STATIC, or it cannot exec" >&2
      echo "on a host without /nix. Something re-enabled cgo; see the CGO_ENABLED note above." >&2
      exit 1
    fi
  '';

  meta.description = "Briard host/guest agent";
}
