#!/usr/bin/env bash
#
# publish-release.sh — build, sign and publish the release channel a stranger
# installs from. The consumer already exists (agent/install/fetch.go + scripts/install.sh),
# so this script has NO LATITUDE: it must emit exactly what the verifier reads, or installs
# refuse. Everything below is read off that verifier rather than designed here.
#
# THE CONTRACT, at <BRIARD_CHANNEL_URL> (default https://get.briard.io):
#   manifest.json      {"artifacts":[{"name","sha256","size","mode"}]}, sha256 lowercase hex,
#                      mode omitted => 0644
#   manifest.json.sig  a RAW 64-byte detached Ed25519 signature over the exact manifest bytes
#                      (PureEdDSA over the whole file — no separate hash step)
#   <name>             each artifact at the base URL: briard-agent (0755), briard-net-wrap
#                      (0755), qemu-bundle.tar.zst, nixos.qcow2.zst — the big two travel
#                      COMPRESSED and the agent expands them after verifying the signed hash,
#                      so the manifest pins the compressed bytes (what the network carries).
#                      The agent itself is never compressed: the bootstrap fetches it with curl
#                      before anything exists that could decompress it.
#   install.sh         at the channel root, fetched by the one-liner before any verification
#                      exists — which is why the repo being public and readable IS the answer
#                      to the `curl | sh` objection.
#
# SIGNING AND PUBLISHING ARE SEPARATE SUBCOMMANDS ON PURPOSE. `sign` needs the key and no
# credential; `publish` needs the credential and no key. Either secret alone is inert — a
# forged signature has nowhere to be served, and the bucket serves what will not verify —
# while together they are arbitrary code on every installed machine. Keeping them apart from
# day one means the split wants is a matter of WHERE each runs, not a rewrite.
#
# A DIRTY TREE IS REFUSED. The version is derived from the tree (flake.nix: epoch + commit
# date + short rev) and a dirty tree has no rev, so it stamps `v3.dirty`. A build nobody can
# reproduce must not be publishable — and the reproducibility acceptance test is precisely
# "re-derive this artifact from its tag", which a working directory can never satisfy.
#
# Subcommands:
#   stage    [DIR]   build the artifacts, lay them out under DIR, write manifest.json
#   sign     [DIR]   detached-sign DIR/manifest.json  (needs $RELEASE_SIGN_KEY, no credential)
#   publish  [DIR]   upload DIR + install.sh to $RELEASE_WRITE  (needs the credential, no key)
#   verify           fetch from the LIVE channel and check it the way a client does
#
# Env:
#   BRIARD_CHANNEL_URL  public read base            (default https://get.briard.io)
#   RELEASE_WRITE       write store URL — required by `publish`, e.g.
#                       's3://get-briard-io?endpoint=<account>.r2.cloudflarestorage.com&region=auto'
#   RELEASE_SIGN_KEY    PKCS8 PEM Ed25519 private key (sign mode; release secret store)
#   RELEASE_PUBKEY      PKIX PEM public key, for `verify` (default: alongside the private key)
#   RELEASE_PURGE_URL   optional: CDN purge endpoint, POSTed a {"files":[...]} list after upload
#   RELEASE_PURGE_TOKEN optional: bearer token for RELEASE_PURGE_URL (see `publish` for why)
#
# Run from the repo root. See also scripts/publish-cache.sh — the OS-closure cache, a
# DISTINCT trust root (nix's per-path narinfo signatures). The two keys are deliberately not
# shared: a compromised cache key is recoverable precisely because re-imaging is verified by
# this one.
set -euo pipefail

# The artifact set lives under /release/; install.sh lives at the site root (the advertised
# one-liner). SITE is DERIVED rather than a second knob -- one setting, one place to override.
CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io/release}"
SITE="${CHANNEL%/release}"
STAGE_DEFAULT="./.release"

die() { echo "publish-release: $*" >&2; exit 1; }
say() { echo ">>> $*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }
# openssl is not in the dev shell and must not become a reason the release cannot be signed on
# whatever machine holds the key. Prefer a real one, fall back to nixpkgs — same shape as
# `publish` reaching awscli2.
ossl() {
	if command -v openssl >/dev/null 2>&1; then openssl "$@"; else nix run nixpkgs#openssl -- "$@"; fi
}

# The release id, and the dirty gate. Asked of the flake rather than recomputed here, so there
# is exactly one definition of what a version is (flake.nix).
release_version() {
	local v
	v=$(nix eval --raw .#artifacts.agent.version) || die "cannot read the release version"
	case "$v" in
		v3.dirty|*dirty*) die "refusing a DIRTY tree (version=$v) — commit first; a build nobody can reproduce must not be published" ;;
		"") die "empty version" ;;
	esac
	echo "$v"
}

