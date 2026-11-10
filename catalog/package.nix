# `nix build .#catalog-test`: catalog/catalog_test.go over the published catalog. The catalog is
# its own repository, the `catalog` flake input; this lays its catalog/ directory and its image
# hashes (catalog-images.json) here, beside the test, and runs it -- so pointing the input
# elsewhere (`--override-input catalog path:.` in the catalog's own checkout, or its `main`)
# checks that catalog with this code. The test is behind a `catalog` build tag, which keeps a
# plain `go test ./...` on a checkout without the files from compiling it: no files is no
# verdict, never a pass. A Go test, not a VM: it builds on any machine with nix, and the per-push
# CI runs it.
{ buildGoModule, catalog }:
buildGoModule {
  pname = "catalog-test";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  postPatch = ''
    cp -r --no-preserve=mode ${catalog}/catalog/. catalog/
    cp --no-preserve=mode ${catalog}/catalog-images.json catalog/
  '';
  buildPhase = ''
    runHook preBuild
    go test -tags catalog -v ./catalog
    runHook postBuild
  '';
  doCheck = false;
  installPhase = ''
    touch $out
  '';
}
