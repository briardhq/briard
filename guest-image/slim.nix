# What this appliance does NOT need to carry.
#
# Every base-OS bump is a closure pull onto every node in the field, so the guest's size is not
# cosmetic -- it is the wait and the bandwidth on every fleet update, on household links we do not
# choose. This file is the answer to [B.5], carried since v1.
#
# MEASURED, ONE LEVER AT A TIME (2026-08-11, `nix path-info -S` on the shipped
# `artifacts.guest-disk.system`), not reasoned about: 1613.7 MB -> 978.0 MB, a 40% cut with the
# stock kernel untouched. The per-lever numbers below are what each one actually removed, because a
# number nobody measured is a number that will be wrong after the next nixpkgs bump.
#
# THE SHAPE OF THE FAT WAS NOT WHAT THE ORIGINAL B.5 ENTRY GUESSED. It said "docs/man/locales, trim
# perl activation, minimal profile". Docs were real (94 MB); locales were already trimmed (3 MB
# total); perl is NOT removable (`update-users-groups.pl` runs at every activation, and
# `system.etc.overlay` -- the documented way off the other perl script -- measured 0.3 MB because of
# it). What actually dominated were three ACCIDENTS, none of which anybody chose: a registry entry
# that dragged in the whole of nixpkgs, a container runtime built with a checkpoint engine that
# propagates Python, and two copies of podman.
#
# WHAT IS DELIBERATELY NOT HERE:
#   - `environment.defaultPackages = []` (perl/rsync/strace). Measured 4.8 MB -- perl stays for the
#     activation script regardless -- and it costs `strace` on a node somebody is trying to debug in
#     a stranger's house. Not a trade worth 5 MB.
#   - Trimming the kernel's module tree, which at 148.7 MB is now the largest single path in the
#     closure (15%). A virtio-only `structuredExtraConfig` would plausibly reach ~20 MB, but it means
#     we build and own the kernel: it leaves cache.nixos.org, our cache serves it on every update,
#     DRBD 9.2 rebuilds against our config, and we inherit a kernel-config security obligation.
#     Evaluated and declined -- 130 MB is not worth becoming a kernel distributor.
{ config, lib, pkgs, ... }:
let
  # crun without the two subsystems this product cannot reach.
  #
  # CRIU (checkpoint/restore) is the expensive one: nixpkgs' crun links it unconditionally and criu
  # PROPAGATES python3 -- an interpreter in an appliance that has no Python in it.
  #
  # ⚠️ QUOTE THE WIRE NUMBERS, NOT THE NAR NUMBERS, because this trade was first argued with the
  # wrong ones and looked ~3x better than it is. The cache stores xz (`Compression: xz`, 2.4-6x)
  # and the disk ships zstd, so what anyone actually waits on is:
  #     install download   438 -> 377 MB   (61 MB, 14%)   measured, both images built
  #     closure fetch      -67 MB xz       (python3 56.5 + libkrunfw 7.8 + libkrun 1.8 + criu 0.8)
  # against a cost of ~14.3 MB xz (podman 13.9 + crun 0.4) that WE serve instead of the CDN, plus a
  # ~47s podman rebuild per nixpkgs bump. Kept on those numbers (owner, 2026-08-12) -- but it is a
  # near thing, and it is the only package we deviate from stock on for SIZE rather than function.
  #
  # There is no cheaper cut. python3's entire foothold is two files -- criu's `bin/criu-ns` and its
  # wrapper, a helper script nothing here calls -- so `rm`ing them would drop 56.5 MB while keeping
  # checkpoint/restore. It is NOT worth doing: any change to criu rebuilds crun rebuilds podman, so
  # the cost is identical, the saving smaller, and a `rm` of a path upstream may rename fails OPEN.
  #
  # Upstream treats CRIU as optional -- `configure.ac` carries `--disable-criu`, every use site is
  # behind `#if HAVE_CRIU` (src/checkpoint.c, src/libcrun/criu.c), and crun *dlopens* libcriu at
  # runtime; nixpkgs' `NIX_LDFLAGS = "-lcriu"` exists only to pin the store reference that the
  # dlopen would otherwise leave dangling. So this removes `podman container checkpoint`, and
  # nothing else. We have never checkpointed a container: the failover model moves a VOLUME between
  # nodes and restarts the workload, which is the opposite technique.
  #
  # libkrun is crun's microVM handler, reached only by an explicit `run.oci.handler=krun`
  # annotation. Our payload is an ordinary host-networked container; there is no path to it.
  #
  # `criu = null` RATHER THAN FILTERING buildInputs BY NAME, and the difference is which way it
  # breaks. A name filter fails OPEN -- a nixpkgs bump that renames the input silently re-admits
  # the interpreter and every closure grows with nobody told. Overriding the named argument fails CLOSED:
  # if the argument goes away, `.override` throws at eval and CI stops. The same reasoning applies
  # to the NIX_LDFLAGS reset, which is coupled to a nixpkgs implementation detail and will fail as
  # a link error rather than quietly.
  #
  # THE COST, stated because it is not free: overriding crun changes podman's hash, so podman and
  # crun leave cache.nixos.org (both are there today) and move onto cache.briard.io -- about 58 MB
  # more for our cache to serve, against a closure that is 636 MB smaller. Rebuild cost is
  # negligible (measured 47s for podman; the Go vendor derivation is unaffected and still
  # substitutes). It does put the container runtime behind the alpha signing key, which is
  # provisional -- but drbd, drbd-reactor, reverse-proxy and briard-agent already ride that key, so
  # it is one more component on a key already scheduled for rotation, not a new trust boundary.
  leanCrun = (pkgs.crun.override {
    criu = null;
    withLibkrun = false;
  }).overrideAttrs (old: {
    configureFlags = (old.configureFlags or [ ]) ++ [ "--disable-criu" ];
    env = (old.env or { }) // { NIX_LDFLAGS = ""; };
  });
