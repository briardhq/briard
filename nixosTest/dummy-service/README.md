# dummy-service

Representative stateful test fixture (permanent): writes persistent state to the
DRBD volume, exposes `/healthz`, and starts slowly — HA's shape minus its quirks.
Drives the failover and upgrade regression tests. See V0.
