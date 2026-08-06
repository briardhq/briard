# guest-image

The NixOS VM image the agent runs: DRBD 9 (pinned kernel + module), drbd-reactor,
the ordered systemd units, and the payload (dummy → HA). Built from the flake.
