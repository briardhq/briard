# `nix build .#catalog-test`: catalog/catalog_test.go over the published catalog (its own
# repository, the `catalog` flake input). This lays the catalog/ directory and its image hashes
# (catalog-images.json) beside the test and runs it, so overriding the input
# (`--override-input catalog path:.` in the catalog checkout, or its `main`) checks that catalog
# with this code. The `catalog` build tag keeps a plain `go test ./...` without the files from
# compiling it: no files is no verdict, never a pass. A Go test, not a VM: it builds on any
# machine with nix, and the per-push CI runs it.
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
