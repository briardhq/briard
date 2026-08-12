# The briard.io module's vendorHash, in ONE place.
#
# Every Go package in this repo shares it, because they all vendor the same thing: this
# module's go.mod/go.sum. It is module-wide, so it is unaffected by build tags and by which
# subPackages a given derivation builds — the vendor dir carries every dep either way.
#
# It lived inline in EIGHT files, coupled only by comments telling the next person to
# "regenerate together with the agent's". That coupling is not free: adding one dependency
# (klauspost/compress, a1d390e) meant editing eight files in lockstep, and the same shape has
# already gone stale twice in the sibling repo, which held copies of its own. One definition,
# no lockstep, nothing to remember.
#
# TO REGENERATE, when you change go.mod or go.sum:
#   1. set this to `lib.fakeHash`
#   2. nix build --no-link .#briard-agent
#   3. copy the reported "got:" hash here
#
# ⚠️ THE PRIVATE SIBLING READS THIS FILE BY PATH. It builds one binary straight from this repo
# (the anchor-side witness-forwarder) and needs this exact value; rather than keep a copy that
# goes stale the moment its pin moves, it imports this path out of the pinned source. So the
# PATH and the SHAPE are load-bearing: this file must stay at the repo root and must evaluate
# to the hash string alone. Move it, or wrap it in an attrset, and the sibling's build breaks.
# (Loudly — it imports rather than greps — but it breaks.)
"sha256-4d/F5wfaBgNfrt0bv6IuElUAR/wVr7yG8BYOX0dSq6c="
