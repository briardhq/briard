#!/usr/bin/env bash
#
# publish-release.sh — build, sign and publish the release channel a stranger installs from
# and a node updates from. The consumer already exists (agent/install/fetch.go +
# scripts/install.sh), so this script has no latitude: everything below is read off that verifier,
# not designed here, and must emit exactly what it reads, or installs refuse.
#
# The tree, at <BRIARD_CHANNEL_URL> (default https://get.briard.io):
#
#   install.sh                          a byte-copy of briard/stable/linux-amd64/install.sh, laid by
#                                       `promote`; unsigned where it is served, since
#                                       the one-liner fetches it before any verification exists
#   install.ps1                         likewise, of briard/stable/windows-amd64/install.ps1
#   briard-setup.exe                    likewise, of briard/stable/windows-amd64/briard-setup.exe -- the
#                                       download page's installer, which installs current stable
#   briard/
#     <version>/linux-amd64/            manifest.json(+.sig), briard-agent, briard-net-wrap,
#                                       install.sh, keyring.pem,
#                                       briard-update,
#                                       qemu-bundle.tar.zst, guest-bundle.tar.zst,
#                                       briard-{agent,update}.service, briard-update.timer
#     <version>/windows-amd64/          manifest.json(+.sig), briard-agent.exe, briard-tray.exe, install.ps1, briard-update.ps1, keyring.pem,
#                                       qemu-bundle-windows.tar.zst, guest-bundle.tar.zst, tap-windows.exe,
#                                       briard-setup.exe
#     latest/{linux,windows}-amd64/     manifest.json(+.sig), briard-agent / briard-agent.exe
#     stable/{linux,windows}-amd64/     likewise
#     dev/{linux,windows}-amd64/        likewise -- the one ungated pointer (see `dev`)
#   vm/
#     <version>/amd64/                  manifest.json(+.sig), nixos.qcow2.zst
#     {latest,stable,dev}/amd64/        manifest.json(+.sig)
#   setup/
#     <version>/briard-store-setup.exe  the Store's installer, carrying that release's windows arm; written once,
#                                       outside both chains (`setup`)
#
# A chain is a release line with its own version series: the briard bundle moves as
# `v<epoch>.<date>.<rev>` on every publish, the guest OS as `vm.<date>.<inputs>` and only when its
# inputs change. The guest image is a function of its inputs (flake.nix guestInputs: the image
# recipe, the packages built into it, the Go packages the guest binary links, the module files,
# the nixpkgs pin) with no commit-derived stamp, so `stage` asks the live channel for a release
# with these exact inputs and reuses it rather than staging a 400 MB image nobody could tell from
# the last one. The pairing therefore lives in the briard manifest: `vm` names the vm release it
# was staged beside, which an installer fetches after the briard chain and `promote` moves
# vm/stable to. Each chain has one more level, the platform arm: the briard bundle per host OS
# and CPU (`linux-amd64`), the guest image per CPU (`amd64`: the same VM on every host OS, on the
# host's CPU).
#
# A pointer is just a path serving a byte-copy of one signed manifest: no pointer file, no second
# signature format, one verified hop. The client resolves every artifact against the `version`
# field of the manifest (<chain>/<version>/<arm>/<name>), never the path it fetched it from, so a
# pointer costs a few KB, not a second 380 MB image. The one exception is `briard-agent` (and its
# .exe), duplicated under the host pointers: install.sh has to curl a bootstrap before anything
# exists that can parse a manifest, and it must come from the target (a stale one that cannot
# parse a newer manifest is the forward-compat bricking the channel layout exists to prevent).
#
#   manifest.json      {"chain","platform","version","artifacts":[{"name","sha256","size","mode"}]},
#                      sha256 lowercase hex, mode omitted => 0644; plus the optional vm,
#                      system, min_briard, inputs and min_upgrade_from (agent/install/fetch.go)
#   manifest.json.sig  a raw 64-byte detached Ed25519 signature over the exact manifest bytes
#                      (PureEdDSA over the whole file — no separate hash step)
#   <name>             the big two travel compressed and the agent expands them after verifying
#                      the signed hash, so the manifest pins the compressed bytes (what the
#                      network carries). The agent itself is never compressed: the bootstrap
#                      fetches it with curl before anything exists that could decompress it.
#   install.sh         an ordinary artifact of briard/<version>/linux-amd64, hashed and signed
#                      like everything else, which `promote` also byte-copies to the channel root.
#                      The one-liner fetches the root copy before any verification exists, so the
#                      public, readable repo is the answer to the `curl | sh` objection, and
#                      `verify` asserting the two are the same bytes is the only tie between the
#                      unsigned root and the signed set. It resolves `stable` by default on both
#                      paths and is never stamped with its own id (see `stage` for why).
#
# Versioned directories are immutable: the property is that an id never names two different
# byte-sets, not that bytes live forever. The guest image is not bit-reproducible (timestamps,
# filesystem UUIDs), so re-staging a commit yields different bytes under the same id, and a
# pointer copied from the old manifest would name bytes the versioned directory no longer serves.
# `gc` deletes, so the bucket alone cannot answer that, hence two guards: `publish` refuses a
# version the bucket holds, and `release_version` refuses a commit older than the gc floor (an id
# carries its commit date, so re-minting one means re-publishing that commit, and anything gc
# removed is past the floor).
#
# Recovery from a bad release is a revert commit published forward, not a pointer move. Read
# agent/install/update.go before reaching for `promote <older-id>`: the `stable` path of the timer
# installs only when date(have) < date(want), so moving stable backward rolls no installed node
# back; it only changes what new installs get and lowers the floor an exact cloud pin may reach.
# A revert commit is a new rev dated today, a forward move for every node needing no old bytes,
# which is why `gc` can delete them.
#
# Publishing points nothing at a release. `publish` uploads versioned directories and stops;
# `latest` moves that pointer once the gates have passed on the exact id, `promote` moves `stable`
# and the root installer once the evidence is in. That order lets a gate run on the bytes the CDN
# actually serves: an id nothing names is fetchable by whoever knows it (ids are commit-derived
# and this repo is public, so untagged is not private) but advertised by no path, taken by no
# timer and installed by no stranger. A release that fails its gate is never pointed at, and `gc`
# collects it.
#
# `stable` is the one that matters: every installed node converges to it nightly with no canary
# on that path, so promotion is meant to be evidence-driven (the canary converged, the fleet
# stayed healthy for a real window), never a release-day action. `promote` refuses a build whose
# date field equals that of the promoted one: the stable path of the timer orders on that field
# alone, so such a promotion would be invisible to it and only look like a release. The field is
# the commit hour (YYMMDDHH, UTC), so a same-hour fix-up takes the next hour.
#
# `dev` is the one ungated pointer, and its release does not outlive its successor. `stable` and
# `latest` are evidence-driven and stay in the bucket until the `gc` floor; a dev release is HEAD,
# published to run on a test node this hour and worth nothing once the next lands. So `dev`
# stages, signs, publishes and points `dev` in one run with no gate anywhere, then deletes the
# previous dev release outright, floor or no floor, unless `stable` or `latest` also names it
# (then it is theirs to keep) or the new dev pairs with its vm image (unchanged inputs: the image
# stays, the briard directory goes). Three pointer names, one mechanism, exactly one of them cheap.
#
# Signing and publishing are separate subcommands on purpose: `sign` needs the key and no
# credential, `publish` the credential and no key. Either secret alone is inert (a forged
# signature has nowhere to be served, and the bucket serves what will not verify); together they
# are arbitrary code on every installed machine. Kept apart from day one, splitting them is a
# matter of where each runs, not a rewrite.
#
# The key is a secret of github.com/briardhq/signing, which only its owner can write to, and git
# carries what it signs: `stage` commits each manifest under releases/<id>/<arm>/ on the signing
# repository's main, pushes that to the fork of the operator and opens a pull request; the owner
# reads the diff and merges, starting the sign workflow, which signs every manifest that has no
# signature and commits the signatures; `sign` waits for them, verifies each and copies it into
# the staged tree. The publishing machine holds the credential and never the key, and nothing it
# can push to holds the key. No key is read here: there is no second way to sign a release.
#
# A key rotates by release. $RELEASE_PUBKEY is the keyring, one or more PEM public keys: it is
# embedded in the installers, and it is the `keyring.pem` artifact of both briard arms, which an
# updating node lays over its own. A rotation that nodes must survive without a reinstall ships
# [old, new] signed by old, and only then is the job's secret replaced by new; a node that has taken
# it trusts releases of the new key. `verify` accepts a signature of any key in it.
#
# A dirty tree is refused. The version is derived from the tree (flake.nix: epoch + commit
# date + short rev) and a dirty tree has no rev, so it stamps `v<epoch>.dirty`. A build nobody can
# reproduce must not be publishable — the reproducibility acceptance test is precisely
# "re-derive this artifact from its tag", which a working directory can never satisfy.
#
# Subcommands:
#   stage    [DIR]        build the artifacts, lay the tree out under DIR, write the manifests,
#                         open the signing pull request (needs gh, logged in as the operator,
#                         and its fork of briardhq/signing)
#   sign     [DIR]        wait for the merged manifests' signatures, verify them, copy them in
#   publish  [DIR]        upload the versioned dirs and nothing else -- no pointer, no root
#                         install.sh, so the release reaches nobody until it is pointed at
#                         (needs the credential, no key; refuses an already-published version)
#   latest   [VERSION]    move `latest` onto a published release, both chains — the second half
#                         of publishing, run once the gates have passed on that exact id
#                         (default: the version staged in the default DIR)
#   promote  [VERSION]    copy <VERSION>'s manifests to `stable` on every chain and arm, and its
#                         installers (install.sh, install.ps1, briard-setup.exe) to the channel root
#                         (default: whatever briard/latest names; refuses a same-hour promotion,
#                         and an upgrade floor newer than the newest Store exe under setup/)
#   dev      [DIR]        stage + sign + publish + move `dev`, in one run and with no gate, then
#                         delete the previous dev release unless stable/latest names it
#                         (needs the credential; a HEAD on pushed main, like any id)
#   gc       [--keep V]…  delete versioned dirs no pointer names and nothing pins, older than
#                         the 30-day floor — whole releases, never files
#   verify   [VERSION]    fetch stable, latest and dev of every chain and arm from the live channel
#                         and check them the way a client does — plus the root installers against
#                         briard/stable's. With a VERSION: that release where it was published,
#                         both arms and the guest it pairs with, which is what a publish is
#                         followed by while no pointer names it yet
#   setup build VERSION   the Store's installer: VERSION's published windows arm, verified,
#                         inside the briard-setup stub, at ./.release-setup/setup/VERSION/
#   setup publish VERSION upload it to setup/VERSION/briard-store-setup.exe, once (needs the credential;
#                         refuses a VERSION older than stable's upgrade floor)
#
# Env:
#   BRIARD_CHANNEL_URL  public read root            (default https://get.briard.io)
#   RELEASE_WRITE       write store URL — required by `publish`/`latest`/`promote`/`dev`/`gc`
#                       and `setup publish`, e.g.
#                       's3://get-briard-io?endpoint=<account>.r2.cloudflarestorage.com&region=auto'
#   RELEASE_PUBKEY      the keyring, one or more PKIX PEM public keys: staged, and what every
#                       signature is verified against
#   RELEASE_PURGE_URL   optional: CDN purge endpoint, POSTed a {"files":[...]} list after upload
#   RELEASE_PURGE_TOKEN optional: bearer token for RELEASE_PURGE_URL (see purge_edge for why)
#   CERTUM_EMAIL        the SimplySign account's e-mail -- required by `stage`/`dev`/`setup build`,
#                       which Authenticode-sign every windows PE (see authenticode)
#   CERTUM_OTP          the base32 seed of that account's one-time codes: a signing secret,
#                       held like the release key
#   CATALOG_DEV_PUBKEY  the catalog's dev key, public half -- required by `stage`, which refuses a
#                       RELEASE_PUBKEY that holds it
#
# Run from the repo root. There is no second publish and no second trust root: the
# guest OS travels as the signed image in this tree, verified by the release keyring the host
# holds.
set -euo pipefail

