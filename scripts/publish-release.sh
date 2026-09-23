#!/usr/bin/env bash
#
# publish-release.sh — build, sign and publish the release channel a stranger installs from
# and a node updates from. The consumer already exists (agent/install/fetch.go +
# scripts/install.sh), so this script has NO LATITUDE: it must emit exactly what the verifier
# reads, or installs refuse. Everything below is read off that verifier rather than designed
# here.
#
# THE TREE, at <BRIARD_CHANNEL_URL> (default https://get.briard.io) — [B.86e]:
#
#   install.sh                          a byte-copy of briard/stable/linux/install.sh, laid by
#                                       `promote` ([B.159](a)); unsigned where it is SERVED, since
#                                       the one-liner fetches it before any verification exists
#   briard/
#     <version>/linux/                  manifest.json(+.sig), briard-agent, briard-net-wrap,
#                                       install.sh,
#                                       briard-{exec,commit,update},
#                                       qemu-bundle.tar.zst, guest-bundle.tar.zst,
#                                       briard-{agent,update}.service, briard-update.timer
#     <version>/windows/                manifest.json(+.sig), qemu-bundle-windows.tar.zst
#                                       (the Windows arm, [V3b.27](b); no consumer until v5)
#     latest/{linux,windows}/           manifest.json(+.sig), briard-agent (linux)
#     stable/{linux,windows}/           likewise
#   vm/
#     <version>/                        manifest.json(+.sig), nixos.qcow2.zst
#     latest/ stable/                   manifest.json(+.sig)
#
# A CHAIN is a release line with its own version series: the briard bundle moves as
# `v3.<date>.<rev>` on every publish; the guest OS as `vm.<date>.<inputs>` and ONLY WHEN ITS
# INPUTS CHANGE ([B.86i]). The guest image is a function of its inputs (flake.nix guestInputs:
# the image recipe, the packages built into it, the Go packages the guest binary links, the
# module files, the nixpkgs pin) and carries no commit-derived stamp, so `stage` asks the live
# channel whether an image with these exact inputs is already published and, if so, REUSES that
# release instead of staging a 400 MB image nobody would be able to tell from the last one. The
# pairing therefore lives in the BRIARD manifest: `vm` names the vm release this briard
# release was staged beside, an installer fetches the briard chain and then the vm release it names,
# and `promote` moves vm/stable to it. The briard chain has one more level, the PLATFORM ARM,
# because a briard bundle is built per host OS; the guest image is the same VM on every host and
# has none. A POINTER is just a path serving a byte-copy of one
# version's signed manifest: no pointer file, no second signature format, one verified hop. The
# client resolves every artifact against the manifest's own `version` field
# (<chain>/<version>[/<arm>]/<name>), never against the path it fetched the manifest from — so a
# pointer costs a few KB, not a second 380 MB image. The ONE exception is `briard-agent`,
# duplicated under the host pointers, because install.sh has to curl a bootstrap before anything
# exists that can parse a manifest, and that bootstrap must come from the TARGET (a stale one
# that cannot parse a newer manifest is the forward-compat bricking [B.86] exists to prevent).
#
#   manifest.json      {"chain","platform","version","artifacts":[{"name","sha256","size","mode"}]},
#                      sha256 lowercase hex, mode omitted => 0644, platform omitted on the guest
#   manifest.json.sig  a RAW 64-byte detached Ed25519 signature over the exact manifest bytes
#                      (PureEdDSA over the whole file — no separate hash step)
#   <name>             the big two travel COMPRESSED and the agent expands them after verifying
#                      the signed hash, so the manifest pins the compressed bytes (what the
#                      network carries). The agent itself is never compressed: the bootstrap
#                      fetches it with curl before anything exists that could decompress it.
#   install.sh         an ORDINARY ARTIFACT of briard/<version>/linux — hashed by the manifest and
#                      covered by its signature like everything else ([B.159](a)) — which is also
#                      byte-copied to the channel root by `promote`. The root copy is fetched by
#                      the one-liner before any verification exists, which is why the repo being
#                      public and readable IS the answer to the `curl | sh` objection; `verify`
#                      asserts the two are the same bytes, which is the only thing tying the
#                      unsigned root to the signed set. It resolves `stable` by default on both
#                      paths and is never stamped with its own id — see `stage` for why.
#
# VERSIONED DIRECTORIES ARE IMMUTABLE — the property is AN ID NEVER NAMES TWO DIFFERENT
# BYTE-SETS, not that bytes live forever. The guest image is not bit-reproducible (timestamps,
# filesystem UUIDs), so re-staging the same commit yields different bytes under the same id, and
# a pointer copied from the old manifest would then name bytes the versioned directory no longer
# serves. Two guards, because one of them stopped being able to see the whole question when `gc`
# started deleting: `publish` refuses a version the bucket already holds, and `release_version`
# refuses a commit older than the gc floor (an id carries its COMMIT DATE, so re-minting one
# means re-publishing that commit, and anything gc removed is past the floor).
#
# RECOVERY FROM A BAD RELEASE IS A REVERT COMMIT, PUBLISHED FORWARD — not a pointer move. Read
# agent/install/update.go before reaching for `promote <older-id>`: the timer's `stable` path
# installs only when date(have) < date(want), so moving stable BACKWARD rolls no installed node
# back. It changes what new installs get and lowers the floor an exact cloud pin may reach, and
# that is all. A revert commit is a new rev with today's date, so it is a forward move for every
# node, and it needs no old bytes — which is why `gc` can delete them (owner, 2026-09-09).
#
# PUBLISHING POINTS NOTHING AT A RELEASE ([B.159]). `publish` uploads versioned directories and
# stops; `latest` moves that pointer once the gates have passed on the exact id, and `promote`
# moves `stable` and the root installer once the evidence is in. The order matters because it is
# what lets a gate run on the bytes the CDN actually serves: an id nothing names is fetchable by
# whoever knows it — they are commit-derived and this repo is public, so untagged is not private
# — but it is advertised by no path, taken by no timer, and installed by no stranger. A release
# that fails its gate is simply never pointed at, and `gc` collects it.
#
# `stable` is the one that matters: every installed node converges to it nightly and there is no
# canary on that path, so promotion is meant to be evidence-driven (the canary converged, the
# fleet stayed healthy for a real window) — never a release-day action. `promote` refuses a build
# whose date equals the currently promoted one: the timer's stable path orders on the date field
# alone ([B.86a]), so a same-date promotion would be invisible to it, and would only LOOK like a
# release. A same-day fix-up takes the next day's number.
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
#   stage    [DIR]        build the artifacts, lay the tree out under DIR, write the manifests
#   sign     [DIR]        detached-sign every manifest
#                         (needs $RELEASE_SIGN_KEY, no credential)
#   publish  [DIR]        upload the versioned dirs and NOTHING ELSE -- no pointer, no root
#                         install.sh, so the release reaches nobody until it is pointed at
#                         ([B.159](b))
#                         (needs the credential, no key; refuses an already-published version)
#   latest   [VERSION]    move `latest` onto a published release, both chains — the second half
#                         of publishing, run once the gates have passed on that exact id
#                         (default: the version staged in the default DIR)
#   promote  [VERSION]    copy <VERSION>'s manifests to `stable` on every chain and arm, and its
#                         install.sh to the channel root
#                         (default: whatever briard/latest names; refuses a same-date promotion)
#   gc       [--keep V]…  DELETE versioned dirs no pointer names and nothing pins, older than
#                         the 30-day floor — whole releases, never files
#   verify   [VERSION]    fetch stable + latest of every chain and arm from the LIVE channel and
#                         check them the way a client does — plus the root installer against
#                         briard/stable's. With a VERSION: that release where it was published,
#                         both arms and the guest it pairs with, which is what a publish is
#                         followed by while no pointer names it yet ([B.159](b))
#
# Env:
#   BRIARD_CHANNEL_URL  public read ROOT            (default https://get.briard.io)
#   RELEASE_WRITE       write store URL — required by `publish`/`promote`/`gc`, e.g.
#                       's3://get-briard-io?endpoint=<account>.r2.cloudflarestorage.com&region=auto'
#   RELEASE_SIGN_KEY    PKCS8 PEM Ed25519 private key (sign mode; release secret store)
#   RELEASE_PUBKEY      PKIX PEM public key, for `verify` (default: alongside the private key)
#   RELEASE_PURGE_URL   optional: CDN purge endpoint, POSTed a {"files":[...]} list after upload
#   RELEASE_PURGE_TOKEN optional: bearer token for RELEASE_PURGE_URL (see purge_edge for why)
#
# Run from the repo root. There is no second publish and no second trust root any more: the
# guest OS travels as the signed image in this tree, verified by the release keyring the host
# holds ([B.86i] retired the nix binary cache and its narinfo key with the closure path).
set -euo pipefail

CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}"
STAGE_DEFAULT="./.release"
CHAINS="briard vm"
# The platform arms of a chain. A chain without the level yields `-`, the FLAT arm, which every
# loop below turns into "" (`arm=${a#-}`) so it runs once over "<chain>/<version>/" — a real
# empty word would vanish from `for` and the chain would never be visited.
arms_of() { case "$1" in briard) echo "linux windows" ;; *) echo "-" ;; esac; }
# Nothing younger than this is ever removed, whatever the pointers say, so `gc` can never race a
# rollback or a fresh pin. It is also the STALE-COMMIT FLOOR `release_version` refuses past —
# one number, because the two are the same fact seen from each end (see there).
GC_FLOOR_DAYS=30

die() { echo "publish-release: $*" >&2; exit 1; }
say() { echo ">>> $*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }
# openssl is not in the dev shell and must not become a reason the release cannot be signed on
# whatever machine holds the key. Prefer a real one, fall back to nixpkgs — same shape as
# `publish` reaching awscli2.
ossl() {
	if command -v openssl >/dev/null 2>&1; then openssl "$@"; else nix run nixpkgs#openssl -- "$@"; fi
}
aws() { nix run nixpkgs#awscli2 -- "$@"; }

# The release id, and the dirty gate. Asked of the flake rather than recomputed here, so there
# is exactly one definition of what a version is (flake.nix).
release_version() {
	local v
	v=$(nix eval --raw .#artifacts.agent.version) || die "cannot read the release version"
	case "$v" in
		v3.dirty|*dirty*) die "refusing a DIRTY tree (version=$v) — commit first; a build nobody can reproduce must not be published" ;;
		"") die "empty version" ;;
	esac
	# THE PUSHED-MAIN GATE. Every tier that tests this code -- CI, the nightly's VM and fleet
	# tiers, the single-test workflow -- tests pushed `main`, so a release cut from an unpushed
	# commit or another branch ships code none of them has seen. An ancestor of `origin/main` is
	# fine: it was on main, and the floor below bounds how old it may be.
	git fetch --quiet origin main || die "cannot fetch origin/main to check that HEAD is on it"
	git merge-base --is-ancestor HEAD origin/main ||
		die "refusing HEAD $(git rev-parse --short HEAD) (version=$v): it is not on origin/main — push it to main first; the suites test pushed main, so anything else would ship untested code"
	# THE STALE-COMMIT FLOOR, and it is what keeps ids unique now that `gc` DELETES. An id is
	# `v3.<commit-date>.<shortrev>`, so the only way to mint one twice is to publish the same
	# commit twice — and `publish` catches that by asking the bucket, which is exactly the
	# question a deleted release makes unanswerable. Everything `gc` removes is past the floor BY
	# PUBLISH DATE, and a commit is never younger than its own publish, so refusing a commit
	# older than the floor closes that hole with no ledger to keep or lose. It costs nothing in
	# practice: releases are cut from HEAD, and shipping old code is a REVERT COMMIT (the header's
	# recovery note) — a new rev with today's date, which passes this.
	local age
	age=$(( ( $(date -u +%s) - $(date -u -d "$(date_of "$v")" +%s) ) / 86400 ))
	[ "$age" -le "$GC_FLOOR_DAYS" ] || die "refusing a commit $age days old (version=$v, floor ${GC_FLOOR_DAYS}d): gc deletes past that floor, so this id may name bytes that existed once and are gone — publish a revert commit, not the old tag"
	echo "$v"
}
# The vm release a PUBLISHED briard release pairs with, read off its live manifest ([B.86i]):
# the one place the pairing is recorded, and the same field an installing node reads.
vm_of() {
	curl -fsS "$CHANNEL/briard/$1/linux/manifest.json" | jq -er '.vm // empty' ||
		die "briard/$1 is not published, or names no vm release (published before [B.86i]?)"
}
# The id a chain uses for the release named by a briard id.
chain_id() { case "$1" in vm) vm_of "$2" ;; *) echo "$2" ;; esac; }
# The vm release the live channel serves for these image inputs, if any: `latest` first
# (what the last publish paired with), then `stable`. Empty when neither matches or the channel
# cannot be read -- in which case `stage` publishes a fresh image, which is always safe.
live_vm_for_inputs() {
	local p m
	for p in latest stable; do
		m=$(curl -fsS "$CHANNEL/vm/$p/manifest.json" 2>/dev/null) || continue
		if [ "$(echo "$m" | jq -r '.inputs // ""')" = "$1" ]; then
			echo "$m" | jq -r .version; return 0
		fi
	done
	return 1
}
# The date field of an id — the ONLY thing the timer's stable path orders on.
date_of() { echo "$1" | cut -d. -f2; }
# A release directory's path below the chain: "<seg>" or "<seg>/<arm>".
sub() { echo "$1${2:+/$2}"; }
# The one versioned directory a staged chain holds.
staged_version() {
	local d
	for d in "$1"/*/; do
		d=$(basename "$d")
		case "$d" in latest|stable) continue ;; esac
		echo "$d"; return 0
	done
	return 1
}
# Read the bucket + endpoint out of a write URL.
bucket_of() { echo "$1" | sed 's|^s3://\([^?]*\).*|s3://\1|'; }
endpoint_of() { echo "https://$(echo "$1" | sed 's|.*endpoint=\([^&]*\).*|\1|')"; }
# Does the bucket hold this key? (`s3 ls` prints nothing and exits 1 for a missing key.)
have_key() { [ -n "$(aws s3 ls "$1" --endpoint-url "$2" 2>/dev/null)" ]; }
# Copy one key to another INSIDE the bucket, `$bucket`-relative on both sides.
#
# ⚠️ `s3api copy-object`, NOT `s3 cp`, and the difference is not style. awscli2's `s3 cp` between
# two S3 keys carries the source object's TAGS, which costs it a GetObjectTagging on the source
# and an `x-amz-tagging-directive` on the copy -- neither of which R2 implements. `--copy-props
# none` only trades the first error for the second. Measured on the FIRST `promote` of this tree,
# which failed with `NotImplemented` AFTER `publish` had already put the new install.sh at the
# root, so the advertised one-liner was resolving a `stable` the failed promote never created.
# (Large objects went through by luck: a multipart copy sets no tagging directive, so exactly the
# small pointer files -- the manifest and its signature -- failed.) Nothing here has tags to lose.
# A `cp` from a LOCAL file has no source object to interrogate and needs none of this.
copy_key() {
	aws s3api copy-object --bucket "${2#s3://}" --key "$3" \
		--copy-source "${2#s3://}/$1" --endpoint-url "$4" >/dev/null
}

# Purge a list of URLs (stdin) at the edge. CDN-specific, so it rides behind two env vars —
# but a publish without them warns loudly, because the alternative is a silently stale
# channel: the CDN caches the artifacts (measured: max-age 14400 on the .zst files), and a
# pointer that moved while the edge still serves its old bytes fails every install closed for
# hours. Found live 2026-08-19; the first-ever publish went green only because nothing was
# cached yet. Versioned directories are immutable, so only POINTER paths and install.sh ever
# need purging.
purge_edge() {
	if [ -n "${RELEASE_PURGE_URL:-}" ] && [ -n "${RELEASE_PURGE_TOKEN:-}" ]; then
		need curl; need jq
		jq -R . | jq -s '{files: .}' |
		curl -sf -X POST -H "Authorization: Bearer $RELEASE_PURGE_TOKEN" \
			-H "Content-Type: application/json" "$RELEASE_PURGE_URL" --data @- |
		jq -e '.success == true' >/dev/null ||
			die "uploaded, but the edge purge FAILED -- the CDN may serve the previous pointer for hours; purge by hand, then verify"
		say "edge cache purged"
	else
		cat >/dev/null
		say "WARNING: RELEASE_PURGE_URL/RELEASE_PURGE_TOKEN unset -- the edge may serve the previous pointer for up to 4h (verify will rightly fail until it clears)"
	fi
}

# The files a pointer directory carries, in the upload order that fails closed: the bootstrap
# agent (if any) first, then the signature, then the manifest. A client racing the upload sees a
# coherent old pair, a coherent new pair, or a manifest/signature mismatch it refuses — never a
# manifest whose bytes are not there yet. (Observed on two consecutive publishes, 2026-08-19:
# `sync --delete` emitted both an upload and a delete for the manifest in one run and the delete
# won, so the live channel 404'd its manifest until a hand-run `cp` restored it. Pointers are
# never synced, only cp'd, one file at a time.)
POINTER_FILES="briard-agent briard-agent.exe manifest.json.sig manifest.json"

# Move ONE chain/arm's pointer onto a published version, server-side, in POINTER_FILES order.
# <chain> <version> <arm> <pointer> <bucket> <endpoint>
#
# ⚠️ ONE IMPLEMENTATION FOR BOTH POINTERS ([B.159](b)). `latest` and `stable` differ entirely in
# what they MEAN -- one says a release exists, the other that every node should take it, and the
# guards around them share nothing -- but moving one is the same act, and it used to be written
# twice: `publish` uploaded the pointer from the LOCAL staged directory while `promote` copied it
# server-side. The local upload was the weaker of the two, because it re-uploaded bytes rather
# than copying the ones that had just been published, so nothing checked that the pointer named
# what the versioned directory actually holds. A server-side copy cannot disagree.
move_pointer() {
	local c=$1 v=$2 arm=$3 ptr=$4 bucket=$5 endpoint=$6 rel p f
	rel=$(sub "$v" "$arm"); p=$(sub "$ptr" "$arm")
	for f in $POINTER_FILES; do
		have_key "$bucket/$c/$rel/$f" "$endpoint" || continue
		copy_key "$c/$rel/$f" "$bucket" "$c/$p/$f" "$endpoint"
	done
}

# Verify ONE served directory: the manifest's signature, that it names the chain and arm its path
# implies (and the version asked for, when one is), and that every artifact it lists DOWNLOADS and
# matches. <base-url> <chain> <arm> <want-version|"">; $tmp, $PUB and $CHANNEL come from `verify`.
#
# ⚠️ ONE IMPLEMENTATION FOR A POINTER AND FOR AN EXACT ID ([B.159](b)). A pointer is just a
# manifest at a path, so the checks are identical and the URL is the only thing that differs --
# which is exactly why this is a function rather than a second copy of the loop. Duplicating a
# signature check is how one copy quietly stops being run.
#
# It never checks a manifest against itself: every artifact is fetched from where the MANIFEST
# says it lives (the versioned directory), not from the path the manifest came from.
verify_manifest_at() {
	local base=$1 c=$2 arm=$3 want=$4 v ch pl vdir label
	label=${base#"$CHANNEL"/}
	curl -fsS "$base/manifest.json" -o "$tmp/manifest.json" || die "no manifest at $base"
	curl -fsS "$base/manifest.json.sig" -o "$tmp/manifest.json.sig" || die "no signature at $base"
	# Verify the way the agent does: raw Ed25519 over the exact bytes. A failure here is the
	# whole point of the check — an unsigned or re-signed channel must not read as green.
	ossl pkeyutl -verify -pubin -inkey "$PUB" -rawin \
		-in "$tmp/manifest.json" -sigfile "$tmp/manifest.json.sig" >/dev/null \
		|| die "the live manifest at $base does NOT verify against $PUB"
	v=$(jq -r .version "$tmp/manifest.json")
	ch=$(jq -r .chain "$tmp/manifest.json"); pl=$(jq -r '.platform // ""' "$tmp/manifest.json")
	[ "$ch" = "$c" ] && [ "$pl" = "$arm" ] ||
		die "$base serves a manifest for '$ch/$pl' — a crossed wire the client would refuse"
	# Asked for an exact id: the directory must not merely verify, it must be the one named. A
	# typo'd id that happened to resolve would otherwise gate the wrong release.
	[ -z "$want" ] || [ "$v" = "$want" ] || die "$base names $v, not the $want it was asked for"
	VERIFIED_V=$v
	vdir="$c/$(sub "$v" "$arm")"
	say "$label -> $v (signature verifies)"
	if [ -f "$tmp/$c.${arm:-flat}.$v.ok" ]; then echo "    (artifacts of $vdir already verified)"; return 0; fi
	jq -r '.artifacts[] | "\(.name) \(.sha256) \(.size)"' "$tmp/manifest.json" |
	while read -r name sum size; do
		curl -fsS "$CHANNEL/$vdir/$name" -o "$tmp/$name" || die "$name is in the $label manifest but not served at $vdir/"
		got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
		gotsize=$(stat -c%s "$tmp/$name")
		[ "$got" = "$sum" ] || die "$vdir/$name: sha256 $got != manifest $sum"
		[ "$gotsize" = "$size" ] || die "$vdir/$name: size $gotsize != manifest $size"
		echo "    ok  $vdir/$name  ($gotsize bytes)"
		# A POINTER serves its own bootstrap copy, which must be the SAME bytes the manifest
		# pins, or install.sh runs a bootstrap that is not the release it then installs. On a
		# versioned path $base IS $vdir, so this re-reads the file just checked — cheap, and it
		# keeps both callers on one path rather than adding a branch to skip it.
		case "$name" in briard-agent|briard-agent.exe)
			curl -fsS "$base/$name" -o "$tmp/$name.ptr" || die "$name is not served under $base (the bootstrap install.sh curls)"
			[ "$(sha256sum "$tmp/$name.ptr" | cut -d' ' -f1)" = "$sum" ] || die "$base/$name differs from $vdir/$name"
			echo "    ok  $label/$name  (bootstrap copy matches)"
		esac
	done
	touch "$tmp/$c.${arm:-flat}.$v.ok"
}
# ⚠️ EVERY SERVER-SIDE (s3->s3) COPY BELOW CARRIES `--copy-props none`. awscli2's `cp` between two
# S3 keys reads the source object's tags first (GetObjectTagging) so it can carry them across, and
# R2 does not implement that API -- without the flag the call dies `NotImplemented`. The FIRST
# `promote` of this tree hit exactly that, AFTER `publish` had already put the new install.sh at
# the root, so the advertised one-liner was resolving a `stable` the failed promote never created.
# Nothing here has tags to lose. A `cp` from a LOCAL file has no source object to interrogate, so
# the upload paths need nothing.

case "${1:-}" in

stage)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix; need sha256sum; need jq
	V=$(release_version)
	# THE VM RELEASE THIS BRIARD RELEASE PAIRS WITH ([B.86i]). The image's inputs hash comes from
	# the flake; if the live channel already serves an image with these exact inputs, that release
	# is reused -- nothing of the vm chain is staged, and the briard manifest names it -- else a
	# new id is minted: the commit date (so the same commit always mints the same id, and the
	# timer's stable path, which orders on the date, sees a real step) and the inputs' short hash.
	INPUTS=$(nix eval --raw .#artifacts.guest-disk.inputs) || die "cannot read the guest image's inputs hash"
	[ "${#INPUTS}" = 64 ] || die "the guest inputs hash is not a sha256 ($INPUTS)"
	VM_REUSED=""
	if GV=$(live_vm_for_inputs "$INPUTS"); then
		VM_REUSED=1
		say "staging release $V into $DIR -- the guest image is unchanged, pairing with the published $GV"
	else
		GV="vm.$(date_of "$V").${INPUTS:0:7}"
		say "staging release $V (vm $GV, new image inputs ${INPUTS:0:12}) into $DIR"
	fi
	rm -rf "$DIR"; mkdir -p "$DIR"
	echo "$GV" > "$DIR/VM"                                  # the pair, for sign/publish/promote
	[ -z "$VM_REUSED" ] || touch "$DIR/VM_REUSED"         # ...and whether it is staged here
	out_of() { nix build --no-link --print-out-paths "$1"; }
	zst() { # in out — -19 not --ultra: 23s and 377 MB against gzip -9's 85s and 465 MB, on a
	        # file published once and downloaded by every household. -T0 uses the release box's
	        # cores; it does not change the output, so the manifest hash is unaffected by what
	        # machine staged it.
		nix run nixpkgs#zstd -- -19 -T0 -q --rm "$1" -o "$2" || die "compressing $1 failed"
		chmod 0644 "$2"
	}
	# Deterministic tar: fixed mtime/owner and a sorted member order, or the same tree would
	# produce a different sha256 on every run and the manifest would churn for no reason.
	dtar() { tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner -cf "$@"; }

	# THE BRIARD CHAIN, LINUX ARM: agent + net-wrap + qemu, one release ([B.86b]: they move as one
	# bundle and commit as one, so they are published as one). Copy, never symlink: the artifacts
	# are uploaded as bytes, and a store symlink would publish a dangling link. `install -m` sets
	# the mode the manifest then records.
	H="$DIR/briard/$V/linux"; mkdir -p "$H"
	install -m0755 "$(out_of .#artifacts.agent)/bin/briard-agent"        "$H/briard-agent"
	install -m0755 "$(out_of .#artifacts.net-wrap)/bin/briard-net-wrap"  "$H/briard-net-wrap"
	# The systemd units, shipped verbatim rather than written by install.sh ([B.157]). Read from
	# the working tree like install.sh below, not from a store path: they are static text with no
	# build step. They are ordinary artifacts from here on -- the manifest hashes them and the
	# signature covers them, which install.sh's heredocs never were.
	install -m0644 scripts/units/briard-agent.service   "$H/briard-agent.service"
	install -m0644 scripts/units/briard-update.service  "$H/briard-update.service"
	install -m0644 scripts/units/briard-update.timer    "$H/briard-update.timer"
	# The agent's three frozen scripts, same reasoning: self-update's pivot pair and the updater,
	# shipped verbatim rather than rendered by install.sh ([B.157]). 0755 -- the manifest records a
	# mode only when it is not the 0644 default, and these are run.
	install -m0755 scripts/agent/briard-exec    "$H/briard-exec"
	install -m0755 scripts/agent/briard-commit  "$H/briard-commit"
	install -m0755 scripts/agent/briard-update  "$H/briard-update"
	# qemu-bundle is a DIRECTORY in the store (bin/ lib/ share/ PROVENANCE) and the contract
	# wants one file, so it is tarred here.
	dtar "$H/qemu-bundle.tar" -C "$(out_of .#artifacts.qemu-bundle)" .
	zst "$H/qemu-bundle.tar" "$H/qemu-bundle.tar.zst"
	# THE GUEST BUNDLE ([B.86j]): the briard binaries the guest runs ride the briard chain and are
	# pushed into the guest by the agent; the image bakes only the guest agent ([B.138]). Same shape as the
	# qemu bundle (a tarred directory), hash-skipped by the update path when unchanged.
	dtar "$H/guest-bundle.tar" -C "$(out_of .#artifacts.guest-bundle)" .
	zst "$H/guest-bundle.tar" "$H/guest-bundle.tar.zst"
	# install.sh, WITH THE RELEASE PUBKEY EMBEDDED, AS AN ARTIFACT OF THIS RELEASE ([B.159](a)).
	# The source tree carries a placeholder and the script dies on it by design ("the embedded key
	# is a build placeholder"), so shipping it unsubstituted would publish an installer that
	# refuses to install.
	#
	# It sits in the briard chain's linux arm, which buys it exactly the three things [B.157] bought
	# the frozen scripts and the units: versioned, diffable, and covered by the release signature.
	# It is ALSO byte-copied to the channel root -- but only by `promote`, never by `publish`. The
	# root URL is what the advertised one-liner fetches on every `stable` install, so writing it
	# at publish time was the one thing that made publishing a release a live change to what
	# strangers run, and [B.159] exists because it stops being one. That root copy is still
	# fetched before anything exists that could verify it -- unchanged, and why the repo being
	# public and this script readable is the real answer to the `curl | sh` objection.
	#
	# ⚠️ IT KEEPS DEFAULTING TO `RELEASE=stable` AND IS NOT STAMPED WITH ITS OWN ID. Stamping would
	# make the deeper link self-contained, and would also make the ROOT copy name an exact id --
	# so an installer somebody saved to disk months ago would silently install THAT release rather
	# than healing forward to current stable, which is the property "old installer, new artifacts"
	# rests on. Installing an exact id passes BRIARD_RELEASE beside the URL instead.
	[ -n "${RELEASE_PUBKEY:-}" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key (embedded into install.sh)"
	grep -q "BEGIN PUBLIC KEY" "$RELEASE_PUBKEY" || die "$RELEASE_PUBKEY is not a PEM PUBLIC KEY"
	awk -v keyfile="$RELEASE_PUBKEY" '
		/^RELEASE_KEYRING_PEM=/ {
			printf "RELEASE_KEYRING_PEM='"'"'"
			while ((getline line < keyfile) > 0) print line
			printf "'"'"'\n"
			next
		} { print }' scripts/install.sh > "$H/install.sh"
	chmod 0755 "$H/install.sh"
	grep -q "__BRIARD_RELEASE_KEYRING_PEM__" "$H/install.sh" \
		&& die "the keyring placeholder survived — the published installer would refuse to install"
	grep -q "BEGIN PUBLIC KEY" "$H/install.sh" \
		|| die "no public key landed in the staged install.sh"
	sh -n "$H/install.sh" || die "the staged install.sh is not valid shell after substitution"

	# The manifest, written BY THE AGENT rather than by this script: the format is a contract
	# between the publisher and every installing node, and it used to have two implementations
	# (a printf loop here, hand-assembling `"mode":493`, and the struct in agent/install). The
	# writer now shares its types with the reader, so the format cannot disagree with itself —
	# and the binary used is the one STAGED IN THIS DIRECTORY, the exact agent this release
	# ships, so the manifest is written by the same build that will later read it on a node.
	[ -x "$H/briard-agent" ] || die "no staged briard-agent to write the manifests with"
	"$H/briard-agent" --stage-manifest "$H" --chain briard --platform linux --release "$V" --vm "$GV" || die "writing the linux manifest failed"

	# THE WINDOWS ARM. `FetchVerified` downloads EVERY artifact a manifest names, so a
	# Windows-only bundle in the Linux manifest would make every Linux install pull tens of MB it
	# can never run. An arm of its own gives it the identical shape — a signed manifest, its
	# signature, the artifacts it names, the two pointers — and a Windows installer will later
	# name it the way the Linux one names `linux`. One path, run twice: no second protocol, no
	# platform field on the wire beyond the manifest's own, and no client change on either side.
	W="$DIR/briard/$V/windows"; mkdir -p "$W"
	dtar "$W/qemu-bundle-windows.tar" -C "$(out_of .#artifacts.qemu-bundle-windows)" .
	zst "$W/qemu-bundle-windows.tar" "$W/qemu-bundle-windows.tar.zst"
	"$H/briard-agent" --stage-manifest "$W" --chain briard --platform windows --release "$V" --vm "$GV" || die "writing the windows manifest failed"

	# THE VM CHAIN: the OS image, its own series, no platform level, and staged ONLY when its
	# inputs changed (above). Measured: 1178 -> 377 MB compressed, which is what a household link
	# actually waits on -- and what every household was re-downloading for a version string.
	MANIFESTS="$H $W"
	if [ -z "$VM_REUSED" ]; then
		G="$DIR/vm/$GV"; mkdir -p "$G"
		install -m0644 "$(out_of .#artifacts.guest-disk)/nixos.qcow2" "$G/nixos.qcow2"
		zst "$G/nixos.qcow2" "$G/nixos.qcow2.zst"
		# The closure the image boots, the oldest briard that tolerates this VM ([B.86d]) -- this
		# very release's briard, the one it is built beside, and the tightest correct value: a node
		# takes briard/stable daily and vm/stable rarely, so it is at or past this by the time
		# the guest is promoted -- and the inputs hash that makes "unchanged" decidable next time.
		"$H/briard-agent" --stage-manifest "$G" --chain vm --release "$GV" \
			--system "$(out_of .#artifacts.guest-disk.system)" --min-briard "$V" --inputs "$INPUTS" \
			|| die "writing the vm manifest failed"
		MANIFESTS="$MANIFESTS $G"
	fi

	for m in $MANIFESTS; do
		jq -e . "$m/manifest.json" >/dev/null || die "the manifest at $m is not valid JSON"
	done

	echo "$V" > "$DIR/VERSION" # not part of the tree; a human-readable marker for the operator
	say "staged $V:"
	for c in $CHAINS; do
		if [ "$c" = vm ] && [ -n "$VM_REUSED" ]; then
			echo "  vm/$GV  (published already; unchanged inputs -- paired, not staged)"; continue
		fi
		for a in $(arms_of "$c"); do arm=${a#-}
			m="$DIR/$c/$(sub "$([ "$c" = vm ] && echo "$GV" || echo "$V")" "$arm")/manifest.json"
			echo "  $c/$(sub "$(jq -r .version "$m")" "$arm")"
			jq -r '.artifacts[] | "    \(.name)  \(.size) bytes  \(.sha256[0:16])…"' "$m"
		done
	done
	say "next: ./scripts/publish-release.sh sign $DIR"
	;;

sign)
	DIR="${2:-$STAGE_DEFAULT}"
	[ -n "${RELEASE_SIGN_KEY:-}" ] || die "set RELEASE_SIGN_KEY to the PKCS8 PEM Ed25519 private key"
	[ -e "$RELEASE_SIGN_KEY" ] || die "no signing key at $RELEASE_SIGN_KEY"
	for c in $CHAINS; do
		if [ "$c" = vm ] && [ -f "$DIR/VM_REUSED" ]; then
			say "vm chain: $(cat "$DIR/VM") is reused (unchanged inputs) -- nothing to sign"; continue
		fi
		v=$(staged_version "$DIR/$c") || die "no staged version under $DIR/$c — run \`stage\` first"
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm"); m="$DIR/$c/$rel/manifest.json"
			[ -f "$m" ] || die "no manifest at $m"
			# -rawin is what makes this PureEdDSA over the whole file, which is what Verify does.
			# Without it openssl would pre-hash and every signature would be rejected.
			ossl pkeyutl -sign -inkey "$RELEASE_SIGN_KEY" -rawin -in "$m" -out "$m.sig"
			n=$(stat -c%s "$m.sig")
			[ "$n" = 64 ] || die "$m.sig is $n bytes, want a raw 64 — the verifier rejects anything else"
			say "signed $c/$rel (64-byte detached Ed25519)"
		done
	done
	;;

publish)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	# The installer is an artifact of the LINUX ARM and lives nowhere else in a staging dir
	# ([B.164]: stage lays no root copy, and `promote` is what puts one at the channel root). The
	# check itself is unchanged: a staging dir carrying no installer was built before the keyring
	# was embedded, and publishing it would ship a release nobody can install.
	[ -f "$DIR/briard/$(staged_version "$DIR/briard")/linux/install.sh" ] ||
		die "no install.sh under $DIR/briard/<version>/linux — run \`stage\` (it embeds the keyring)"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	say "publishing $(cat "$DIR/VERSION" 2>/dev/null || echo '?') to $RELEASE_WRITE"

	# THE PAIR: which vm release this briard release names, and whether it is staged here or
	# already published (a reuse). A reused guest must actually BE in the bucket, or the host
	# manifest would name a release no installer can fetch.
	GV=$(cat "$DIR/VM" 2>/dev/null) || die "no $DIR/VM — run \`stage\` first"
	VM_REUSED=""; [ ! -f "$DIR/VM_REUSED" ] || VM_REUSED=1
	if [ -n "$VM_REUSED" ]; then
		have_key "$bucket/vm/$GV/manifest.json" "$endpoint" ||
			die "the briard manifest names vm/$GV as its pair, but the bucket does not hold it — stage again against the live channel"
	fi

	# IMMUTABILITY FIRST, across every chain, before a byte moves: a half-published release
	# (briard uploaded, vm refused) would leave `latest` naming a pair nobody tested together.
	for c in $CHAINS; do
		[ "$c" = vm ] && [ -n "$VM_REUSED" ] && continue
		v=$(staged_version "$DIR/$c") || die "no staged version under $DIR/$c"
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm")
			[ -f "$DIR/$c/$rel/manifest.json.sig" ] || die "$c/$rel is unsigned — run \`sign\` before publishing"
			! have_key "$bucket/$c/$rel/manifest.json" "$endpoint" ||
				die "$c/$rel is ALREADY PUBLISHED and versioned directories are immutable (see header) — to re-point, \`promote\`; to ship a fix, commit and stage again"
		done
	done

	for c in $CHAINS; do
		# A reused vm release is already in the bucket, checked above; there is nothing to upload and
		# no pointer to move here any more ([B.159](b)).
		[ "$c" = vm ] && [ -n "$VM_REUSED" ] && continue
		v=$(staged_version "$DIR/$c")
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm")
			# The versioned directory: artifacts first, manifest pair last. No --delete anywhere
			# in this script any more: nothing is ever overwritten, so there is nothing to clean
			# up — and the bucket ALSO holds `catalog/` (live runtime content the agent fetches
			# for `briard app install`, produced by nothing in this repo), which a wide --delete
			# would silently remove.
			aws s3 sync "$DIR/$c/$rel" "$bucket/$c/$rel/" --endpoint-url "$endpoint" \
				--exclude manifest.json --exclude manifest.json.sig --exclude "*/*" --no-progress
			aws s3 cp "$DIR/$c/$rel/manifest.json.sig" "$bucket/$c/$rel/manifest.json.sig" --endpoint-url "$endpoint" --no-progress
			aws s3 cp "$DIR/$c/$rel/manifest.json"     "$bucket/$c/$rel/manifest.json"     --endpoint-url "$endpoint" --no-progress
			say "published $c/$rel"
		done
	done

	# ⚠️ NOTHING THAT POINTS AT A RELEASE IS WRITTEN HERE, AND THAT IS THE POINT ([B.159]).
	# `publish` used to move `latest` and overwrite the root install.sh in the same breath as the
	# upload, which made publishing a release a live change to what the fleet and what strangers
	# get — so the only place a gate could stand was BEFORE the upload, on bytes the CDN had never
	# served. An id nothing names reaches nobody: it is fetchable by whoever knows it (the ids are
	# commit-derived and the repo is public, so "untagged" is not "private"), signed, immutable,
	# and advertised by not one path. That is what lets the gates run on the bytes the CDN
	# actually serves. `latest` is moved by the `latest` subcommand once they pass; `stable` and
	# the root install.sh by `promote`.
	#
	# The cost, stated rather than discovered: a release that fails its gate is dead bytes in the
	# bucket until `gc` — immutable, named by no pointer, which is precisely the state `gc` was
	# written to collect.
	#
	# NOTHING NEEDS PURGING EITHER. Only pointer paths and the root installer are ever
	# overwritten, and this step now writes neither; a versioned directory is immutable, so the
	# edge has nothing stale to hold.
	say "published — now run: ./scripts/publish-release.sh verify $(cat "$DIR/VERSION" 2>/dev/null || echo '<version>')"
	say "   nothing points at it yet: \`latest\` once the gates pass, \`promote\` once the evidence is in"
	;;

latest)
	# MOVE `latest` ONTO A PUBLISHED RELEASE ([B.159](b)) — the second half of a publish, run once
	# the gates have passed on the exact id. Separate from `publish` because that is the whole
	# point of the item: uploading is inert, and this is the step that makes a release visible.
	#
	# It is NOT `promote` with a different argument, though it shares `move_pointer` with it.
	# `stable` is what every installed node converges to and what a stranger gets, so promotion
	# carries the no-same-date rule, the root installer and an evidence bar. `latest` says only
	# "this exists and the gates liked it": it is what a cloud canary pins and what `briard update
	# self -to latest` asks for by name — nothing arrives at it by default any more ([B.159](f)).
	# Two decisions, one mechanism.
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	V="${2:-}"
	if [ -z "$V" ]; then
		V=$(cat "$STAGE_DEFAULT/VERSION" 2>/dev/null) || die "no version given and no $STAGE_DEFAULT/VERSION to read one from"
		say "no version given — taking the staged $V"
	fi
	# The PAIR moves together or not at all, the same obligation `promote` carries: `latest` on
	# both chains must name the two releases that were staged, gated and verified beside each
	# other. The briard manifest is where that pairing lives ([B.86i]), so it is read from the
	# PUBLISHED manifest rather than from anything local — this verb is about what is in the
	# bucket, and a stage directory may be a different build by now.
	have_key "$bucket/briard/$V/linux/manifest.json" "$endpoint" || die "briard/$V is not published; nothing to point at"
	GV=$(curl -fsS "$CHANNEL/briard/$V/linux/manifest.json" | jq -r '.vm // ""') || die "cannot read briard/$V/linux to find its vm pair"
	[ -n "$GV" ] || die "briard/$V names no vm release — published before [B.86i]; stage and publish again"
	have_key "$bucket/vm/$GV/manifest.json" "$endpoint" ||
		die "briard/$V pairs with vm/$GV, which the bucket does not hold — the pair cannot be pointed at"
	for a in $(arms_of briard); do arm=${a#-}
		move_pointer briard "$V" "$arm" latest "$bucket" "$endpoint"
		say "briard/$(sub latest "$arm") -> $V"
	done
	move_pointer vm "$GV" "" latest "$bucket" "$endpoint"
	say "vm/latest -> $GV"
	{
		for a in $(arms_of briard); do arm=${a#-}
			for f in $POINTER_FILES; do echo "$CHANNEL/briard/$(sub latest "$arm")/$f"; done
		done
		for f in $POINTER_FILES; do echo "$CHANNEL/vm/latest/$f"; done
	} | purge_edge
	say "latest -> $V (vm $GV) — now run: ./scripts/publish-release.sh verify"
	;;

promote)
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	V="${2:-}"
	if [ -z "$V" ]; then
		V=$(curl -fsS "$CHANNEL/briard/latest/linux/manifest.json" | jq -r .version) || die "cannot read briard/latest to default the version"
		say "no version given — promoting what briard/latest names: $V"
	fi
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
	# Every chain and arm is checked before any of them moves: promotion is of the PAIR
	# (briard/stable + vm/stable is the tested pair by construction — there is no top-level
	# install pointer, and this is the obligation that stands in for one).
	for c in $CHAINS; do
		v=$(chain_id "$c" "$V")
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm"); p=$(sub stable "$arm"); tag="$c.${arm:-flat}"
			have_key "$bucket/$c/$rel/manifest.json" "$endpoint" || die "$c/$rel is not published; nothing to promote"
			# [B.159](a): the root installer is a byte-copy of the promoted release's, so that
			# release has to carry one. A release staged before [B.159] does not, and promoting it
			# would move every pointer and leave the root serving the PREVIOUS installer — the
			# silent half-promotion this whole check loop exists to refuse.
			if [ "$c" = briard ] && [ "$arm" = linux ]; then
				have_key "$bucket/$c/$rel/install.sh" "$endpoint" ||
					die "$c/$rel carries no install.sh — it was staged before [B.159](a); re-stage and publish it"
			fi
			if curl -fsS "$CHANNEL/$c/$p/manifest.json" -o "$tmp/$tag.stable.json" 2>/dev/null; then
				cur=$(jq -r .version "$tmp/$tag.stable.json")
				if [ "$cur" = "$v" ]; then
					say "$c/$p already names $v"
				else
					# THE NO-SAME-DATE RULE ([B.86a]). The timer's stable path orders on the
					# date field alone and deliberately has no same-date rule (one would make
					# the timer revert a cloud pin that shares a date with stable). So the
					# constraint sits here, in the layer we can fix: a build whose date equals
					# the promoted one is invisible to the fleet, and promoting it would only
					# LOOK like a release.
					[ "$(date_of "$cur")" != "$(date_of "$v")" ] ||
						die "$c/$p is $cur, same date as $v — the timer cannot tell them apart; a same-day fix-up needs the next day's number"
				fi
			else
				say "$c/$p has no stable yet — this promotion creates it"
			fi
		done
	done
	for c in $CHAINS; do
		v=$(chain_id "$c" "$V")
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm"); p=$(sub stable "$arm"); tag="$c.${arm:-flat}"
			if [ "$(jq -r .version "$tmp/$tag.stable.json" 2>/dev/null)" != "$v" ]; then
				move_pointer "$c" "$v" "$arm" stable "$bucket" "$endpoint"
				say "promoted $c/$p -> $v"
			fi
		done
	done
	# THE CHANNEL ROOT'S INSTALLER, byte-copied from the release just promoted ([B.159](a)) and
	# laid LAST, after every pointer — so the root can never advertise a `stable` that is not
	# there yet, which is the ordering the first publish of this tree got wrong.
	#
	# Unconditional, rather than folded into the pointer loop above: the pointer may already name
	# $V (a re-run, or a promote that died after the pointers moved) while the root still serves
	# the previous release's installer, and repairing exactly that half-laid state is what a
	# re-run is for.
	copy_key "briard/$V/linux/install.sh" "$bucket" "install.sh" "$endpoint"
	say "install.sh at the root -> briard/$V/linux/install.sh"
	{
		for c in $CHAINS; do
			for a in $(arms_of "$c"); do arm=${a#-}
				for f in $POINTER_FILES; do echo "$CHANNEL/$c/$(sub stable "$arm")/$f"; done
			done
		done
		echo "$CHANNEL/install.sh"
	} | purge_edge
	say "promoted $V — now run: ./scripts/publish-release.sh verify"
	;;

gc)
	shift
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	# gc DELETES (owner, 2026-09-09), and what makes that safe is that nothing needs the old
	# BYTES — only the old CODE, which git has. An id is `v3.<commit-date>.<shortrev>`, so the
	# property a client depends on is "an id never names two different byte-sets", not "bytes
	# live forever"; shipping old code is a REVERT COMMIT, which is a new rev under a new id and
	# satisfies that trivially. `release_version`'s stale-commit floor is the other half.
	#
	# AND THERE IS NO COLD ARCHIVE, deliberately. The case for one is "the bytes a rollback
	# re-points to", and agent/install/update.go does not support it: `stable` installs only when
	# date(have) < date(want), so a backward pointer move reaches new installs and cloud pins and
	# no installed node. Against that: a second bucket, a second credential's blast radius, and a
	# move R2 cannot perform — `s3 mv` fetches object tags for any copy past the 8 MB multipart
	# threshold, i.e. every image and every agent, so it half-moves a release and leaves a
	# manifest whose bytes are elsewhere, the one state this file exists to prevent (measured
	# against the live bucket, 2026-09-09). `--keep` and the floor cover the pin case instead.
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	# Pins live in the cloud's rollout state, which this script cannot see; the operator names
	# them. A release that is pinned and not named here is DELETED, and the pinned node's next
	# tick fails its fetch loudly (a failed directive, not a silent no-op) — recovered by
	# publishing a revert commit and re-pointing the pin at it, not by restoring bytes. This is
	# the one place `--keep` earns its keep, and the floor is what buys time to use it.
	keep=""
	while [ $# -gt 0 ]; do
		case "$1" in --keep) keep="$keep $2"; shift 2 ;; *) die "gc: unknown argument $1" ;; esac
	done
	floor=$(date -u -d "-$GC_FLOOR_DAYS days" +%Y-%m-%d)
	for c in $CHAINS; do
		# The first arm (or the flat one) stands for the release: every arm of a version is
		# published and promoted together, so one manifest's pointer and LastModified speak for all.
		first=$(arms_of "$c" | cut -d' ' -f1); first=${first#-}
		live=""
		for p in stable latest; do
			live="$live $(curl -fsS "$CHANNEL/$c/$(sub "$p" "$first")/manifest.json" 2>/dev/null | jq -r .version)"
		done
		aws s3 ls "$bucket/$c/" --endpoint-url "$endpoint" | awk '/ PRE /{print $2}' | tr -d / |
		while read -r v; do
			case "$v" in stable|latest) continue ;; esac
			case " $live " in *" $v "*) echo "  keep  $c/$v (a pointer names it)"; continue ;; esac
			pinned=""
			for k in $keep; do
				if [ "$(chain_id "$c" "$k")" = "$v" ]; then pinned=1; fi
			done
			if [ -n "$pinned" ]; then echo "  keep  $c/$v (pinned)"; continue; fi
			# Age from the bucket's own clock (the manifest's LastModified), not the id's date:
			# the id says when it was committed, the floor is about when it was published.
			when=$(aws s3 ls "$bucket/$c/$(sub "$v" "$first")/manifest.json" --endpoint-url "$endpoint" | awk '{print $1}')
			if [ -z "$when" ] || [ "$when" \> "$floor" ]; then
				echo "  keep  $c/$v (published $when, inside the $GC_FLOOR_DAYS-day floor)"; continue
			fi
			# WHOLE RELEASES, NEVER FILES: a release directory is what a signed manifest names,
			# and a partial one is a manifest whose bytes are gone — indistinguishable, to a
			# client, from an attack. `rm --recursive` needs no server-side copy, so it has no
			# object-tagging call to trip on and cannot leave a release half-removed the way a
			# move does — but the rule stands for whatever ever replaces it.
			say "deleting $c/$v (published $when, past the ${GC_FLOOR_DAYS}-day floor)"
			aws s3 rm "$bucket/$c/$v/" --recursive --endpoint-url "$endpoint" --no-progress
		done
	done
	;;

