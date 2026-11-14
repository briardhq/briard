# A VM test result is evidence about this machine, so it must never arrive from a cache.
#
# A nixosTest's output is a "this passed" marker addressed by the hash of its inputs: once it
# exists anywhere nix can reach, asking again returns it without starting a VM, and the build
# exits 0 having printed nothing. The tier deletes every marker before it builds and proves the
# delete took, but deletion cannot cover substitution: a marker deleted locally can come straight
# back from a binary cache, and no checking of the local store sees it coming.
# `allowSubstitutes = false` (nix's own instrument, as nixpkgs' trivial builders use it) makes the
# guarantee structural instead of detected: once the tier has proved a marker absent, it can be
# valid again only if this machine built it.
#
# ⚠️ It is also right on its own terms: these tests boot real VMs under real KVM to make a claim
# about the machine running them, and a marker fetched from elsewhere is a claim about elsewhere,
# a category error, not a speedup. The consequence: on a host that cannot meet
# `requiredSystemFeatures` (kvm, nixos-test) a test fails to build rather than quietly fetching
# someone else's verdict, which is the honest outcome.
#
# `overrideTestDerivation` is the supported hook for the run derivation; `overrideAttrs` does not
# exist on a test object.
{ lib }:

lib.mapAttrs (_group: lib.mapAttrs
  (_name: test: test.overrideTestDerivation (_: { allowSubstitutes = false; })))
