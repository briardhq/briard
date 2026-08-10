# Briard

**The best way to run Home Assistant on a machine you already own.**

You install one agent. It turns the machine into a node that runs your services inside a
managed VM and does the sysadmin: tested updates that **undo themselves automatically** if
something regresses, snapshots and one-command rollback, and — with a second machine —
**takes over in seconds** when one dies.

**No account. No cloud. No telemetry.** Briard is local-first: your home keeps working with
the internet down, and nothing on the path to serving your house depends on us. What you
install reports nothing, ever.

The one thing we can see is that *someone* downloaded a release — an ordinary web request
for signed static files. We keep a daily count per file and nothing that identifies you, and
we deliberately don't deduplicate, because working out whether two downloads are the same
person means tracking that person. Once it is installed, it never calls home.

> **Status: alpha.** The failover, safe-upgrade, and reach-by-name stack is built and
> tested, including under a long-running fault soak. It has run on machines we control; it
> has not yet run on many machines we don't. Expect rough edges, expect to read carefully,
> and please [tell us what broke](https://github.com/briardhq/briard/issues).

## Install

```sh
curl -fsSL https://get.briard.io/install.sh | sudo sh
```

Artifacts are signed, and the installer verifies signature, hash, and size before using
anything — refusing and leaving the existing install untouched if verification fails. If you
would rather read before you run, that is precisely why the script is in this repo:
[`scripts/install.sh`](scripts/install.sh) is what that URL serves, plus the release public
key embedded at publish time. You can also install [from source](#install-from-source).

**What the channel records.** Downloads are counted from the channel's own request logs — a
per-day, per-file count of successful fetches, and nothing else is kept. Briard itself never
reports a download, or anything at all: a free install talks to no server of ours. The count
is deliberately **not de-duplicated**, because telling a returning visitor from a new one
would mean tracking someone — so it over-counts, and we would rather say that than imply a
precision we did not earn.

The installer checks the machine first and **refuses with the reason** if it is not
suitable, rather than half-installing and leaving you to work out why.

What you get is the **node**: ready, replicating, and able to fail over. It installs **no
service** — a machine is set up first, and then you choose what runs on it.

**If something goes wrong, there are two logs and you want both.** `journalctl -u briard-agent`
is the host's side; `/var/log/briard-guest-console.log` is the guest's own serial console, which
is the only view into the VM — the guest is a full citizen of your LAN but is deliberately
isolated from the host, so nothing else can see inside it. Both belong in any bug report.

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

> **Alpha gap:** the front door at `http://<vip>/` does not route to a service you install this
> way yet — it keeps serving Briard's own page, and the service answers on its own port
> (Home Assistant: `http://<vip>:8123/`). Per-domain routing to installed services is the next
> thing landing. The catalog is also one entry long today.

## How it works

One privileged agent on the host; the workload in a guest VM it manages. DRBD replicates
storage and is the sole write-authority — single primary, quorum decides, and nothing ever
force-promotes. Failover is driven by `drbd-reactor`; the agent only observes it.

[**ARCHITECTURE.md**](ARCHITECTURE.md) is the short version of why those choices hold.

## Build it yourself

You need [Nix](https://nixos.org/download/) with flakes enabled. The Go toolchain comes
from the dev shell — there is nothing to install globally.

```sh
nix develop                       # dev shell: go, gopls, staticcheck
go build ./... && go test ./...   # logic + the enforced architecture guards
nix flake check                   # flake integrity; boots no VMs
```

## Install from source

The one-liner above is the ordinary path. This is the same install without the download: you
build the artifacts the installer would otherwise fetch, then point the installer at them — for
auditing what you run, or installing from a checkout you have modified. It needs Nix (as above)
and a Linux host with KVM.

```sh
# build the artifacts from this checkout
nix build .#artifacts.agent       -o result-agent
nix build .#artifacts.net-wrap    -o result-netwrap
nix build .#artifacts.qemu-bundle -o result-qemu
nix build .#artifacts.guest-disk  -o result-guest    # ~2.5 GB; the guest VM image

# assemble a staging directory the installer reads
mkdir -p stage
install -m0755 result-agent/bin/briard-agent      stage/briard-agent
install -m0755 result-netwrap/bin/briard-net-wrap stage/briard-net-wrap
cp -aL result-qemu stage/qemu && chmod -R u+w stage/qemu
cp -L  result-guest/nixos.qcow2 stage/nixos.qcow2

# install from the staging directory
sudo BRIARD_ARTIFACTS="$PWD/stage" ./scripts/install.sh
```

The last step runs as root: it sets up the guest's networking and boots the VM, the same
thing the hosted installer does — so it changes the machine, and it checks the host first and
**refuses with the reason** if it is unsuitable. This is the offline path the signed channel
will make a one-liner; the script is the same either way, so
[read it](scripts/install.sh) before you run it.

## Run the tests that back the claims

These boot real VMs and drive real DRBD — not mocks. They take minutes on a laptop and are
the honest version of "see for yourself":

```sh
nix build .#tests.drbd-failover -L   # kill the primary → survivor takes over, data intact
nix build .#tests.drbd-fence -L      # partition the minority → it self-fences
nix build .#tests.ha-payload -L      # real Home Assistant serving in the payload slot
nix build .#drbd                     # the whole failover net
nix log .#tests.drbd-fence           # what a run printed
```

The architecture guards are ordinary tests, and they fail the build:

```sh
go test ./internal/arch/
```

They reject a force-promotion appearing anywhere in the tree, an orchestrator importing a
concrete provider instead of its interface, and any direct promote/demote call.

## Documentation

| | |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | how it works, and why |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the enforced invariants, and how to propose a change |

Found a security bug? Email **security@briard.io** rather than opening a public issue.

## License and status

Apache-2.0 — see [LICENSE](LICENSE). Contributions are accepted under the DCO; there is no
CLA, deliberately.

This repository was extracted from our private monorepo at open-sourcing, so its history
starts at that point; development continues here in the open.
