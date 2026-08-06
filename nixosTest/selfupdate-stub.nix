# TEST-ONLY build of the self-update trial stub (nixosTest/briard-selfupdate-stub/main.go),
# used by agent-selfupdate.nix to exercise the frozen commit/revert pivot hermetically —
# NOT part of the shipped agent (agent/package.nix builds only the product binary).
#
# vendorHash is module-wide: same value as
# agent/package.nix; regenerate both together on go.mod/go.sum changes.
{ buildGoModule }:
buildGoModule {
  pname = "briard-selfupdate-stub";
  version = "0.0.0";
  src = ../.;
  vendorHash = "sha256-5qbgoM8+XGR9gxMHkq+WqNO9EqTTk+1s9bZ6poMBYQc=";
  subPackages = [ "nixosTest/briard-selfupdate-stub" ];
  meta.description = "Test stand-in agent for the self-update pivot proof";
}
