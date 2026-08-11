#!/bin/sh
# publish-cache.sh — publish the guest OS closure to our nix binary
# cache, so a field guest can receive a new system closure.
#
# THE MODEL (settled 2026-07-30, after the first attempt failed against nix).
# We publish the WHOLE toplevel closure and let substituter *priority* produce the
# split, rather than uploading only the paths cache.nixos.org lacks. The reason is
# not preference: nix's binary-cache writer refuses any path whose references are
# not valid in the destination cache — `cannot add drbd-9.33.0 … because the
# reference glibc-2.42-67 is not valid` — so a strict our-paths-only cache cannot
# be written with `nix copy` at all. Only a server that bypasses that rule
# (cachix, attic) can hold one, and both are services we deliberately do not run.
#
# What a GUEST downloads is unchanged, because substituters are ranked and lower
# wins: we serve `Priority: 100`, cache.nixos.org serves 40. Stock nixpkgs keeps
# coming from the public CDN; only our overlaid paths (drbd / drbd-reactor /
# reverse-proxy / briard-agent / podman + crun) and this guest's own toplevel come
# from us — measured 2026-08-11: 79 paths, 140 MB of a 982 MB closure. `check`
# reports that split and IS the assertion that it still holds. (Was 56 paths, 74 MB
# of ~1.45 GB before [B.5]: the closure shrank by a third while our share grew, since
# slimming crun changes podman's hash and takes it off the public cache.)
#
# Holding the rest costs us storage (first publish ~476 MB; later releases are
# incremental, since store paths are content-addressed) and buys a real property:
# if cache.nixos.org is unreachable, our cache alone can complete an update.
#
# That `Priority: 100` is load-bearing and nix does NOT write it — the writer
# creates a nix-cache-info carrying StoreDir only. It is uploaded once by hand
# (see `cache-info`), and `verify` re-checks it against the live cache so the
# assumption is tested rather than remembered. Without it we would rank at nix's
# default and could outrank the CDN, which is precisely the ~1 GB per guest the
# split exists to avoid.
#
# ALWAYS RUN `verify` AFTER `publish`, and expect a skip rather than be surprised
# by one. `nix copy` decides what to upload by asking whether the destination
# already has each path, and caches that answer locally for
# narinfo-cache-positive-ttl — THIRTY DAYS by default. So anything deleted from the
# bucket by hand stays "present" to the local cache long after it is gone, and the
# next publish silently skips it. Observed 2026-08-06: the first real publish
# omitted 4 paths (glibc, xgcc-libgcc, libidn2, libunistring) because an end-to-end
# probe five days earlier had uploaded and then deleted exactly those. `verify`
# caught it; nothing else would have, and the result would have been a cache that
# works only while cache.nixos.org is up — precisely the property this design buys.
# NOTE `--option narinfo-cache-positive-ttl 0` does NOT help: it is a restricted
# setting and is silently ignored for non-trusted users. What clears a stale entry
# is a lookup that 404s, which is what `verify` does — so recovery is
# verify → publish → verify.
#
# The signing key is a trust root DISTINCT from the release keyring: nix's
# own per-path narinfo signatures, not a detached signature over a manifest.
# Signing is a deliberate act with an offline key, never CI's (custody).
# ⚠️ `keygen`'s default private-key path is ./cache-priv-key.pem — i.e. THIS repo,
# which is published publicly. It is gitignored as a backstop, but pass
# CACHE_SIGN_KEY and keep the key outside the tree.
#
# Subcommands:
#   keygen     [KEYNAME]     generate the signing keypair (KEYNAME default cache.briard.io-1)
#   check      [FLAKE_ATTR]  what only we can serve, and how big — the "split holds" assertion
#   publish    [FLAKE_ATTR]  sign + copy the closure to $CACHE_WRITE
#   cache-info               print the nix-cache-info the cache must serve (upload once)
#   verify     [FLAKE_ATTR]  assert the live cache's priority + that it serves the closure
#
# Env:
#   CACHE_URL       public read base, as baked into configuration.nix (default https://cache.briard.io)
#   CACHE_WRITE     write store URL — required by `publish`, e.g.
#                   's3://briard-cache?endpoint=<account>.r2.cloudflarestorage.com&region=auto'
#                   with AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from the R2 token
#   UPSTREAM        the public cache we rank behind        (default https://cache.nixos.org)
#   CACHE_SIGN_KEY  path to the private signing key        (publish mode; release secret store)
#
# Run from the briard repo root (nix resolves the flake from the cwd). The default
# attr is the free (no-service) guest; pass another to publish HA.
set -eu

CACHE_URL="${CACHE_URL:-https://cache.briard.io}"
UPSTREAM="${UPSTREAM:-https://cache.nixos.org}"
# Our rank. Lower wins, so this must stay ABOVE cache.nixos.org's 40 — see the header.
PRIORITY=100
# The SHIPPED guest's running generation (guest-disk's `system` passthru) — not
# `nixosConfigurations.guest`, which is the framework-boot variant carrying neither a
# bootloader nor the briard-agent units, i.e. a closure no field guest ever runs.
DEFAULT_ATTR=".#artifacts.guest-disk.system"