in
{
  # -205 MB, the single largest item, and pure accident. Nothing here uses flakes at runtime, but
  # the default registry writes `/etc/nix/registry.json` with a `path` entry pointing at the
  # nixpkgs source -- so the ENTIRE nixpkgs tree is a runtime dependency of the guest, shipped to
  # every household, to serve a `nix flake` command nobody will ever type on this box. `setNixPath`
  # goes with it (the module asserts registry is required for it, and `<nixpkgs>` is equally unused).
  nixpkgs.flake.setFlakeRegistry = false;
  nixpkgs.flake.setNixPath = false;

  # -94 MB. The NixOS manual, the Nix manual, man pages, info pages, and the groff/texinfo/man-db
  # toolchain to read them. This is a headless appliance whose console exists for the serial log;
  # documentation reaches its operator through the host, not through a guest nobody logs into.
  documentation.enable = false;
  documentation.nixos.enable = false;
  documentation.man.enable = false;
  documentation.info.enable = false;
  documentation.doc.enable = false;

  # -75 MB. podman 5 uses netavark (the generated containers.conf says so), and CNI is the legacy
  # backend it will not reach for -- but the NixOS module writes `cni_plugin_dirs` unconditionally,
  # which is enough to make the whole plugin set a runtime dependency. Removing the path removes the
  # reference; the backend is unchanged.
  virtualisation.containers.containersConf.cniPlugins = lib.mkForce [ ];

  # -162 MB of closure / -61 MB of install download, jointly with `disableInstallerTools` below:
  # python3 had TWO referrers (criu, and nixos-rebuild), so neither lever alone removes it. They are
  # separable in principle and joint in effect, which is why they are both here and why removing one
  # of them later will look like it saved nothing.
  virtualisation.podman.package = pkgs.podman.override { crun = leanCrun; };

  # The other half of the python3 pair. `nixos-rebuild` is a Python program now, and this guest does
  # not rebuild itself: the host agent owns the whole update lifecycle and drives it through
  # `nix-env --set` + `switch-to-configuration` (agent/guestagent), never through nixos-rebuild.
  # ⚠️ This also removes `nixos-version`. Nothing in the product calls it (agent, scripts, and the
  # report card all checked) -- if that ever changes, take the per-tool
  # `system.tools.nixos-rebuild.enable = false` form instead of this blanket one.
  system.disableInstallerTools = true;

  # -14 MB (nano, plus the `file` database it carries). The guest is dumb hands: it holds no
  # configuration a human is meant to edit, and every file that decides anything is either written
  # by the agent or replicated on the volume. An editor here would be a way to create state nothing
  # reconciles.
  programs.nano.enable = false;

  # -12 MB. MIME databases, icon caches, desktop-menu and autostart scaffolding, on a machine with
  # no desktop, no user session and no graphics.
  xdg.mime.enable = false;
  xdg.icons.enable = false;
  xdg.sounds.enable = false;
  xdg.autostart.enable = false;
  xdg.menus.enable = false;
}