# Every path below is root-relative (`.#artifacts…`, `scripts/units/…`), so a run from anywhere
# else fails minutes in, inside a nix eval, with a message about the flake. Say it at once instead.
[ -f flake.nix ] && [ -f scripts/publish-release.sh ] ||
	{ echo "publish-release: run from the repo root (no ./flake.nix here)" >&2; exit 1; }

CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}"
STAGE_DEFAULT="./.release"
CHAINS="briard vm"
# The platform arms of a chain: the briard bundle is built per host OS and CPU, the VM image per
# CPU (the same VM on every host OS). LINUX_ARM is the one install.sh and the agent fetch, and the
# one the root installer, the vm pairing and the dev bookkeeping are read from.
LINUX_ARM=linux-amd64
WINDOWS_ARM=windows-amd64
VM_ARM=amd64
arms_of() { case "$1" in briard) echo "$LINUX_ARM $WINDOWS_ARM" ;; vm) echo "$VM_ARM" ;; esac; }
# Nothing younger than this is ever removed, whatever the pointers say, so `gc` can never race a
# rollback or a fresh pin. It is also the stale-commit floor `release_version` refuses past —
# one number, because the two are the same fact seen from each end (see there).
GC_FLOOR_DAYS=30

die() { echo "publish-release: $*" >&2; exit 1; }
say() { echo ">>> $*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }
# openssl is not in the dev shell and must not become a reason the release cannot be signed on
# whatever machine holds the key. Prefer a real one, fall back to nixpkgs — same shape as
# aws() below reaching awscli2.
ossl() {
	if command -v openssl >/dev/null 2>&1; then openssl "$@"; else nix run nixpkgs#openssl -- "$@"; fi
}
aws() { nix run nixpkgs#awscli2 -- "$@"; }
# The Authenticode signature: Windows' own trust, never mixed with the channel's. The release
# keyring signs manifests, which nodes verify; this certificate signs the PE files Windows
# evaluates -- every .exe and .dll of the windows arm we ship, and the Store exe. A PE already
# signed keeps its signature (the TAP driver is OpenVPN's). Timestamped, so a signature outlives
# the one-year certificate. The login is CERTUM_EMAIL and CERTUM_OTP (the SimplySign account's
# e-mail and the seed of its one-time codes); without them the windows arm is not staged at all,
# never staged unsigned. One ssign run signs a whole list on one login.
pe_signed() { # file -- true when its security directory names a certificate table
	local lfanew magic dirs
	[ "$(head -c2 "$1")" = MZ ] || return 1
	lfanew=$(od -An -tu4 -j60 -N4 "$1" | tr -d ' ')
	magic=$(od -An -tx2 -j$((lfanew + 24)) -N2 "$1" | tr -d ' ')
	case "$magic" in 020b) dirs=$((lfanew + 24 + 112)) ;; 010b) dirs=$((lfanew + 24 + 96)) ;; *) return 1 ;; esac
	[ "$(od -An -tu4 -j$((dirs + 4 * 8 + 4)) -N4 "$1" | tr -d ' ')" -gt 0 ]
}
authenticode() { # file... -- signs each unsigned one in place, then refuses any left unsigned
	local f todo=()
	for f in "$@"; do pe_signed "$f" || todo+=("$f"); done
	if [ "${#todo[@]}" -gt 0 ]; then
		[ -n "${CERTUM_EMAIL:-}" ] && [ -n "${CERTUM_OTP:-}" ] ||
			die "set CERTUM_EMAIL and CERTUM_OTP (the SimplySign login, from .env) -- a windows PE is never published unsigned"
		"$(nix build --no-link --print-out-paths .#ssign)/bin/ssign" -n Briard -u https://briard.io "${todo[@]}" >&2 ||
			die "Authenticode signing failed"
	fi
	for f in "$@"; do pe_signed "$f" || die "$f carries no Authenticode signature"; done
	say "Authenticode: ${#todo[@]} signed, $(($# - ${#todo[@]})) already signed"
}

# The release id, and the dirty gate. Asked of the flake rather than recomputed here, so there
# is exactly one definition of what a version is (flake.nix).
release_version() {
	local v
	v=$(nix eval --raw .#artifacts.agent.version) || die "cannot read the release version"
	case "$v" in
		*dirty*) die "refusing a DIRTY tree (version=$v) — commit first; a build nobody can reproduce must not be published" ;;
		"") die "empty version" ;;
	esac
	# The pushed-main gate. Every tier that tests this code -- CI, the nightly's VM and fleet
	# tiers, the single-test workflow -- tests pushed `main`, so a release cut from an unpushed
	# commit or another branch ships code none of them has seen. An ancestor of `origin/main` is
	# fine: it was on main, and the floor below bounds how old it may be.
	git fetch --quiet origin main || die "cannot fetch origin/main to check that HEAD is on it"
	git merge-base --is-ancestor HEAD origin/main ||
		die "refusing HEAD $(git rev-parse --short HEAD) (version=$v): it is not on origin/main — push it to main first; the suites test pushed main, so anything else would ship untested code"
	# The stale-commit floor keeps ids unique although `gc` deletes. An id is
	# `v<epoch>.<commit-hour>.<shortrev>`, so minting one twice means publishing the same commit twice,
	# which `publish` catches by asking the bucket — exactly the question a deleted release makes
	# unanswerable. Everything `gc` removes is past the floor by publish date, and a commit is
	# never younger than its own publish, so refusing a commit older than the floor closes that
	# hole with no ledger to keep or lose. It costs nothing in practice: releases are cut from HEAD,
	# and shipping old code is a revert commit (the recovery note in the header), a new rev with
	# today's date, which passes this.
	local age
	age=$(( ( $(date -u +%s) - $(date -u -d "$(day_of "$v")" +%s) ) / 86400 ))
	[ "$age" -le "$GC_FLOOR_DAYS" ] || die "refusing a commit $age days old (version=$v, floor ${GC_FLOOR_DAYS}d): gc deletes past that floor, so this id may name bytes that existed once and are gone — publish a revert commit, not the old tag"
	echo "$v"
}
# The vm release a published briard release pairs with, read off its live manifest:
# the one place the pairing is recorded, and the same field an installing node reads.
vm_of() {
	curl -fsS "$CHANNEL/briard/$1/$LINUX_ARM/manifest.json" | jq -er '.vm // empty' ||
		die "briard/$1 is not published, or names no vm release (published before the pairing was recorded?)"
}
# The id a chain uses for the release named by a briard id.
chain_id() { case "$1" in vm) vm_of "$2" ;; *) echo "$2" ;; esac; }
# The vm release the live channel serves for these image inputs, if any: `latest` first
# (what the last publish paired with), then `stable`, then `dev`, then the exact id these inputs
# would mint today (<candidate>) -- an image a run published and then died before pointing at,
# which `publish` would otherwise refuse as already there. Empty when none matches or the channel
# cannot be read -- in which case `stage` publishes a fresh image, which is always safe.
live_vm_for_inputs() { # <inputs> <candidate id>
	local p m
	for p in latest stable dev "$2"; do
		m=$(curl -fsS "$CHANNEL/vm/$p/$VM_ARM/manifest.json" 2>/dev/null) || continue
		if [ "$(echo "$m" | jq -r '.inputs // ""')" = "$1" ]; then
			echo "$m" | jq -r .version; return 0
		fi
	done
	return 1
}
# The date field of an id — the only thing the timer's stable path orders on, as a number. It is
# the commit's YYMMDDHH (UTC); older releases carry YYYYMMDD, and every one of those is below
# every YYMMDDHH, so numeric comparison orders both forms by time.
date_of() { echo "$1" | cut -d. -f2; }
# The calendar day of an id's date field, as `date -d` reads it, in either form: a YYYYMMDD field
# always starts `20`, which as YYMMDDHH would be the year 2020, before any id had the form.
day_of() {
	local d
	d=$(date_of "$1")
	case "$d" in
		20??????) echo "${d:0:4}-${d:4:2}-${d:6:2}" ;;
		*) echo "20${d:0:2}-${d:2:2}-${d:4:2}" ;;
	esac
}
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
# Does the bucket hold exactly this key? `s3 ls <key>` is a prefix listing: asked for
# `briard-agent` in a directory holding `briard-agent.exe` it prints that, and a pointer move
# that trusted it would copy a key that does not exist (the Windows arm). So the listing's last
# column must equal the key's own name.
have_key() { aws s3 ls "$1" --endpoint-url "$2" 2>/dev/null | awk -v n="${1##*/}" '$NF == n { f = 1 } END { exit !f }'; }
# Copy one key to another inside the bucket, `$bucket`-relative on both sides.
#
# ⚠️ `s3api copy-object`, deliberately not `s3 cp`: between two S3 keys awscli2's `s3 cp`
# carries the source object's tags, costing a GetObjectTagging on the source and an
# `x-amz-tagging-directive` on the copy, neither of which R2 implements (`--copy-props none` only
# trades the first error for the second). A pointer move would fail with `NotImplemented` partway,
# on exactly the small pointer files -- the manifest and its signature -- since a multipart copy
# of a large object sets no tagging directive. Nothing here has tags to lose. A `cp` from a local
# file has no source object to interrogate and needs none of this.
copy_key() {
	aws s3api copy-object --bucket "${2#s3://}" --key "$3" \
		--copy-source "${2#s3://}/$1" --endpoint-url "$4" >/dev/null
}

