# The briard.io module's vendorHash, in ONE place.
#
# Every Go package in this repo shares it, because they all vendor the same thing: this
# module's go.mod/go.sum. It is module-wide, so it is unaffected by build tags and by which
# subPackages a given derivation builds — the vendor dir carries every dep either way.
#
# Copies of it, coupled only by comments, would have to be edited in lockstep for every new
# dependency and go stale when one is missed. One definition, no lockstep, nothing to remember.
#
# TO REGENERATE, when you change go.mod or go.sum:
#   1. set this to the string `lib.fakeHash` names (no `lib` is in scope in this file)
#   2. nix build --no-link .#artifacts.agent
#   3. copy the reported "got:" hash here
#
# ⚠️ THE PRIVATE SIBLING READS THIS FILE BY PATH. It builds one binary straight from this repo
# (the anchor-side witness-forwarder) and needs this exact value; rather than keep a copy that
# goes stale the moment its pin moves, it imports this path out of the pinned source. So the
# PATH and the SHAPE are load-bearing: this file must stay at the repo root and must evaluate
# to the hash string alone. Move it, or wrap it in an attrset, and the sibling's build breaks.
# (Loudly — it imports rather than greps — but it breaks.)
"sha256-jcMD0leD8MtoCWaiLKu2zuEVPpCxgpn9agOqxtx9zlk="
