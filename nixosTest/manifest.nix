# The test manifest — what the tier CONTAINS, as data rather than as a build target ([B.149]).
#
# WHY THIS EXISTS. The membership, the curation and the per-test cost used to be carried by a
# `linkFarm` per tag (`.#drbd` … `.#all`): building one realised every member, so the aggregates
# were simultaneously the only written-down answer to "what is the tier" and a way to start ~40
# nixosTests at once. That second property killed the box twice on 2026-09-12, and it cannot be
# guarded from inside — a linkFarm's builder runs AFTER its members are realised, and an eval-time
# guard cannot read the machine because flakes evaluate pure. So the data stays and the
# buildability goes: `nix build .#all` is gone, and what it knew is a text file.
#
# ⚠️ NOTHING IN HERE MAY CARRY STRING CONTEXT. `t.outPath` is a string that REFERS to the test's
# derivation, so a manifest built from it verbatim would take every test as a build input — the
# exact trap this file removes, rebuilt one level down and harder to see. `discard` strips the
# context off the assembled text, which is sound because an outPath is a pure function of the
# derivation: this records the NAME of a path, it does not ask for the path.
#
# WHY TSV AND NOT JSON. The only machine consumer is `lab/scripts/tier-concurrency.sh`, which is
# awk throughout; a JSON nobody parses would be a second encoding of the same facts to keep in
# step (AGENTS §5). The per-tag name lists exist for the other consumer — a person — because a tag
# was a thing you could run and has to stay one.
{ pkgs, tags, name ? "briard-test-manifest" }:

let
  lib = pkgs.lib;

  # CURATION, decided here and nowhere else. `debug` harnesses stay reachable by name and out of
  # the tier — the same rule `allTests` encoded back when the aggregates defined it.
  curated = lib.foldl' (a: b: a // b) { } (lib.attrValues (removeAttrs tags [ "debug" ]));
  every = curated // (tags.debug or { });

  tagsOf = n: lib.attrNames (lib.filterAttrs (_n: g: g ? ${n}) tags);
  nodesOf = t: lib.attrValues t.nodes;

  # The cost model, verbatim from [B.127]: the guests' declared memory (a hard ceiling — qemu gets
  # `-m N` with no balloon and no free-page reporting, so a guest's pages are faulted in and never
  # handed back) plus ~128 MB of qemu overhead per node. Recorded as its two components rather
  # than the sum, so a reader can see which half moved.
  row = n: t: lib.concatStringsSep "\t" [
    n
    (lib.concatStringsSep "," (tagsOf n))
    (if curated ? ${n} then "1" else "0")
    (toString (lib.length (nodesOf t)))
    (toString (lib.foldl' (a: c: a + c.virtualisation.memorySize) 0 (nodesOf t)))
    t.outPath
  ];

  discard = builtins.unsafeDiscardStringContext;
  tsv = pkgs.writeText "tests.tsv"
    (discard (lib.concatStringsSep "\n" (lib.mapAttrsToList row every) + "\n"));

  tagNames = lib.attrNames tags ++ [ "all" ];
  membersOf = t: if t == "all" then lib.attrNames curated else lib.attrNames tags.${t};
  tagFile = t: pkgs.writeText "tag-${t}"
    (discard (lib.concatStringsSep "\n" (membersOf t) + "\n"));
in
# `all` is the reserved tag-file name for the curated set — what `.#all` used to mean. A real tag
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
      # failed" — the vacuous-green shape this whole tier keeps relearning (B.81).
      [ -s $out/tests.tsv ] || { echo "manifest is empty — no tests were enumerated" >&2; exit 1; }
    ''
)