# Purge a list of URLs (stdin) at the edge. CDN-specific, so it rides behind two env vars, but a
# publish without them warns loudly rather than leave a silently stale channel: the CDN caches the
# artifacts (measured: max-age 14400 on the .zst files), and a pointer that moved while the edge
# still serves its old bytes fails every install closed for hours. Versioned directories are
# immutable, so only pointer paths and the root installers ever need purging.
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

# Stage an installer with the release public key embedded: the placeholder, wherever it sits on
# its line, becomes the PEM inside the quotes around it -- the same for install.sh's
# `RELEASE_KEYRING_PEM='…'` and install.ps1's `$RELEASE_KEYRING_PEM = '…'`, so one function
# stages both and neither can be shipped with the placeholder its script refuses on.
# <src> <dst>; $RELEASE_PUBKEY is the PEM file.
embed_keyring() {
	local src=$1 dst=$2 ph=__BRIARD_RELEASE_KEYRING_PEM__
	awk -v keyfile="$RELEASE_PUBKEY" -v ph="$ph" '
		index($0, ph) {
			i = index($0, ph)
			printf "%s", substr($0, 1, i - 1)
			while ((getline line < keyfile) > 0) print line
			print substr($0, i + length(ph))
			next
		} { print }' "$src" > "$dst"
	chmod 0755 "$dst"
	grep -q "$ph" "$dst" \
		&& die "the keyring placeholder survived in $dst — the published installer would refuse to install"
	grep -q "BEGIN PUBLIC KEY" "$dst" \
		|| die "no public key landed in the staged $dst"
}

# Does <sig> verify <file> under any key of the keyring <pem>? The way a node's Keyring does: openssl
# reads only the first key of a PEM file, so each block gets its own file.
verify_sig() {
	local file=$1 sig=$2 pem=$3 d k
	d=$(mktemp -d)
	awk -v d="$d" '/BEGIN PUBLIC KEY/ { n++ } n { print > (d "/" n ".pem") }' "$pem"
	for k in "$d"/*.pem; do
		if ossl pkeyutl -verify -pubin -inkey "$k" -rawin -in "$file" -sigfile "$sig" >/dev/null 2>&1; then
			rm -rf "$d"; return 0
		fi
	done
	rm -rf "$d"; return 1
}

# The repository whose main holds every signed manifest and the job that signs them. Fetched into
# a ref of this checkout, so its objects and briard's share one store and nothing else moves.
SIGNING=briardhq/signing
SIGNING_MAIN=refs/signing/main
fetch_signing() { git fetch --quiet "https://github.com/$SIGNING.git" "+main:$SIGNING_MAIN"; }

# The release's manifests, proposed to the signing repository, where the owner's merge starts the
# sign job. <dir>..., each staged as <chain>/<id>/<arm>. One commit on its main, built with
# plumbing so the checkout does not move, force-pushed to release-<id> on the operator's fork
# (re-staging the id updates it). A manifest staged again under the same id (a run that never
# reached `publish`) drops the signature of the old bytes, so the job's question stays "has this a
# signature at all". The description is this machine's claim; the diff is what is reviewed.
propose_manifests() {
	local idx d p blob commit me v branch paths=""
	need gh
	v=$(cat "$DIR/VERSION")
	branch="release-$v"
	me=$(gh api user --jq .login) || die "gh is not logged in: the manifests reach $SIGNING as a pull request from your fork"
	fetch_signing || die "cannot fetch $SIGNING's main"
	idx=$(mktemp -u)
	GIT_INDEX_FILE=$idx git read-tree "$SIGNING_MAIN"
	for d in "$@"; do
		p="releases/$(basename "$(dirname "$d")")/$(basename "$d")"
		blob=$(git hash-object -w --no-filters "$d/manifest.json")
		GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,$p/manifest.json"
		GIT_INDEX_FILE=$idx git update-index --force-remove "$p/manifest.json.sig"
		paths="$paths
- \`$p/manifest.json\`"
	done
	commit=$(git commit-tree "$(GIT_INDEX_FILE=$idx git write-tree)" -p "$SIGNING_MAIN" -m "release: the manifests of $v")
	rm -f "$idx"
	git -c credential.helper= -c 'credential.helper=!gh auth git-credential' \
		push --quiet --force "https://github.com/$me/${SIGNING#*/}.git" "$commit:refs/heads/$branch" ||
		die "cannot push to $me/${SIGNING#*/}: fork it first (gh repo fork $SIGNING --clone=false)"
	if [ -z "$(gh pr list -R "$SIGNING" --head "$branch" --state open --json number --jq '.[].number')" ]; then
		gh pr create -R "$SIGNING" --base main --head "$me:$branch" --title "release: $v" --body "Release \`$v\`, cut from briardhq/briard@$(git rev-parse HEAD) by $me on $(hostname) at $(date -u +%FT%TZ).
$paths

Merging signs every manifest on main that has no signature." >/dev/null ||
			die "cannot open the pull request on $SIGNING"
	fi
	say "proposed the manifests to $SIGNING ($me:$branch): merge the pull request to sign them"
}

# The sign job's signature of one staged manifest, verified and copied beside it. <manifest> <id>
# <arm>. An hour, then it gives up: a pull request waits a month for its merge, a publish does not.
pull_signature() {
	local m=$1 p="releases/$2/$3" deadline
	deadline=$(($(date +%s) + 3600))
	# Other bytes under the id on main are the ones a re-stage replaces, until its merge lands.
	until fetch_signing && git cat-file -e "$SIGNING_MAIN:$p/manifest.json.sig" 2>/dev/null &&
		git show "$SIGNING_MAIN:$p/manifest.json" | cmp -s - "$m"; do
		[ "$(date +%s)" -lt "$deadline" ] ||
			die "no signature of the staged $p after an hour: is the pull request on $SIGNING merged, and was the id staged again elsewhere?"
		sleep 15
	done
	git show "$SIGNING_MAIN:$p/manifest.json.sig" > "$m.sig"
	verify_sig "$m" "$m.sig" "$RELEASE_PUBKEY" ||
		{ rm -f "$m.sig"; die "the signature of $p does NOT verify against $RELEASE_PUBKEY"; }
	say "signed $p (signature from $SIGNING, verified)"
}

# The files a pointer directory carries, in the upload order that fails closed: the bootstrap
# agent (if any) first, then the signature, then the manifest. A client racing the upload sees a
# coherent old pair, a coherent new pair, or a manifest/signature mismatch it refuses — never a
# manifest whose bytes are not there yet. Pointers are never synced, only copied one file at a
# time: `sync --delete` can emit both an upload and a delete for the manifest in one run, and when
# the delete wins the live channel 404s its manifest.
POINTER_FILES="briard-agent briard-agent.exe manifest.json.sig manifest.json"

# Move one chain/arm's pointer onto a published version, server-side, in POINTER_FILES order.
# <chain> <version> <arm> <pointer> <bucket> <endpoint>
#
# ⚠️ One implementation for every pointer: `latest` and `stable` differ entirely in meaning (a
# release exists; every node should take it) and share no guards, but moving one is the same act.
# It is a server-side copy of the bytes just published, never an upload from the local staged
# directory, which could name something other than what the versioned directory holds with
# nothing checking; a server-side copy cannot disagree.
move_pointer() {
	local c=$1 v=$2 arm=$3 ptr=$4 bucket=$5 endpoint=$6 rel p f
	rel=$(sub "$v" "$arm"); p=$(sub "$ptr" "$arm")
	for f in $POINTER_FILES; do
		have_key "$bucket/$c/$rel/$f" "$endpoint" || continue
		copy_key "$c/$rel/$f" "$bucket" "$c/$p/$f" "$endpoint"
	done
}