say() { printf '>>> %s\n' "$*" >&2; }
die() { printf 'publish-cache: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }

# The out path of a flake attr (builds/substitutes it first).
build() { nix build --no-link --print-out-paths "$1" || die "could not build $1"; }

# Filter stdin store paths to those UPSTREAM does NOT serve — one narinfo HEAD per
# path, parallelised. This is the split: what a guest can only get from us.
not_upstream() {
	xargs -r -P8 -I@ sh -c '
		h=${1#/nix/store/}; h=${h%%-*}
		curl -sfI -o /dev/null "$2/$h.narinfo" || printf "%s\n" "$1"
	' _ @ "$UPSTREAM"
}

# Total NAR bytes of the store paths on stdin.
nar_bytes() {
	# -s slurps, so a long path list that xargs splits across several `nix path-info`
	# invocations still sums to ONE number (otherwise the caller's $((...)) sees two
	# lines and dies). `.[][]` then iterates each document, whose `.[]` yields an
	# array's elements and an object's values alike — both shapes --json has used.
	xargs -r nix path-info --json | jq -s '[.[][].narSize // 0] | add // 0'
}

need nix
need nix-store
need curl
need xargs
need jq

sub="${1:-}"
[ "$#" -gt 0 ] && shift || true
case "$sub" in
keygen)
	name="${1:-cache.briard.io-1}"
	priv="${CACHE_SIGN_KEY:-cache-priv-key.pem}"
	pub="$priv.pub"
	[ -e "$priv" ] && die "$priv exists; refusing to overwrite a signing key"
	nix-store --generate-binary-cache-key "$name" "$priv" "$pub"
	say "private key -> $priv  (SECRET — release store, NEVER commit)"
	say "public  key -> $pub   (bake into configuration.nix trusted-public-keys)"
	cat "$pub"
	;;
check)
	out=$(build "${1:-$DEFAULT_ATTR}")
	all=$(nix-store --query --requisites "$out")
	ours=$(printf '%s\n' "$all" | not_upstream)
	n=$(printf '%s' "$ours" | grep -c . || true)
	tot=$(printf '%s' "$all" | grep -c . || true)
	say "$n of $tot paths are not on $UPSTREAM — what a guest can ONLY get from us:"
	printf '%s\n' "$ours"
	if [ "$n" -gt 0 ]; then
		o=$(printf '%s\n' "$ours" | nar_bytes)
		a=$(printf '%s\n' "$all" | nar_bytes)
		say "guest downloads from us: $((o / 1000000)) MB of a $((a / 1000000)) MB closure (NAR, pre-compression)"
		say "we store all $tot paths; priority $PRIORITY keeps the other $((tot - n)) coming from $UPSTREAM"
	fi
	;;
publish)
	[ -n "${CACHE_WRITE:-}" ] || die "set CACHE_WRITE to the write store URL (see header)"
	[ -n "${CACHE_SIGN_KEY:-}" ] || die "set CACHE_SIGN_KEY to the private signing key"
	[ -e "$CACHE_SIGN_KEY" ] || die "no signing key at $CACHE_SIGN_KEY"
	out=$(build "${1:-$DEFAULT_ATTR}")
	# Append the key to whatever query the write URL already carries.
	case "$CACHE_WRITE" in
	*\?*) sep='&' ;;
	*) sep='?' ;;
	esac
	say "signing + copying the closure of $out"
	nix copy --to "$CACHE_WRITE${sep}secret-key=$CACHE_SIGN_KEY" "$out"
	say "done — now run: $0 verify"
	;;
cache-info)
	# nix writes only StoreDir, so the file that ranks us behind the CDN is ours to
	# upload (once, at bucket creation). `verify` asserts the live cache serves it.
	cat <<-EOF
		StoreDir: /nix/store
		WantMassQuery: 1
		Priority: $PRIORITY
	EOF
	;;
verify)
	info=$(curl -sf "$CACHE_URL/nix-cache-info") || die "no nix-cache-info at $CACHE_URL"
	prio=$(printf '%s\n' "$info" | sed -n 's/^Priority: *//p')
	[ -n "$prio" ] || die "$CACHE_URL serves no Priority line — we would rank at nix's default and could outrank $UPSTREAM, making every guest pull the whole base from us. Upload \`$0 cache-info\`."
	[ "$prio" -gt 40 ] || die "Priority $prio does not rank behind cache.nixos.org (40) — guests would pull stock nixpkgs from us"
	say "priority $prio — $UPSTREAM (40) still wins for stock paths"
	out=$(build "${1:-$DEFAULT_ATTR}")
	# A plain GET, not HEAD: narinfos are ~1 KB, and GET is the one form that works
	# against a file:// URL too — so this check can be rehearsed against a local cache
	# before it is ever pointed at the real one.
	missing=$(nix-store --query --requisites "$out" | xargs -r -P8 -I@ sh -c '
		h=${1#/nix/store/}; h=${h%%-*}
		curl -sf -o /dev/null "$2/$h.narinfo" || printf "%s\n" "$1"
	' _ @ "$CACHE_URL")
	m=$(printf '%s' "$missing" | grep -c . || true)
	[ "$m" -eq 0 ] || {
		printf '%s\n' "$missing" >&2
		die "$m closure paths are absent from $CACHE_URL — a guest that cannot reach $UPSTREAM could not complete this update"
	}
	say "$CACHE_URL serves the whole closure of $out"
	;;
*)
	die "usage: publish-cache.sh {keygen|check|publish|cache-info|verify} [FLAKE_ATTR]  (see header)"
	;;
esac
