# The relocatable qemu bundle. The free-local install ships its own qemu (no distro package) so
# prod runs exactly the qemu the nixosTests run. A nixpkgs qemu has /nix/store RPATHs and a
# /nix/store ELF interpreter, so it cannot run on a stock (non-Nix) host; this derivation
# relocates it into a self-contained tree at a fixed prefix (/opt/briard/qemu, our install
# location): the interpreter is baked to <prefix>/lib/ld-linux and every RPATH is
# $ORIGIN-relative, so the tree never touches /nix/store.
#
# The test runs the bundle in a mount namespace with /nix replaced by an empty tmpfs -- the
# stock-host condition: if qemu still runs, it provably does not reach into /nix/store.
{ pkgs }:

let
  lib = pkgs.lib;

  # qemu_test is nixpkgs' headless build for the VM-test driver: the real binary directly (the
  # `qemu-system-x86_64` of qemu_kvm is a Nix C wrapper execing a hardcoded /nix path, which a
  # relocated/masked-/nix run cannot reach), no GTK/SDL/X11 (60 libs vs 169), module-free, and
  # already cached (every nixosTest boots it). Being the qemu our tests run, it also closes the
  # test-vs-prod gap. A tighter product build (our own --disable-* override: smaller artifact +
  # CVE surface) is the follow-on once relocatability is proven.
  baseQemu = pkgs.qemu_test;

  # The install location on the target host. The ELF interpreter is an absolute path (the kernel
  # does not expand $ORIGIN in PT_INTERP), so it must be the real one; we always install to
  # /opt/briard, so baking it is correct and still fully off /nix.
  installPrefix = "/opt/briard/qemu";

  # qemu-system-x86_64 boots the guest; qemu-img makes the writable guest overlay at install time
  # (no distro qemu-img to lean on). One qemu build, so one .so closure, shared in $out/lib.
  # smartctl rides along for the disk-health check (agent/reportcard/diskhealth.go): same glibc
  # and libstdc++, so it costs its own 1.2 MB and no library the tree did not already hold.
  bundleBins = "${baseQemu}/bin/qemu-system-x86_64 ${baseQemu}/bin/qemu-img ${pkgs.smartmontools}/bin/smartctl";

  # Copy the binaries, their transitive .so closure and the loader into a flat tree, then
  # patchelf the interpreter to <prefix>/lib and every RPATH to $ORIGIN-relative. ldd does not
  # capture dlopen'd modules: a baseQemu with modules is a known gap for booting (block drivers);
  # --version and accel probing do not dlopen, which is what assertion (a) proves.
  bundle = pkgs.runCommand "briard-qemu-bundle" {
    nativeBuildInputs = [ pkgs.patchelf pkgs.glibc.bin pkgs.coreutils pkgs.diffutils ];
    inherit installPrefix bundleBins;
  } ''
    mkdir -p "$out/bin" "$out/lib"
    for src in $bundleBins; do
      b=$(basename "$src")
      cp "$src" "$out/bin/$b"
      chmod u+w "$out/bin/$b"
      # The transitive .so closure (ldd shows all of it for a non-dlopen binary). The tree is
      # flat, so two binaries needing two builds of one library would silently get the first:
      # refuse that instead.
      for so in $(ldd "$src" | awk '{print $3}' | grep '^/'); do
        dst="$out/lib/$(basename "$so")"
        if [ -e "$dst" ]; then
          cmp -s "$so" "$dst" || { echo "$b needs a different $(basename "$so") than the bundle holds" >&2; exit 1; }
        else
          cp -L "$so" "$dst"
        fi
      done
    done
    # The dynamic loader itself (identical for every binary: one glibc).
    loader=$(patchelf --print-interpreter "${baseQemu}/bin/qemu-system-x86_64")
    cp -Ln "$loader" "$out/lib/$(basename "$loader")" 2>/dev/null || true

    chmod -R u+w "$out/lib"

    # The kernel finds ld-linux via PT_INTERP, baked to the fixed prefix; ld.so then reads the
    # binary RUNPATH ($ORIGIN-relative, bin/ and lib/ staying siblings) to find libc.so.6 and the
    # rest in lib/, wherever the tree is copied. The loader must stay pristine: patchelf corrupts
    # ld-linux (segfault in _dl_start), so the rpath pass below skips it.
    loaderBase=$(basename "$loader")
    for b in $(cd "$out/bin" && ls); do
      patchelf --set-interpreter "${installPrefix}/lib/$loaderBase" \
               --set-rpath '$ORIGIN/../lib' "$out/bin/$b"
    done
    for l in "$out"/lib/*.so*; do
      [ -f "$l" ] || continue
      case "$(basename "$l")" in
        ld-linux*|ld-*.so*) continue ;; # never patchelf the loader
      esac
      patchelf --set-rpath '$ORIGIN' "$l" 2>/dev/null || true
    done

    # Firmware: --version needs none, but booting a disk does, and the compiled-in datadir is a
    # /nix/store path absent on a stock host -- so SeaBIOS/vgabios/option-ROMs travel here and
    # the launcher points `-L` at them (QEMU_DATADIR -> platform.qemuArgs). share/qemu is ~300 MB
    # (every arch, UEFI images); an x86 SeaBIOS virtio boot needs only this allowlist (KB each). A
    # missing one surfaces as firmware-not-found / "No bootable device" in the boot test, not a
    # silent hang.
    mkdir -p "$out/share/qemu"
    for f in \
      bios-256k.bin bios.bin \
      kvmvapic.bin linuxboot_dma.bin pvh.bin \
      vgabios-stdvga.bin vgabios-bochs-display.bin vgabios.bin \
      efi-virtio.rom ; do
      cp -Ln "${baseQemu}/share/qemu/$f" "$out/share/qemu/$f" 2>/dev/null || true
    done

    # A record of what we bundled, for debugging drift.
    for b in $(cd "$out/bin" && ls); do ${pkgs.file}/bin/file "$out/bin/$b" >> "$out/PROVENANCE" || true; done
    echo "prefix=${installPrefix}" >> "$out/PROVENANCE"
    echo "firmware:" >> "$out/PROVENANCE"
    ls "$out/share/qemu" >> "$out/PROVENANCE" || true
  '';

  # The hermetic proof: the bundle at its real prefix, /nix hidden behind an empty tmpfs in a
  # private mount namespace; qemu running means it never reached into /nix/store.
  test = pkgs.testers.runNixOSTest {
    name = "qemu-bundle";
    skipTypeCheck = true;

    nodes.machine = { ... }: {
      environment.systemPackages = [ pkgs.util-linux ];
    };

    testScript = ''
      machine.wait_for_unit("multi-user.target")

      # Install the bundle at its production prefix (the interpreter is baked to it).
      machine.succeed("mkdir -p /opt/briard")
      machine.succeed("cp -r ${bundle} /opt/briard/qemu")
      machine.succeed("chmod -R u+w /opt/briard/qemu")

      # Sanity: it runs normally at the baked prefix (with /nix present).
      print(machine.succeed("/opt/briard/qemu/bin/qemu-system-x86_64 --version"))

      # Assertion (a): a private mount namespace (real root, no userns: the minimal test kernel
      # restricts unprivileged user namespaces) masks /nix with an empty tmpfs. qemu printing its
      # version proves it resolves its loader and every lib within /opt/briard/qemu. `mount` runs
      # before the mask (still visible); qemu is exec'd after /nix is gone.
      out = machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec /opt/briard/qemu/bin/qemu-system-x86_64 --version'"
      )
      print("bundled qemu with /nix masked:\n" + out)
      assert "QEMU emulator" in out, "bundled qemu did not run with /nix hidden"

      # Non-vacuity: a plain /nix/store binary must fail under the same mask, so the tmpfs really
      # hid /nix.
      machine.fail(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec ${pkgs.hello}/bin/hello --version'"
      )

      # Assertion (b) prerequisite: the SeaBIOS blob a disk boot needs resolves off /opt under the
      # mask (the compiled-in datadir is a /nix path, so without it the guest would not boot). The
      # boot itself is proven in install-macvtap.nix.
      machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && test -s /opt/briard/qemu/share/qemu/bios-256k.bin'"
      )

      # qemu-img (the installer makes the guest overlay with it) relocates the same way.
      out = machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec /opt/briard/qemu/bin/qemu-img --version'"
      )
      assert "qemu-img" in out, "bundled qemu-img did not run with /nix hidden"

      # And smartctl, which the agent's disk-health check runs from this tree.
      out = machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec /opt/briard/qemu/bin/smartctl --version'"
      )
      assert "smartctl" in out, "bundled smartctl did not run with /nix hidden"
    '';
  };
in
{
  inherit bundle test installPrefix;
}
