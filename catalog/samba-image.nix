# The Samba server Files' pod runs beside the file browser: the household's files as a drive.
#
# This is the one catalog image Briard builds. Every other is pulled from its upstream by
# digest; Samba publishes no container, and the ones other people publish are not maintained to
# a standard a catalog can pin.
#
# A file server and nothing else. nixpkgs' samba is a general build: Python (the AD tooling and
# its bindings), systemd, dbus, regedit, libunwind, profiling and PAM, none of which has a job
# in a container serving one share to accounts read from a file. Without them the closure is a
# quarter of the size. talloc, tevent, tdb and ldb are Samba's bundled copies, because nixpkgs'
# shared ones are built with their Python bindings and would bring Python back; dbus is what
# vfs_snapper needs, so that module goes with it.
#
# The image holds smbd and the directories it insists on. Its configuration, its accounts and
# their passwords are rendered outside and mounted at /briard, so the image is the same in
# every home and a rebuild changes no household's settings.
#
# The override is never in the public binary cache: every build compiles Samba.
#
# nixpkgsRev is stamped on the image as a label, so an image says which nixpkgs it was built
# from and can be built again: `nix build .#samba-image --override-input nixpkgs
# github:NixOS/nixpkgs/<that revision>`.
{ lib, dockerTools, samba, python3Packages, dbus, bash, talloc, tevent, tdb, ldb, nixpkgsRev ? "unknown" }:

let
  drop = [ python3Packages.python dbus bash talloc tevent tdb ldb ];
  smbd = (samba.override {
    systemd = null;
    enableRegedit = false;
    enableLibunwind = false;
    enableProfiling = false;
    enablePam = false;
  }).overrideAttrs (old: {
    buildInputs = lib.filter (p: p != null && !(lib.elem p drop)) old.buildInputs;
    pythonPath = [ ];
    wafConfigureFlags =
      map (f: if f == "--with-shared-modules=ALL" then "--with-shared-modules=ALL,!vfs_snapper" else f)
        (lib.filter (f: !(lib.hasPrefix "--bundled-libraries=!ldb" f)) old.wafConfigureFlags)
      ++ [ "--disable-python" "--without-systemd" ];
    postFixup = lib.replaceStrings [ "wrapPythonPrograms" ] [ "" ] old.postFixup;
  });
in
dockerTools.streamLayeredImage {
  name = "samba";
  tag = smbd.version;
  # /etc/passwd is a link into /briard: smbd maps every account to a system user, and the
  # accounts are rendered with the rest.
  extraCommands = ''
    mkdir -p etc var/lib/samba/private var/lock/samba var/cache/samba var/log/samba var/run/samba tmp
    chmod 1777 tmp
    ln -s /briard/passwd etc/passwd
    printf 'root:x:0:\nnogroup:x:65534:\n' > etc/group
  '';
  config = {
    Entrypoint = [ "${smbd}/bin/smbd" "--foreground" "--no-process-group" "--debug-stdout" "--configfile=/briard/smb.conf" ];
    ExposedPorts."445/tcp" = { };
    Labels = {
      "org.opencontainers.image.title" = "samba";
      "org.opencontainers.image.version" = smbd.version;
      "org.opencontainers.image.licenses" = "GPL-3.0-or-later";
      "org.opencontainers.image.source" = "https://github.com/briardhq/briard";
      "io.briard.nixpkgs" = nixpkgsRev;
    };
  };
  passthru = { inherit smbd; };
}
