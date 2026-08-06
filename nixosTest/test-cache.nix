# A signed binary cache, inside a nixosTest — the stand-in for cache.briard.io.
#
# Hermetic tests have no network, so "the guest fetches a closure it does not have" needs a
# cache the test stands up itself. This is that, factored out of os-stage.nix once a second
# caller appeared: the nested tests stop baking their upgrade targets into a disk
# variant and stage them from here instead, which is what deletes whole OS closures from the
# image `system.extraDependencies` was carrying.
#
# Two decisions are baked in and both are load-bearing:
#
# `nix-serve`, NOT a `file://` cache. Writing a file cache means `nix copy`, and nix refuses
# to write a cache that does not also hold the references — the same rule that reshaped
# the publish model — so delivering a few kB of delta would cost ~1.6 GB of copying
# inside the test. nix-serve synthesises narinfos from the store on demand, so the test pays
# for the delta only. The guest speaks the identical protocol either way.
#
# A keypair MINTED AT RUN TIME, not `require-sigs = false`. The guest does a genuine
# signature-checked substitution at its production default; the override swaps WHICH key it
# trusts, never whether it checks. Minting inside the test also means nothing secret-shaped
# is committed and the real cache key is never involved.
#
# Usage — the two halves are deliberately separate, because one is Nix and one is Python:
#
#   cache = import ./test-cache.nix { inherit pkgs; } { paths = [ target ]; };
#   nodes.host = { ... }: { imports = [ cache.module ]; … };
#   testScript = ''
#     ${cache.start "host"}          # mints the key, serves, and proves it serves
#     # `cache_pubkey` and `cache_url` are now in scope for whatever consumes them
#   '';
#
# The serving node must also let the client reach it — for a nested guest that means
# `networking.firewall.enable = false`, which is the caller's topology decision and
# deliberately not set here.
{ pkgs }:
{
  # Store paths the cache must hold and serve. They enter the serving node's store via
  # additionalPaths, which is what makes this node a stand-in for a cache that already holds
  # a freshly published release.
  paths,
  # Where it listens. The default matches os-stage's original inline setup.
  port ? 8080,
  # The signing key's name. It appears in every narinfo signature, so `start` can assert the
  # cache really signed what it served rather than trusting that it did.
  keyName ? "briard-test-1",
  # Where the client reaches it. The L1 shim address every nested-guest test uses.
  host ? "192.168.1.1",
}:
{
  # The serving node's configuration. nix-serve is left out of multi-user.target on purpose:
  # its secretKeyFile does not exist until `start` mints one, so letting it come up at boot
  # would be a guaranteed unit failure before the test has done anything.
  module =
    { ... }:
    {
      virtualisation.additionalPaths = paths;
      services.nix-serve = {
        enable = true;
        bindAddress = "0.0.0.0"; # reachable from the nested guest, not just from localhost
        port = port;
        secretKeyFile = "/run/briard-cache.key";
      };
      systemd.services.nix-serve.wantedBy = pkgs.lib.mkForce [ ];
    };

  # The URL a client on the nested side substitutes from.
  url = "http://${host}:${toString port}";

  # Python: mint the key, start serving, and PROVE the cache serves each path signed by that
  # key before any client is asked to fetch it. That check is not ceremony — it is what makes
  # a later guest-side failure unambiguously the guest's fetch rather than a broken fixture,
  # which is the difference between a diagnosis and a bisect. Leaves `cache_pubkey` and
  # `cache_url` in the test script's scope.
  start =
    node:
    ''
      ${node}.succeed("nix-store --generate-binary-cache-key ${keyName} /run/briard-cache.key /run/briard-cache.pub")
      cache_pubkey = ${node}.succeed("cat /run/briard-cache.pub").strip()
      cache_url = "${"http://${host}:${toString port}"}"
      ${node}.succeed("systemctl start nix-serve")
      ${node}.wait_for_open_port(${toString port})
    ''
    + pkgs.lib.concatMapStrings (p: ''
      _hash = "${p}".split("/")[3].split("-")[0]
      _narinfo = ${node}.succeed(f"curl -sf http://127.0.0.1:${toString port}/{_hash}.narinfo")
      assert "${keyName}:" in _narinfo, f"cache did not sign ${p}:\n{_narinfo}"
    '') paths;
}
