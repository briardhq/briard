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
--inputs-from . nixpkgs#<pkg>.src` resolves that same pin and fetches any component's source.

## QEMU — GPL-2.0

Two artifacts, from two different places, and the difference matters:

- **`qemu-bundle.tar.zst`** (Linux; installed at `/opt/briard/qemu`) **is our build.** The recipe is
  [`nixosTest/qemu-bundle.nix`](nixosTest/qemu-bundle.nix), over nixpkgs' `qemu_test` (its
  headless build of `qemu`) at the revision pinned in [`flake.lock`](flake.lock).
  `nix build .#artifacts.qemu-bundle` reproduces it, and
  `nix build --inputs-from . nixpkgs#qemu_test.src` fetches the QEMU source it was compiled from.

- **`windows/qemu-bundle-windows.tar.zst`** **is not our build** — it is repackaged, meaning
  unpacked and trimmed rather than recompiled, from the Windows installer that QEMU's own
  [download page](https://www.qemu.org/download/) points to: the build Stefan Weil publishes at
  <https://qemu.weilnetz.de/w64/>. The exact installer and its SHA-256 are pinned in
  [`nixosTest/qemu-bundle-windows.nix`](nixosTest/qemu-bundle-windows.nix) and recorded in the
  bundle's own `PROVENANCE` file, so the upstream bytes we started from are identified exactly.
  That build's sources are published by its builder, linked from <https://qemu.weilnetz.de/>.

QEMU's upstream source, for either: <https://www.qemu.org/download/> ·
<https://gitlab.com/qemu-project/qemu>.

## Go modules compiled into our binaries

The agent (`briard-agent`, the same binary on the host and as `briard`), the guest agent and the
firmware the guest image bakes, the front door and the dashboard are Go programs, and they link the modules pinned in
[`go.mod`](go.mod) / [`go.sum`](go.sum) — permissive licenses, all of them, and each module's
`LICENSE` file ships in its source:

- [`filippo.io/age`](https://github.com/FiloSottile/age) (BSD-3-Clause) and its dependency
  `filippo.io/hpke` (BSD-3-Clause) — the encrypted off-site backup.
- [`github.com/klauspost/compress`](https://github.com/klauspost/compress) (BSD-3-Clause, with
  Apache-2.0 and MIT parts; see its LICENSE) — decompressing the release artifacts.
- [`github.com/pion/mdns/v2`](https://github.com/pion/mdns) (MIT) and its dependency
  `github.com/pion/logging` (MIT) — the front door's `.local` names.
- `golang.org/x/net`, `golang.org/x/crypto`, `golang.org/x/sys` (BSD-3-Clause).

Under Nix the module set is vendored from those exact versions (`vendor-hash.nix` pins it), so the
binary the release ships is built from nothing else.
