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

## The index: what a household can see

`index.json` is the catalog's listing — every app the dashboard and the website show, in the
order they show it: the ones a node can install today and the ones a household may ask for. It
is published beside the manifests with a detached signature over its exact bytes, made with the
same release key and verified the same way (`shared/manifest.ParseIndex`, `Catalog.FetchIndex`),
so a mirror that cannot forge a manifest cannot forge the list either. Unlike a manifest its
bytes are not an identity: an edit here changes what people read, never what a node runs.

One entry per line, so a diff reads per app. Each carries:

| Field | What it is |
|---|---|
| `name` | the catalog slug — the key; the manifest for an installable app is `<name>.json` beside this file |
| `title` | the app's name for a person |
| `functionality` | two to five brand-less words on what it does, shown beside the title ("document management system") |
| `description` | a few lines on what it does for a household; no promise, no version |
| `icon` | the app's mark as an SVG data URI, at most 16 KB decoded, with no script, handler, external reference or foreign content |
| `homepage` | the upstream project's site |

**Availability is by omission.** An app is installable when `<name>.json` + `.sig` exist beside
the index, and not otherwise. There is no status field: it would be a second statement of the
same fact, one that could disagree with the first, and flipping it would mean re-signing the
index while a manifest's arrival already is a signed act. `catalog_test.go` checks the one
relation that must hold — every manifest here is listed — and parses the index with the
function a node uses.

**Choosing what to list.** The index is for apps a household wants, not a mirror of everything
that can be self-hosted. Left out on purpose: anything whose role the platform already fills
(reverse proxies, container managers, remote-access tunnels, auto-updaters, uptime and metrics
stacks). Apps that need a user-data store, or a design pass of their own, are listed although
none is installable yet: the index exists to learn which of them to build.

**Icons** come from [homarr-labs/dashboard-icons](https://github.com/homarr-labs/dashboard-icons)
(Apache-2.0 for the collection; each mark remains its project's) unless the collection's copy is
a bitmap, in which case the project's own vector logo is used — see `THIRD-PARTY.md`. A page
shows them through `<img>`, where an SVG's own styles and scripts cannot reach the page; the
validation above is the second line.

## Publishing

The catalog is **not release content** and has its own lifecycle: `publish-release.sh` never
touches `catalog/`, and `--delete` on a release sync is prefix-scoped so it cannot remove it. An
entry goes live by signing these bytes with the release key and uploading the pair
(`<name>.json`, `<name>.json.sig`) to the bucket; the index goes live the same way
(`index.json`, `index.json.sig`).

An entry may only be published once its image has been through whatever gate the catalog demands
of it: for Home Assistant that includes booting under the token wrapper (`agent/hass`), because an
image whose s6 furniture moved would otherwise become a fleet incident rather than a failed
promotion.

## What an entry costs: `size` and `installedSize`

Every entry declares two totals, measured once when the entry is made and signed with the rest:
`size` is what a node **downloads** (the compressed layers of every container's image, summed)
and `installedSize` is what the image store **holds** afterwards (the same layers uncompressed —
two to four times more; Home Assistant 2026.9.4 is 646 MB down and 2.47 GB on disk). The first is the
dashboard's progress bar; the second is the free-space check a node runs *before* the first byte
moves. Two scalars rather than a layer table: the digest already commits to every layer, and
podman's own store knows each finished layer's size. To measure a new entry:

```sh
skopeo copy docker://<image@digest> dir:/tmp/img            # then, in /tmp/img:
jq '[.layers[].size] | add' manifest.json                     # size
for l in $(jq -r '.layers[].digest' manifest.json); do zstd -dc "${l#sha256:}"; done | wc -c   # installedSize (zstd layers; gzip -dc for a gzip image -- the mediaType says which)
```

## What an entry needs to run: `minMemoryMB`

The guest starts small and grows while it runs, so an entry does not declare its worst case — only
what it needs from its **first second**, before growth can react: its settled footprint with room
for its own startup. A node boots its guest with at least the system's share plus every installed
entry's `minMemoryMB`, and grows the running guest to that before installing one more (refusing
only when the host cannot give that much at all). Anything a service needs beyond its minimum is
found at runtime, by growth. Measure a new entry from its anonymous memory settled after startup
(the cgroup's `anon` in `memory.stat`) — not from `docker stats`, which counts page cache too.
Omitted, the entry counts for nothing until it needs more.

## What is not in a manifest

The schema deliberately cannot express host binds, privileges, host networking or a command line —
capability comes by omission, so a catalog entry cannot reach the host. Anything a service needs
beyond that is **product-side, keyed on the service name** (`agent/services`): Home Assistant's
control channel, mosquitto's config file. If a new entry cannot be expressed here, that is the
question to answer, not a field to add.
