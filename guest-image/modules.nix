# The kernel module tree, denylisted. The stock tree was 149 MB on disk and, since modules ship
# as .ko.xz and compress no further, 141 MB of the 358 MB image a household downloads (39%):
# 7296 modules, of which a qemu/virtio guest can load perhaps a hundred. Trimming the kernel was
# evaluated and declined (slim.nix: it makes us a kernel distributor). Instead the kernel is the
# upstream store path, byte for byte, and only the module tree is copied without the families a
# virtio guest has no hardware for (sound cards, GPUs, USB, sensors, physical NICs and radios)
# and the protocol stacks and filesystems nothing in the product speaks or mounts. What remains
# is every module that could conceivably matter: all of virtio, netfilter and bridging (podman
# networking loads these on demand), the block and md layers, the filesystems we use, crypto,
# and the out-of-tree DRBD module. Measured: 149 -> 35 MB on disk, 141 -> 32 MB compressed,
# 7296 -> 1724 modules.
#
# A denylist, not an allowlist, because of where a mistake surfaces: a wrong allowlist guess about
# what loads on demand fails weeks later at the feature that first needs the module, while a
# denylist removes only what cannot bind to this machine, so its failure is a `modprobe` refusing
# loudly in the journal. The initrd module closure is built with allowMissing=false, so a denied
# module the initrd asks for fails the build, not a boot; hence the initrd lists below are pinned
# to the virtio set: the qemu-guest profile asks for 9p/virtiofs (denied) and virtio_gpu
# (drivers/gpu, denied; this guest has a serial console and no display), and the NixOS default
# list asks for every SATA, USB and HID driver.
{ config, lib, pkgs, ... }:
let
  kernel = config.boot.kernelPackages.kernel;
  # The full tree as NixOS would aggregate it: the kernel's modules plus the out-of-tree ones
  # (DRBD), one depmod'd directory.
  full = pkgs.aggregateModules ([ (lib.getOutput "modules" kernel) ] ++ config.boot.extraModulePackages);
  # Directories under lib/modules/<ver>/kernel/ this guest can never use. Families, not files:
  # a new driver upstream lands inside one of these and is dropped with it.
  denied = [
    "sound"
    # hardware a virtio guest does not have
    "drivers/media" "drivers/gpu" "drivers/iio" "drivers/usb" "drivers/hid"
    # drivers/input: the subdirectories only (this guest has no keyboards, mice or touchscreens).
    # `evdev` (CONFIG_INPUT_EVDEV=m) sits at the top of the family and turns the ACPI power button
    # into a /dev/input/event* node, the only thing logind watches to shut down on that button.
    # Denying the family wholesale leaves the guest deaf to it: the host presses the button,
    # waits its minute and kills the VM, so every clean stop becomes a hard one (agent-readopt
    # asserts the clean stop).
    "drivers/input/touchscreen" "drivers/input/tablet" "drivers/input/joystick"
    "drivers/input/mouse" "drivers/input/keyboard" "drivers/input/misc" "drivers/input/rmi4"
    "drivers/input/gameport" "drivers/input/serio"
    "drivers/infiniband" "drivers/hwmon" "drivers/platform" "drivers/video" "drivers/staging"
    "drivers/regulator" "drivers/mtd" "drivers/mfd" "drivers/ata" "drivers/w1" "drivers/bluetooth"
    "drivers/isdn" "drivers/nfc" "drivers/leds" "drivers/thermal" "drivers/power" "drivers/pcmcia"
    "drivers/parport" "drivers/firewire" "drivers/gnss" "drivers/comedi" "drivers/greybus"
    "drivers/most" "drivers/misc" "drivers/rtc" "drivers/spi" "drivers/i2c" "drivers/i3c"
    "drivers/soundwire" "drivers/accel" "drivers/fpga" "drivers/edac" "drivers/mmc"
    "drivers/memstick" "drivers/rapidio" "drivers/message" "drivers/uio" "drivers/xen" "drivers/hv"
    "drivers/vfio" "drivers/dca" "drivers/ntb" "drivers/peci" "drivers/pwm" "drivers/siox"
    "drivers/slimbus" "drivers/pps" "drivers/ptp" "drivers/rpmsg"
    # physical NICs and radios; virtio_net, veth, tun, macvlan, bridge helpers stay at drivers/net/
    "drivers/net/wireless" "drivers/net/ethernet" "drivers/net/wan" "drivers/net/usb"
    "drivers/net/can" "drivers/net/fddi" "drivers/net/hamradio" "drivers/net/wwan"
    # protocol stacks nothing here speaks (netfilter, bridge, ipv4/6, unix, tls, xfrm stay)
    "net/wireless" "net/bluetooth" "net/x25" "net/tipc" "net/smc" "net/rxrpc" "net/rds" "net/ax25"
    "net/appletalk" "net/atm" "net/batman-adv" "net/can" "net/dccp" "net/hsr" "net/ieee802154"
    "net/mac80211" "net/mac802154" "net/nfc" "net/qrtr" "net/6lowpan" "net/lapb" "net/phonet"
    "net/rfkill" "net/rose" "net/netrom" "net/caif" "net/mpls" "net/openvswitch" "net/vmw_vsock"
    "net/sctp" "net/sunrpc" "net/9p"
    # filesystems nothing here mounts (ext4, btrfs, overlay, erofs, vfat, squashfs, fuse stay; erofs + overlay are /etc, guest-image/disk-image.nix)
    "fs/xfs" "fs/zonefs" "fs/vboxsf" "fs/ufs" "fs/udf" "fs/ubifs" "fs/romfs" "fs/smb" "fs/nfs"
    "fs/nfsd" "fs/ceph" "fs/ocfs2" "fs/gfs2" "fs/afs" "fs/9p" "fs/orangefs" "fs/jfs" "fs/reiserfs"
    "fs/hfs" "fs/hfsplus" "fs/befs" "fs/bfs" "fs/efs" "fs/minix" "fs/sysv" "fs/qnx4" "fs/qnx6"
    "fs/omfs" "fs/adfs" "fs/affs" "fs/coda" "fs/cramfs" "fs/f2fs" "fs/jffs2" "fs/nilfs2"
    "fs/ntfs3" "fs/exfat" "fs/freevxfs" "fs/hpfs" "fs/lockd" "fs/dlm"
  ];
  pruned = pkgs.runCommand "${kernel.name}-modules-pruned" { nativeBuildInputs = [ pkgs.kmod ]; } ''
    mkdir -p $out
    # -L: the aggregate is symlinks into the kernel's store path; the copy must be real files, or
    # the prune would be deleting inside a read-only store path.
    cp -rL ${full}/lib $out/lib
    chmod -R u+w $out/lib
    for v in $out/lib/modules/*/; do
      for d in ${lib.concatStringsSep " " denied}; do
        rm -rf "$v/kernel/$d"
      done
    done
    # depmod here: the aggregation below is a buildEnv that re-runs depmod only when the version
    # directory is writable, and with one input it is a read-only symlink into this store path
    # (left to it, the image ships no dependency tables and the initrd closure finds no
    # virtio_blk). Regenerated over the pruned set, the tables never name a file that is gone.
    for v in $out/lib/modules/*/; do
      find "$v" -maxdepth 1 -name "modules.*" ! -name "modules.builtin*" ! -name "modules.order*" -delete
      depmod -b $out -a "$(basename "$v")"
    done
  '';