out_of() { nix build --no-link --print-out-paths "$1"; }

case "${1:-}" in

stage)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix; need sha256sum; need jq
	V=$(release_version)
	say "staging release $V into $DIR"
	rm -rf "$DIR"; mkdir -p "$DIR"

	# Copy, never symlink: the artifacts are uploaded as bytes, and a store symlink would
	# publish a dangling link. `install -m` sets the mode the manifest then records.
	install -m0755 "$(out_of .#artifacts.agent)/bin/briard-agent"        "$DIR/briard-agent"
	install -m0755 "$(out_of .#artifacts.net-wrap)/bin/briard-net-wrap"  "$DIR/briard-net-wrap"
	install -m0644 "$(out_of .#artifacts.guest-disk)/nixos.qcow2"        "$DIR/nixos.qcow2"
	# qemu-bundle is a DIRECTORY in the store (bin/ lib/ share/ PROVENANCE) and the contract
	# wants one file, so it is tarred here. Deterministically: fixed mtime/owner and a sorted
	# member order, or the same tree would produce a different sha256 on every run and the
	# manifest would churn for no reason.
	tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
	    -cf "$DIR/qemu-bundle.tar" -C "$(out_of .#artifacts.qemu-bundle)" .
	chmod 0644 "$DIR/qemu-bundle.tar"

	# COMPRESS THE BIG TWO. Measured on the shipped set: the guest image 1178 -> 377 MB and the
	# bundle 86 -> 19 MB, so an install goes from ~1273 MB on the wire to ~404 MB. The agent
	# expands them after verifying the signed hash (agent/install/fetch.go).
	#
	# `briard-agent` and `briard-net-wrap` are deliberately left PLAIN. install.sh fetches the
	# bootstrap agent with curl/wget before any agent exists to decompress anything -- compress it
	# and the installer cannot open the tool whose job is opening things.
	#
	# -19 not --ultra: 23s and 377 MB against gzip -9's 85s and 465 MB, on a file published once
	# and downloaded by every household. -T0 uses the release box's cores; it does not change the
	# output, so the manifest hash is unaffected by what machine staged it.
	#
	# The manifest below then pins the COMPRESSED bytes, which is what the network carries and
	# therefore what a signature has to cover.
	for f in nixos.qcow2 qemu-bundle.tar; do
		nix run nixpkgs#zstd -- -19 -T0 -q --rm "$DIR/$f" -o "$DIR/$f.zst" ||
			die "compressing $f failed"
		chmod 0644 "$DIR/$f.zst"
	done

	# The manifest, written BY THE AGENT rather than by this script.
	#
	# It used to be a printf loop right here, hand-assembling the JSON -- including `"mode":493`,
	# which is 0o755 converted to decimal by a human and re-checked by nobody. That made the
	# format a contract between a shell script and a Go struct with nothing holding the two in
	# step: a renamed field, a mode that stopped being hand-converted, or a size that drifted from
	# what the reader bounds on would have published a channel no agent could install, and neither
	# side's tests could have caught it. The writer now shares its types with the reader
	# (agent/install), so the round trip is testable in one language and the format cannot
	# disagree with itself.
	#
	# The binary used is the one STAGED IN THIS DIRECTORY -- the exact agent this release ships,
	# so the manifest is written by the same build that will later read it on a node.
	[ -x "$DIR/briard-agent" ] || die "no staged briard-agent to write the manifest with"
	"$DIR/briard-agent" --stage-manifest "$DIR" || die "writing the manifest failed"
	jq -e . "$DIR/manifest.json" >/dev/null || die "the manifest we just wrote is not valid JSON"

	# THE WINDOWS ARM, IN ITS OWN SUBDIRECTORY AND UNDER ITS OWN MANIFEST -- and the subdirectory
	# IS the mechanism, not an organising whim. `FetchVerified` downloads EVERY artifact the
	# manifest names, so putting a Windows-only bundle in the Linux manifest would make every
	# Linux install download tens of MB it can never run. `WriteManifest` skips directories, so a
	# subdir is invisible to the set above it, and a Windows installer will later point
	# BRIARD_CHANNEL_URL at $CHANNEL/windows and get the identical shape: a signed manifest, its
	# detached signature, and the artifacts it names. One path, run twice -- no second protocol,
	# no platform field on the wire, and no client change on either side.
	#
	# There is no consumer yet: the Windows host agent is v5. It is published anyway so the path
	# is exercised rather than assumed, which is the whole reason [V3b.27](b) exists.
	mkdir -p "$DIR/windows"
	tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
	    -cf "$DIR/windows/qemu-bundle-windows.tar" -C "$(out_of .#artifacts.qemu-bundle-windows)" .
	chmod 0644 "$DIR/windows/qemu-bundle-windows.tar"
	nix run nixpkgs#zstd -- -19 -T0 -q --rm "$DIR/windows/qemu-bundle-windows.tar" \
		-o "$DIR/windows/qemu-bundle-windows.tar.zst" || die "compressing the windows bundle failed"
	chmod 0644 "$DIR/windows/qemu-bundle-windows.tar.zst"
	"$DIR/briard-agent" --stage-manifest "$DIR/windows" || die "writing the windows manifest failed"
	jq -e . "$DIR/windows/manifest.json" >/dev/null || die "the windows manifest is not valid JSON"

	# install.sh, WITH THE RELEASE PUBKEY EMBEDDED. The source tree carries a placeholder, and
	# the script dies on it by design ("the embedded key is a build placeholder") — so shipping
	# the file unsubstituted would publish an installer that refuses to install. It is the one
	# artifact deliberately OUTSIDE the signed set, because the one-liner fetches it before
	# anything can verify anything: the key travels with the script over TLS (the standard
	# installer-carries-the-pubkey pattern), which is why the repo being public and the script
	# readable is the real answer to the `curl | sh` objection.
	[ -n "${RELEASE_PUBKEY:-}" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key (embedded into install.sh)"
	grep -q "BEGIN PUBLIC KEY" "$RELEASE_PUBKEY" || die "$RELEASE_PUBKEY is not a PEM PUBLIC KEY"
	awk -v keyfile="$RELEASE_PUBKEY" '
		/^RELEASE_KEYRING_PEM=/ {
			printf "RELEASE_KEYRING_PEM='"'"'"
			while ((getline line < keyfile) > 0) print line
			printf "'"'"'\n"
			next
		} { print }' scripts/install.sh > "$DIR/install.sh"
	chmod 0755 "$DIR/install.sh"
	grep -q "__BRIARD_RELEASE_KEYRING_PEM__" "$DIR/install.sh" \
		&& die "the keyring placeholder survived — the published installer would refuse to install"
	grep -q "BEGIN PUBLIC KEY" "$DIR/install.sh" \
		|| die "no public key landed in the staged install.sh"
	sh -n "$DIR/install.sh" || die "the staged install.sh is not valid shell after substitution"

	echo "$V" > "$DIR/VERSION" # not part of the contract; a human-readable marker for the operator
	say "staged $V — $(jq -r '.artifacts|length' "$DIR/manifest.json") artifacts"
	jq -r '.artifacts[] | "    \(.name)  \(.size) bytes  \(.sha256[0:16])…"' "$DIR/manifest.json"
	say "next: ./scripts/publish-release.sh sign $DIR"
	;;

sign)
	DIR="${2:-$STAGE_DEFAULT}"
	:
	[ -f "$DIR/manifest.json" ] || die "no manifest at $DIR/manifest.json — run `stage` first"
	[ -n "${RELEASE_SIGN_KEY:-}" ] || die "set RELEASE_SIGN_KEY to the PKCS8 PEM Ed25519 private key"
	[ -e "$RELEASE_SIGN_KEY" ] || die "no signing key at $RELEASE_SIGN_KEY"
	# -rawin is what makes this PureEdDSA over the whole file, which is what Verify does. Without
	# it openssl would pre-hash and every signature would be rejected.
	#
	# EVERY manifest in the staged set, not just the top one: the windows/ subdirectory carries a
	# manifest of its own (see `stage`), and an unsigned one is not merely useless -- the verifier
	# refuses it, so it would publish a channel arm that cannot install. Looping is what keeps a
	# future third arm from needing anyone to remember this.
	for m in "$DIR/manifest.json" "$DIR"/*/manifest.json; do
		[ -f "$m" ] || continue
		ossl pkeyutl -sign -inkey "$RELEASE_SIGN_KEY" -rawin -in "$m" -out "$m.sig"
		n=$(stat -c%s "$m.sig")
		[ "$n" = 64 ] || die "$m.sig is $n bytes, want a raw 64 — the verifier rejects anything else"
		say "signed $m (64-byte detached Ed25519)"
	done
	;;

publish)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix
	[ -f "$DIR/manifest.json.sig" ] || die "unsigned — run `sign` before publishing"
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	say "publishing $(cat "$DIR/VERSION" 2>/dev/null || echo '?') to $RELEASE_WRITE"
	[ -f "$DIR/install.sh" ] || die "no install.sh in $DIR — run \`stage\` (it embeds the keyring)"
	bucket=$(echo "$RELEASE_WRITE" | sed 's|^s3://\([^?]*\).*|s3://\1|')
	endpoint="https://$(echo "$RELEASE_WRITE" | sed 's|.*endpoint=\([^&]*\).*|\1|')"

	# THE ARTIFACT SET, under /release/, with --delete.
	#
	# ⚠️ `--delete` IS ONLY SAFE BECAUSE OF THE PREFIX, and the near-miss is worth recording: this
	# bucket ALSO holds `catalog/` -- live runtime content the agent fetches for
	# `briard service install`, uploaded by hand and produced by nothing in this repo -- plus
	# install.sh at the root. A bare `--delete` against the bucket root would have removed the
	# catalog on the next publish and broken service installs fleet-wide, silently, because
	# nothing here knows the catalog exists. Scoped to the prefix a release owns, the flag can
	# only remove things a release put there.
	#
	# What it buys: a RENAMED artifact cleans itself up. Overwrite-by-fixed-name leaves nothing
	# behind, but a rename (nixos.qcow2 -> nixos.qcow2.zst) stranded the old key forever, and a
	# 2.5 GB orphan that no signed manifest references is exactly the kind of thing that is
	# still being served years later.
	#
	# install.sh is EXCLUDED here and uploaded to the root below -- it is the advertised
	# one-liner and does not live in the release namespace.
	#
	# THE MANIFEST AND ITS SIGNATURE ARE ALSO EXCLUDED, and cp'd LAST. Observed live on two
	# consecutive publishes (2026-08-19): `sync --delete` emitted BOTH an upload and a delete
	# for release/manifest.json in a single run, in whichever order the parallel operations
	# happened to land -- the second time the delete won and the live channel 404'd its
	# manifest until a hand-run `cp` restored it. Taking the pair out of the sync removes the
	# key from --delete's reach, and uploading it after the artifacts buys the right ordering
	# for free: a client racing a publish now sees a coherent old set, a coherent new set, or
	# a manifest/signature mismatch that fails closed -- never a manifest naming bytes that
	# are not there yet.
	nix run nixpkgs#awscli2 -- s3 sync "$DIR" "$bucket/release/" \
		--endpoint-url "$endpoint" \
		--delete --exclude VERSION --exclude install.sh \
		--exclude manifest.json --exclude manifest.json.sig \
		--exclude "*/manifest.json" --exclude "*/manifest.json.sig" --no-progress
	# Every arm's manifest pair, uploaded after the bytes it names. The windows/ subdirectory
	# needs the same treatment for the same reason, and its exclude patterns above need the `*/`
	# form because an AWS CLI filter with no wildcard matches one exact relative key -- so a bare
	# `--exclude manifest.json` would have left the windows manifest inside --delete's reach and
	# inside the sync's ordering, which is the exact pair of properties this dance exists to avoid.
	for m in "$DIR/manifest.json" "$DIR"/*/manifest.json; do
		[ -f "$m" ] || continue
		rel=${m#"$DIR/"}
		for f in "$rel" "$rel.sig"; do
			nix run nixpkgs#awscli2 -- s3 cp "$DIR/$f" "$bucket/release/$f" \
				--endpoint-url "$endpoint" --no-progress
		done
	done

	# ...and the installer itself, at the root the one-liner names.
	nix run nixpkgs#awscli2 -- s3 cp "$DIR/install.sh" "$bucket/install.sh" \
		--endpoint-url "$endpoint" --no-progress

	# PURGE THE EDGE, or a REpublish contradicts itself for hours. The CDN in front of the
	# public URL caches the big artifacts (measured: the .zst files, max-age 14400; the small
	# ones come back uncached) -- and `verify`'s own downloads are what prime it. So the second
	# and every later publish serves the NEW manifest with the OLD image until expiry, and each
	# install in that window fails closed on the hash check. Found live 2026-08-19; the
	# first-ever publish went green only because nothing was cached yet. The purge list is read
	# off the staged manifest so a renamed artifact cannot drift out of it. Purging is
	# CDN-specific, so it rides behind two env vars -- but a publish without them warns loudly,
	# because the alternative is a silently stale channel.
	if [ -n "${RELEASE_PURGE_URL:-}" ] && [ -n "${RELEASE_PURGE_TOKEN:-}" ]; then
		need curl; need jq
		# Read off EVERY staged manifest, one arm per subdirectory, so a renamed artifact cannot
		# drift out of the purge list and a new arm cannot be forgotten into staleness.
		{
			for m in "$DIR/manifest.json" "$DIR"/*/manifest.json; do
				[ -f "$m" ] || continue
				rel=${m#"$DIR/"}; base="$CHANNEL/${rel%manifest.json}"; base=${base%/}
				jq -r --arg b "$base" '.artifacts[].name | $b+"/"+.' "$m"
				echo "$base/manifest.json"; echo "$base/manifest.json.sig"
			done
			echo "$SITE/install.sh"
		} | jq -R . | jq -s '{files: .}' |
		curl -sf -X POST -H "Authorization: Bearer $RELEASE_PURGE_TOKEN" \
			-H "Content-Type: application/json" "$RELEASE_PURGE_URL" --data @- |
		jq -e '.success == true' >/dev/null ||
			die "published, but the edge purge FAILED -- the CDN may serve the previous release for hours; purge by hand, then verify"
		say "edge cache purged"
	else
		say "WARNING: RELEASE_PURGE_URL/RELEASE_PURGE_TOKEN unset -- the edge may serve the previous release's artifacts for up to 4h (verify will rightly fail until it clears)"
	fi
	say "published — now run: ./scripts/publish-release.sh verify"
	;;

