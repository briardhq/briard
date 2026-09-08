# THE KERNEL MODULE TREE, DENYLISTED ([B.136]).
#
# The stock module tree was 149 MB on disk and -- because modules ship as .ko.xz and compress no
# further -- 141 MB of the 358 MB image a household downloads (39%): 7296 modules, of which a
# qemu/virtio guest can load perhaps a hundred. Trimming the KERNEL was evaluated and declined
# (slim.nix: it makes us a kernel distributor). This does the cheap thing instead: the kernel is
# the upstream store path, byte for byte, and only the module TREE is copied without the families
# a virtio guest has no hardware for -- sound cards, GPUs, USB, sensors, physical NICs and radios,
# and the protocol stacks and filesystems nothing in the product speaks or mounts. What remains
# is every module that could conceivably matter: all of virtio, all of netfilter and bridging
# (podman's networking loads these on demand), the block and md layers, the filesystems we use,
# crypto, and DRBD's out-of-tree module. Measured 2026-09-08: 149 -> 35 MB on disk, 141 -> 32 MB
# compressed, 7296 -> 1724 modules.
#
# A DENYLIST, NOT AN ALLOWLIST, and the difference is where a mistake surfaces. An allowlist guesses
# what loads on demand and a wrong guess fails weeks later at the feature that first needs the
# module. A denylist removes only what cannot bind to this machine at all, so the failure it can
# produce is a `modprobe` refusing loudly in the journal -- and the initrd's own module closure is
# built with allowMissing=false, so a denied module the initrd asks for fails the BUILD, not a
# boot. That is why the initrd's lists are pinned below to the virtio set: the qemu-guest profile
# asks for 9p/virtiofs (denied) and virtio_gpu (drivers/gpu, denied -- this guest has a serial
# console and no display), and NixOS's default list asks for every SATA, USB and HID driver.
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
    # drivers/input: THE SUBDIRECTORIES ONLY, and this is the one denial that has already bitten.
    # `evdev` (CONFIG_INPUT_EVDEV=m) sits at the top of this family, and it is what turns the ACPI
    # power button into a /dev/input/event* node -- the only thing logind watches to shut a
    # machine down on that button. Denying the family wholesale left the guest deaf to it: the
    # host pressed the button, waited its minute and killed the VM, so every clean stop became a
    # hard one (agent-readopt, the nightly of 2026-09-08, the night this denylist landed). The
    # devices themselves -- keyboards, mice, touchscreens -- this guest still has none of.
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
    # filesystems nothing here mounts (ext4, btrfs, overlay, vfat, squashfs, fuse stay)
    "fs/xfs" "fs/zonefs" "fs/vboxsf" "fs/ufs" "fs/udf" "fs/ubifs" "fs/romfs" "fs/smb" "fs/nfs"
    "fs/nfsd" "fs/ceph" "fs/ocfs2" "fs/gfs2" "fs/afs" "fs/9p" "fs/orangefs" "fs/jfs" "fs/reiserfs"
    "fs/hfs" "fs/hfsplus" "fs/befs" "fs/bfs" "fs/efs" "fs/minix" "fs/sysv" "fs/qnx4" "fs/qnx6"
    "fs/omfs" "fs/adfs" "fs/affs" "fs/coda" "fs/cramfs" "fs/erofs" "fs/f2fs" "fs/jffs2" "fs/nilfs2"
    "fs/ntfs3" "fs/exfat" "fs/freevxfs" "fs/hpfs" "fs/lockd" "fs/dlm"
  ];
  pruned = pkgs.runCommand "${kernel.name}-modules-pruned" { nativeBuildInputs = [ pkgs.kmod ]; } ''
    mkdir -p $out
    # -L: the aggregate is symlinks into the kernel's store path; the copy must be real files, or
    # the prune would be deleting inside a read-only store path (it was, the first time).
    cp -rL ${full}/lib $out/lib
    chmod -R u+w $out/lib
    for v in $out/lib/modules/*/; do
      for d in ${lib.concatStringsSep " " denied}; do
        rm -rf "$v/kernel/$d"
      done
    done
    # depmod HERE. The aggregation below is a buildEnv that only re-runs depmod when the version
    # directory is writable -- with one input it is a symlink into this store path and is not
    # (measured: the second attempt shipped no dependency tables at all, and the initrd's closure
    # found no virtio_blk). So the tables are regenerated over the pruned set right here, and
    # never name a file that is gone.
    for v in $out/lib/modules/*/; do
      find "$v" -maxdepth 1 -name "modules.*" ! -name "modules.builtin*" ! -name "modules.order*" -delete
      depmod -b $out -a "$(basename "$v")"
    done
  '';
in
{
  # The tree NixOS installs as /run/current-system/kernel-modules (its `apply` symlinks it; depmod ran above).
  system.modulesTree = lib.mkForce [ pruned ];
  # The initrd's closure is built from that tree with allowMissing=false: a denied module named
  # here fails the image build. The virtio set, and nothing else: this guest has virtio disks,
  # NICs, a console and an RNG, no display, no 9p share, no SATA, no USB.
  # ...plus the root filesystem, named here because mkForce also discards the entry NixOS adds for
  # it (measured: the first pruned image reached the initrd and could not mount /sysroot).
  boot.initrd.availableKernelModules = lib.mkForce [ "virtio_net" "virtio_pci" "virtio_mmio" "virtio_blk" "virtio_scsi" "ext4" ];
  boot.initrd.kernelModules = lib.mkForce [ "virtio_balloon" "virtio_console" "virtio_rng" ];
  # Loaded at boot by systemd-modules-load: DRBD (configuration.nix) and loop; NixOS's default
  # `atkbd` (a PS/2 keyboard driver) would now fail to load and fail the unit.
  # `evdev` is loaded EXPLICITLY rather than left to udev's modalias matching: it is what carries
  # the ACPI power button, and a clean stop of this guest is the host's whole shutdown contract
  # ([B.51], [B.127], [B.132]). A shutdown path must not depend on a device-matching rule firing.
  # NixOS's own default here is `atkbd`, for a keyboard this guest does not have.
  boot.kernelModules = lib.mkForce [ "drbd" "loop" "evdev" ];
}