# Verify one served directory: the manifest's signature, that it names the chain and arm its path
# implies (and the version asked for, when one is), and that every artifact it lists downloads and
# matches. <base-url> <chain> <arm> <want-version|"">; $tmp, $PUB and $CHANNEL come from the
# caller (`verify` or `setup`).
#
# ⚠️ One implementation for a pointer and an exact id: a pointer is just a manifest at a path, so
# only the URL differs, so it is one function, not a second copy of the loop; a duplicated
# signature check is how one copy quietly stops being run.
# It never checks a manifest against itself: every artifact is fetched from where the manifest
# says it lives (the versioned directory), not from the path the manifest came from.
verify_manifest_at() {
	local base=$1 c=$2 arm=$3 want=$4 v ch pl vdir label
	label=${base#"$CHANNEL"/}
	curl -fsS "$base/manifest.json" -o "$tmp/manifest.json" || die "no manifest at $base"
	curl -fsS "$base/manifest.json.sig" -o "$tmp/manifest.json.sig" || die "no signature at $base"
	# Verify the way the agent does: raw Ed25519 over the exact bytes. A failure here is the
	# whole point of the check — an unsigned or re-signed channel must not read as green.
	verify_sig "$tmp/manifest.json" "$tmp/manifest.json.sig" "$PUB" \
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
	if [ -f "$tmp/$c.$arm.$v.ok" ]; then echo "    (artifacts of $vdir already verified)"; return 0; fi
	jq -r '.artifacts[] | "\(.name) \(.sha256) \(.size)"' "$tmp/manifest.json" |
	while read -r name sum size; do
		curl -fsS "$CHANNEL/$vdir/$name" -o "$tmp/$name" || die "$name is in the $label manifest but not served at $vdir/"
		got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
		gotsize=$(stat -c%s "$tmp/$name")
		[ "$got" = "$sum" ] || die "$vdir/$name: sha256 $got != manifest $sum"
		[ "$gotsize" = "$size" ] || die "$vdir/$name: size $gotsize != manifest $size"
		echo "    ok  $vdir/$name  ($gotsize bytes)"
		# A pointer serves its own bootstrap copy, which must be the same bytes the manifest
		# pins, or install.sh runs a bootstrap that is not the release it then installs. On a
		# versioned path $base is $vdir, so this re-reads the file just checked: cheap, and it
		# keeps both callers on one path rather than adding a branch to skip it.
		case "$name" in briard-agent|briard-agent.exe)
			curl -fsS "$base/$name" -o "$tmp/$name.ptr" || die "$name is not served under $base (the bootstrap install.sh curls)"
			[ "$(sha256sum "$tmp/$name.ptr" | cut -d' ' -f1)" = "$sum" ] || die "$base/$name differs from $vdir/$name"
			echo "    ok  $label/$name  (bootstrap copy matches)"
		esac
	done
	touch "$tmp/$c.$arm.$v.ok"
}
# ⚠️ Every server-side (s3->s3) copy below goes through copy_key, never `s3 cp`: R2 implements
# neither tagging call awscli2 makes on such a copy (see copy_key). Uploads from a local file
# need nothing.

case "${1:-}" in

