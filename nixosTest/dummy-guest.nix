# The guest image with the dummy fixture in the payload slot — the node most mechanism tests
# boot, and the test-side sibling of guest-image/ha-guest.nix.
#
# The DRBD / upgrade / failover tests need a payload that writes deterministic, checkable data,
# so they select the fixture here rather than getting it from the product's defaults (the slot ships empty). Tests that assert the *shipped* shape use configuration.nix directly —
# see zero-service.nix.
{
  imports = [
    ../guest-image/configuration.nix
    ./dummy-payload.nix
  ];
}
