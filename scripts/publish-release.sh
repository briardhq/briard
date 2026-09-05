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
#   install.sh                          unsigned, outside every chain (see below)
#   host/
#     <version>/linux/                  manifest.json(+.sig), briard-agent, briard-net-wrap,
#                                       qemu-bundle.tar.zst
#     <version>/windows/                manifest.json(+.sig), qemu-bundle-windows.tar.zst
#                                       (the Windows arm, [V3b.27](b); no consumer until v5)
#     latest/{linux,windows}/           manifest.json(+.sig), briard-agent (linux)
#     stable/{linux,windows}/           likewise
#   guest/
#     <version>/                        manifest.json(+.sig), nixos.qcow2.zst
#     latest/ stable/                   manifest.json(+.sig)
#
# A CHAIN is a release line with its own version series: the host bundle moves as
# `v3.<date>.<rev>`, the guest OS as `guest.<date>.<rev>` (same date and rev — both are staged
# from one commit — but two chains, not two flavours of one release). The host chain has one
# more level, the PLATFORM ARM, because a host bundle is built per host OS; the guest image is
# the same VM on every host and has none. A POINTER is just a path serving a byte-copy of one
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
#   install.sh         at the channel root, fetched by the one-liner before any verification
#                      exists — which is why the repo being public and readable IS the answer
#                      to the `curl | sh` objection.
#
# VERSIONED DIRECTORIES ARE IMMUTABLE. `publish` refuses a version the bucket already holds.
# The guest image is not bit-reproducible (timestamps, filesystem UUIDs), so re-staging the same
# commit yields different bytes under the same id — and a pointer copied from the old manifest
# would then name bytes the versioned directory no longer serves. Recovery from a bad release is
# therefore MOVING A POINTER (`promote` an older version), never re-publishing from the tag.
#
# `latest` moves on every publish; `stable` moves only on `promote`, and promotion is meant to
# be evidence-driven (canary converged, fleet healthy for a real window) — never a release-day
# action. The publish sequence in the operator skill tests a release at `latest` before anyone
# promotes it. `promote` refuses a build whose date equals the currently promoted one: the
# timer's stable path orders on the date field alone ([B.86a]), so a same-date promotion would
# be invisible to it. A same-day fix-up takes the next day's number.
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
#   sign     [DIR]        detached-sign every manifest and lay the `latest` pointers
#                         (needs $RELEASE_SIGN_KEY, no credential)
#   publish  [DIR]        upload the versioned dirs, move `latest`, upload install.sh
#                         (needs the credential, no key; refuses an already-published version)
#   promote  [VERSION]    copy <VERSION>'s manifests to `stable` on every chain and arm
#                         (default: whatever host/latest names; refuses a same-date promotion)
#   gc       [--keep V]…  archive versioned dirs no pointer names and nothing pins, older than
#                         the 30-day floor, to $RELEASE_ARCHIVE — whole releases, never files
#   verify                fetch stable + latest of every chain and arm from the LIVE channel and
#                         check them the way a client does
#
# Env:
#   BRIARD_CHANNEL_URL  public read ROOT            (default https://get.briard.io)
#   RELEASE_WRITE       write store URL — required by `publish`/`promote`/`gc`, e.g.
#                       's3://get-briard-io?endpoint=<account>.r2.cloudflarestorage.com&region=auto'
#   RELEASE_ARCHIVE     cold store URL for `gc` (same shape); refused unset — gc never deletes
#   RELEASE_SIGN_KEY    PKCS8 PEM Ed25519 private key (sign mode; release secret store)
#   RELEASE_PUBKEY      PKIX PEM public key, for `verify` (default: alongside the private key)
#   RELEASE_PURGE_URL   optional: CDN purge endpoint, POSTed a {"files":[...]} list after upload
#   RELEASE_PURGE_TOKEN optional: bearer token for RELEASE_PURGE_URL (see purge_edge for why)
#
# Run from the repo root. See also scripts/publish-cache.sh — the OS-closure cache, a
# DISTINCT trust root (nix's per-path narinfo signatures). The two keys are deliberately not
# shared: a compromised cache key is recoverable precisely because re-imaging is verified by
# this one.
set -euo pipefail

