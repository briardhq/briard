# nixosTest — the tier-1 hermetic harness

nixosTest topologies (3-node majority, and 2-node + diskless witness) and the kill-primary
chaos test — the permanent regression net.

## The invariant checklist, tier-1 side

These tests are the **independent Python implementation** of the invariant checklist below —
the *same spec* our soak oracle implements over the seam, asserted here over a live guest. A
**shared checklist, not a cross-language module**: the two must agree on *what* holds and
share no code, the seam and the guest image being all they have in common.

| Invariant | Asserted by |
|---|---|
| reachable | `curl -fsS <VIP>/healthz` everywhere; `https://<name>` in `tls-serving.nix`; and with **no backend configured** in `zero-service.nix` — the shipped shape, where the front door itself is what answers |
| single-primary | `drbd-promote.nix` (promote unit on the primary and nowhere else) |
| minority-refuses | `drbd-fence.nix`, `drbd-witness-loss.nix` (isolated minority demotes, never promotes) |
| data-not-rewound | `drbd-failover.nix`, `ha-failover.nix` (`t2 >= t1` ticks across the kill) |
| settles-in-slo | `wait_until_succeeds(..., timeout=N)` bounds on the post-kill reconverge |
| alert-correctness | not here — needs a long-running fleet (red ⇒ alert ⇒ gate blocks; heal ⇒ recovered ⇒ green) |
| no-silent-restarts | not here — a hermetic run is too short for a crash loop to appear |
| no-bad-kernel-log | not here — needs sustained runtime |

The last three are checked by our long-running soak rather than by these tests; the rows are
kept so the set stays complete and the gaps are visible rather than implied.

When a test adds or changes an assertion here, update the table with it. The value of the
checklist is that it says what is actually covered — a stale row is worse than a missing one.
