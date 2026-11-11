# guest-image

The NixOS VM image the agent runs: DRBD 9 (pinned kernel + module), drbd-reactor,
the ordered systemd units, and the reverse proxy. It ships running NO apps: they are installed
at runtime from signed manifests. Built from the flake.
