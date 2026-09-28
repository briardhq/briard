package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"briard.io/agent/install"
)

// Version is this binary's release id. The stamp is agent/host's (buildVersion, set by -ldflags),
// and this package does not link agent/host, so main() copies it here before the CLI runs. Empty
// in a binary built without the stamp.
var Version string

// guestReleaseRecord is the node's record of the VM release it runs ([B.86d]). It mirrors
// ConfigFromEnv's GUEST_RELEASE_CACHE default (agent/host/config.go); a test asserts the two agree.
const guestReleaseRecord = "/var/lib/briard/guest-release.json"

// runVersion is `briard version`: which briard this is, and which VM it runs apps in -- the first
// question on every bug report. It reads no socket, so it answers with the agent down.
func runVersion(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard version: takes no arguments\n")
		return 2
	}
	fmt.Fprint(stdout, versionLines(guestReleaseRecord))
	return 0
}

// versionLines is the two lines `version` prints and `doctor` heads its report with. The VM half
// is the node's own record, so a machine with none -- not installed, or its state dir unreadable
// -- says which file it could not read rather than printing a blank.
func versionLines(record string) string {
	briard := Version
	if briard == "" {
		briard = "development build (no version stamped)"
	}
	vm := "unknown"
	if m, err := install.ReadManifest(record); err != nil {
		vm = fmt.Sprintf("unknown (%v)", err)
	} else if m.Version != "" {
		vm = m.Version
	}
	return fmt.Sprintf("briard %s\nvm     %s\n", briard, vm)
}
