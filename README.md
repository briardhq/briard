# Briard

**The best way to run Home Assistant on a machine you already own.**

You install one agent. It turns the machine into a node that runs your services inside a
managed VM and does the sysadmin: tested updates that **undo themselves automatically** if
something regresses, snapshots and one-command rollback, and — with a second machine —
**takes over in seconds** when one dies.

**No account. No cloud. No telemetry.** Your home keeps working with the internet down, and
what you install reports nothing, ever. ([Why, and how you can check it.](ARCHITECTURE.md#local-first-and-cloudless-by-default))

> **Status: alpha.** The failover, safe-upgrade, and reach-by-name stack is built and
> tested, including under a long-running fault soak. It has run on machines we control; it
> has not yet run on many machines we don't. Expect rough edges, expect to read carefully,
> and please [tell us what broke](https://github.com/briardhq/briard/issues).

## Install

```sh
curl -fsSL https://get.briard.io/install.sh | sudo sh
```

The installer checks the machine first and **refuses with the reason** if it is not suitable,
rather than half-installing and leaving you to work out why. Artifacts are signed, and it
verifies signature, hash, and size before using anything.

What you get is the **node**: ready, replicating, and able to fail over. It installs **no
service** — a machine is set up first, and then you choose what runs on it. The closing line
tells you where the node answers:

> `http://briard-<name>.local/` (or `http://<vip>/`)

The name first, because it stays true if the address ever moves; the address as the fallback
for a client whose mDNS does not resolve (Android is the usual offender).

If you would rather read before you run, that URL serves
[`scripts/install.sh`](scripts/install.sh) from this repo, plus the release public key embedded
at publish time. You can also [build and install from source](CONTRIBUTING.md#build-and-install-from-source).

## Install a service

```sh
sudo briard service install home-assistant
```

The name is an entry in the **catalog** — signed static files at
`https://get.briard.io/catalog`, verified against the same release key as the install itself.
A catalog entry pins the image by digest, so what you get does not depend on trusting the
registry, and that manifest is written to the replicated volume: the node keeps running,
failing over and rolling back with the catalog unreachable. The install pulls the image, puts
its data on the replicated volume, and starts it behind a health gate that reverts the node if
it does not come up.

> **Alpha gap:** the front door at `http://briard-<name>.local/` does not route to a service you
> install this way yet — it keeps serving Briard's own page, and the service answers on its own
> port (Home Assistant: `http://briard-<name>.local:8123/`). Per-domain routing is the next
> thing landing. The catalog is also one entry long today.

## Commands

`briard` administers the node it runs on. All of it needs root, and `briard help <command>`
shows one command's options.

**Everyday**

| | |
|---|---|
| `sudo briard alerts` | what this node has warned about |
| `sudo briard logs` | what this node has logged (`-follow` to stream) |
| `sudo briard service install <name>` | install a catalogued service on this node |
| `sudo briard handover` | hand this node's work to a peer (a planned failover) |

**Repair and maintenance**

| | |
|---|---|
| `sudo briard rescue` | rebuild this node's guest from its image (`-yes` to confirm) |
| `sudo briard os upgrade <closure>` | switch this node to a system closure, health-gated |
| `sudo briard directive <kind> [payload]` | submit a directive to the local agent |
| `sudo briard run` | run the agent itself — the installer's units do this for you |

## When something looks off

**Start with `sudo briard alerts`.** A free install talks to no server of ours, so there is
nobody to send you mail — the node records what it notices and waits to be asked. `alerts`
prints what this node has warned about (a lost replica, an upgrade that failed and rolled back,
a guest that lost contact with its host) and **names any surface it could not read**, so an
empty result is never a guess. Nothing is pushed to you, so run it when something looks wrong,
or on a timer.

**`sudo briard logs` reads both logs**, and a bug report wants both: `journalctl -u briard-agent`
is the host's side, and `/var/log/briard-guest-console.log` is the guest's own serial console —
the only view into the VM's own boot and kernel.

Both work even when the agent is down.

## More

| | |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | how it works, and why |
| [CONTRIBUTING.md](CONTRIBUTING.md) | build it, run the tests, propose a change |

Found a security bug? Email **security@briard.io** rather than opening a public issue.

Apache-2.0 — see [LICENSE](LICENSE); the third-party software the release carries, and where its
source is, is in [THIRD-PARTY.md](THIRD-PARTY.md). Contributions are accepted under the DCO; there
is no CLA, deliberately. This repository was extracted from our private monorepo at open-sourcing, so its
history starts at that point; development continues here in the open.
