package reportcard

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// LiveFacts is what `briard doctor` reads off an INSTALLED host by itself -- the half
// that still answers with the agent down, which is when someone is most likely to run it. The
// agent's half (agent/host, doctor.go) judges what only the agent can see.
type LiveFacts struct {
	// DiskFreeMB is free space where the install lives; 0 means "could not read".
	DiskFreeMB int
	// NTPSynced is timedatectl's NTPSynchronized: "yes", "no", or "" when it could not be read.
	NTPSynced string
}

// The live disk threshold: a running node's volumes are already paid for, so what it needs free is
// what any computer keeps for its own system -- which is also where an update stages the next
// image beside the running one, and what the agent keeps when it grows the guest's disk.
const liveDiskFailMB = HostDiskReserveMB

// AssessLive judges an installed host's own facts. Pure, like Assess.
func AssessLive(f LiveFacts) []Check {
	var cs []Check
	switch {
	case f.DiskFreeMB == 0:
		cs = append(cs, Check{"disk", Warn, "could not measure free space on this machine", ""})
	case f.DiskFreeMB < liveDiskFailMB:
		cs = append(cs, Check{"disk", Refuse, fmt.Sprintf("%d MB free on this machine", f.DiskFreeMB),
			fmt.Sprintf("free some space: keep about %s free for this computer's own system; updates and new apps are refused without it", gbOf(HostDiskReserveMB))})
	default:
		cs = append(cs, Check{"disk", Pass, fmt.Sprintf("%s free on this machine", gbOf(f.DiskFreeMB)), ""})
	}
	// THE CLOCK. A wrong clock turns a valid cert into a refusal and misdates every
	// alert and backup, and an RTC-less board boots with whatever time it last saved.
	switch f.NTPSynced {
	case "yes":
		cs = append(cs, Check{"clock", Pass, "synchronised with a time server", ""})
	case "no":
		cs = append(cs, Check{"clock", Warn, "not synchronised with a time server",
			"turn it on with `timedatectl set-ntp true`; certificates, alerts and backups are dated by this clock"})
	default:
		cs = append(cs, Check{"clock", Warn, "could not read whether the clock is synchronised (timedatectl)", ""})
	}
	return cs
}

// GatherLive reads LiveFacts. Best-effort: an unreadable fact reads as "could not tell", which
// AssessLive says out loud rather than passing.
func GatherLive(ctx context.Context) LiveFacts {
	return LiveFacts{DiskFreeMB: diskFreeMB(installRoot()), NTPSynced: NTPSynced(ctx)}
}

// NTPSynced is timedatectl's NTPSynchronized: "yes", "no", or "" when it could not be read (no
// timedatectl, as on Windows, or no answer in 5 s). The doctor and the agent's clock alert
// both read it here.
func NTPSynced(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
