# Architecture

A high-level tour of how Briard is built and why. It is deliberately short — enough to
judge the foundations and find your way around the code, not a subsystem reference.

## The shape

One **agent** runs on the host you own. It is privileged, and it is the only thing you
install. Everything it manages runs inside a **guest VM** built from a NixOS image that
ships with the agent.

```
        ┌──────────────────────────────────────────────┐
        │ host (your machine)                          │
        │                                              │
        │   briard-agent ──────┐                       │
        │   (privileged)       │ control channel       │
        │                      ▼                       │
        │   ┌──────────────────────────────────────┐   │
        │   │ guest VM (NixOS)                     │   │
        │   │   front door (answers the VIP)       │   │
        │   │   your services — or none yet        │   │
        │   │   DRBD + drbd-reactor, over LUKS     │   │
        │   │   a small guest agent                │   │
        │   └──────────────────────────────────────┘   │
        │        │ NICs: children of the host's NIC    │
        └────────┼─────────────────────────────────────┘
                 ▼ your LAN — the guest is an ordinary machine on it
```

**The host/workload boundary is real and never collapses.** The agent orchestrates; the
workload runs in the guest; they speak only over a defined channel. The host holds every
identity and every decision; the guest agent is *hands* — it carries out what the host pushes
and reports what it sees. That boundary is what lets the agent replace, snapshot, or roll back
the entire guest without the workload having any say in it — and what keeps a compromised
workload away from the host.

The guest is **cattle**. It is a build artifact, rebuilt rather than repaired, delivered as a
whole disk image, and its identity is the store path of its system closure. Everything that
makes a node *this* node — its name, its addresses, its storage layout, its peers — is held by
the host and pushed in at every bring-up, never baked into the image.

**The guest is a citizen of your LAN, not a tenant behind your host.** Its network interfaces
are children of the host's own NIC (macvtap), so it takes an address from your router like
any other machine, owns the service address (the **VIP**) natively, and is found by name
(`briard-<name>.local`) with no reflector or relay. We never create a bridge, never move the
host's address, and never run a second DHCP server; if the machine already has a bridge —
libvirt, Proxmox, Incus — the guest joins it instead. Which shape a node gets is *derived from
the machine*, never configured.

**There is no Briard account and no Briard password.** Proof of access to the `briard` CLI on
the host is the household's credential: it mints a one-time code that becomes a per-device
session, further devices join by a six-digit quick-connect code, and the admin is whoever owns
the Home Assistant instance. The dashboard is served from the guest at the VIP; the host keeps
only a loopback console for the jobs a guest cannot do for itself.

## High availability

Storage replication is **DRBD**, and DRBD is the sole write-authority. The rules are few
and absolute:

- **Single primary.** Exactly one node may write at a time.
- **Quorum decides.** A node that loses quorum stops serving rather than guessing. Losing
  a machine is recoverable; two divergent copies of your home is not.
- **Nothing ever force-promotes.** There is no override, no flag, and no code path. This
  is enforced by a test that fails the build.

Failover itself is driven by **drbd-reactor**, not by the agent. The agent *observes and
reports* — it never promotes, demotes, or claims the service address. This is not a
stylistic choice: an orchestrator that can also promote is an orchestrator that can cause
split brain when it is confused, partitioned, or simply wrong. Removing that power removes
the failure mode. Two architecture tests hold the line, one at the API surface and one at
the exec surface.

A two-machine home can use a third **diskless witness** as a tiebreaker — it votes but
stores nothing, so quorum is real without a third full copy of your data.

**A lone machine runs without DRBD.** With one copy, replication protects nothing and turns
a single bad block into a dead node; so a single node runs its filesystem directly on the
volume, and DRBD is slid underneath it — in place, one reboot, no migration — the day a
second machine joins. The storage layout reserves the room for that from the first install.

Each of those guarantees is checked continuously by a long-running fault soak, and each has
a test in this repository you can run yourself:

| guarantee | holds when | run it |
|---|---|---|
| single primary | drbd-reactor promotes exactly one node, and starts the service stack only there | `.#tests.drbd-promote` |
| the minority refuses | an isolated node loses quorum and self-fences rather than promoting | `.#tests.drbd-fence` |
| failover keeps data | kill the primary; a survivor takes over with data intact | `.#tests.drbd-failover` |
| a witness is enough | a two-machine home survives losing one machine, and behaves when the witness itself goes | `.#tests.drbd-witness`, `.#tests.drbd-witness-loss` |
| writes really replicate | a write on the primary is on the peer before it is acknowledged | `.#tests.drbd-replicate` |
| no force-promotion exists | the pattern appears nowhere in the tree | `go test ./internal/arch/` |

## Storage

Your data lives on one disk the guest owns, built as
`disk → LUKS → LVM → (DRBD, once there is a peer) → btrfs`. The LVM layer exists for one
property: a logical volume can have its backing swapped underneath a device that is open, so
encryption can be turned on — or the disk moved — while DRBD never closes its device and never
resyncs. The layout is written by the host and built by the guest at bring-up, on a blank disk
only; a disk that already carries a signature is refused, never overwritten.

**Encrypted at rest by default, ready to be armed.** Where the CPU has AES the volume is
LUKS2 with a key stored in a token on the same disk, so it opens unattended on every boot —
the only unlock scheme that works on a stranger's LAN with nobody at a keyboard. That is not
protection against someone who has the disk; it is the state from which protection is one
command away, without a reformat. Where the CPU has no AES it runs in the clear rather than
crawl.

Each service's data is its own btrfs subvolume, so a snapshot and a rollback touch that
service and nothing else.

## Updates, and undoing them

The update model is the product, so it is built to be reversible. There are two kinds of
update, and they are deliberately kept apart:

**A service update** — a new signed manifest pinning a new container image — is a
`{code + data}` unit:

1. **Snapshot** the service's data before anything changes.
2. **Switch** to the new manifest.
3. **Gate** on health: the service must actually come back and serve.
4. **Roll back automatically** if it does not — *code and data together*, to the pair that
   was known good.

Code and data revert as a unit deliberately. Rolling back code while leaving migrated data
in place is how a "safe" rollback corrupts a home; the snapshot and the manifest are pinned
to each other so that cannot happen.

**An OS update** — a whole new guest image — never touches your services. The new OS boots,
runs the same containers on the same data, and is gated on their health; if it fails the
gate, the OS reverts and the workload has not been stopped, snapshotted, or restored. The
line is *read versus mutate*: the OS gate may read service health to judge itself; it may
never rewrite what a service owns. On a two-machine home the standby is updated first and
must prove itself before the primary follows.

The agent can also update **itself**. Because an agent cannot supervise its own
replacement, the mechanism is deliberately dumb and agent-independent: a frozen wrapper
starts the new binary, and the new binary signalling readiness *is* the commit. If it never
signals, the old one comes back with no timer, no coordinator, and no decision to get wrong.

The agent itself is watched by init: a systemd watchdog catches the one failure shape with
no other reflex — an agent that is alive but stuck. A trip kills it with a full stack dump
of every goroutine in the journal (evidence, not just a restart), the restarted agent
re-adopts the running guest, and your service never notices.

## Local-first, and cloudless by default

**The home keeps working with the internet down, and with our cloud down.** Local access is
the floor. Nothing on the critical path to serving your smart home requires reaching us.

What you install here goes further: it **reports nothing, ever**. No account, no telemetry,
no callback. If you install from source or from a release artifact, nothing contacts a
service we run. You can verify that claim rather than trust it — the whole agent is here.

**What the download channel records**, since it is the one place we see anything at all:
`get.briard.io` serves signed static files, and its own request logs give a per-day, per-file
count of successful fetches. Nothing else is kept, and nothing in that log identifies you. The
count is deliberately **not de-duplicated** — telling a returning visitor from a new one would
mean tracking someone — so it over-counts, and we would rather say that than imply a precision we
did not earn. Briard itself never reports a download, or anything at all: once installed, it never
calls home.

A managed tier — where we operate the machines and are on the hook for them — is the one
case where minimal health signals leave the house. What may ever be sent is a **closed
allowlist**, readable in one place (`shared/api`). Two properties make it checkable:

- Widening it is a visible source change. There is no dynamic field, no free-form blob.
- **Nothing about what a node reports is remotely toggleable.** No flag we flip can make
  your node say more than the code in front of you says it does.

Your home's data — automations, history, photos — stays on your hardware either way.

## Services

