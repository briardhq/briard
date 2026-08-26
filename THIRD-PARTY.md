# Third-party components we redistribute

Briard's own code is Apache-2.0 ([LICENSE](LICENSE)). The release channel also carries software
other people wrote, under their own licenses — most of it copyleft. Those licenses ask that whoever
receives the binaries can get the corresponding source; everything here is built from public
sources, so this file's job is to say **which** sources, precisely enough to be useful.

Nothing below is a separate download you have to trust: each artifact is named by the signed
release manifest, and each is either built by this repository or pinned in it by hash.

## The guest OS image — `nixos.qcow2`

A NixOS system image, so it carries the Linux kernel, systemd and the rest of a small distribution,
under their respective licenses (GPL-2.0 and others). **We build it from this repository**: the
recipe is [`guest-image/`](guest-image/), and the exact nixpkgs revision every package comes from is
pinned in [`flake.lock`](flake.lock). `nix build .#artifacts.guest-disk` rebuilds it; `nix build
nixpkgs#<pkg>.src` against that same pin fetches any component's source. The build closure is also
served at `cache.briard.io`.

## QEMU — GPL-2.0

Two artifacts, from two different places, and the difference matters:

- **`qemu-bundle.tar.zst`** (Linux; installed at `/opt/briard/qemu`) **is our build.** The recipe is
  [`nixosTest/qemu-bundle.nix`](nixosTest/qemu-bundle.nix), over nixpkgs' `qemu` at the revision
  pinned in [`flake.lock`](flake.lock). `nix build .#artifacts.qemu-bundle` reproduces it, and
  `nix build nixpkgs#qemu.src` fetches the QEMU source it was compiled from.

- **`windows/qemu-bundle-windows.tar.zst`** **is not our build** — it is repackaged, meaning
  unpacked and trimmed rather than recompiled, from the Windows installer that QEMU's own
  [download page](https://www.qemu.org/download/) points to: the build Stefan Weil publishes at
  <https://qemu.weilnetz.de/w64/>. The exact installer and its SHA-256 are pinned in
  [`nixosTest/qemu-bundle-windows.nix`](nixosTest/qemu-bundle-windows.nix) and recorded in the
  bundle's own `PROVENANCE` file, so the upstream bytes we started from are identified exactly.
  That build's sources are published by its builder, linked from <https://qemu.weilnetz.de/>.

QEMU's upstream source, for either: <https://www.qemu.org/download/> ·
<https://gitlab.com/qemu-project/qemu>.
