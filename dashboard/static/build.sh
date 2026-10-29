#!/usr/bin/env sh
# Build what the dashboard embeds besides its templates: the stylesheet (Tailwind over Basecoat,
# with the parts in ../css/input.css, scanning the templates) and htmx, the one script. Both
# outputs are COMMITTED here, so `go build` and `go test` need no node and no network, and the
# guest fetches nothing. Run this after changing input.css, a template's classes, or a pinned
# version below, and commit the result. Each download is named by hash (nix verifies it) and
# Tailwind comes from the flake's nixpkgs, so two machines build the same files.
set -eu
cd "$(dirname "$0")"
basecoat=https://registry.npmjs.org/basecoat-css/-/basecoat-css-1.1.0.tgz
basecoat_hash=sha256-Rusc7c04RQE9A6L0Y0HGPsUflxhc75Mu63mjMqfBK0g=
htmx=https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.11.tgz
htmx_hash=sha256-FlomVQc+nm5V4wenx06OvNt2r7dwH5wWz9EGoTrdQI4=
fetch() { nix store prefetch-file --json --name "$2" --expected-hash "$3" "$1" | sed 's/.*"storePath":"\([^"]*\)".*/\1/'; }

tgz=$(fetch "$basecoat" basecoat-css.tgz "$basecoat_hash")
rm -rf ../css/node_modules
mkdir -p ../css/node_modules/basecoat-css
tar -xzf "$tgz" -C ../css/node_modules/basecoat-css --strip-components=1
nix shell --inputs-from "$(git rev-parse --show-toplevel)" nixpkgs#tailwindcss_4 -c tailwindcss -i ../css/input.css -o app.css --minify
rm -rf ../css/node_modules

tgz=$(fetch "$htmx" htmx.tgz "$htmx_hash")
rm -f htmx-*.min.js
tar -xzf "$tgz" -O package/dist/htmx.min.js > htmx-2.0.11.min.js
