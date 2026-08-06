# Contributing

Thanks for looking. Issues, pull requests, and forks are all welcome. This file is the stuff worth
knowing first: a few invariants the tests enforce, and the defaults a review will apply — written
down so they are not a surprise.

## The honest bit

Briard is written by a very small team, and it is early. The design still moves, and we will
sometimes decline a patch because it does not fit where the thing is going. If that happens we will
say so directly rather than let the PR rot.

So: for anything bigger than a bug fix, open an issue before writing the patch. Not as a process
gate — it is just a shame to write something that was never going to land.

Everything here is Apache-2.0. See [LICENSE](LICENSE).

**No CLA.** Contributions come in under the [Developer Certificate of Origin](https://developercertificate.org/)
— sign off your commits with `git commit -s`, which appends a `Signed-off-by:` line asserting you
have the right to submit the work. We deliberately do **not** ask for a Contributor License
Agreement: a CLA is what lets a company relicense contributors' code later, so not having one makes
the no-rug-pull promise structural rather than a matter of our continued good intentions.

## The invariants — enforced, not requested

These are properties of the *code*, checked at build time by `go test ./internal/arch/`. They are
not style preferences, and a change that breaks one will fail CI. If a change appears to require
breaking one, it is almost certainly mis-scoped — open an issue and let's talk about it before
writing the patch.

1. **Host seam discipline.** The orchestration package `agent/host` must not import the concrete
   providers (`netbird`, `libvirt`). It reaches each one only through its seam
   interface; the concrete implementations are wired in at `main` and injected. `agent/host` *may*
   import `agent/drbd`, which is the observe-and-report package guarded by (2). A transitive
   dependency test enforces this, so an indirect import fails too.

2. **Failover stays out of the agent.** The agent *observes and reports* failover; it never drives
   it. No code path promotes or demotes DRBD, or claims the VIP, directly — `drbd-reactor` owns that
   lifecycle. This is checked twice: the `agent/drbd` package must expose no lifecycle-driving API,
   and no code may shell out to `drbdadm`/`drbdsetup` with a promoting verb.

3. **No force-promotion anywhere.** Nothing calls `drbdadm primary --force` or
   `--overwrite-data-of-peer`. DRBD is the sole write-authority, and a split brain resolved by
   forcing one side is data loss with extra steps.

Two further rules are not machine-checked but are equally load-bearing:

- **The host/payload boundary is real.** The agent runs on the host and is privileged; the workload
  runs inside the guest; they communicate only over the defined channel. Never run workload logic in
  the agent or agent logic in the guest.
- **What leaves the house is a closed, audited allowlist.** Everything that can ever be sent upward
  is readable in `shared/api`. Widening it is a deliberate, visible change — never a quiet field —
  and nothing about what a node sends is remotely toggleable.

## What a good change looks like

**Make the smallest change that satisfies the requirement.** The failure mode this project guards
against is expansion, not omission. If a change grows an abstraction, a dependency, a module, a
config option, or a *second way to do something that already has a way*, it needs a conversation
first — open an issue rather than leading with the patch.

Some specific defaults, so they are not a surprise in review:

- **New dependency: the default answer is no.** Not a reflexive ban — a small, stable, focused
  dependency that absorbs genuinely hard or security-sensitive logic (crypto, TLS/ACME) usually
  wins. A large SDK replacing a few trivial stable lines does not. Justify it in the PR.
- **New abstraction: not until three real call sites need it.** Don't abstract on the first or
  second instance.
- **New config option or flag: the default answer is no.** Flags multiply the states that have to be
  tested. Bake the decision into the code. Deployment wiring (device names, CIDRs, peer addresses,
  disk paths) is read from environment variables in one place and is not what this rule is about.
- **One way per concern.** Go standard library `net/http` for HTTP, standard `log` for logging,
  `fmt.Errorf` with `%w` for error wrapping. No client framework, no logging framework, no error
  package. Shared types are defined once in `shared/` and imported by both sides, never redefined.

## Testing

- **Decision logic gets a unit test**, exhaustively — enumerate the state combinations rather than
  sampling them.
- **Mechanisms get a `nixosTest`.** Don't mock the thing you are verifying: the failover, upgrade,
  and install tests boot real VMs with real DRBD, real `drbd-reactor`, and a real VIP.
- **New behaviour ships with the test that proves it**, and the test must be able to fail. An
  assertion that cannot fail — because the sandbox is offline, or the condition is vacuously true —
  is worse than no assertion, because it reads as coverage.
- Keep the `internal/arch` guards green.

You can run the fast suite anywhere:

```sh
go build ./... && go vet ./... && go test ./...
nix flake check          # evaluates the flake and builds config closures; boots no VMs
```

The VM tests need KVM and take longer, but they run on a laptop and are the honest version of
"run it yourself":

```sh
nix build .#tests.drbd-failover -L      # one test
nix build .#drbd                        # a whole tag
```

## Where the design lives

[ARCHITECTURE.md](ARCHITECTURE.md) is the public account of the design, written to be read rather
than referenced. Comments in the tree explain what the code does and why it is shaped the way it
is; the longer decision history sits in our own planning documents, which are not published.

Nothing here should require them. If a comment only makes sense with a document you cannot open,
that is a bug in the comment — please report it.

## Issues, discussions, and security

- **Issues** are for bugs — something behaves differently from what the docs say.
- **Discussions** are for help, questions, and "should this work like X?".
- **Security bugs**: please email **security@briard.io** instead of opening a public issue, so it
  can be fixed before it is public. You will get a human reply, and credit in the fix unless you
  would rather not be named.
