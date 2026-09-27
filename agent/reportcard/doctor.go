package reportcard

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// LiveFacts is what `briard doctor` reads off an INSTALLED host by itself ([V3c.3]) -- the half
// that still answers with the agent down, which is when someone is most likely to run it. The
// agent's half (agent/host, doctor.go) judges what only the agent can see.
type LiveFacts struct {
	// DiskFreeMB is free space where the install lives; 0 means "could not read".
	DiskFreeMB int
	// NTPSynced is timedatectl's NTPSynchronized: "yes", "no", or "" when it could not be read.
	NTPSynced string
}

// Live disk thresholds, lower than the install's: the guest's thin root disk grows into this
// space, so a host that fills up stops the guest underneath a running home. Below the install
// floor is worth a warning; under 2 GB the next image update will not fit.
const liveDiskFailMB = 2 * 1024

// AssessLive judges an installed host's own facts. Pure, like Assess.
func AssessLive(f LiveFacts) []Check {
	var cs []Check
	switch {
	case f.DiskFreeMB == 0:
		cs = append(cs, Check{"disk", Warn, "could not measure free space on this machine", ""})
	case f.DiskFreeMB < liveDiskFailMB:
		cs = append(cs, Check{"disk", Refuse, fmt.Sprintf("%d MB free on this machine", f.DiskFreeMB),
			"free some space: the guest's disk grows into it, and a full disk stops the guest"})
	case f.DiskFreeMB < diskFloorMB:
		cs = append(cs, Check{"disk", Warn, fmt.Sprintf("%d GB free on this machine (under %d GB)", f.DiskFreeMB/1024, diskFloorMB/1024),
			"free some space before installing more apps or updates"})
	default:
		cs = append(cs, Check{"disk", Pass, fmt.Sprintf("%d GB free on this machine", f.DiskFreeMB/1024), ""})
	}
	// THE CLOCK ([V3c.9]). A wrong clock turns a valid cert into a refusal and misdates every
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
	f := LiveFacts{DiskFreeMB: diskFreeMB(installRoot())}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value").Output(); err == nil {
		f.NTPSynced = strings.TrimSpace(string(out))
	}
	return f
}
