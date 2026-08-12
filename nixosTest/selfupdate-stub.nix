# TEST-ONLY build of the self-update trial stub (nixosTest/briard-selfupdate-stub/main.go),
# used by agent-selfupdate.nix to exercise the frozen commit/revert pivot hermetically —
# NOT part of the shipped agent (agent/package.nix builds only the product binary).
#
# vendorHash is module-wide: the same one definition
# agent/package.nix uses (vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "briard-selfupdate-stub";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "nixosTest/briard-selfupdate-stub" ];
  meta.description = "Test stand-in agent for the self-update pivot proof";
}
