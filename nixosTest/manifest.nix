# The test manifest — what the tier CONTAINS, as data rather than as a build target.
#
# WHY THIS EXISTS. The membership, the curation and the per-test cost are data here, not a
# buildable aggregate per tag: building such an aggregate realises every member, which starts ~40
# nixosTests at once and takes the machine down, and that cannot be
# guarded from inside — a linkFarm builder runs AFTER its members are realised, and an eval-time
# guard cannot read the machine because flakes evaluate pure. So the data stays and the
# buildability goes: no tag is a build target, and what a tag knows is a text file.
#
# ⚠️ NO OUTPATH, AND NOTHING IN HERE MAY CARRY STRING CONTEXT. A test's outPath is the one fact here
# that costs a whole evaluation: naming it computes the test's entire derivation graph -- every
# node's system, the guest image -- and for every test in one process that is ~26 GB, where the
# rest of this file is ~4 GB. A consumer that needs the paths of the tests it RUNS evaluates those
# one at a time. And a string that refers to a derivation would make the manifest take that
# derivation as a build input -- the trap this file removes, rebuilt one level down -- so the
# assembled text has its context discarded.
#
# WHY TSV AND NOT JSON. The only machine consumer is the tier scheduler of the private sibling,
# which is awk throughout; a JSON nobody parses would be a second encoding of the same facts to
# keep in step. The per-tag name lists exist for the other consumer — a person — because a tag
# is still a thing you can run (CONTRIBUTING shows how, with the cap spelled out).
{ pkgs, tags, name ? "briard-test-manifest" }:

let
  lib = pkgs.lib;

  # CURATION, decided here and nowhere else. `debug` harnesses and the apps' `edge` tests (run by the
  # catalog run, about an upstream release) stay reachable by name and out of
  # the tier — the same rule as `allTests` in flake.nix.
  curated = lib.foldl' (a: b: a // b) { } (lib.attrValues (removeAttrs tags [ "debug" "edge" ]));
  every = curated // (tags.debug or { }) // (tags.edge or { });

  tagsOf = n: lib.attrNames (lib.filterAttrs (_n: g: g ? ${n}) tags);
  nodesOf = t: lib.attrValues t.nodes;

  # The cost model: the guests' declared memory (a hard ceiling — qemu gets
  # `-m N` with no balloon and no free-page reporting, so a guest's pages are faulted in and never
  # handed back) plus ~128 MB of qemu overhead per node. Recorded as its two components rather
  # than the sum, so a reader can see which half moved.
  row = n: t: lib.concatStringsSep "\t" [
    n
    (lib.concatStringsSep "," (tagsOf n))
    (if curated ? ${n} then "1" else "0")
    (toString (lib.length (nodesOf t)))
    (toString (lib.foldl' (a: c: a + c.virtualisation.memorySize) 0 (nodesOf t)))
  ];

  discard = builtins.unsafeDiscardStringContext;
  tsv = pkgs.writeText "tests.tsv"
    (discard (lib.concatStringsSep "\n" (lib.mapAttrsToList row every) + "\n"));

  tagNames = lib.attrNames tags ++ [ "all" ];
  membersOf = t: if t == "all" then lib.attrNames curated else lib.attrNames tags.${t};
  tagFile = t: pkgs.writeText "tag-${t}"
    (discard (lib.concatStringsSep "\n" (membersOf t) + "\n"));
in
# `all` is the reserved tag-file name for the curated set. A real tag
# by that name would be silently shadowed by it, so refuse to evaluate instead.
lib.throwIf (tags ? all) "manifest.nix: `all` is reserved as the curated-set tag file" (
  pkgs.runCommand name
    {
      passthru.testCount = lib.length (lib.attrNames every);
    }
    ''
      mkdir -p $out/tags
      cp ${tsv} $out/tests.tsv
      ${lib.concatMapStrings (t: "cp ${tagFile t} $out/tags/${t}\n") tagNames}
      # A manifest with no rows would sail through every consumer as "nothing to run, nothing
      # failed" — the vacuous-green shape this whole tier keeps relearning.
      [ -s $out/tests.tsv ] || { echo "manifest is empty — no tests were enumerated" >&2; exit 1; }
    ''
)