verify)
	need curl; need sha256sum
	PUB="${RELEASE_PUBKEY:-${RELEASE_SIGN_KEY:-}}"
	[ -n "$PUB" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key"
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
	# EVERY ARM, not just the Linux one. A channel arm is a base URL with a signed manifest under
	# it (see `stage`), so verifying one is verifying all of them with the URL changed -- and an
	# arm nobody verifies is an arm nobody knows is broken until a stranger runs into it.
	for arm in "" "/windows"; do
		base="$CHANNEL$arm"
		curl -fsS "$base/manifest.json"     -o "$tmp/manifest.json"     || die "no manifest at $base"
		curl -fsS "$base/manifest.json.sig" -o "$tmp/manifest.json.sig" || die "no signature at $base"
		# Verify the way the agent does: raw Ed25519 over the exact bytes. A failure here is the
		# whole point of the check — an unsigned or re-signed channel must not read as green.
		ossl pkeyutl -verify -pubin -inkey "$PUB" -rawin \
		        -in "$tmp/manifest.json" -sigfile "$tmp/manifest.json.sig" >/dev/null \
			|| die "the live manifest at $base does NOT verify against $PUB"
		say "manifest signature verifies: $base"
		# ...and every artifact matches what the signed manifest claims. Checked by DOWNLOADING,
		# not by trusting the manifest against itself.
		jq -r '.artifacts[] | "\(.name) \(.sha256) \(.size)"' "$tmp/manifest.json" |
		while read -r name sum size; do
			curl -fsS "$base/$name" -o "$tmp/$name" || die "$name is in the $base manifest but not served"
			got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
			gotsize=$(stat -c%s "$tmp/$name")
			[ "$got" = "$sum" ] || die "$name: sha256 $got != manifest $sum"
			[ "$gotsize" = "$size" ] || die "$name: size $gotsize != manifest $size"
			echo "    ok  $name  ($gotsize bytes)"
		done
	done
	# install.sh at the SITE root, not under the artifact prefix -- that is the URL the advertised
	# one-liner names, so it is the one this must assert.
	curl -fsS -o /dev/null "$SITE/install.sh" || die "install.sh is not served at $SITE/install.sh"
	# The installer a stranger runs must agree with where the artifacts actually are. A published
	# install.sh still defaulting to the old flat root would fetch nothing and fail closed, which
	# is safe but silent -- and would not be caught by any check above, since every one of them
	# uses $CHANNEL rather than what the script itself believes.
	# Downloaded to a FILE and then grepped, never `curl … | grep -q`. `-q` exits on the first
	# match, which closes the pipe under a curl that is still writing; curl then dies with
	# "(23) Failure writing output to destination" and `||` reads that as the assertion failing.
	# Measured on the first real run of this check: a correctly-published channel was reported as
	# wrong. A verification step that cries wolf is worse than none, because the next person
	# learns to skip it.
	curl -fsS "$SITE/install.sh" -o "$tmp/install.sh" || die "install.sh is not fetchable at $SITE"
	grep -q "BRIARD_CHANNEL_URL:-$CHANNEL" "$tmp/install.sh" ||
		die "the served install.sh does not default to $CHANNEL — it would look for artifacts in the wrong place"
	say "$CHANNEL verifies end to end: signed manifest, every artifact matching, install.sh served at $SITE and pointing here"
	;;

*)
	die "usage: publish-release.sh {stage|sign|publish|verify} [DIR]  (see header)"
	;;
esac
