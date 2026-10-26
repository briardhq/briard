# ssign: Authenticode signing with a Certum SimplySign cloud certificate, over HTTPS -- the
# signer `publish-release.sh` runs on every PE of the windows arm and on the Store exe. Built from
# a pinned release, never fetched at run time: the program holds the code-signing login while it
# runs, so what runs is exactly what was read. It talks to Certum's login and signing service and
# its timestamp authority, nothing else.
{ rustPlatform, fetchFromGitHub }:

rustPlatform.buildRustPackage {
  pname = "ssign";
  version = "0.1.7";
  src = fetchFromGitHub {
    owner = "Le-Syl21";
    repo = "ssign";
    rev = "v0.1.7";
    hash = "sha256-vhssCHHVKUEO5I5CYyTtixxlQUaAtA2PQT/Lxq7kFBM=";
  };
  cargoHash = "sha256-FQNrsEq07dF3cjQ97nKVBpODvlrb8oqCYZvSw2KbJ/g=";
  # The CLI alone; the workspace's PKCS#11 module is for script signing, which this pipeline
  # does not do.
  cargoBuildFlags = [ "--package" "ssign" ];
  cargoTestFlags = [ "--package" "ssign" ];
}