verify)
	need curl; need sha256sum; need jq
	PUB="${RELEASE_PUBKEY:-${RELEASE_SIGN_KEY:-}}"
	[ -n "$PUB" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key"
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

	# ONE EXACT RELEASE, NAMED ([B.159](b)) — what follows a `publish` now that publishing points
	# nothing at the release. The same checks a pointer gets, at the versioned path, plus the
	# vm release it pairs with. No pointer check and no root installer, because this release is not
	# claiming to be either yet; that is the whole state being verified.
	if [ -n "${2:-}" ]; then
		V="$2"
		say "verifying $V where it was published — no pointer names it yet"
		for a in $(arms_of briard); do arm=${a#-}
			verify_manifest_at "$CHANNEL/briard/$(sub "$V" "$arm")" briard "$arm" "$V"
		done
		GV=$(curl -fsS "$CHANNEL/briard/$V/linux/manifest.json" | jq -r '.vm // ""') ||
			die "cannot read briard/$V/linux — is $V published?"
		[ -n "$GV" ] || die "briard/$V names no vm release — published before [B.86i]; stage and publish again"
		verify_manifest_at "$CHANNEL/vm/$GV" vm "" "$GV"
		# The installer a gate on this id will actually curl ([B.159](a)). Its BYTES are already
		# checked — it is an artifact of the linux arm above — so what this adds is that the
		# deeper URL serves it, which is the one the gate names: the advertised root URL still
		# serves the PROMOTED release and cannot reach this one at all.
		curl -fsS -o /dev/null "$CHANNEL/briard/$V/linux/install.sh" ||
			die "briard/$V/linux/install.sh is not served — an install gate on $V has no installer to fetch"
		say "$V verifies: both arms, vm $GV, every artifact matching, and its own install.sh served"
		say "   install it with: BRIARD_RELEASE=$V, fetching $CHANNEL/briard/$V/linux/install.sh"
		exit 0
	fi

	# EVERY CHAIN, EVERY ARM, BOTH POINTERS. A pointer is a manifest at a path, so verifying one
	# is verifying all of them with the path changed — and a pointer nobody verifies is a pointer
	# nobody knows is broken until a stranger (or the timer, fleet-wide) runs into it.
	# `stable` may not exist yet on a fresh tree; that is said out loud rather than failed,
	# because the first publish of the tree is the one run where it is expected.
	for c in $CHAINS; do
		for a in $(arms_of "$c"); do arm=${a#-}
			for p in stable latest; do
				rel=$(sub "$p" "$arm"); base="$CHANNEL/$c/$rel"
				# `stable` may not exist yet on a fresh tree; that is said out loud rather than
				# failed, because the first publish of the tree is the one run where it is
				# expected. A missing `latest` stays fatal: since [B.159](b) a release can sit
				# published with nothing naming it, but that is what `verify <VERSION>` is for —
				# reaching here means the pointers are being checked, and one of them is gone.
				if ! curl -fsS -o /dev/null "$base/manifest.json" 2>/dev/null; then
					[ "$p" = stable ] && { say "WARNING: no $c/$rel yet — nothing promoted here"; continue; }
					die "no manifest at $base"
				fi
				verify_manifest_at "$base" "$c" "$arm" ""
			done
		done
	done
	# THE PAIR HOLDS AT BOTH POINTERS ([B.86i]): what briard/<p> names as its vm is what vm/<p>
	# serves. This is the obligation "briard/stable + vm/stable is the tested pair" now rests on,
	# since the vm id is no longer derivable from the briard id; a pointer moved by hand on one
	# chain and not the other fails here, before an installer meets it.
	for p in stable latest; do
		hg=$(curl -fsS "$CHANNEL/briard/$p/linux/manifest.json" 2>/dev/null | jq -r '.vm // ""') || hg=""
		gv=$(curl -fsS "$CHANNEL/vm/$p/manifest.json" 2>/dev/null | jq -r .version) || gv=""
		[ -n "$hg$gv" ] || continue # neither exists yet (a fresh tree's stable): said above
		[ -n "$hg" ] || die "briard/$p names no vm release — published before [B.86i]; stage and publish again"
		[ "$hg" = "$gv" ] || die "briard/$p pairs with vm/$hg but vm/$p serves $gv — the pointers disagree"
		say "briard/$p <-> vm/$p agree on $gv (the tested pair)"
	done
	# install.sh at the channel root -- that is the URL the advertised one-liner names, so it
	# is the one this must assert. The installer a stranger runs must agree with where the
	# artifacts actually are: a published install.sh still defaulting to an old layout would
	# fetch nothing and fail closed, which is safe but silent -- and would not be caught by any
	# check above, since every one of them uses $CHANNEL rather than what the script believes.
	# Downloaded to a FILE and then grepped, never `curl … | grep -q`: `-q` exits on the first
	# match, which closes the pipe under a curl that is still writing; curl then dies with "(23)
	# Failure writing output to destination" and `||` reads that as the assertion failing.
	curl -fsS "$CHANNEL/install.sh" -o "$tmp/install.sh" || die "install.sh is not fetchable at $CHANNEL/install.sh"
	grep -q "BRIARD_CHANNEL_URL:-$CHANNEL" "$tmp/install.sh" ||
		die "the served install.sh does not default to $CHANNEL — it would look for the tree in the wrong place"
	# ...and it is the PROMOTED RELEASE'S installer, byte for byte ([B.159](a)). Since the root
	# copy is unsigned by construction — the one-liner fetches it before anything exists that
	# could verify a signature — this equality is the only thing that ties it to the signed set
	# at all: matching `briard/stable/linux/install.sh` makes it exactly as trustworthy as that
	# manifest, which the loop above already verified against the release key. Without it the
	# root could serve any installer at all and every check above would still pass.
	#
	# Skipped only when nothing is promoted yet (a fresh tree), which the loop above already
	# reported; the sha of a 404 would otherwise read as a mismatch and bury that.
	if curl -fsS "$CHANNEL/briard/stable/linux/manifest.json" -o "$tmp/stable.json" 2>/dev/null; then
		sv=$(jq -r .version "$tmp/stable.json")
		want=$(jq -r '.artifacts[] | select(.name=="install.sh") | .sha256' "$tmp/stable.json")
		[ -n "$want" ] ||
			die "briard/stable ($sv) names no install.sh — it was published before [B.159](a), so nothing pins what the root serves"
		got=$(sha256sum "$tmp/install.sh" | cut -d' ' -f1)
		[ "$got" = "$want" ] ||
			die "the root install.sh is NOT briard/stable/linux/install.sh ($got != $want) — a promote that did not finish, or a hand-edited root"
		say "install.sh at the root is briard/$sv/linux/install.sh, byte for byte"
	fi
	say "$CHANNEL verifies end to end: every pointer signed, every artifact matching, install.sh served at the root and pointing here"
	;;

*)
	die "usage: publish-release.sh {stage|sign|publish|latest|promote|gc|verify} [ARGS]  (see header)"
	;;
esac
