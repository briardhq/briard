# The relocatable qemu bundle.
#
# The free-local install ships its own qemu (no distro package) so prod runs exactly the qemu the
# nixosTests run. The one hard problem: a nixpkgs qemu has /nix/store RPATHs + a /nix/store ELF
# interpreter, so it can't run on a stock (non-Nix) host. This derivation relocates it into a
# self-contained tree installed at a FIXED prefix (/opt/briard/qemu, our install location): the ELF
# interpreter is baked to <prefix>/lib/ld-linux, and every binary's RPATH is $ORIGIN-relative, so
# the tree resolves entirely within itself and never touches /nix/store.
#
# The proof (test below) runs the bundle in a mount namespace with /nix replaced by an empty tmpfs:
# if qemu still runs, it provably doesn't reach into /nix/store -- exactly the stock-host condition.
{ pkgs }:

let
  lib = pkgs.lib;

  # Base qemu. qemu_test is nixpkgs' headless build for the VM-test driver: the real binary
  # DIRECTLY (no Nix C wrapper -- qemu_kvm's `qemu-system-x86_64` is a wrapper that execs a
  # hardcoded /nix path, which a relocated/masked-/nix run can't reach), no GTK/SDL/X11 (60 libs
  # vs 169), module-free, and already cached (every nixosTest boots it -- no from-source rebuild).
  # It is exactly the qemu our tests run, so bundling it also closes the test-vs-prod gap. A yet
  # tighter product build (our own --disable-* override: smaller artifact + CVE surface) is the
  # follow-on once relocatability is proven.
  baseQemu = pkgs.qemu_test;

  # Where the bundle is installed on the target host. The ELF interpreter is an absolute path (the
  # kernel does not expand $ORIGIN in PT_INTERP), so it must be the real install location; we always
  # install to /opt/briard, so baking that prefix is correct + still fully off /nix.
  installPrefix = "/opt/briard/qemu";

  # The binaries the free-local install needs: qemu-system-x86_64 to boot the guest, and
  # qemu-img to make the writable guest overlay at install time (the installer ships its own
  # qemu, so it can't lean on a distro qemu-img). They share one qemu build, so one identical
  # .so closure -- relocate both into $out/bin over the shared $out/lib.
  qemuBins = "qemu-system-x86_64 qemu-img";

  # Relocate: copy the qemu binaries + their transitive .so closure + the loader into a flat
  # tree, then patchelf the interpreter to <prefix>/lib and every RPATH to $ORIGIN-relative.
  # dlopen'd modules are NOT captured by ldd -- baseQemu with modules is a known gap for booting
  # (block drivers); --version + accel probing don't dlopen, which is what assertion (a) proves.
  bundle = pkgs.runCommand "briard-qemu-bundle" {
    nativeBuildInputs = [ pkgs.patchelf pkgs.glibc.bin pkgs.coreutils ];
    inherit installPrefix qemuBins;
  } ''
    mkdir -p "$out/bin" "$out/lib"
    for b in $qemuBins; do
      cp "${baseQemu}/bin/$b" "$out/bin/$b"
      chmod u+w "$out/bin/$b"
      # Transitive shared-library closure (ldd shows the full transitive set for a non-dlopen binary).
      for so in $(ldd "${baseQemu}/bin/$b" | awk '{print $3}' | grep '^/'); do
        cp -Ln "$so" "$out/lib/" 2>/dev/null || true
      done
    done
    # The dynamic loader itself (identical for both binaries).
    loader=$(patchelf --print-interpreter "${baseQemu}/bin/qemu-system-x86_64")
    cp -Ln "$loader" "$out/lib/$(basename "$loader")" 2>/dev/null || true

    chmod -R u+w "$out/lib"

    # Bake the interpreter to the fixed install prefix; RPATH $ORIGIN-relative so libc + siblings
    # resolve within the tree wherever it is copied (bin/ and lib/ stay siblings). The dynamic
    # loader (ld-linux) is found by the kernel via this PT_INTERP; ld.so then reads THIS RUNPATH to
    # locate libc.so.6 in lib/. The loader itself must stay pristine -- patchelf'ing ld-linux
    # corrupts it (segfault in _dl_start), so it is explicitly excluded from the rpath pass below.
    loaderBase=$(basename "$loader")
    for b in $qemuBins; do
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

    # Firmware/BIOS blobs: --version needs none of these, but BOOTING a
    # disk does -- qemu's compiled-in datadir is a /nix/store path absent on a stock host, so
    # SeaBIOS/vgabios/option-ROMs must travel in the bundle and the launcher points `-L` here
    # (QEMU_DATADIR -> platform.qemuArgs). baseQemu's share/qemu is ~300 MB (every arch's
    # firmware + UEFI images); an x86 SeaBIOS disk boot with virtio needs only this handful, so
    # we copy an allowlist (KB each) rather than the whole tree. A missing one surfaces as a
    # firmware-not-found / "No bootable device" in the boot test, not a silent hang.
    mkdir -p "$out/share/qemu"
    for f in \
      bios-256k.bin bios.bin \
      kvmvapic.bin linuxboot_dma.bin pvh.bin \
      vgabios-stdvga.bin vgabios-bochs-display.bin vgabios.bin \
      efi-virtio.rom ; do
      cp -Ln "${baseQemu}/share/qemu/$f" "$out/share/qemu/$f" 2>/dev/null || true
    done

    # A record of what we bundled, for debugging drift.
    for b in $qemuBins; do ${pkgs.file}/bin/file "$out/bin/$b" >> "$out/PROVENANCE" || true; done
    echo "prefix=${installPrefix}" >> "$out/PROVENANCE"
    echo "firmware:" >> "$out/PROVENANCE"
    ls "$out/share/qemu" >> "$out/PROVENANCE" || true
  '';

  # The hermetic proof: install the bundle at its real prefix, hide /nix behind an empty tmpfs in a
  # private mount namespace, and run the bundled qemu. Success = it never reached into /nix/store.
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

      # THE PROOF (assertion a): in a private mount namespace (real root, no userns -- the minimal
      # test kernel restricts unprivileged user namespaces), mask /nix with an empty tmpfs, then run
      # the bundled qemu. If it still prints its version, it provably resolves its loader + every lib
      # within /opt/briard/qemu and never touches /nix -- exactly the stock (non-Nix) host condition.
      # `mount` runs before the mask (still visible); qemu is exec'd after /nix is gone.
      out = machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec /opt/briard/qemu/bin/qemu-system-x86_64 --version'"
      )
      print("bundled qemu with /nix masked:\n" + out)
      assert "QEMU emulator" in out, "bundled qemu did not run with /nix hidden"

      # Non-vacuity: a plain /nix/store binary MUST fail under the same mask (proves the tmpfs
      # actually hid /nix, so the proof above isn't passing for the wrong reason).
      machine.fail(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec ${pkgs.hello}/bin/hello --version'"
      )

      # Firmware travels with the bundle (assertion b prerequisite): the SeaBIOS blob a disk
      # boot needs resolves off /opt under the /nix mask -- qemu's compiled-in datadir is a
      # /nix path, so without this the guest would not boot. (The actual boot is proven in
      # install-macvtap.nix; here we only assert the blob is present + reachable off /nix.)
      machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && test -s /opt/briard/qemu/share/qemu/bios-256k.bin'"
      )

      # qemu-img travels too (the installer makes the guest overlay with it) and relocates the
      # same way -- prove it runs off /opt with /nix masked.
      out = machine.succeed(
          "unshare --mount --propagation private "
          + "sh -c 'mount -t tmpfs none /nix && exec /opt/briard/qemu/bin/qemu-img --version'"
      )
      assert "qemu-img" in out, "bundled qemu-img did not run with /nix hidden"
    '';
  };
in
{
  inherit bundle test installPrefix;
}
