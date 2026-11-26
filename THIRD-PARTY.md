# Third-party components we redistribute

Briard's own code is Apache-2.0 ([LICENSE](LICENSE)). The release channel, and one container image
the catalog names, also carry software other people wrote, under their own licenses — most of it
copyleft. Those licenses ask that whoever receives the binaries can get the corresponding source;
everything here is built from public sources, so this file's job is to say **which** sources,
precisely enough to be useful.

Nothing below is a separate download you have to trust: each artifact is named by the signed
release manifest (the Samba image by the signed catalog), and each is either built by this
repository or pinned in it by hash.

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

- **`windows/qemu-bundle-windows.tar.zst`** — **QEMU itself is our build; the libraries beside
  it are MSYS2's.** The recipe is
  [`nixosTest/qemu-bundle-windows.nix`](nixosTest/qemu-bundle-windows.nix): the QEMU release
  tarball it pins by SHA-256 (from <https://download.qemu.org/>), with the patches in
  [`nixosTest/qemu-windows-patches/`](nixosTest/qemu-windows-patches/) applied, cross-compiled for
  Windows with the mingw-w64 gcc from the nixpkgs revision in [`flake.lock`](flake.lock).
  `nix build .#artifacts.qemu-bundle-windows` reproduces it. The DLLs it links are binaries from
  [MSYS2](https://www.msys2.org/)'s UCRT64 repository, unmodified and pinned by SHA-256 in the
  same file: glib (LGPL-2.1+), libiconv and gettext's libintl (LGPL-2.1+), libslirp, pcre2 and
  zstd (BSD-3-Clause), pixman and libffi (MIT), zlib (Zlib), winpthreads (MIT and BSD-3-Clause).
  Each package's source archive is at <https://repo.msys2.org/mingw/sources/> under the same name
  and version, and its recipe at <https://github.com/msys2/MINGW-packages>. Two runtime DLLs come
  from the compiler instead, from the same nixpkgs revision: `libmcfgthread-2.dll` (mcfgthread,
  LGPL-3.0+ with the GCC runtime exception) and `libssp-0.dll` (GCC, GPL-3.0+ with the GCC
  runtime exception). The bundle's own `PROVENANCE` file lists every pin.

QEMU's upstream source, for either: <https://www.qemu.org/download/> ·
<https://gitlab.com/qemu-project/qemu>.

## tap-windows6 — GPL-2.0

**`windows/tap-windows.exe` is not our build** — it is OpenVPN Inc.'s signed installer for the
tap-windows6 driver (9.24.7, the newest standalone build), byte for byte, served from our channel
so a Windows install fetches everything from one origin. The exact file and its SHA-256 are pinned
in [`nixosTest/outputs.nix`](nixosTest/outputs.nix) (`artifacts.tap-windows`), and the Windows
installer (`scripts/install.ps1`) checks the same hash before running it. A kernel driver is signed
by whoever Microsoft attests, which is OpenVPN, so the bytes are never rebuilt or re-signed here.
Source: <https://github.com/OpenVPN/tap-windows6> · releases at
<https://build.openvpn.net/downloads/releases/>.

## The Samba image — GPL-3.0

The Files app's drive is served by Samba, in a container image we build and publish at
`ghcr.io/briardhq/catalog/samba`; the catalog pins it by digest, and a node pulls it as it pulls
any app's image. It is not on the release channel. **We build it from this repository**: the
recipe is [`catalog/samba-image.nix`](catalog/samba-image.nix), Samba as nixpkgs packages it
(nixpkgs' patches are in nixpkgs), configured as a file server only. It is built against the head
of the nixpkgs channel this repository follows, not the revision `flake.lock` pins, so every image
names the revision it was built from, in its label `io.briard.nixpkgs`:

```sh
skopeo inspect docker://ghcr.io/briardhq/catalog/samba@<digest> | jq -r '.Labels["io.briard.nixpkgs"]'
nix build .#samba-image --override-input nixpkgs github:NixOS/nixpkgs/<revision>   # the image again
nix build github:NixOS/nixpkgs/<revision>#samba.src                                 # Samba's source
```

The image also holds the libraries Samba links (glibc, GnuTLS and others), from that same revision
and under their own licenses; `github:NixOS/nixpkgs/<revision>#<pkg>.src` fetches each one's source.

Samba's upstream source: <https://www.samba.org/samba/download/> ·
<https://gitlab.com/samba-team/samba>

## Go modules compiled into our binaries

The agent (`briard-agent`, the same binary on the host and as `briard`), the guest agent and the
firmware the guest image bakes, the reverse proxy and the dashboard are Go programs, and they link the modules pinned in
[`go.mod`](go.mod) / [`go.sum`](go.sum) — permissive licenses, all of them, and each module's
`LICENSE` file ships in its source:

- [`github.com/klauspost/compress`](https://github.com/klauspost/compress) (BSD-3-Clause, with
  Apache-2.0 and MIT parts; see its LICENSE) — decompressing the release artifacts.
- [`github.com/pion/mdns/v2`](https://github.com/pion/mdns) (MIT) and its dependency
  `github.com/pion/logging` (MIT) — the reverse proxy's `.local` names.
- `golang.org/x/net`, `golang.org/x/crypto`, `golang.org/x/sys` (BSD-3-Clause).

Under Nix the module set is vendored from those exact versions (`vendor-hash.nix` pins it), so the
binary the release ships is built from nothing else.

## The flock-name word lists — Apache-2.0

The adjective and animal lists a flock's name is drawn from, in
[`shared/flockname/words.go`](shared/flockname/words.go), are derived from
[`github.com/dustinkirkland/golang-petname`](https://github.com/dustinkirkland/golang-petname)
(Apache-2.0): snapshotted from it and curated, then copied into this repository rather than
imported, so they compile into the agent with no dependency on that module.

## The dashboard's stylesheet, script and icons — MIT, 0BSD, ISC

The household dashboard's one stylesheet, `dashboard/static/app.css`, and its one script are built and fetched ahead of time by
[`dashboard/static/build.sh`](dashboard/static/build.sh) and committed, so the guest fetches nothing:

- [Basecoat CSS](https://basecoatui.com) 1.1.0 (MIT) — the components; fetched from npm by the
  hash pinned in the script, so the build verifies it.
- [Tailwind CSS](https://tailwindcss.com) v4 (MIT) — the build tool and the utilities the parts in
  `dashboard/css/input.css` are made of; nixpkgs' `tailwindcss_4` at the revision pinned in
  [`flake.lock`](flake.lock).
- [Lucide](https://lucide.dev) (ISC) — the six sidebar icons, the alert's, and the phone of "On your
  phone", inline in `dashboard/templates/shell.html`.
- [htmx](https://htmx.org) 2.0.11 (0BSD) — `dashboard/static/htmx-2.0.11.min.js`, the one script on the
  page, which polls a card while it moves; fetched from npm by the hash pinned in the script.

## The EFF's short word list — CC BY 3.0 US

`dashboard/eff_short_wordlist_1.txt` is the Electronic Frontier Foundation's
[short word list](https://www.eff.org/deeplinks/2016/07/new-wordlists-random-passphrases)
(`eff_short_wordlist_1.txt`), unmodified, licensed
[CC BY 3.0 US](https://creativecommons.org/licenses/by/3.0/us/). The dashboard picks three of its
words for the password a phone's music app signs in to Navidrome with.