stage)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix; need sha256sum; need jq
	V=$(release_version)
	# The vm release this briard release pairs with. The image's inputs hash comes from the flake;
	# if the live channel already serves an image with these exact inputs, that release is reused
	# (nothing of the vm chain is staged; the briard manifest names it), else a new id is minted:
	# the commit date (so the same commit always mints the same id, and the timer's stable path,
	# which orders on the date, sees a real step) and the inputs' short hash.
	INPUTS=$(nix eval --raw .#artifacts.guest-disk.inputs) || die "cannot read the guest image's inputs hash"
	[ "${#INPUTS}" = 64 ] || die "the guest inputs hash is not a sha256 ($INPUTS)"
	VM_REUSED=""
	if GV=$(live_vm_for_inputs "$INPUTS" "vm.$(date_of "$V").${INPUTS:0:7}"); then
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

	# The briard chain, linux arm: agent + net-wrap + qemu, one release (they move as one
	# bundle and commit as one, so they are published as one). Copy, never symlink: the artifacts
	# are uploaded as bytes, and a store symlink would publish a dangling link. `install -m` sets
	# the mode the manifest then records.
	H="$DIR/briard/$V/$LINUX_ARM"; mkdir -p "$H"
	install -m0755 "$(out_of .#artifacts.agent)/bin/briard-agent"        "$H/briard-agent"
	install -m0755 "$(out_of .#artifacts.net-wrap)/bin/briard-net-wrap"  "$H/briard-net-wrap"
	# The systemd units, shipped verbatim rather than generated (the agent install copies them into
	# place): ordinary artifacts the manifest hashes and the signature covers. Read from the working
	# tree like install.sh below, not a store path: static text with no build step.
	install -m0644 scripts/units/briard-agent.service   "$H/briard-agent.service"
	install -m0644 scripts/units/briard-update.service  "$H/briard-update.service"
	install -m0644 scripts/units/briard-update.timer    "$H/briard-update.timer"
	# The frozen update launcher, likewise. 0755 -- the manifest records a mode only when it is not
	# the 0644 default, and this is run.
	install -m0755 scripts/agent/briard-update  "$H/briard-update"
	# qemu-bundle is a directory in the store (bin/ lib/ share/ PROVENANCE) and the contract
	# wants one file, so it is tarred here.
	dtar "$H/qemu-bundle.tar" -C "$(out_of .#artifacts.qemu-bundle)" .
	zst "$H/qemu-bundle.tar" "$H/qemu-bundle.tar.zst"
	# The guest bundle: the briard binaries the guest runs ride the briard chain and are
	# pushed into the guest by the agent; the image bakes only the guest agent. Same shape as the
	# qemu bundle (a tarred directory), hash-skipped by the update path when unchanged.
	dtar "$H/guest-bundle.tar" -C "$(out_of .#artifacts.guest-bundle)" .
	zst "$H/guest-bundle.tar" "$H/guest-bundle.tar.zst"
	# install.sh, with the release pubkey embedded, as an artifact of this release. The source tree
	# carries a placeholder the script dies on by design ("the embedded key is a build
	# placeholder"), so shipping it unsubstituted would publish an installer that refuses to
	# install. In the linux-amd64 arm it is, like the frozen script and the units, versioned,
	# diffable and covered by the release signature. It is also byte-copied to the channel root,
	# but only by `promote`, never by `publish`: the advertised one-liner fetches the root URL on
	# every `stable` install, so writing it at publish time would make publishing a live change to
	# what strangers run. That root copy is fetched before anything exists that could verify it,
	# which is why the repo being public and this script readable is the real answer to the
	# `curl | sh` objection.
	#
	# ⚠️ It keeps defaulting to `RELEASE=stable` and is not stamped with its own id. Stamping would
	# make the deeper link self-contained but would also make the root copy name an exact id, so an
	# installer somebody saved to disk months ago would silently install that release rather than
	# heal forward to current stable, the property "old installer, new artifacts" rests on.
	# Installing an exact id passes BRIARD_RELEASE beside the URL instead. install.ps1 is the same
	# on the windows-amd64 arm below: same placeholder, same embedding, same root copy at `promote`.
	[ -n "${RELEASE_PUBKEY:-}" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key (embedded into install.sh)"
	grep -q "BEGIN PUBLIC KEY" "$RELEASE_PUBKEY" || die "$RELEASE_PUBKEY is not a PEM PUBLIC KEY"
	# The catalog's dev key never ships. It signs the candidate list the test rigs read, so a
	# release whose keyring held it would let every node take a candidate signed by nobody but a
	# test machine. Fail closed: a publisher that cannot name the dev key cannot prove its absence.
	[ -f "${CATALOG_DEV_PUBKEY:-}" ] || die "set CATALOG_DEV_PUBKEY to the catalog's dev public key, which no release keyring may hold"
	devkey=$(grep -v "PUBLIC KEY" "$CATALOG_DEV_PUBKEY")
	[ -n "$devkey" ] && ! grep -qxF "$devkey" "$RELEASE_PUBKEY" ||
		die "$RELEASE_PUBKEY holds the catalog's dev key ($CATALOG_DEV_PUBKEY); a release keyring never does"
	embed_keyring scripts/install.sh "$H/install.sh"
	sh -n "$H/install.sh" || die "the staged install.sh is not valid shell after substitution"
	# The keyring again, as an artifact: the installers carry it to a new node, this to an old one.
	install -m0644 "$RELEASE_PUBKEY" "$H/keyring.pem"

	# The manifest is written by the agent, not this script: the format is a contract between the
	# publisher and every installing node, so the writer shares its types with the reader
	# (agent/install) and cannot disagree with it. The binary is the one staged in this directory,
	# so the build this release ships writes the manifest it will later read on a node.
	[ -x "$H/briard-agent" ] || die "no staged briard-agent to write the manifests with"
	"$H/briard-agent" --stage-manifest "$H" --chain briard --platform "$LINUX_ARM" --release "$V" --vm "$GV" || die "writing the linux manifest failed"

	# The windows arm. `install.FetchRelease` downloads every artifact a manifest names, so a
	# Windows-only bundle in the Linux manifest would make every Linux install pull tens of MB it can
	# never run.
	# An arm of its own has the identical shape (a signed manifest, its signature, the artifacts it
	# names, the pointers), named by the Windows installer as the Linux one names `linux-amd64`.
	# One path, run twice: no second protocol, no platform field on the wire beyond the manifest's
	# own, no client change on either side.
	W="$DIR/briard/$V/$WINDOWS_ARM"; mkdir -p "$W"
	# The Windows agent, and the guest bundle it pushes. The bundle's bytes are the Linux arm's --
	# the guest is Linux whatever runs it -- copied so a Windows install fetches one arm, not two.
	install -m0755 "$(out_of .#artifacts.agent-windows)/bin/windows_amd64/briard-agent.exe" "$W/briard-agent.exe"
	install -m0755 "$(out_of .#artifacts.tray-windows)/bin/windows_amd64/briard-tray.exe" "$W/briard-tray.exe"
	cp "$H/guest-bundle.tar.zst" "$W/guest-bundle.tar.zst"
	# The TAP driver, OpenVPN's signed bytes pinned in the flake: the installer fetches it from this
	# arm and checks the same hash, so a Windows install touches one origin.
	install -m0644 "$(out_of .#artifacts.tap-windows)" "$W/tap-windows.exe"
	# The qemu tree is signed before it is packed, so a writable copy out of the store, under DIR
	# outside every versioned directory: nothing uploads it and the next stage clears it.
	Q="$DIR/.qemu-windows"
	cp -r "$(out_of .#artifacts.qemu-bundle-windows)/." "$Q"; chmod -R u+w "$Q"
	mapfile -t QPE < <(find "$Q" -type f \( -iname '*.exe' -o -iname '*.dll' \))
	# The download page's installer: the briard-setup stub with only the release public key
	# appended, so it verifies stable's manifest and the install.ps1 that names, and runs that,
	# installing current stable whenever it runs (never its own release), as the one-liner does; its
	# own version matters only for its own code. Signed with the rest after the key is appended, so
	# the signature covers the key.
	K="$DIR/.setup-web"; rm -rf "$K"; mkdir -p "$K"
	install -m0644 "$RELEASE_PUBKEY" "$K/keyring.pem"
	nix run nixpkgs#zip -- -q -j -X "$K/payload.zip" "$K/keyring.pem" || die "zipping the release key failed"
	cat "$(out_of .#artifacts.setup-windows)/bin/windows_amd64/briard-setup.exe" "$K/payload.zip" > "$W/briard-setup.exe"
	rm -rf "$K"
	authenticode "$W/briard-agent.exe" "$W/briard-tray.exe" "$W/tap-windows.exe" "$W/briard-setup.exe" "${QPE[@]}"
	dtar "$W/qemu-bundle-windows.tar" -C "$Q" .
	rm -rf "$Q"
	zst "$W/qemu-bundle-windows.tar" "$W/qemu-bundle-windows.tar.zst"
	embed_keyring scripts/install.ps1 "$W/install.ps1"
	install -m0644 "$RELEASE_PUBKEY" "$W/keyring.pem"
	# The update launcher, the windows arm's twin of briard-update: frozen, laid down by the install.
	install -m0644 scripts/agent/briard-update.ps1 "$W/briard-update.ps1"
	"$H/briard-agent" --stage-manifest "$W" --chain briard --platform "$WINDOWS_ARM" --release "$V" --vm "$GV" || die "writing the windows manifest failed"

	# The vm chain: the OS image, its own series, per CPU, and staged only when its
	# inputs changed (above). Measured: 1178 -> 377 MB compressed, which is what a household link
	# actually waits on -- and what every household would otherwise re-download for a version string.
	MANIFESTS="$H $W"
	if [ -z "$VM_REUSED" ]; then
		G="$DIR/vm/$GV/$VM_ARM"; mkdir -p "$G"
		install -m0644 "$(out_of .#artifacts.guest-disk)/nixos.qcow2" "$G/nixos.qcow2"
		zst "$G/nixos.qcow2" "$G/nixos.qcow2.zst"
		# The closure the image boots; the oldest briard that tolerates this VM, which is the
		# briard built beside it and the tightest correct value (a node takes briard/stable daily
		# and vm/stable rarely, so it is at or past this by the time the guest is promoted); and the
		# inputs hash that makes "unchanged" decidable next time.
		"$H/briard-agent" --stage-manifest "$G" --chain vm --platform "$VM_ARM" --release "$GV" \
			--system "$(out_of .#artifacts.guest-disk.system)" --min-briard "$V" --inputs "$INPUTS" \
			|| die "writing the vm manifest failed"
		MANIFESTS="$MANIFESTS $G"
	fi

	for m in $MANIFESTS; do
		jq -e . "$m/manifest.json" >/dev/null || die "the manifest at $m is not valid JSON"
	done

	echo "$V" > "$DIR/VERSION" # not part of the tree; a human-readable marker for the operator
	propose_manifests $MANIFESTS
	say "staged $V:"
	for c in $CHAINS; do
		if [ "$c" = vm ] && [ -n "$VM_REUSED" ]; then
			echo "  vm/$GV  (published already; unchanged inputs -- paired, not staged)"; continue
		fi
		for arm in $(arms_of "$c"); do
			m="$DIR/$c/$(sub "$([ "$c" = vm ] && echo "$GV" || echo "$V")" "$arm")/manifest.json"
			echo "  $c/$(sub "$(jq -r .version "$m")" "$arm")"
			jq -r '.artifacts[] | "    \(.name)  \(.size) bytes  \(.sha256[0:16])…"' "$m"
		done
	done
	say "next: ./scripts/publish-release.sh sign $DIR"
	;;

sign)
	DIR="${2:-$STAGE_DEFAULT}"
	[ -n "${RELEASE_PUBKEY:-}" ] || die "set RELEASE_PUBKEY to the keyring the signatures verify against"
	for c in $CHAINS; do
		if [ "$c" = vm ] && [ -f "$DIR/VM_REUSED" ]; then
			say "vm chain: $(cat "$DIR/VM") is reused (unchanged inputs) -- nothing to sign"; continue
		fi
		v=$(staged_version "$DIR/$c") || die "no staged version under $DIR/$c — run \`stage\` first"
		for arm in $(arms_of "$c"); do
			rel=$(sub "$v" "$arm"); m="$DIR/$c/$rel/manifest.json"
			[ -f "$m" ] || die "no manifest at $m"
			pull_signature "$m" "$v" "$arm"
		done
	done
	;;

publish)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	# Each installer is an artifact of its arm and lives nowhere else in a staging dir (only
	# `promote` puts one at the channel root). A staging dir carrying no installer was built before
	# the keyring was embedded, and publishing it would ship a release nobody can install.
	[ -f "$DIR/briard/$(staged_version "$DIR/briard")/$LINUX_ARM/install.sh" ] ||
		die "no install.sh under $DIR/briard/<version>/$LINUX_ARM — run \`stage\` (it embeds the keyring)"
	[ -f "$DIR/briard/$(staged_version "$DIR/briard")/$WINDOWS_ARM/install.ps1" ] ||
		die "no install.ps1 under $DIR/briard/<version>/$WINDOWS_ARM — run \`stage\` (it embeds the keyring)"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	say "publishing $(cat "$DIR/VERSION" 2>/dev/null || echo '?') to $RELEASE_WRITE"

	# The pair: which vm release this briard release names, and whether it is staged here or
	# already published (a reuse). A reused guest must actually be in the bucket, or the host
	# manifest would name a release no installer can fetch.
	GV=$(cat "$DIR/VM" 2>/dev/null) || die "no $DIR/VM — run \`stage\` first"
	VM_REUSED=""; [ ! -f "$DIR/VM_REUSED" ] || VM_REUSED=1
	if [ -n "$VM_REUSED" ]; then
		have_key "$bucket/vm/$GV/$VM_ARM/manifest.json" "$endpoint" ||
			die "the briard manifest names vm/$GV as its pair, but the bucket does not hold it — stage again against the live channel"
	fi

	# Immutability first, across every chain, before a byte moves: a half-published release
	# (briard uploaded, vm refused) would leave `latest` naming a pair nobody tested together.
	for c in $CHAINS; do
		[ "$c" = vm ] && [ -n "$VM_REUSED" ] && continue
		v=$(staged_version "$DIR/$c") || die "no staged version under $DIR/$c"
		for arm in $(arms_of "$c"); do
			rel=$(sub "$v" "$arm")
			[ -f "$DIR/$c/$rel/manifest.json.sig" ] || die "$c/$rel is unsigned — run \`sign\` before publishing"
			! have_key "$bucket/$c/$rel/manifest.json" "$endpoint" ||
				die "$c/$rel is ALREADY PUBLISHED and versioned directories are immutable (see header) — to re-point, \`promote\`; to ship a fix, commit and stage again"
		done
	done

	for c in $CHAINS; do
		# A reused vm release is already in the bucket, checked above; there is nothing to upload and
		# no pointer to move here.
		[ "$c" = vm ] && [ -n "$VM_REUSED" ] && continue
		v=$(staged_version "$DIR/$c")
		for arm in $(arms_of "$c"); do
			rel=$(sub "$v" "$arm")
			# The versioned directory: artifacts first, manifest pair last. No --delete anywhere
			# in this script: nothing is ever overwritten, so there is nothing to clean up, and the
			# bucket also holds `catalog/` (live runtime content the agent fetches for `briard app
			# install`, produced by nothing in this repo), which a wide --delete would silently
			# remove.
			aws s3 sync "$DIR/$c/$rel" "$bucket/$c/$rel/" --endpoint-url "$endpoint" \
				--exclude manifest.json --exclude manifest.json.sig --exclude "*/*" --no-progress
			aws s3 cp "$DIR/$c/$rel/manifest.json.sig" "$bucket/$c/$rel/manifest.json.sig" --endpoint-url "$endpoint" --no-progress
			aws s3 cp "$DIR/$c/$rel/manifest.json"     "$bucket/$c/$rel/manifest.json"     --endpoint-url "$endpoint" --no-progress
			say "published $c/$rel"
		done
	done

	# ⚠️ Nothing that points at a release is written here, and that is the point. Were `publish` to
	# move `latest` or overwrite the root install.sh with the upload, publishing would be a live
	# change to what the fleet and strangers get, and the only place for a gate would be before the
	# upload, on bytes the CDN had never served. An id nothing names reaches nobody: fetchable by
	# whoever knows it (ids are commit-derived and the repo is public, so "untagged" is not
	# "private"), signed, immutable, advertised by not one path, so the gates run on the bytes the
	# CDN actually serves. `latest` moves by the `latest` subcommand once they pass; `stable` and
	# the root install.sh by `promote`. The cost: a release that fails its gate is dead bytes in the
	# bucket until `gc`, immutable and named by no pointer, precisely the state `gc` collects.
	# Nothing needs purging either: only pointer paths and the root installer are ever overwritten,
	# this step writes neither, and a versioned directory is immutable.
	say "published — now run: ./scripts/publish-release.sh verify $(cat "$DIR/VERSION" 2>/dev/null || echo '<version>')"
	say "   nothing points at it yet: \`latest\` once the gates pass, \`promote\` once the evidence is in"
	;;

