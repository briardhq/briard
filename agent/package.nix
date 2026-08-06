# The Briard agent binary, built from the repo's Go module. External deps are
# kept minimal (CONTRIBUTING.md: new dependencies default to no); currently lego (ACME DNS-01) + its tree,
# vendored via vendorHash. Runs host-side on the machine, or `--guest` inside the
# guest VM to serve the host over the virtio-serial channel.
#
# vendorHash: regenerate on go.mod/go.sum changes (set to lib.fakeHash, build,
# copy the reported "got:" hash). vendorHash is module-wide, so it's unaffected by
# build tags (the vendor dir carries every dep; tags only change what the binary links).
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
{ buildGoModule, tags ? [ ], version ? "0.0.0-dev" }:
buildGoModule {
  pname = "briard-agent" + (if tags == [ ] then "" else "-" + builtins.concatStringsSep "-" tags);
  inherit version;
  src = ../.;
  vendorHash = "sha256-5qbgoM8+XGR9gxMHkq+WqNO9EqTTk+1s9bZ6poMBYQc=";
  subPackages = [ "agent/cmd/briard-agent" ];
  inherit tags;
  # Stamp the release id the agent reports as NodeStatus.AgentVersion and converges to on a
  # self-update. Overridable by a real release version at build time.
  ldflags = [ "-X" "briard.io/agent/host.buildVersion=${version}" ];
  meta.description = "Briard host/guest agent";
}