in
{
  # The tree NixOS installs as /run/current-system/kernel-modules (its `apply` symlinks it; depmod ran above).
  system.modulesTree = lib.mkForce [ pruned ];
  # The initrd closure is built from that tree with allowMissing=false: a denied module named
  # here fails the image build. The virtio set and nothing else (virtio disks, NICs, a console
  # and an RNG; no display, 9p share, SATA or USB), plus the root filesystem, named because
  # mkForce also discards the entry NixOS adds for it (without it the pruned image reaches the
  # initrd and cannot mount /sysroot), and the three of the /etc overlay, which the etc module
  # adds the same way (disk-image.nix).
  boot.initrd.availableKernelModules = lib.mkForce [ "virtio_net" "virtio_pci" "virtio_mmio" "virtio_blk" "virtio_scsi" "ext4" "loop" "erofs" "overlay" ];
  boot.initrd.kernelModules = lib.mkForce [ "virtio_balloon" "virtio_console" "virtio_rng" ];
  # Loaded at boot by systemd-modules-load: DRBD (configuration.nix) and loop. The NixOS default
  # here, `atkbd` (a PS/2 keyboard driver, for a keyboard this guest does not have), would fail to
  # load from the pruned tree and fail the unit. `evdev` is loaded explicitly, not left to udev
  # modalias matching: it carries the ACPI power button, a clean stop of this guest is the whole
  # host shutdown contract, and a shutdown path must not depend on a device-matching rule firing.
  boot.kernelModules = lib.mkForce [ "drbd" "loop" "evdev" ];
}
