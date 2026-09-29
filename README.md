# Briard

**The best way to run Home Assistant on a machine you already own.**

You install one agent. It turns the machine into one that runs your apps inside a managed VM
and does the sysadmin: tested updates that **undo themselves automatically** if something
regresses, snapshots and one-command rollback, and — with a second machine — **takes over in
seconds** when one dies.

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

What you get is the **machine**: ready, replicating, and able to fail over. It installs **no
app** — a machine is set up first, and then you choose what runs on it. The closing lines tell
you where the machine answers, by name rather than address because the name stays true if the
address ever moves, and end with a one-time link to its Briard page:

> `http://briard-<name>.local/?code=…`

The link works once, for ten minutes; `sudo briard open` prints a fresh one any time.

If you would rather read before you run, that URL serves
[`scripts/install.sh`](scripts/install.sh) from this repo, plus the release public key embedded
at publish time. You can also [build and install from source](CONTRIBUTING.md#build-and-install-from-source).

## Install an app

Open the link the installer printed. The Briard page's first card is **Set up Home Assistant**:
one press, a minute or two while the machine downloads it, and **Open Home Assistant** lands you
in a logged-in Home Assistant. The same install from the machine that runs Briard:

```sh
sudo briard app install home-assistant
```

The name is an entry in the **catalog** — signed static files at
`https://get.briard.io/catalog`, verified against the same release key as the install itself.
A catalog entry pins the image by digest, so what you get does not depend on trusting the
registry, and that manifest is written to the replicated volume: the machine keeps running,
failing over and rolling back with the catalog unreachable. The install downloads the image,
puts its data on the replicated volume, and starts it behind a health gate that reverts the
machine if it does not come up.

Each installed app gets its own name on the LAN — `briard-<name>-<app>.local` (Home Assistant
on a machine called `brave-elf`: `http://briard-brave-elf-home-assistant.local/`) — and the
machine routes to it. `briard app install` prints the address when it finishes.
`http://briard-<name>.local/` stays the machine's own Briard page and lists what it runs.

Those names are `.local` (mDNS), so they work on the LAN and nowhere else. The Briard page
also offers a free name of the form `<name>.briard.casa` with a real certificate — you type an
email, click the link it receives, and the machine keeps the address and the certificate current
from then on. It is optional (**Skip** keeps the anonymous install first-class), and it is the one
time a free install tells a server of ours anything about itself.

> **Alpha gap:** the catalog is two entries long today — Home Assistant and the Mosquitto MQTT
> broker.

## Commands

`briard` administers the machine it runs on. All of it needs root, and `briard help <command>`
shows one command's options.

**Everyday**

| | |
|---|---|
| `sudo briard alerts` | what this machine has warned about |
| `sudo briard logs` | what this machine has logged (`-follow` to stream) |
| `sudo briard doctor` | check this machine now, and say what is wrong |
| `sudo briard app install <name>` | install an app from the catalog on this machine |
| `sudo briard app history <name>` | what happened to an app, oldest first, with the point that undoes each row |
| `sudo briard app undo <point>` | undo that row and everything after it (the undo is itself a row) |
| `sudo briard handover` | hand this machine's work to the other machine (a planned failover) |
| `sudo briard open` | print a one-time link that opens this home's Briard page, trusted |

**Repair and maintenance**

| | |
|---|---|
| `sudo briard version` | which briard this is, and which VM it runs your apps in |
| `sudo briard rescue` | rebuild briard on this machine from its image (`-yes` to confirm) |
| `sudo briard update [-vm]` | update briard's own software, or with `-vm` the VM it runs apps in, from the release channel |
| `sudo briard uninstall -yes [-delete-data]` | remove briard from this machine; your data is kept unless told otherwise |
| `sudo briard directive <kind> [payload]` | submit a directive to the local agent |
| `sudo briard run` | run the agent itself — the installer's units do this for you |

## When something looks off

**Start with `sudo briard alerts`.** A free install reports to no server of ours, so there is
nobody to send you mail — the machine records what it notices and waits to be asked. `alerts`
prints what this machine has warned about (a lost replica, an upgrade that failed and rolled
back, an app that lost contact with the agent) and **names any surface it could not read**, so
an empty result is never a guess. Nothing is pushed to you, so run it when something looks
wrong, or on a timer.

**`sudo briard logs` reads both logs**, and a bug report wants both: `journalctl -u briard-agent`
is the agent's side, and `/var/log/briard-guest-console.log` is the serial console of the system
your apps run in — the only view into its own boot and kernel.

Both work even when the agent is down, as do `briard version` and the machine's own half of
`briard doctor`.

## More

| | |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | how it works, and why |
| [CONTRIBUTING.md](CONTRIBUTING.md) | build it, run the tests, propose a change |

Found a security bug? Email **security@briard.io** rather than opening a public issue.

Apache-2.0 — see [LICENSE](LICENSE); the third-party software the release carries, and where its
source is, is in [THIRD-PARTY.md](THIRD-PARTY.md). Contributions are accepted under the DCO; there
is no CLA, deliberately. This repository was extracted from our private monorepo at open-sourcing,
so its history starts at that point; development continues here in the open.