A node installs **no service**: a machine is set up first, and then you choose what runs on
it. What can run is the **catalog** — signed manifests, served beside a detached signature
over their exact bytes and checked in here (`catalog/`) so a change to what the fleet installs
is reviewable before it is live. A manifest names the image by digest; the node keeps the raw
manifest on its replicated volume, renders it into Podman units on whichever node is primary,
and the front door routes to it by name. Nothing about a service is baked into the guest
image, which is why the OS and the services update on independent schedules.

## The protocols

There are two: **host ↔ guest**, over a control channel, and **agent ↔ cloud**, used only by
a managed machine. Both are defined in [`shared/api`](shared/api), which is the normative
definition — one set of types, imported by both sides, so they cannot drift apart.

The host↔guest channel is a virtio-serial port, not a network socket: it does not share fate
with the guest's IP stack, so it survives exactly the events it exists to observe — a VIP
teardown, a DRBD partition, a self-fence. The protocol is explicitly versioned
(`GuestProtocol` / `MinGuestProtocol`). The host handshakes on connect and **refuses a guest
whose protocol it cannot speak** rather than proceeding and failing later. That is what lets
the host agent and the guest image update on independent schedules.

A prose specification will follow when there is a cloud tier worth writing one for. Until
then, pointing you at types that are compiled and tested is more honest than a document
that could quietly drift from them.

## Reproducibility

The system is built with **Nix**, so the guest image and the agent are deterministic build
products of this repository — not binaries we ask you to trust.

The identity of what your house runs is a **store path**, and a store path is a hash of
every input that produced it. So "am I running what they published?" is a question with an
exact answer:

```sh
# what this source tree produces
nix path-info --derivation .#artifacts.agent
nix path-info --derivation .#nixosConfigurations.guest.config.system.build.toplevel
```

Build from a checkout of the tag you installed and compare against the path recorded in the
release manifest. Matching hashes mean the artifact came from this source and nothing else —
no trust in our build machine required. Differing hashes mean something is wrong, and that
is worth telling us about.

This is also how rollback is defined internally: a rollback point is a store path, not a
version number, which is why reverting is exact rather than approximate.

## Testing

The claims above are mechanical, so they are tested mechanically — against real VMs, real
DRBD, and a real `drbd-reactor`, not mocks. Killing a primary in a test kills a real
primary.

You can run this yourself in minutes on a laptop:

```sh
nix build .#tests.drbd-failover -L   # kill the primary, survivor takes over, data intact
nix build .#tests.drbd-fence -L      # partition the minority, it self-fences
```

A whole tag is a list of names, not a build target — each test boots two or three nested VMs, so
running a group takes an explicit cap:

```sh
m=$(nix build --no-link --print-out-paths .#test-manifest)
nix build --max-jobs 1 -L $(sed 's|^|.#tests.|' $m/tags/drbd)   # the whole failover net
```

A test that cannot fail is not evidence, so the suite is written to fail: the rollback
tests use a deliberately broken upgrade, and the fencing tests assert that a node
*refuses* to promote.

## Code map

```
agent/            the host daemon: orchestration, and the provider seams
  agent/host      orchestration — talks to providers only through interfaces
  agent/drbd      reads DRBD status; drives nothing
  agent/guest     the host↔guest boundary and the upgrade/rollback mechanism
  agent/guestagent  the guest side of that boundary: executes and reports, decides nothing
  agent/nic, agent/subnet  the guest's place on your LAN, derived from the host's
  agent/reportcard  the install-time verdict on whether this machine can be a node
shared/           wire types (api), domain types (model), the manifest schema, the notify seam
catalog/          the signed service manifests, exactly as published
guest-image/      the NixOS guest: DRBD, drbd-reactor, storage, the front door (ships running nothing)
reverse-proxy/    the front door: answers the VIP, routes by name, terminates TLS, hot-reloads both
dashboard/        the household dashboard, served from the guest at the VIP
internal/arch     the architecture guards, as failing tests
nixosTest/        real-VM tests of the mechanisms above
scripts/          the installer
```

Provider integrations (overlay, DNS, guest management, cloud) sit behind **interfaces**,
each with a real implementation and a stub. The orchestrator only ever sees the interface,
which is why the whole system is testable without any of them.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the invariants that are enforced rather than
merely intended.
