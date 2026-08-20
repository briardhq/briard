# Architecture

A high-level tour of how Briard is built and why. It is deliberately short — enough to
judge the foundations and find your way around the code, not a subsystem reference.

## The shape

One **agent** runs on the host you own. It is privileged, and it is the only thing you
install. Everything it manages runs inside a **guest VM** built from a NixOS image that
ships with the agent.

```
        ┌──────────────────────────────────────────┐
        │ host (your machine)                      │
        │                                          │
        │   briard-agent ──────┐                   │
        │   (privileged)       │ control channel   │
        │                      ▼                   │
        │   ┌──────────────────────────────────┐   │
        │   │ guest VM (NixOS)                 │   │
        │   │   reverse proxy (answers the VIP)│   │
        │   │   your service — or none yet     │   │
        │   │   DRBD + drbd-reactor            │   │
        │   └──────────────────────────────────┘   │
        └──────────────────────────────────────────┘
```

**The host/payload boundary is real and never collapses.** The agent orchestrates; the
workload runs in the guest; they speak only over a defined channel. No workload logic
lives in the agent, and no agent logic lives in the guest. That boundary is what lets the
agent replace, snapshot, or roll back the entire guest without the workload having any say
in it — and what keeps a compromised payload away from the host.

The guest is **cattle**. It is a build artifact, rebuilt rather than repaired, and its
identity is the store path of its system closure.

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

## Updates, and undoing them

The update model is the product, so it is built to be reversible:

1. **Snapshot** the service data before anything changes.
2. **Switch** the code — a whole system closure, or a digest-pinned payload image.
3. **Gate** on health: the payload must actually come back and serve.
4. **Roll back automatically** if it does not — *code and data together*, to the pair that
   was known good.

Code and data revert as a unit deliberately. Rolling back code while leaving migrated data
in place is how a "safe" rollback corrupts a home; the snapshot and the closure are pinned
to each other so that cannot happen.

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

A managed tier — where we operate the machines and are on the hook for them — is the one
case where minimal health signals leave the house. What may ever be sent is a **closed
allowlist**, readable in one place (`shared/api`). Two properties make it checkable:

- Widening it is a visible source change. There is no dynamic field, no free-form blob.
- **Nothing about what a node reports is remotely toggleable.** No flag we flip can make
  your node say more than the code in front of you says it does.

Your home's data — automations, history, photos — stays on your hardware either way.

## The protocols

There are two: **host ↔ guest**, over a control channel, and **agent ↔ cloud**, used only by
a managed machine. Both are defined in [`shared/api`](shared/api), which is the normative
definition — one set of types, imported by both sides, so they cannot drift apart.

The host↔guest protocol is explicitly versioned (`GuestProtocol` / `MinGuestProtocol`). The
host handshakes on connect and **refuses a guest whose protocol it cannot speak** rather
than proceeding and failing later. That is what lets the host agent and the guest image
update on independent schedules.

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
nix build .#drbd                     # the whole failover net
```

A test that cannot fail is not evidence, so the suite is written to fail: the rollback
tests use a deliberately broken upgrade, and the fencing tests assert that a node
*refuses* to promote.

## Code map

```
agent/          the host daemon: orchestration, and the provider seams
  agent/host    orchestration — talks to providers only through interfaces
  agent/drbd    reads DRBD status; drives nothing
  agent/guest   the host↔guest boundary and the upgrade/rollback mechanism
shared/         wire types (api) and domain types (model), plus the dns/notify seams
guest-image/    the NixOS guest: DRBD, drbd-reactor, and the payload slot (empty as shipped)
reverse-proxy/  the front door: answers the VIP, terminates TLS, hot-reloads certs
internal/arch   the architecture guards, as failing tests
nixosTest/      real-VM tests of the mechanisms above
scripts/        the installer
```

Provider integrations (overlay, DNS, guest management, cloud) sit behind **interfaces**,
each with a real implementation and a stub. The orchestrator only ever sees the interface,
which is why the whole system is testable without any of them.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the invariants that are enforced rather than
merely intended.
