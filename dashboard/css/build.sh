#!/usr/bin/env sh
# Build the dashboard's stylesheet: Tailwind over Basecoat, with the parts in input.css, scanning
# the templates. The output, static/app.css, is COMMITTED: `go build` and `go test` need no node
# and no network, and the guest fetches nothing. Run this after changing input.css or a template's
# classes, and commit the result. Basecoat is fetched by hash (nix verifies it) and Tailwind comes
# from the flake's nixpkgs, so two machines build the same file.
set -eu
cd "$(dirname "$0")"
basecoat=https://registry.npmjs.org/basecoat-css/-/basecoat-css-1.1.0.tgz
basecoat_hash=sha256-Rusc7c04RQE9A6L0Y0HGPsUflxhc75Mu63mjMqfBK0g=
tgz=$(nix store prefetch-file --json --name basecoat-css.tgz --expected-hash "$basecoat_hash" "$basecoat" | sed 's/.*"storePath":"\([^"]*\)".*/\1/')
rm -rf node_modules
mkdir -p node_modules/basecoat-css
tar -xzf "$tgz" -C node_modules/basecoat-css --strip-components=1
nix shell --inputs-from "$(git rev-parse --show-toplevel)" nixpkgs#tailwindcss_4 -c tailwindcss -i input.css -o ../static/app.css --minify
rm -rf node_modules
