# The Windows qemu bundle: QEMU built here, from a pinned stable release, for Windows.
#
# THE RECIPE is the one the de-facto Windows QEMU builds use (qemu.weilnetz.de's GitHub workflow):
# a mingw cross gcc on Linux, MSYS2's prebuilt Windows libraries as the sysroot, then QEMU's own
# `configure --cross-prefix`. We build it rather than repackage someone's installer for one reason
# above the rest: a Windows node has to be able to take a QEMU point release the day it needs one,
# and those installers are built from master, not from bugfix releases.
#
# WHY NOT `pkgsCross.*.qemu`: nixpkgs would cross-build glib and libslirp too, and through them a
# Python for mingw that nixpkgs marks broken. Here Python runs only on the build machine, and glib,
# libslirp and the rest of the library closure are MSYS2's own signed CI builds, pinned by hash.
#
# UCRT64, not MSYS2's older msvcrt `mingw64` environment: that one no longer carries libslirp, and
# the UCRT is part of Windows 10 and later.
#
# WHAT OUR SIGNATURE MEANS HERE: the QEMU binaries are built by us from the pinned source below,
# with the patches in qemu-windows-patches/ applied (each one says what it fixes and on what host);
# the DLLs beside them are MSYS2's binaries and the toolchain's runtime, pinned and unmodified.
#
# GPL: QEMU is GPL-2.0 and several of the libraries are LGPL. The corresponding source is the QEMU
# release tarball pinned below plus MSYS2's source package for each pinned library (THIRD-PARTY.md).
#
# THE ASSERTION IS NOT IN CI: a Linux runner cannot run a Windows binary. What the derivation CAN
# assert about a Windows tree, it does, below; the rest is a real boot on a Windows machine.
{ pkgs }:

