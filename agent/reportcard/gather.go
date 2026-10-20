package reportcard

import "os"

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DiskFreeMB is the free space on the filesystem holding path, in MB; 0 when it cannot be read.
// The host agent checks the same number before it grows the guest's state disk.
func DiskFreeMB(path string) int { return diskFreeMB(path) }

// The any-ethernet / wired-ethernet survey used to live here, and the NIC selection retired it: it asked
// whether the MACHINE had a wired device, not whether the device the install would actually use
// was one. A laptop with an unplugged eth0 and the default route on wlan0 passed it, and then
// macvtapped onto the wireless station. The question is now asked of nic.Choose's answer.
