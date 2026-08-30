# The published catalog

These are the **exact bytes** served at `https://get.briard.io/catalog/<name>.json`, beside a
detached Ed25519 signature over those bytes made with the release key. A node fetches both,
verifies the signature *before* parsing, and keeps the raw bytes on its replicated volume: the
service's identity is `sha256` of exactly this file (`shared/manifest`).

Until now these documents existed only in the bucket. They are here so that a change to what the
fleet installs is reviewable, diffable and testable before it is live — `catalog_test.go` parses
and validates every entry against the real schema, so a typo fails in CI instead of on a
household's node.

## ⚠️ The bytes are the identity

**Any** change to a file here — a reordered field, an added trailing newline, a reformat — mints a
new service identity. Nodes read that as a new version of the service and will take the whole
upgrade path for it: snapshot, switch, health-gate, and a rollback if it fails. There is no such
thing as a cosmetic edit in this directory.

That is also why these files have **no trailing newline**: it is what the currently-published
bytes have, and matching them is what keeps `home-assistant.json` here the same service as the one
already installed in every household.

## Publishing

The catalog is **not release content** and has its own lifecycle: `publish-release.sh` never
touches `catalog/`, and `--delete` on a release sync is prefix-scoped so it cannot remove it. An
entry goes live by signing these bytes with the release key and uploading the pair
(`<name>.json`, `<name>.json.sig`) to the bucket — see the `publish-release` skill.

An entry may only be published once its image has been through whatever gate the catalog demands
of it: for Home Assistant that includes booting under the token wrapper (`agent/hass`), because an
image whose s6 furniture moved would otherwise become a fleet incident rather than a failed
promotion.

## What is not in a manifest

The schema deliberately cannot express host binds, privileges, host networking or a command line —
capability comes by omission, so a catalog entry cannot reach the host. Anything a service needs
beyond that is **product-side, keyed on the service name** (`agent/services`): Home Assistant's
control channel, mosquitto's config file. If a new entry cannot be expressed here, that is the
question to answer, not a field to add.
