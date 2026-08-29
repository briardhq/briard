# guest-image

The NixOS VM image the agent runs: DRBD 9 (pinned kernel + module), drbd-reactor,
the ordered systemd units, and the front door. It ships running NO services: they are installed
at runtime from signed manifests. Built from the flake.