latest)
	# Move `latest` onto a published release: the second half of a publish, run once the gates
	# have passed on the exact id. Separate from `publish` because uploading is inert and this is
	# the step that makes a release visible. It is not `promote` with another argument, though it
	# shares `move_pointer`: `stable` is what every installed node converges to and a stranger gets,
	# so promotion carries the no-same-date rule, the root installer and an evidence bar; `latest`
	# says only "this exists and the gates liked it", what a cloud canary pins and `briard update
	# -to latest` asks for by name, and nothing arrives at it by default. Two decisions, one
	# mechanism.
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	V="${2:-}"
	if [ -z "$V" ]; then
		V=$(cat "$STAGE_DEFAULT/VERSION" 2>/dev/null) || die "no version given and no $STAGE_DEFAULT/VERSION to read one from"
		say "no version given — taking the staged $V"
	fi
	# The pair moves together or not at all, as in `promote`: `latest` on both chains must name the
	# two releases staged, gated and verified beside each other. The pairing is read from the
	# published briard manifest, not from anything local: this verb is about what is in the bucket,
	# and a stage directory may be a different build by now.
	have_key "$bucket/briard/$V/$LINUX_ARM/manifest.json" "$endpoint" || die "briard/$V is not published; nothing to point at"
	GV=$(curl -fsS "$CHANNEL/briard/$V/$LINUX_ARM/manifest.json" | jq -r '.vm // ""') || die "cannot read briard/$V/$LINUX_ARM to find its vm pair"
	[ -n "$GV" ] || die "briard/$V names no vm release — published before the pairing was recorded; stage and publish again"
	have_key "$bucket/vm/$GV/$VM_ARM/manifest.json" "$endpoint" ||
		die "briard/$V pairs with vm/$GV, which the bucket does not hold — the pair cannot be pointed at"
	for arm in $(arms_of briard); do
		move_pointer briard "$V" "$arm" latest "$bucket" "$endpoint"
		say "briard/$(sub latest "$arm") -> $V"
	done
	move_pointer vm "$GV" "$VM_ARM" latest "$bucket" "$endpoint"
	say "vm/latest -> $GV"
	{
		for arm in $(arms_of briard); do
			for f in $POINTER_FILES; do echo "$CHANNEL/briard/$(sub latest "$arm")/$f"; done
		done
		for f in $POINTER_FILES; do echo "$CHANNEL/vm/latest/$VM_ARM/$f"; done
	} | purge_edge
	say "latest -> $V (vm $GV) — now run: ./scripts/publish-release.sh verify"
	;;

promote)
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	V="${2:-}"
	if [ -z "$V" ]; then
		V=$(curl -fsS "$CHANNEL/briard/latest/$LINUX_ARM/manifest.json" | jq -r .version) || die "cannot read briard/latest to default the version"
		say "no version given — promoting what briard/latest names: $V"
	fi
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
	# Every chain and arm is checked before any of them moves: promotion is of the pair
	# (briard/stable + vm/stable is the tested pair by construction — there is no top-level
	# install pointer, and this is the obligation that stands in for one).
	for c in $CHAINS; do
		v=$(chain_id "$c" "$V")
		for arm in $(arms_of "$c"); do
			rel=$(sub "$v" "$arm"); p=$(sub stable "$arm"); tag="$c.$arm"
			have_key "$bucket/$c/$rel/manifest.json" "$endpoint" || die "$c/$rel is not published; nothing to promote"
			# The root installer is a byte-copy of the promoted release's, so that release must
			# carry one. One staged before install.sh was carried in the release does not, and
			# promoting it would move every pointer and leave the root serving the previous
			# installer — the silent half-promotion this whole check loop exists to refuse.
			if [ "$c" = briard ] && [ "$arm" = "$LINUX_ARM" ]; then
				have_key "$bucket/$c/$rel/install.sh" "$endpoint" ||
					die "$c/$rel carries no install.sh — it was staged before install.sh was carried in the release; re-stage and publish it"
			fi
			if [ "$c" = briard ] && [ "$arm" = "$WINDOWS_ARM" ]; then
				have_key "$bucket/$c/$rel/install.ps1" "$endpoint" ||
					die "$c/$rel carries no install.ps1 — it was staged before the Windows installer was carried in the release; re-stage and publish it"
				have_key "$bucket/$c/$rel/briard-setup.exe" "$endpoint" ||
					die "$c/$rel carries no briard-setup.exe — it was staged before the download page's installer was carried in the release; re-stage and publish it"
				# The Store's exe stays upgradable. A Store install lays down the agent its exe
				# carries, and that agent's first start updates it to stable -- which refuses an
				# installed release older than stable's floor, so a floor past the exe leaves every
				# new Store install unable to finish. The newest exe under setup/ is taken as the
				# Store's: one is published only to be submitted. Listed with s3api, which answers
				# an empty prefix with nothing rather than an error, so a failed listing stops here.
				floor=$(curl -fsS "$CHANNEL/$c/$rel/manifest.json" | jq -r '.min_upgrade_from // ""') ||
					die "cannot read $c/$rel/manifest.json for its upgrade floor"
				exes=$(aws s3api list-objects-v2 --bucket "${bucket#s3://}" --prefix setup/ --delimiter / \
					--endpoint-url "$endpoint" --query 'CommonPrefixes[].Prefix' --output text) ||
					die "cannot list the published Store exes under setup/"
				exe=$(printf '%s\n' $exes | sed -n 's|^setup/\(v[^/]*\)/$|\1|p' |
					awk -F. '$2 > best { best = $2; id = $0 } END { print id }')
				if [ -z "$exe" ]; then
					say "no Store exe under setup/ yet — no floor to hold against it"
				elif [ -n "$floor" ] && [ "$(date_of "$floor")" -gt "$(date_of "$exe")" ]; then
					die "$c/$rel's upgrade floor $floor is newer than the Store's exe ($exe) — every Store install would be refused its first update; build and publish a newer exe (\`setup\`), submit it, then promote"
				fi
			fi
			if curl -fsS "$CHANNEL/$c/$p/manifest.json" -o "$tmp/$tag.stable.json" 2>/dev/null; then
				cur=$(jq -r .version "$tmp/$tag.stable.json")
				if [ "$cur" = "$v" ]; then
					say "$c/$p already names $v"
				else
					# The no-same-hour rule. The timer's stable path orders on the date field
					# alone and deliberately has no same-date rule (one would make the timer revert
					# a cloud pin sharing stable's date), so the constraint sits here, in the layer
					# we can fix: a build whose date field equals the promoted one's is invisible
					# to the fleet, and promoting it would only look like a release.
					[ "$(date_of "$cur")" != "$(date_of "$v")" ] ||
						die "$c/$p is $cur, same date field as $v — the timer cannot tell them apart; a same-hour fix-up needs the next hour's number"
				fi
			else
				say "$c/$p has no stable yet — this promotion creates it"
			fi
		done
	done
	for c in $CHAINS; do
		v=$(chain_id "$c" "$V")
		for arm in $(arms_of "$c"); do
			rel=$(sub "$v" "$arm"); p=$(sub stable "$arm"); tag="$c.$arm"
			if [ "$(jq -r .version "$tmp/$tag.stable.json" 2>/dev/null)" != "$v" ]; then
				move_pointer "$c" "$v" "$arm" stable "$bucket" "$endpoint"
				say "promoted $c/$p -> $v"
			fi
		done
	done
	# The channel root's installers, byte-copied from the release just promoted and laid last,
	# after every pointer, so the root can never advertise a `stable` that is not there yet.
	# Unconditional rather than folded into the pointer loop: the pointer may already name $V (a
	# re-run, or a promote that died after the pointers moved) while the root still serves the
	# previous installer, and repairing exactly that half-laid state is what a re-run is for.
	copy_key "briard/$V/$LINUX_ARM/install.sh" "$bucket" "install.sh" "$endpoint"
	say "install.sh at the root -> briard/$V/$LINUX_ARM/install.sh"
	copy_key "briard/$V/$WINDOWS_ARM/install.ps1" "$bucket" "install.ps1" "$endpoint"
	say "install.ps1 at the root -> briard/$V/$WINDOWS_ARM/install.ps1"
	copy_key "briard/$V/$WINDOWS_ARM/briard-setup.exe" "$bucket" "briard-setup.exe" "$endpoint"
	say "briard-setup.exe at the root -> briard/$V/$WINDOWS_ARM/briard-setup.exe"
	{
		for c in $CHAINS; do
			for arm in $(arms_of "$c"); do
				for f in $POINTER_FILES; do echo "$CHANNEL/$c/$(sub stable "$arm")/$f"; done
			done
		done
		echo "$CHANNEL/install.sh"
		echo "$CHANNEL/install.ps1"
		echo "$CHANNEL/briard-setup.exe"
	} | purge_edge
	say "promoted $V — now run: ./scripts/publish-release.sh verify"
	;;

