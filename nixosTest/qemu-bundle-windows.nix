# The Windows qemu bundle.
#
# WHY IT IS REPACKAGED UPSTREAM BYTES AND NOT A BUILD OF OURS -- the decision [V3b.27](b) was filed
# to make, with the reason named so nobody re-derives it. `pkgsCross.mingwW64.qemu` was the preferred
# candidate (ours, reproducible, same flake, same signed manifest) and it is BLOCKED, measured on the
# pinned nixpkgs: qemu's `meta.platforms` excludes Windows; forcing past that, both `glib` and
# `libslirp` -- neither optional, libslirp being the guest's WAN NIC -- pull `python3`, which nixpkgs
# marks broken on mingw because its mingw patches exist only for 3.11; substituting 3.11 then breaks
# sphinx. Unblocking that is an open-ended port of someone else's Python that upstream does not claim
# to support and we would maintain forever. MSYS2 is signed and current in every way except the one
# that matters -- its newest qemu is 9.2.3, below the 10.2 floor `query-accelerators` needs.
#
# WHAT OUR SIGNATURE MEANS HERE, AND IT CHANGES. For every other artifact it attests "we built these
# bytes from this source". For this one it attests "we pinned, unpacked and tested exactly these
# bytes". The upstream installer's own Authenticode certificate EXPIRED 2023-12-09 and Windows does
# not validate it, and the published SHA-512 is served from the same host over the same TLS as the
# binary -- so it is a corruption check, not a trust root. The trust root is the pin below plus the
# release signature over the manifest that names this artifact's hash.
#
# GPL: QEMU is GPL-2.0. Distributing these binaries obliges us to accompany them with the
# corresponding source -- upstream publishes it (https://qemu.weilnetz.de/, "QEMU sources"), and the
# obligation is ours to meet, not theirs, because we are the ones redistributing.
#
# THE ASSERTION IS NOT IN CI, and that is stated rather than hidden: a Linux runner cannot run a
# Windows binary, so the proof is `lab/vanilla-windows` -- unpack this tar, hide the machine's own
# qemu, and boot the shipped guest out of the bundle (`probe qemubundle -Unpack`, then
# `probe guestboot -QemuDir C:\qbundle`). What the derivation CAN assert, it does, below.
{ pkgs }:

let
  lib = pkgs.lib;

  # Pinned upstream. A dated filename, not a "latest" redirect: the point of a pin is that the
  # bytes cannot change under us, and this is the one artifact whose bytes we did not produce.
  version = "20260811";
  installer = pkgs.fetchurl {
    url = "https://qemu.weilnetz.de/w64/qemu-w64-setup-${version}.exe";
    hash = "sha256-+YqK619/rql2W23uKDFsJmzRedgDVKL+2OUBdvmi5Z8=";
  };

  # The two binaries the product runs: qemu-system-x86_64 boots the guest, qemu-img makes the
  # install-time overlay. Same pair as the Linux bundle, for the same reasons.
  qemuBins = [ "qemu-system-x86_64.exe" "qemu-img.exe" ];

  # The x86 firmware allowlist, out of a share/ carrying every architecture's blobs. It is the
  # Linux bundle's list plus efi-e1000.rom: QEMU adds a default e1000 NIC to any launch that names
  # no netdev and then wants its option ROM. Every installed node names one, so this is the bundle
  # being wider than the product needs rather than the product needing it -- but a bundle that only
  # works for the launches we happen to make is a trap for whoever makes a different one.
  firmware = [
    "bios-256k.bin" "bios.bin"
    "kvmvapic.bin" "linuxboot_dma.bin" "pvh.bin"
    "vgabios-stdvga.bin" "vgabios-bochs-display.bin" "vgabios.bin"
    "efi-virtio.rom" "efi-e1000.rom"
  ];

  bundle = pkgs.runCommand "briard-qemu-bundle-windows-${version}" {
    nativeBuildInputs = [ pkgs.p7zip pkgs.binutils ];
    inherit version;
    bins = lib.concatStringsSep " " qemuBins;
    fw = lib.concatStringsSep " " firmware;
    src = installer;
    url = installer.url;
  } ''
    # NSIS, which 7z unpacks: DLLs and .exes flat at the top, firmware under share/.
    mkdir unpacked && 7z x -ounpacked "$src" > /dev/null

    mkdir -p "$out/share/qemu"

    # THE DLL SET IS THE PE IMPORT CLOSURE, not "every DLL in the tree" -- the same doctrine the
    # Linux bundle applies with ldd, and objdump reads a PE import table as readily as an ELF
    # NEEDED. It trims 114 DLLs to 104 and 141 MB to 98; the reason it trims so little is that the
    # upstream build enables the GTK UI, which we never use (`-display none`) and cannot turn off
    # without building qemu ourselves -- the option this artifact exists because we cannot take.
    #
    # ⚠️ The same gap the Linux bundle names applies here: an import table does not capture a
    # runtime LoadLibrary, so the honest check is a real boot, and that is what the tier-4 rig does.
    cd unpacked
    queue="$bins"
    seen=""
    while [ -n "$queue" ]; do
      next=""
      for f in $queue; do
        case " $seen " in *" $f "*) continue ;; esac
        seen="$seen $f"
        [ -f "$f" ] || continue
        for d in $(objdump -p "$f" 2>/dev/null | awk '/DLL Name:/ {print $3}'); do
          [ -f "$d" ] && next="$next $d"
        done
      done
      queue="$next"
    done
    for f in $seen; do cp "$f" "$out/"; done
    cd ..

    for f in $fw; do
      cp "unpacked/share/$f" "$out/share/qemu/$f" ||
        { echo "firmware blob missing from the upstream tree: $f" >&2; exit 1; }
    done

    # Build-time assertions -- the ones a Linux builder CAN make about a Windows tree. They exist
    # because the loop above is a text scan over someone else's installer layout: a reorganised
    # upstream would otherwise produce an empty, well-formed, signed and useless artifact.
    for b in $bins; do
      [ -s "$out/$b" ] || { echo "missing binary: $b" >&2; exit 1; }
    done
    n=$(ls "$out"/*.dll | wc -l)
    [ "$n" -ge 50 ] || { echo "only $n DLLs resolved -- the import scan found nothing to walk" >&2; exit 1; }

    {
      echo "upstream=$url"
      echo "sha256=${installer.outputHash}"
      echo "qemu=$(strings -a "$out/qemu-system-x86_64.exe" | grep -m1 '^QEMU emulator version' || echo unknown)"
      echo "dlls=$n"
      echo "firmware:"; ls "$out/share/qemu"
    } > "$out/PROVENANCE"
  '';
in
{
  inherit bundle version;
}