CHANNEL="${BRIARD_CHANNEL_URL:-https://get.briard.io}"
STAGE_DEFAULT="./.release"
CHAINS="host guest"
# The platform arms of a chain. A chain without the level yields `-`, the FLAT arm, which every
# loop below turns into "" (`arm=${a#-}`) so it runs once over "<chain>/<version>/" — a real
# empty word would vanish from `for` and the chain would never be visited.
arms_of() { case "$1" in host) echo "linux windows" ;; *) echo "-" ;; esac; }
# Nothing younger than this is ever archived, whatever the pointers say, so `gc` can never
# race a rollback or a fresh pin.
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
	echo "$v"
}
# The guest chain's id for a host id: same date and rev, its own series. One place, so the
# installer's BRIARD_RELEASE=<host id> (which derives the guest id the same way) and this script
# cannot disagree.
guest_id() { echo "guest.${1#*.}"; }
# The id a chain uses for the release named by a host id.
chain_id() { case "$1" in guest) guest_id "$2" ;; *) echo "$2" ;; esac; }
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

case "${1:-}" in

stage)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix; need sha256sum; need jq
	V=$(release_version); GV=$(guest_id "$V")
	say "staging release $V (guest $GV) into $DIR"
	rm -rf "$DIR"; mkdir -p "$DIR"
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

	# THE HOST CHAIN, LINUX ARM: agent + net-wrap + qemu, one release ([B.86b]: they move as one
	# bundle and commit as one, so they are published as one). Copy, never symlink: the artifacts
	# are uploaded as bytes, and a store symlink would publish a dangling link. `install -m` sets
	# the mode the manifest then records.
	H="$DIR/host/$V/linux"; mkdir -p "$H"
	install -m0755 "$(out_of .#artifacts.agent)/bin/briard-agent"        "$H/briard-agent"
	install -m0755 "$(out_of .#artifacts.net-wrap)/bin/briard-net-wrap"  "$H/briard-net-wrap"
	# qemu-bundle is a DIRECTORY in the store (bin/ lib/ share/ PROVENANCE) and the contract
	# wants one file, so it is tarred here.
	dtar "$H/qemu-bundle.tar" -C "$(out_of .#artifacts.qemu-bundle)" .
	zst "$H/qemu-bundle.tar" "$H/qemu-bundle.tar.zst"
	# The manifest, written BY THE AGENT rather than by this script: the format is a contract
	# between the publisher and every installing node, and it used to have two implementations
	# (a printf loop here, hand-assembling `"mode":493`, and the struct in agent/install). The
	# writer now shares its types with the reader, so the format cannot disagree with itself —
	# and the binary used is the one STAGED IN THIS DIRECTORY, the exact agent this release
	# ships, so the manifest is written by the same build that will later read it on a node.
	[ -x "$H/briard-agent" ] || die "no staged briard-agent to write the manifests with"
	"$H/briard-agent" --stage-manifest "$H" --chain host --platform linux --release "$V" || die "writing the linux manifest failed"

	# THE WINDOWS ARM. `FetchVerified` downloads EVERY artifact a manifest names, so a
	# Windows-only bundle in the Linux manifest would make every Linux install pull tens of MB it
	# can never run. An arm of its own gives it the identical shape — a signed manifest, its
	# signature, the artifacts it names, the two pointers — and a Windows installer will later
	# name it the way the Linux one names `linux`. One path, run twice: no second protocol, no
	# platform field on the wire beyond the manifest's own, and no client change on either side.
	W="$DIR/host/$V/windows"; mkdir -p "$W"
	dtar "$W/qemu-bundle-windows.tar" -C "$(out_of .#artifacts.qemu-bundle-windows)" .
	zst "$W/qemu-bundle-windows.tar" "$W/qemu-bundle-windows.tar.zst"
	"$H/briard-agent" --stage-manifest "$W" --chain host --platform windows --release "$V" || die "writing the windows manifest failed"

	# THE GUEST CHAIN: the OS image, its own series, no platform level. Measured: 1178 -> 377 MB
	# compressed, which is what a household link actually waits on.
	G="$DIR/guest/$GV"; mkdir -p "$G"
	install -m0644 "$(out_of .#artifacts.guest-disk)/nixos.qcow2" "$G/nixos.qcow2"
	zst "$G/nixos.qcow2" "$G/nixos.qcow2.zst"
	"$H/briard-agent" --stage-manifest "$G" --chain guest --release "$GV" || die "writing the guest manifest failed"

	for m in "$H" "$W" "$G"; do
		jq -e . "$m/manifest.json" >/dev/null || die "the manifest at $m is not valid JSON"
	done

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

	echo "$V" > "$DIR/VERSION" # not part of the tree; a human-readable marker for the operator
	say "staged $V:"
	for c in $CHAINS; do
		for a in $(arms_of "$c"); do arm=${a#-}
			m="$DIR/$c/$(sub "$(chain_id "$c" "$V")" "$arm")/manifest.json"
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
		v=$(staged_version "$DIR/$c") || die "no staged version under $DIR/$c — run \`stage\` first"
		rm -rf "$DIR/$c/latest"
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm"); m="$DIR/$c/$rel/manifest.json"
			[ -f "$m" ] || die "no manifest at $m"
			# -rawin is what makes this PureEdDSA over the whole file, which is what Verify does.
			# Without it openssl would pre-hash and every signature would be rejected.
			ossl pkeyutl -sign -inkey "$RELEASE_SIGN_KEY" -rawin -in "$m" -out "$m.sig"
			n=$(stat -c%s "$m.sig")
			[ "$n" = 64 ] || die "$m.sig is $n bytes, want a raw 64 — the verifier rejects anything else"
			say "signed $c/$rel (64-byte detached Ed25519)"
			# THE `latest` POINTER, laid here rather than in `publish`: a pointer is a byte-copy
			# of the signed manifest, so it exists the moment the signature does — and a staged
			# tree that already carries it is served AS-IS by the publish gate
			# (lab/vanilla-linux), which installs from `latest` before anything is uploaded.
			p="$DIR/$c/$(sub latest "$arm")"; mkdir -p "$p"
			cp "$m" "$m.sig" "$p/"
			for b in briard-agent briard-agent.exe; do
				if [ -f "$DIR/$c/$rel/$b" ]; then cp -p "$DIR/$c/$rel/$b" "$p/$b"; fi
			done
			# A chain without arms ran its one (empty) arm; a chain with arms must not run an
			# extra empty one after them.
		done
	done
	;;

publish)
	DIR="${2:-$STAGE_DEFAULT}"
	need nix
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	[ -f "$DIR/install.sh" ] || die "no install.sh in $DIR — run \`stage\` (it embeds the keyring)"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	say "publishing $(cat "$DIR/VERSION" 2>/dev/null || echo '?') to $RELEASE_WRITE"

	# IMMUTABILITY FIRST, across every chain, before a byte moves: a half-published release
	# (host uploaded, guest refused) would leave `latest` naming a pair nobody tested together.
	for c in $CHAINS; do
		v=$(staged_version "$DIR/$c") || die "no staged version under $DIR/$c"
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm")
			[ -f "$DIR/$c/$rel/manifest.json.sig" ] || die "$c/$rel is unsigned — run \`sign\` before publishing"
			[ -f "$DIR/$c/$(sub latest "$arm")/manifest.json" ] || die "$c has no latest pointer for $rel — run \`sign\`"
			! have_key "$bucket/$c/$rel/manifest.json" "$endpoint" ||
				die "$c/$rel is ALREADY PUBLISHED and versioned directories are immutable (see header) — to re-point, \`promote\`; to ship a fix, commit and stage again"
		done
	done

	for c in $CHAINS; do
		v=$(staged_version "$DIR/$c")
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm")
			# The versioned directory: artifacts first, manifest pair last (same ordering
			# argument as the pointers). No --delete anywhere in this script any more: nothing is
			# ever overwritten, so there is nothing to clean up — and the bucket ALSO holds
			# `catalog/` (live runtime content the agent fetches for `briard service install`,
			# produced by nothing in this repo), which a wide --delete would silently remove.
			aws s3 sync "$DIR/$c/$rel" "$bucket/$c/$rel/" --endpoint-url "$endpoint" \
				--exclude manifest.json --exclude manifest.json.sig --exclude "*/*" --no-progress
			aws s3 cp "$DIR/$c/$rel/manifest.json.sig" "$bucket/$c/$rel/manifest.json.sig" --endpoint-url "$endpoint" --no-progress
			aws s3 cp "$DIR/$c/$rel/manifest.json"     "$bucket/$c/$rel/manifest.json"     --endpoint-url "$endpoint" --no-progress
			# ...and only now the pointer, so `latest` never names bytes that are not there yet.
			p=$(sub latest "$arm")
			for f in $POINTER_FILES; do
				[ -f "$DIR/$c/$p/$f" ] || continue
				aws s3 cp "$DIR/$c/$p/$f" "$bucket/$c/$p/$f" --endpoint-url "$endpoint" --no-progress
			done
			say "published $c/$rel, $p -> $v"
		done
	done

	# ...and the installer itself, at the root the one-liner names.
	aws s3 cp "$DIR/install.sh" "$bucket/install.sh" --endpoint-url "$endpoint" --no-progress

	{
		for c in $CHAINS; do
			find "$DIR/$c/latest" -type f | sed "s|^$DIR/|$CHANNEL/|"
		done
		echo "$CHANNEL/install.sh"
	} | purge_edge
	say "published — now run: ./scripts/publish-release.sh verify   (then, once the evidence is in: promote)"
	;;

