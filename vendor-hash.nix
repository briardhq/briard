# The briard.io module's vendorHash, in one place. Every Go package in this repo vendors the
# same go.mod/go.sum, so it is module-wide: unaffected by build tags and by which subPackages a
# derivation builds (the vendor dir carries every dep either way). Copies coupled only by
# comments would need lockstep edits for every new dependency and go stale when one is missed.
#
# To regenerate, when you change go.mod or go.sum:
#   1. set this to the string `lib.fakeHash` names (no `lib` is in scope in this file)
#   2. nix build --no-link .#artifacts.agent
#   3. copy the reported "got:" hash here
#
# ⚠️ The private sibling reads this file by path: it builds one binary straight from this repo
# (the anchor-side witness-forwarder), needs this exact value, and imports it out of the pinned
# source rather than keep a copy that goes stale the moment its pin moves. So the path and the
# shape are load-bearing: this file must stay at the repo root and evaluate to the hash string
# alone. Move it or wrap it in an attrset and the sibling build breaks (loudly, since it imports
# rather than greps, but it breaks).
"sha256-SlRdQFPUCDMNC1At6B1lN6YC6TgdBvsGKJYt7AKYbIo="
