package reportcard

// platformChecks are the Linux host's hard requirements: the hypervisor, the tap driver, the
// network tool and the init the install registers with.
func platformChecks(f HostFacts) []Check {
	var cs []Check
	// KVM -- the one thing we can't bundle (kernel side). Distinguish "virt disabled in BIOS"
	// (flags absent) from "module not loaded" (flags present, /dev/kvm absent) so the fix is exact.
	switch {
	case f.DevKVM:
		cs = append(cs, Check{"kvm", Pass, "/dev/kvm available", ""})
	case !f.VirtFlags:
		cs = append(cs, Check{"kvm", Refuse, "no CPU virtualization (vmx/svm) detected",
			"enable VT-x / AMD-V in the BIOS/UEFI (often labelled \"Virtualization\" or \"SVM Mode\"); if this CPU truly lacks it, it can't be a briard node"})
	default:
		cs = append(cs, Check{"kvm", Refuse, "CPU supports virtualization but /dev/kvm is absent",
			"load the KVM module: `sudo modprobe kvm_intel` (Intel) or `sudo modprobe kvm_amd` (AMD)"})
	}

	// TUN/TAP -- kernel side, but we modprobe it, so refuse only if the kernel genuinely lacks it.
	if f.DevNetTun || f.TunModule {
		cs = append(cs, Check{"tun", Pass, "TUN/TAP available", ""})
	} else {
		cs = append(cs, Check{"tun", Refuse, "no TUN/TAP driver (/dev/net/tun absent, tun module unavailable)",
			"this kernel lacks CONFIG_TUN; use a stock distro kernel (Debian/Ubuntu/Fedora/Arch all ship it)"})
	}

	// Iproute2 -- near-universal base; the installer can't conjure it, so refuse with the line.
	if f.HasIP {
		cs = append(cs, Check{"iproute2", Pass, "`ip` present", ""})
	} else {
		cs = append(cs, Check{"iproute2", Refuse, "`ip` (iproute2) not found on PATH",
			"install iproute2: `sudo apt install iproute2` (or your distro's equivalent)"})
	}

	// SYSTEMD -- and it is here, with the other hard requirements, because it is one.
	//
	// install.sh writes unit files and runs `systemctl daemon-reload`/`enable`/`start`; there is no
	// non-systemd path and none is planned (we do not own the host's init). It had a
	// guard already, but the LAST branch of the script: a box booted with OpenRC or runit passed
	// every gate, downloaded and unpacked ~400 MB into /opt/briard, created taps, formatted the
	// data volume, wrote units nothing would ever read -- and only then heard "no systemd". Every
	// other hard requirement refuses before anything is written; this is the one that did not.
	//
	// BOOTED, not merely installed. `systemctl` on PATH is the weaker question: a host with the
	// binary present but another init as PID 1 answers yes and then fails at daemon-reload, which
	// is exactly the late failure this check exists to move forward. /run/systemd/system is the
	// documented "systemd is running this machine" test, so that is what is asked.
	if f.SystemdBooted {
		cs = append(cs, Check{"systemd", Pass, "systemd is the init", ""})
	} else {
		cs = append(cs, Check{"systemd", Refuse, "systemd is not running this machine (/run/systemd/system absent)",
			"this install path targets systemd hosts -- briard registers itself as systemd units; on OpenRC/runit/s6 there is nothing for it to register with"})
	}
	return cs
}

// platformAdvisories are the warnings that depend on the substrate the card derived: the macvtap
// caveats, on a wired NIC that is not a bridge. Not on a wireless station either: its guest rides
// the station's own MAC (ipvtap), so neither caveat is about anything that happens there.
func platformAdvisories(f HostFacts) []Check {
	if f.NIC.Bridge || f.NIC.Wireless {
		return nil
	}
	return MacvtapAdvisories(f)
}