dev)
	# A dev release end to end, with no gate (the header says why it exists). Staging, signing and
	# publishing are the same steps a real release takes, called rather than re-spelled, so a dev
	# id is minted, signed and laid out exactly like one that will be promoted, and `verify <id>`
	# reads it the same way. What it does not share is any gate or floor: nothing is tested, and
	# the previous dev release is deleted on the spot. The id is an ordinary id, cut from a
	# committed HEAD on pushed main like every other (release_version's two gates stand), so only
	# the pointer naming it tells a dev release from a real one; and since `publish` refuses a
	# version the bucket holds, the same commit cannot be dev-published twice: the last one is
	# still there, under that id.
	DIR="${2:-$STAGE_DEFAULT}"
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	say "dev release: reading what dev names now (quiet for a moment -- awscli through nix), then stage, sign, publish, point"
	# What a pointer names, read from the bucket rather than the edge: this decides a delete, and
	# the edge may still serve the pointer that was purged a moment ago. Empty when the pointer
	# does not exist yet -- and a missing pointer is the normal first answer, so the failing
	# `aws` is swallowed inside the pipeline: under `pipefail` its status would otherwise be the
	# substitution's, and `set -e` would end the whole run before a line was printed.
	named_by() { { aws s3 cp "$bucket/$1/$(sub "$2" "$3")/manifest.json" - --endpoint-url "$endpoint" 2>/dev/null || true; } | jq -r "$4 // \"\""; }
	# The previous dev, both chains, before anything moves.
	prev=$(named_by briard dev "$LINUX_ARM" .version); prev_vm=$(named_by briard dev "$LINUX_ARM" .vm)
	[ -z "$prev" ] || say "dev currently names $prev (vm ${prev_vm:-?}); it is retired once the new one is up"

	# A dev run whose `sign` gave up before the merge resumes on the tree it staged: staging this
	# HEAD again would propose its manifests anew and drop the signatures the merge made.
	staged=$(cat "$DIR/VERSION" 2>/dev/null || true)
	if [ -n "$staged" ] && [ "${staged##*.}" = "$(git rev-parse --short=7 HEAD)" ]; then
		say "resuming: $DIR already holds $staged, staged for this HEAD"
	else
		"$0" stage "$DIR"
	fi
	"$0" sign "$DIR"
	"$0" publish "$DIR"
	V=$(cat "$DIR/VERSION") || die "no $DIR/VERSION after stage"
	GV=$(cat "$DIR/VM") || die "no $DIR/VM after stage"
	[ "$V" != "$prev" ] || die "dev already names $V (publish should have refused this)"

	for arm in $(arms_of briard); do
		move_pointer briard "$V" "$arm" dev "$bucket" "$endpoint"
		say "briard/$(sub dev "$arm") -> $V"
	done
	move_pointer vm "$GV" "$VM_ARM" dev "$bucket" "$endpoint"
	say "vm/dev -> $GV"
	{
		for arm in $(arms_of briard); do
			for f in $POINTER_FILES; do echo "$CHANNEL/briard/$(sub dev "$arm")/$f"; done
		done
		for f in $POINTER_FILES; do echo "$CHANNEL/vm/dev/$VM_ARM/$f"; done
	} | purge_edge

	# Retire the previous dev. Whole release directories, the rule `gc` keeps (a partial one is a
	# manifest whose bytes are gone), and no floor: a dev release's only reader is the test node
	# that asked for `dev`, and `dev` now names something else. The exceptions are the other
	# pointers' releases -- `stable`'s and `latest`'s are theirs, not dev's to delete, however they
	# came to share an id -- and, on the vm chain, the image the new dev just paired with.
	if [ -n "$prev" ]; then
		keep=""
		for p in stable latest; do
			[ "$(named_by briard "$p" "$LINUX_ARM" .version)" != "$prev" ] || keep=$p
		done
		if [ -n "$keep" ]; then
			say "keeping briard/$prev: $keep names it"
		else
			say "deleting briard/$prev (the previous dev; nothing else names it)"
			aws s3 rm "$bucket/briard/$prev/" --recursive --endpoint-url "$endpoint" --only-show-errors
		fi
	fi
	if [ -n "$prev_vm" ] && [ "$prev_vm" != "$GV" ]; then
		keep=""
		for p in stable latest; do
			[ "$(named_by vm "$p" "$VM_ARM" .version)" != "$prev_vm" ] || keep=$p
		done
		if [ -n "$keep" ]; then
			say "keeping vm/$prev_vm: $keep names it"
		else
			say "deleting vm/$prev_vm (the previous dev's image; nothing else names it)"
			aws s3 rm "$bucket/vm/$prev_vm/" --recursive --endpoint-url "$endpoint" --only-show-errors
		fi
	elif [ -n "$prev_vm" ]; then
		say "keeping vm/$prev_vm: the new dev pairs with it (unchanged image inputs)"
	fi
	say "dev -> $V (vm $GV), ungated — install it with BRIARD_RELEASE=dev, or \`briard update -to dev\`"
	;;

gc)
	shift
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	# gc deletes, and that is safe because nothing needs the old bytes, only the old code, which
	# git has. With ids `v<epoch>.<commit-hour>.<shortrev>`, a client depends on "an id never names two
	# different byte-sets", not "bytes live forever"; shipping old code is a revert commit, a new
	# rev under a new id, which satisfies that trivially. `release_version`'s stale-commit floor is
	# the other half.
	#
	# There is deliberately no cold archive. The case for one is "the bytes a rollback re-points
	# to", which agent/install/update.go does not support: `stable` installs only when date(have) <
	# date(want), so a backward pointer move reaches new installs and cloud pins, no installed node.
	# Against it: a second bucket, a second credential's blast radius, and a move R2 cannot perform
	# — `s3 mv` fetches object tags for any copy past the 8 MB multipart threshold (every image and
	# agent), so it half-moves a release, leaving a manifest whose bytes are elsewhere, the one
	# state this file exists to prevent. `--keep` and the floor cover the pin case instead.
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	# Pins live in the cloud's rollout state, which this script cannot see; the operator names
	# them. A release that is pinned and not named here is deleted, and the pinned node's next
	# tick fails its fetch loudly (a failed directive, not a silent no-op) — recovered by
	# publishing a revert commit and re-pointing the pin at it, not by restoring bytes. This is
	# the one place `--keep` earns its keep, and the floor is what buys time to use it.
	keep=""
	while [ $# -gt 0 ]; do
		case "$1" in --keep) keep="$keep $2"; shift 2 ;; *) die "gc: unknown argument $1" ;; esac
	done
	floor=$(date -u -d "-$GC_FLOOR_DAYS days" +%Y-%m-%d)
	for c in $CHAINS; do
		# The first arm stands for the release: every arm of a version is
		# published and promoted together, so one manifest's pointer and LastModified speak for all.
		first=$(arms_of "$c" | cut -d' ' -f1)
		live=""
		for p in stable latest dev; do
			live="$live $(curl -fsS "$CHANNEL/$c/$(sub "$p" "$first")/manifest.json" 2>/dev/null | jq -r .version)"
		done
		aws s3 ls "$bucket/$c/" --endpoint-url "$endpoint" | awk '/ PRE /{print $2}' | tr -d / |
		while read -r v; do
			# The pointer directories themselves, which the loop would otherwise age out: a pointer is
			# rewritten only when something is published to it, and `dev` can sit untouched for weeks.
			case "$v" in stable|latest|dev) continue ;; esac
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
			# Whole releases, never files: a release directory is what a signed manifest names, and
			# a partial one is a manifest whose bytes are gone, to a client indistinguishable from
			# an attack. `rm --recursive` needs no server-side copy, so it has no tagging call to
			# trip on and cannot half-remove a release the way a move does; the rule stands for
			# whatever replaces it.
			say "deleting $c/$v (published $when, past the ${GC_FLOOR_DAYS}-day floor)"
			aws s3 rm "$bucket/$c/$v/" --recursive --endpoint-url "$endpoint" --only-show-errors
		done
	done
	;;

