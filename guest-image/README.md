# guest-image

The NixOS VM image the agent runs: DRBD 9 (pinned kernel + module), drbd-reactor,
the ordered systemd units, and the front door. It ships running NO service: one is installed
at runtime from a signed manifest. Built from the flake.
