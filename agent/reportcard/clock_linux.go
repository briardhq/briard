package reportcard

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ClockRemedy is how a household turns time synchronisation on, said by the doctor and the
// agent's clock alert.
const ClockRemedy = "turn it on with `timedatectl set-ntp true`"

// NTPSynced is timedatectl's NTPSynchronized: "yes", "no", or "" when it could not be read (no
// timedatectl, or no answer in 5 s). The doctor and the agent's clock alert both read it here.
func NTPSynced(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