verify)
	need curl; need sha256sum; need jq
	PUB="${RELEASE_PUBKEY:-}"
	[ -n "$PUB" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key"
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

	# One exact release, named — what follows a `publish`, since publishing points nothing at the
	# release: the checks a pointer gets, at the versioned path, plus the vm release it pairs with.
	# No pointer check and no root installer: this release claims to be neither yet, which is the
	# whole state being verified.
	if [ -n "${2:-}" ]; then
		V="$2"
		say "verifying $V where it was published — no pointer names it yet"
		for arm in $(arms_of briard); do
			verify_manifest_at "$CHANNEL/briard/$(sub "$V" "$arm")" briard "$arm" "$V"
		done
		GV=$(curl -fsS "$CHANNEL/briard/$V/$LINUX_ARM/manifest.json" | jq -r '.vm // ""') ||
			die "cannot read briard/$V/$LINUX_ARM — is $V published?"
		[ -n "$GV" ] || die "briard/$V names no vm release — published before the pairing was recorded; stage and publish again"
		verify_manifest_at "$CHANNEL/vm/$GV/$VM_ARM" vm "$VM_ARM" "$GV"
		# The installer a gate on this id will actually curl. Its bytes are already checked (an
		# artifact of the linux-amd64 arm above); this adds that the deeper URL the gate names serves
		# it: the advertised root URL serves the promoted release and cannot reach this one at all.
		curl -fsS -o /dev/null "$CHANNEL/briard/$V/$LINUX_ARM/install.sh" ||
			die "briard/$V/$LINUX_ARM/install.sh is not served — an install gate on $V has no installer to fetch"
		say "$V verifies: both arms, vm $GV, every artifact matching, and its own install.sh served"
		say "   install it with: BRIARD_RELEASE=$V, fetching $CHANNEL/briard/$V/$LINUX_ARM/install.sh"
		say "   on Windows: \$env:BRIARD_RELEASE='$V'; irm $CHANNEL/briard/$V/$WINDOWS_ARM/install.ps1 | iex"
		exit 0
	fi

	# Every chain, every arm, every pointer. A pointer is a manifest at a path, so verifying one is
	# verifying all of them with the path changed, and a pointer nobody verifies is one nobody
	# knows is broken until a stranger (or the timer, fleet-wide) runs into it.
	for c in $CHAINS; do
		for arm in $(arms_of "$c"); do
			for p in stable latest dev; do
				rel=$(sub "$p" "$arm"); base="$CHANNEL/$c/$rel"
				# A missing `stable` is said out loud rather than failed: the first publish of a
				# fresh tree is the one run where it is expected. A missing `dev` just means no dev
				# release is out. A missing `latest` stays fatal: a release can sit published with
				# nothing naming it (publishing moves no pointer), but that is what
				# `verify <VERSION>` is for; here the pointers are being checked, and one is gone.
				if ! curl -fsS -o /dev/null "$base/manifest.json" 2>/dev/null; then
					[ "$p" = stable ] && { say "WARNING: no $c/$rel yet — nothing promoted here"; continue; }
					[ "$p" = dev ] && { say "(no $c/$rel — no dev release out)"; continue; }
					die "no manifest at $base"
				fi
				verify_manifest_at "$base" "$c" "$arm" ""
			done
		done
	done
	# The pair holds at every pointer: what briard/<p> names as its vm is what vm/<p>
	# serves. This is the obligation "briard/stable + vm/stable is the tested pair" rests on,
	# since the vm id is not derivable from the briard id; a pointer moved by hand on one
	# chain and not the other fails here, before an installer meets it.
	for p in stable latest dev; do
		hg=$(curl -fsS "$CHANNEL/briard/$p/$LINUX_ARM/manifest.json" 2>/dev/null | jq -r '.vm // ""') || hg=""
		gv=$(curl -fsS "$CHANNEL/vm/$p/$VM_ARM/manifest.json" 2>/dev/null | jq -r .version) || gv=""
		[ -n "$hg$gv" ] || continue # neither exists yet (a fresh tree's stable): said above
		[ -n "$hg" ] || die "briard/$p names no vm release — published before the pairing was recorded; stage and publish again"
		[ "$hg" = "$gv" ] || die "briard/$p pairs with vm/$hg but vm/$p serves $gv — the pointers disagree"
		say "briard/$p <-> vm/$p agree on $gv (the tested pair)"
	done
	# install.sh at the channel root, the URL the advertised one-liner names, must agree with where
	# the artifacts actually are: one still defaulting to an old layout would fetch nothing and
	# fail closed, safe but silent, and no check above would catch it, since each uses $CHANNEL
	# rather than what the script believes. Downloaded to a file and then grepped, never
	# `curl … | grep -q`: `-q` exits on the first match, closing the pipe under a curl still
	# writing, which dies with "(23) Failure writing output to destination", and `||` reads that as
	# the assertion failing.
	curl -fsS "$CHANNEL/install.sh" -o "$tmp/install.sh" || die "install.sh is not fetchable at $CHANNEL/install.sh"
	grep -q "BRIARD_CHANNEL_URL:-$CHANNEL" "$tmp/install.sh" ||
		die "the served install.sh does not default to $CHANNEL — it would look for the tree in the wrong place"
	# ...and it is the promoted release's installer, byte for byte. The root copy is unsigned by
	# construction (the one-liner fetches it before anything could verify a signature), so this
	# equality is the only tie to the signed set: matching `briard/stable/linux-amd64/install.sh`
	# makes it exactly as trustworthy as that manifest, which the loop above verified against the
	# release key. Without it the root could serve any installer and every check above would pass.
	# Skipped only when nothing is promoted yet (a fresh tree), which the loop above already
	# reported; the sha of a 404 would otherwise read as a mismatch and bury that.
	if curl -fsS "$CHANNEL/briard/stable/$LINUX_ARM/manifest.json" -o "$tmp/stable.json" 2>/dev/null; then
		sv=$(jq -r .version "$tmp/stable.json")
		want=$(jq -r '.artifacts[] | select(.name=="install.sh") | .sha256' "$tmp/stable.json")
		[ -n "$want" ] ||
			die "briard/stable ($sv) names no install.sh — it was published before install.sh was carried in the release, so nothing pins what the root serves"
		got=$(sha256sum "$tmp/install.sh" | cut -d' ' -f1)
		[ "$got" = "$want" ] ||
			die "the root install.sh is NOT briard/stable/$LINUX_ARM/install.sh ($got != $want) — a promote that did not finish, or a hand-edited root"
		say "install.sh at the root is briard/$sv/$LINUX_ARM/install.sh, byte for byte"
		# The Windows installers, the same way -- but a stable from before one was carried is said,
		# not failed: nothing is wrong with a channel whose root serves no installer its promoted
		# release never had.
		curl -fsS "$CHANNEL/briard/stable/$WINDOWS_ARM/manifest.json" -o "$tmp/stable-windows.json" ||
			die "cannot read briard/stable/$WINDOWS_ARM"
		for f in install.ps1 briard-setup.exe; do
			want=$(jq -r --arg f "$f" '.artifacts[] | select(.name==$f) | .sha256' "$tmp/stable-windows.json")
			if [ -z "$want" ]; then
				say "briard/stable ($sv) predates $f — the root serves none yet"
				continue
			fi
			curl -fsS "$CHANNEL/$f" -o "$tmp/$f" || die "$f is not fetchable at $CHANNEL/$f"
			got=$(sha256sum "$tmp/$f" | cut -d' ' -f1)
			[ "$got" = "$want" ] ||
				die "the root $f is NOT briard/stable/$WINDOWS_ARM/$f ($got != $want) — a promote that did not finish, or a hand-edited root"
			say "$f at the root is briard/$sv/$WINDOWS_ARM/$f, byte for byte"
		done
	fi
	say "$CHANNEL verifies end to end: every pointer signed, every artifact matching, the installers served at the root and pointing here"
	;;

setup)
	# The Store's installer, built from a release already published: its windows arm, fetched from
	# the live channel and verified the way `verify` does, unpacked into the shape install.ps1
	# installs from (BRIARD_ARTIFACTS: the qemu tree extracted, the guest bundle decompressed to its
	# tar), zipped and appended to the briard-setup stub. No guest image: the installed agent's
	# first start fetches it. Run when the Store listing needs a new installer (the upgrade floor
	# moved past the one it embeds, or the TAP driver or qemu bundle changed), never per release:
	# the Store never updates an installed node, our self-update does. `setup build` makes it
	# locally with no credential, so it can be proven on a Windows machine first; `setup publish`
	# uploads that file.
	#
	# ⚠️ One URL, one binary, forever. The Store certifies the bytes at the URL it was given and
	# re-certifies on its own if they move, so setup/<VERSION>/briard-store-setup.exe is written
	# once and refused after. It lives outside both chains, so `gc` never collects it. Named apart
	# from the download page's briard-setup.exe (`stage`): the same stub, a different program.
	need curl; need sha256sum; need jq
	V="${3:-}"; [ -n "$V" ] || die "usage: setup {build|publish} VERSION (the published release whose windows arm it carries)"
	key="setup/$V/briard-store-setup.exe"
	OUT="./.release-setup/$key"
	case "$2" in
	build) ;;
	publish)
		[ -f "$OUT" ] || die "no $OUT -- run \`setup build $V\` first"
		[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE (see header)"
		bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
		! have_key "$bucket/$key" "$endpoint" || die "$key is already published, and the Store certified those bytes; a new installer is a new release"
		# The other side of `promote`'s check: an exe older than stable's floor could never finish
		# an install, since its first start is the update to stable. A failed read stops here; only
		# a channel with no stable yet (404) has no floor to hold.
		code=$(curl -sS -o "$OUT.stable.json" -w '%{http_code}' "$CHANNEL/briard/$(sub stable "$WINDOWS_ARM")/manifest.json") ||
			die "cannot read stable's manifest for its upgrade floor"
		case "$code" in 200 | 404) ;; *) die "stable's manifest answered $code" ;; esac
		if [ "$code" = 200 ]; then
			floor=$(jq -r '.min_upgrade_from // ""' "$OUT.stable.json"); rm -f "$OUT.stable.json"
			[ -z "$floor" ] || [ "$(date_of "$floor")" -le "$(date_of "$V")" ] ||
				die "$V is older than stable's upgrade floor $floor — its installs would be refused their first update; build the exe from a newer release"
		fi
		aws s3 cp "$OUT" "$bucket/$key" --endpoint-url "$endpoint" --only-show-errors || die "uploading $key failed"
		say "published $CHANNEL/$key ($(sha256sum "$OUT" | cut -d' ' -f1)) -- the URL the Store listing takes"
		exit 0
		;;
	*) die "usage: setup {build|publish} VERSION" ;;
	esac
	PUB="${RELEASE_PUBKEY:-}"
	[ -n "$PUB" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key"
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
	verify_manifest_at "$CHANNEL/briard/$(sub "$V" "$WINDOWS_ARM")" briard "$WINDOWS_ARM" "$V"
	A="$tmp/arm"; mkdir -p "$A/qemu"
	for f in briard-agent.exe briard-tray.exe tap-windows.exe install.ps1 briard-update.ps1 manifest.json manifest.json.sig; do
		install -m0644 "$tmp/$f" "$A/$f"
	done
	nix run nixpkgs#zstd -- -d -q "$tmp/qemu-bundle-windows.tar.zst" -o "$tmp/qemu.tar" || die "expanding the qemu bundle failed"
	tar -xf "$tmp/qemu.tar" -C "$A/qemu"
	chmod -R u+w "$A/qemu" # the tree keeps the store's read-only modes, which the EXIT trap cannot remove
	nix run nixpkgs#zstd -- -d -q "$tmp/guest-bundle.tar.zst" -o "$A/guest-bundle.tar" || die "expanding the guest bundle failed"
	(cd "$A" && nix run nixpkgs#zip -- -q -r -X "$tmp/payload.zip" .) || die "zipping the arm failed"
	stub=$(nix build --no-link --print-out-paths .#artifacts.setup-windows)/bin/windows_amd64/briard-setup.exe
	mkdir -p "$(dirname "$OUT")"
	cat "$stub" "$tmp/payload.zip" > "$OUT"
	# Signed whole, after the payload is appended: the signature covers the payload, and the stub
	# reads its payload up to where the signature begins.
	authenticode "$OUT"
	say "built $OUT for $V: $(stat -c%s "$OUT") bytes, sha256 $(sha256sum "$OUT" | cut -d' ' -f1)"
	;;

*)
	die "usage: publish-release.sh {stage|sign|publish|latest|promote|dev|gc|verify|setup} [ARGS]  (see header)"
	;;
esac