promote)
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	V="${2:-}"
	if [ -z "$V" ]; then
		V=$(curl -fsS "$CHANNEL/host/latest/linux/manifest.json" | jq -r .version) || die "cannot read host/latest to default the version"
		say "no version given — promoting what host/latest names: $V"
	fi
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
	# Every chain and arm is checked before any of them moves: promotion is of the PAIR
	# (host/stable + guest/stable is the tested pair by construction — there is no top-level
	# install pointer, and this is the obligation that stands in for one).
	for c in $CHAINS; do
		v=$(chain_id "$c" "$V")
		for a in $(arms_of "$c"); do arm=${a#-}
			rel=$(sub "$v" "$arm"); p=$(sub stable "$arm"); tag="$c.${arm:-flat}"
			have_key "$bucket/$c/$rel/manifest.json" "$endpoint" || die "$c/$rel is not published; nothing to promote"
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
				# Server-side copies, in POINTER_FILES order and for the same reason.
				for f in $POINTER_FILES; do
					have_key "$bucket/$c/$rel/$f" "$endpoint" || continue
					aws s3 cp "$bucket/$c/$rel/$f" "$bucket/$c/$p/$f" --endpoint-url "$endpoint" --no-progress
				done
				say "promoted $c/$p -> $v"
			fi
		done
	done
	{
		for c in $CHAINS; do
			for a in $(arms_of "$c"); do arm=${a#-}
				for f in $POINTER_FILES; do echo "$CHANNEL/$c/$(sub stable "$arm")/$f"; done
			done
		done
	} | purge_edge
	say "promoted $V — now run: ./scripts/publish-release.sh verify"
	;;

gc)
	shift
	need nix; need curl; need jq
	[ -n "${RELEASE_WRITE:-}" ] || die "set RELEASE_WRITE to the channel's write URL"
	# gc NEVER DELETES. Old releases are needed as upgrade targets for a pin, and as the bytes
	# a rollback re-points to — rebuilding will not reproduce them (see the header). So they
	# leave the SERVING tree and go cold, and the cold store is a required input rather than a
	# default, because "archive to nowhere" is deletion with a nicer name.
	[ -n "${RELEASE_ARCHIVE:-}" ] || die "set RELEASE_ARCHIVE to the cold store URL — gc archives, it does not delete"
	bucket=$(bucket_of "$RELEASE_WRITE"); endpoint=$(endpoint_of "$RELEASE_WRITE")
	archive=$(bucket_of "$RELEASE_ARCHIVE")
	# Pins live in the cloud's rollout state, which this script cannot see; the operator names
	# them. A release that is pinned and not named here is archived, and the pinned node's next
	# tick fails its fetch loudly (a failed directive, not a silent no-op) — recoverable by
	# copying the release back, which is why the archive holds whole releases.
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
			# client, from an attack. (The nix cache has the sharper version of this rule:
			# `nix copy` remembers "the destination has it" for 30 days, so a path deleted by
			# hand stays present to the next publish and is silently never re-uploaded —
			# observed 2026-08-06. Whoever builds cache GC inherits that.)
			say "archiving $c/$v (published $when) -> $archive/$c/$v/"
			aws s3 mv "$bucket/$c/$v/" "$archive/$c/$v/" --recursive --endpoint-url "$endpoint" --no-progress
		done
	done
	;;

verify)
	need curl; need sha256sum; need jq
	PUB="${RELEASE_PUBKEY:-${RELEASE_SIGN_KEY:-}}"
	[ -n "$PUB" ] || die "set RELEASE_PUBKEY to the PKIX PEM public key"
	tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
	# EVERY CHAIN, EVERY ARM, BOTH POINTERS. A pointer is a manifest at a path, so verifying one
	# is verifying all of them with the path changed — and a pointer nobody verifies is a pointer
	# nobody knows is broken until a stranger (or the timer, fleet-wide) runs into it.
	# `stable` may not exist yet on a fresh tree; that is said out loud rather than failed,
	# because the first publish of the tree is the one run where it is expected.
	for c in $CHAINS; do
		for a in $(arms_of "$c"); do arm=${a#-}
			for p in stable latest; do
				rel=$(sub "$p" "$arm"); base="$CHANNEL/$c/$rel"
				if ! curl -fsS "$base/manifest.json" -o "$tmp/manifest.json" 2>/dev/null; then
					[ "$p" = stable ] && { say "WARNING: no $c/$rel yet — nothing promoted here"; continue; }
					die "no manifest at $base"
				fi
				curl -fsS "$base/manifest.json.sig" -o "$tmp/manifest.json.sig" || die "no signature at $base"
				# Verify the way the agent does: raw Ed25519 over the exact bytes. A failure
				# here is the whole point of the check — an unsigned or re-signed channel must
				# not read as green.
				ossl pkeyutl -verify -pubin -inkey "$PUB" -rawin \
				        -in "$tmp/manifest.json" -sigfile "$tmp/manifest.json.sig" >/dev/null \
					|| die "the live manifest at $base does NOT verify against $PUB"
				v=$(jq -r .version "$tmp/manifest.json")
				ch=$(jq -r .chain "$tmp/manifest.json"); pl=$(jq -r '.platform // ""' "$tmp/manifest.json")
				[ "$ch" = "$c" ] && [ "$pl" = "$arm" ] ||
					die "$base serves a manifest for '$ch/$pl' — a crossed wire the client would refuse"
				vdir="$c/$(sub "$v" "$arm")"
				say "$c/$rel -> $v (signature verifies)"
				# ...and every artifact matches what the signed manifest claims, fetched from
				# WHERE THE MANIFEST SAYS (the versioned directory), by DOWNLOADING, not by
				# trusting the manifest against itself. A version already checked under the
				# other pointer is not downloaded twice.
				if [ -f "$tmp/$c.${arm:-flat}.$v.ok" ]; then echo "    (artifacts of $vdir already verified)"; continue; fi
				jq -r '.artifacts[] | "\(.name) \(.sha256) \(.size)"' "$tmp/manifest.json" |
				while read -r name sum size; do
					curl -fsS "$CHANNEL/$vdir/$name" -o "$tmp/$name" || die "$name is in the $c/$rel manifest but not served at $vdir/"
					got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
					gotsize=$(stat -c%s "$tmp/$name")
					[ "$got" = "$sum" ] || die "$vdir/$name: sha256 $got != manifest $sum"
					[ "$gotsize" = "$size" ] || die "$vdir/$name: size $gotsize != manifest $size"
					echo "    ok  $vdir/$name  ($gotsize bytes)"
					# The pointer's own bootstrap copy must be the SAME bytes the manifest pins,
					# or install.sh runs a bootstrap that is not the release it then installs.
					case "$name" in briard-agent|briard-agent.exe)
						curl -fsS "$base/$name" -o "$tmp/$name.ptr" || die "$name is not served under $base (the bootstrap install.sh curls)"
						[ "$(sha256sum "$tmp/$name.ptr" | cut -d' ' -f1)" = "$sum" ] || die "$base/$name differs from $vdir/$name"
						echo "    ok  $c/$rel/$name  (bootstrap copy matches)"
					esac
				done
				touch "$tmp/$c.${arm:-flat}.$v.ok"
			done
		done
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
	say "$CHANNEL verifies end to end: every pointer signed, every artifact matching, install.sh served at the root and pointing here"
	;;

*)
	die "usage: publish-release.sh {stage|sign|publish|promote|gc|verify} [ARGS]  (see header)"
	;;
esac
