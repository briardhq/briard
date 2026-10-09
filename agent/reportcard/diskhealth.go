package reportcard

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// smartctlBin is smartmontools' smartctl, shipped inside the qemu bundle so it runs on any host
// without a distro package; the bundle's prefix is a constant (installRoot).
const smartctlBin = "/opt/briard/qemu/bin/smartctl"

// smartBudget bounds one disk's read. A disk behind a slow bridge can sit in a command timeout
// for longer, which is why the agent reads in the background (smartAlerter) and never on its loop.
const smartBudget = 30 * time.Second

// SMART is one disk's own health report, reduced to the readings worth acting on. A disk keeps
// hundreds of counters, and most are vendor-specific or noise; these few mean the same on every
// drive and rarely fire on a healthy one. Their silence proves less than their word: many disks
// die without first recording anything here, so a clean report reads "no warnings", never
// "healthy".
type SMART struct {
	Device string // e.g. /dev/nvme0n1
	// Read is whether the disk gave a verdict. False is "could not tell" -- no SMART through this
	// connection, a disk asleep (never woken to ask), no smartctl -- and is never an all-clear.
	Read bool
	// Failing is the disk's own verdict of FAILED, or an NVMe critical warning: worn past its
	// spare blocks, too hot, reliability degraded, or gone read-only.
	Failing bool
	// Errors counts the bad sectors and media errors the disk has recorded: ATA reallocated (5),
	// reported uncorrectable (187), pending (197) and offline uncorrectable (198); NVMe media
	// errors. Some disks run for years at a small steady count; a GROWING one is the warning.
	Errors int64
	Why    string // what failed, or why it could not be read
}

// ReadSMART asks the disk under each device path for its health, one at a time.
func ReadSMART(ctx context.Context, devs []string) []SMART {
	var out []SMART
	for _, d := range devs {
		out = append(out, readSMART(ctx, d))
	}
	return out
}

func readSMART(ctx context.Context, dev string) SMART {
	ctx, cancel := context.WithTimeout(ctx, smartBudget)
	defer cancel()
	// -n standby: a sleeping disk is not spun up to be asked; it reads as unknown. Exit status is
	// a bitmask that is non-zero for a failing disk too, so the JSON is read whatever it is.
	raw, err := exec.CommandContext(ctx, smartctlBin, "-j", "-H", "-A", "-n", "standby", dev).Output()
	if len(raw) == 0 && err != nil {
		return SMART{Device: dev, Why: fmt.Sprintf("smartctl: %v", err)}
	}
	return parseSMART(dev, raw)
}

// smartctlJSON is the part of `smartctl -j -H -A` parseSMART reads.
type smartctlJSON struct {
	Smartctl struct {
		Messages []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	NVMe *struct {
		CriticalWarning int   `json:"critical_warning"`
		MediaErrors     int64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
	ATA *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

// nvmeWarnings names the NVMe critical-warning bits, lowest first.
var nvmeWarnings = []string{"spare blocks below threshold", "temperature out of range", "reliability degraded",
	"read-only", "volatile memory backup failed"}

// parseSMART reads smartctl's JSON. No verdict in it is "could not tell", with smartctl's own
// message as the reason.
func parseSMART(dev string, raw []byte) SMART {
	s := SMART{Device: dev}
	var j smartctlJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		s.Why = fmt.Sprintf("smartctl answered something that is not JSON: %v", err)
		return s
	}
	if j.SmartStatus == nil {
		s.Why = "no health report through this connection"
		if len(j.Smartctl.Messages) > 0 {
			s.Why = j.Smartctl.Messages[0].String
		}
		return s
	}
	s.Read = true
	var why []string
	if !j.SmartStatus.Passed {
		s.Failing = true
		why = append(why, "its own health check says FAILED")
	}
	if j.NVMe != nil {
		s.Errors = j.NVMe.MediaErrors
		for i, w := range nvmeWarnings {
			if j.NVMe.CriticalWarning&(1<<i) != 0 {
				s.Failing = true
				why = append(why, w)
			}
		}
	}
	if j.ATA != nil {
		for _, a := range j.ATA.Table {
			switch a.ID {
			case 5, 187, 197, 198:
				// The low 16 bits: some vendors pack more into the upper bytes of these raw values.
				s.Errors += a.Raw.Value & 0xffff
			}
		}
	}
	s.Why = strings.Join(why, ", ")
	return s
}

// SMARTCheck judges one disk for the doctor. An unreadable disk is a warning, like every other
// fact this package could not read -- not a pass.
func SMARTCheck(s SMART) Check {
	c := Check{Name: "disk-health"}
	switch {
	case !s.Read:
		c.Status, c.Detail = Warn, fmt.Sprintf("could not read %s's health report: %s", s.Device, s.Why)
		c.Fix = "nothing to fix; Briard cannot warn you before this disk fails"
	case s.Failing:
		c.Status, c.Detail = Refuse, fmt.Sprintf("%s reports it is failing: %s", s.Device, s.Why)
		c.Fix = "replace this disk soon -- your data lives on it"
	case s.Errors > 0:
		c.Status, c.Detail = Warn, fmt.Sprintf("%s has recorded %d bad sectors or media errors", s.Device, s.Errors)
		c.Fix = "nothing to do while the count holds; Briard alerts if it grows"
	default:
		c.Status, c.Detail = Pass, fmt.Sprintf("%s: no warnings in its health report", s.Device)
	}
	return c
}
