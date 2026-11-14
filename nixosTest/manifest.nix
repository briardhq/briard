# The test manifest: what the tier contains, as data rather than a build target.
#
# Membership, curation and per-test cost are data, not a buildable aggregate per tag: building one
# realises every member, starting ~40 nixosTests at once and taking the machine down, and that
# cannot be guarded from inside (a linkFarm builder runs after its members are realised; an
# eval-time guard cannot read the machine, flakes evaluate pure). So no tag is a build target, and
# what a tag knows is a text file.
#
# ⚠️ No outPath, and nothing here may carry string context. Naming a test's outPath computes its
# entire derivation graph (every node's system, the guest image): ~26 GB for every test in one
# process, against ~4 GB for the rest of this file. A consumer that needs the paths of the tests it
# runs evaluates those one at a time. A string referring to a derivation would make the manifest
# take it as a build input, the trap this file removes rebuilt one level down, so the assembled
# text has its context discarded.
#
# TSV, not JSON: the only machine consumer is the tier scheduler of the private sibling, awk
# throughout, and a JSON nobody parses would be a second encoding to keep in step. The per-tag
# name lists are for the other consumer, a person: a tag is still a thing you can run
# (CONTRIBUTING shows how, with the cap spelled out).
{ pkgs, tags, name ? "briard-test-manifest" }:

let
  lib = pkgs.lib;

  # Curation, decided here and nowhere else. `debug` harnesses and the apps' `edge` tests (run by
  # the catalog run, about an upstream release) stay reachable by name and out of the tier, the
  # same rule as `allTests` in flake.nix.
  curated = lib.foldl' (a: b: a // b) { } (lib.attrValues (removeAttrs tags [ "debug" "edge" ]));
  every = curated // (tags.debug or { }) // (tags.edge or { });

  tagsOf = n: lib.attrNames (lib.filterAttrs (_n: g: g ? ${n}) tags);
  nodesOf = t: lib.attrValues t.nodes;

  # The cost model: the guests' declared memory (a hard ceiling: qemu gets `-m N` with no balloon
  # or free-page reporting, so pages are faulted in and never handed back) plus ~128 MB of qemu
  # overhead per node. Recorded as its two components, not the sum, so a reader sees which moved.
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
# `all` is the reserved tag-file name for the curated set; a real tag by that name would be
# silently shadowed, so refuse to evaluate instead.
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
