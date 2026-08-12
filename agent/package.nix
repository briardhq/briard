# The Briard agent binary, built from the repo's Go module. External deps are
# kept minimal (CONTRIBUTING.md: new dependencies default to no); currently lego (ACME DNS-01) + its tree,
# vendored via vendorHash. Runs host-side on the machine, or `--guest` inside the
# guest VM to serve the host over the virtio-serial channel.
#
# vendorHash lives in ../vendor-hash.nix, which every Go package here imports -- it used to be
# copied into eight files kept in step by comments, so adding one dependency meant editing eight
# of them in lockstep. That file carries the regenerate recipe. It is module-wide, so it is
# unaffected by build tags (the vendor dir carries every dep; tags only change what links).
#
# tags: pass [ "guest" ] to build the guest-only binary -- it excludes the host
# subsystems (platform/QEMU launcher, net/http, crypto/tls), roughly halving the binary
# and shedding the TLS CVE surface the guest never uses. The guest VM runs `agent
# --guest`; the L1 host runs the untagged (full) binary.
#
# version: the release id stamped into the binary. Shape `<epoch>.<commit-date>.<short-rev>`
# — e.g. `v3.20260806.92d4eee` — computed in flake.nix from `self`, i.e. DERIVED FROM THE TREE and
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
{ buildGoModule, patchelf, tags ? [ ], version ? "0.0.0-dev" }:
buildGoModule {
  pname = "briard-agent" + (if tags == [ ] then "" else "-" + builtins.concatStringsSep "-" tags);
  inherit version;
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "agent/cmd/briard-agent" ];
  inherit tags;
  env.CGO_ENABLED = 0;
  # Stamp the release id the agent reports as NodeStatus.AgentVersion and converges to on a
  # self-update. Overridable by a real release version at build time.
  ldflags = [ "-X" "briard.io/agent/host.buildVersion=${version}" ];

  # ...and PROVE it, at build time. Setting CGO_ENABLED is an intention; an ELF interpreter is a
  # fact, and it is the fact a stranger's kernel reads. A future dependency that quietly re-enables
  # cgo would otherwise ship an agent that cannot start on any machine without this exact store
  # path -- the failure we already shipped once, and one no NixOS test can see. This fails the
  # build instead: it is checked on the built binary, so it cannot go vacuous.
  nativeBuildInputs = [ patchelf ];
  postFixup = ''
    if interp=$(patchelf --print-interpreter "$out/bin/briard-agent" 2>/dev/null) && [ -n "$interp" ]; then
      echo "briard-agent has an ELF interpreter ($interp) -- it must be STATIC, or it cannot exec" >&2
      echo "on a host without /nix. Something re-enabled cgo; see the CGO_ENABLED note above." >&2
      exit 1
    fi
  '';

  meta.description = "Briard host/guest agent";
}