let
  lib = pkgs.lib;

  version = "11.1.2";
  src = pkgs.fetchurl {
    url = "https://download.qemu.org/qemu-${version}.tar.xz";
    hash = "sha256-cxtWgeS7GL4xMjFXm479ApbFsBX6NtxTOHS2ObqDgBY=";
  };

  # The library closure of the two binaries, as MSYS2 resolves it for glib, libslirp, pixman,
  # zlib and zstd, plus winpthreads' headers (clock_gettime). A bump is: re-resolve against the
  # repo's ucrt64.db, re-pin every hash. Fixed-output: the bytes cannot change under the pin.
  msys2 = map (p: pkgs.fetchurl {
    url = "https://repo.msys2.org/mingw/ucrt64/mingw-w64-ucrt-x86_64-${p.name}-any.pkg.tar.zst";
    inherit (p) sha256;
  }) [
    { name = "cc-libs-16.2.0-6"; sha256 = "d8fff9e62443168bcda8ce746697243519c4f090ea607357ce94feac0bd10a50"; }
    { name = "gcc-libs-16.2.0-6"; sha256 = "20b4df1cc16e5c5d38d6510b4fb586a96922b7817d455c0d30c3c1257a88cc9c"; }
    { name = "gettext-runtime-1.0-1"; sha256 = "ba693dda4ac375af76ce481ff3a6e7481286546cc7dc6d56c7021dae34084157"; }
    { name = "glib2-2.90.1-1"; sha256 = "9a7826349279bcca0c935b01880d812be9de9609aef2205be68fd7a6128c1695"; }
    { name = "libffi-3.8.0-1"; sha256 = "99ad12f4ecfa00a889ef9c5c0188592368a5bac109d0c508ce4e04f29ea95a76"; }
    { name = "libgcc-16.2.0-6"; sha256 = "80b2497f019977dd9528b1abc8621b1328a0115ab1f724047ccbb56bf323f11d"; }
    { name = "libiconv-1.19-1"; sha256 = "9a500f38c2b91808741c62fae746b3e9110b33a1ecf5c30fa0c66dbedddf7e16"; }
    { name = "libslirp-4.9.3-1"; sha256 = "c982be43767a950f220b498f0f212eb2e2c945d93c968bd0e4f66b1b5da94f6c"; }
    { name = "libsystre-1.0.2-3"; sha256 = "62f564b1395d4862db1599b77f6588c32de5c80936845751cf369b31eee1560b"; }
    { name = "libtre-0.9.0-2"; sha256 = "d50f22545c50d7c0cd584474bba83f0c7a2cd8aab1aed1ad6c1f9c6452f3c4fc"; }
    { name = "libwinpthread-14.0.0.r426.g4564ee4b5-2"; sha256 = "a02f5e09d666d3be657c78c86fe67c975d04fe33b5211cd5ff1b23a91c118524"; }
    { name = "pcre2-10.49-1"; sha256 = "63172179db724ac71e2439a7981134c1bcab8049bdddfd349b32b8f76ed52d1e"; }
    { name = "pixman-0.46.4-3"; sha256 = "4c01b986563d2b5f7ad6fcec2d0ec9915d2f242ddac5eacddc19971964241cc5"; }
    { name = "winpthreads-14.0.0.r426.g4564ee4b5-2"; sha256 = "a29b3919ef25c1d1671b8899bd947903bbf8733e7ada4a71be5bf02553dceab3"; }
    { name = "zlib-1.3.2-2"; sha256 = "841401182976d2f9e17e5c0ebaac51f2a8014140ea53d67625e91c8fb3c85ea0"; }
    { name = "zstd-1.5.7-2"; sha256 = "dbdb8427280046a2b41697780aa4c52983b708082b0da4755951dc3bea96ca89"; }
  ];

  win = pkgs.pkgsCross.ucrt64;
  # Two runtime DLLs come from the cross gcc rather than MSYS2: its threads are mcfgthread, and
  # QEMU builds with -fstack-protector-strong, which links libssp.
  toolchainDlls = [
    "${win.windows.mcfgthreads}/bin/libmcfgthread-2.dll"
    "${win.buildPackages.gcc.cc.lib}/x86_64-w64-mingw32/lib/libssp-0.dll"
  ];

  unpackSysroot = ''
    mkdir -p "$NIX_BUILD_TOP/sysroot"
    for p in ${lib.concatStringsSep " " msys2}; do
      tar -C "$NIX_BUILD_TOP/sysroot" --zstd -xf "$p" --exclude='.*'
    done
  '';

  # A plain Linux stdenv driving the cross gcc by prefix, so nothing of nixpkgs' own Windows
  # package set is evaluated -- that is where the broken mingw Python lives.
  qemu = pkgs.stdenv.mkDerivation {
    pname = "qemu-windows";
    inherit version src;
    patches = [ ./qemu-windows-patches/whpx-cpuid-0xd-ebx.patch ];
    nativeBuildInputs = with pkgs; [
      win.buildPackages.gcc pkg-config flex bison meson ninja perl zstd
      # QEMU's configure builds an offline venv that installs its vendored wheels (qemu.qmp), and
      # that needs these in the build machine's Python.
      (python3.withPackages (ps: [ ps.distlib ps.setuptools ps.wheel ps.pip ]))
    ];
    dontUseMesonConfigure = true; # QEMU's configure drives meson itself
    dontUseNinjaBuild = true;
    dontUseNinjaInstall = true;
    dontFixup = true; # PE files: no patchelf, and --enable-strip already stripped them
    postUnpack = unpackSysroot;
    preConfigure = ''
      patchShebangs .
      # stdenv exports the NATIVE toolchain, and QEMU's configure prefers $CC over --cross-prefix:
      # left set, meson configures for Linux and fails on WHPX.
      unset CC CXX LD AR AS NM RANLIB STRIP OBJCOPY OBJDUMP WINDRES CPP READELF SIZE STRINGS
      export PKG_CONFIG=pkg-config
      export PKG_CONFIG_LIBDIR=$NIX_BUILD_TOP/sysroot/ucrt64/lib/pkgconfig:$NIX_BUILD_TOP/sysroot/ucrt64/share/pkgconfig
      export PKG_CONFIG_SYSROOT_DIR=$NIX_BUILD_TOP/sysroot
      # Here, not in configureFlags: those are not shell-expanded.
      appendToVar configureFlags \
        "--extra-cflags=-isystem$NIX_BUILD_TOP/sysroot/ucrt64/include" \
        "--extra-ldflags=-L$NIX_BUILD_TOP/sysroot/ucrt64/lib"
    '';
    configurePlatforms = [ ];
    # Headless and single-target: the product runs -display none on x86_64, so every UI, audio,
    # USB and remote-storage backend is off, and so is every library it would have pulled in.
    configureFlags = [
      "--cross-prefix=x86_64-w64-mingw32-"
      "--target-list=x86_64-softmmu"
      "--enable-whpx" "--enable-slirp" "--enable-tools" "--enable-strip"
      "--disable-gtk" "--disable-sdl" "--disable-docs"
      "--disable-guest-agent" "--disable-guest-agent-msi"
      "--disable-download" "--disable-werror"
    ];
    enableParallelBuilding = true;
    # The Windows install layout: executables at the prefix root, firmware under share/.
    installPhase = ''
      runHook preInstall
      make install DESTDIR="$NIX_BUILD_TOP/inst"
      cp -r "$NIX_BUILD_TOP/inst$out" "$out"
      runHook postInstall
    '';
  };

  # The two binaries the product runs: qemu-system-x86_64 boots the guest, qemu-img makes the
  # install-time overlay. Same pair as the Linux bundle, for the same reasons.
  qemuBins = [ "qemu-system-x86_64.exe" "qemu-img.exe" ];

  # The x86 firmware allowlist. It is the Linux bundle's list plus efi-e1000.rom: QEMU adds a
  # default e1000 NIC to any launch that names no netdev and then wants its option ROM. Every
  # installed node names one, so this is the bundle being wider than the product needs -- but a
  # bundle that only works for the launches we happen to make is a trap for whoever makes another.
  firmware = [
    "bios-256k.bin" "bios.bin"
    "kvmvapic.bin" "linuxboot_dma.bin" "pvh.bin"
    "vgabios-stdvga.bin" "vgabios-bochs-display.bin" "vgabios.bin"
    "efi-virtio.rom" "efi-e1000.rom"
  ];

  bundle = pkgs.runCommand "briard-qemu-bundle-windows-${version}" {
    nativeBuildInputs = [ pkgs.zstd pkgs.binutils ];
    bins = lib.concatStringsSep " " qemuBins;
    fw = lib.concatStringsSep " " firmware;
  } ''
    ${unpackSysroot}
    # One flat pool to resolve against: the binaries, every MSYS2 DLL, the toolchain's two.
    mkdir pool
    cp ${qemu}/*.exe "$NIX_BUILD_TOP"/sysroot/ucrt64/bin/*.dll ${lib.concatStringsSep " " toolchainDlls} pool/
    chmod -R u+w pool
    mkdir -p "$out/share/qemu"

    # THE DLL SET IS THE PE IMPORT CLOSURE, not "every DLL in the pool" -- the same doctrine the
    # Linux bundle applies with ldd, and objdump reads a PE import table as readily as an ELF
    # NEEDED. A DLL not in the pool is Windows' own (KERNEL32, the api-ms-win-crt-* UCRT set).
    #
    # ⚠️ The same gap the Linux bundle names applies here: an import table does not capture a
    # runtime LoadLibrary, so the honest check is a real boot on Windows.
    cd pool
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
    for f in $seen; do [ -f "$f" ] && cp "$f" "$out/"; done
    cd ..

    for f in $fw; do
      cp "${qemu}/share/$f" "$out/share/qemu/$f" ||
        { echo "firmware blob missing from the build: $f" >&2; exit 1; }
    done

    # Build-time assertions -- the ones a Linux builder CAN make about a Windows tree.
    for b in $bins; do
      [ -s "$out/$b" ] || { echo "missing binary: $b" >&2; exit 1; }
    done
    # Every import the closure did not resolve must be a Windows system DLL. A library missing
    # from the pins would otherwise ship as a bundle that fails to start, and only on Windows.
    for f in "$out"/*.exe "$out"/*.dll; do
      for d in $(objdump -p "$f" | awk '/DLL Name:/ {print $3}'); do
        [ -f "$out/$d" ] && continue
        case "$(echo "$d" | tr A-Z a-z)" in
          api-ms-win-*|kernel32.dll|advapi32.dll|user32.dll|shell32.dll|shlwapi.dll|ole32.dll|\
          ws2_32.dll|iphlpapi.dll|dnsapi.dll|winmm.dll|bcrypt.dll|crypt32.dll|secur32.dll|\
          ntdll.dll|gdi32.dll|userenv.dll|setupapi.dll|cfgmgr32.dll|winhvplatform.dll|\
          winhvemulation.dll|oleaut32.dll|shcore.dll|dwmapi.dll|uxtheme.dll|imm32.dll|version.dll) ;;
          *) echo "$(basename "$f") imports $d, which is neither bundled nor a Windows DLL" >&2; exit 1 ;;
        esac
      done
    done

    {
      echo "qemu=${version} ${src.url}"
      echo "sha256=${src.outputHash}"
      echo "msys2 (ucrt64):"
      for p in ${lib.concatStringsSep " " (map (p: p.name) msys2)}; do echo "  $p"; done
      echo "dlls=$(ls "$out"/*.dll | wc -l)"
      echo "firmware:"; ls "$out/share/qemu"
    } > "$out/PROVENANCE"
  '';
in
{
  inherit bundle version;
}
