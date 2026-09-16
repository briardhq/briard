# A VM test result is evidence about THIS machine, so it must never arrive from a cache ([B.149]).
#
# WHAT THIS CLOSES. A nixosTest's output is a marker meaning "this passed", addressed by the hash
# of its inputs — so once the marker exists anywhere nix can reach, asking for it again returns it
# without starting a VM, and the build exits 0 having printed nothing. The tier defends against
# that by DELETING every marker before it builds, and proving the delete took. But deletion cannot
# cover substitution: a marker deleted locally can come straight back from a binary cache, and no
# amount of checking the local store sees it coming.
#
# `allowSubstitutes = false` is nix's own instrument for this — the same one nixpkgs' trivial
# builders use — and it makes the guarantee structural instead of detected: after the tier has
# proved a marker absent, the only way it can be valid again is that this machine built it.
#
# ⚠️ IT IS ALSO RIGHT ON ITS OWN TERMS, independently of caching. These tests boot real VMs under
# real KVM to make a claim about the machine running them; a marker fetched from somewhere else is
# a claim about somewhere else. Substituting a test result is not a speedup, it is a category
# error. The consequence to know: on a host that cannot meet `requiredSystemFeatures` (kvm,
# nixos-test) a test now fails to build rather than quietly fetching someone else's verdict, which
# is the honest outcome.
#
# `overrideTestDerivation` is the supported hook for reaching the run derivation; `overrideAttrs`
# does not exist on a test object.
{ lib }:

lib.mapAttrs (_group: lib.mapAttrs
  (_name: test: test.overrideTestDerivation (_: { allowSubstitutes = false; })))
