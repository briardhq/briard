package host

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LOCAL TIMEZONE DETECTION -- what the node tells the cloud at registration so
// its home can be given a local-time update window.
//
// It reads the HOST's configuration, which is the right place for it twice over: the host is
// where a human installed the machine and set its clock ([[logic-on-host-by-default]]), and the
// guest is a NixOS appliance whose zone is ours, not the household's.
//
// Go's own time.Local is deliberately not used. It resolves to a *zone offset* -- Local.String()
// is "Local" and a formatted time carries an abbreviation like "EEST" -- and neither is an IANA
// name. Abbreviations are ambiguous (IST is Ireland, Israel and India) and an offset is not a
// zone at all: it cannot say when the clocks change, which is the entire question the window
// predicate asks (the cloud-side window predicate).

// localTimezone returns the machine's IANA zone name, or "" if it cannot be established. root is
// the filesystem root, so this is testable without touching the machine's real configuration.
//
// The three sources, in order of how much they mean:
//
//  1. TZ, which overrides everything else for this process by definition.
//  2. /etc/localtime's symlink target -- the systemd/timedatectl convention and what every
//     distro we install on actually sets. The name is the tail after "zoneinfo/".
//  3. /etc/timezone -- Debian's plain-text answer, kept as a fallback because a machine that was
//     configured before systemd (or by a minimal image) may have it and nothing else.
//
// A source that names a zone we cannot load is skipped rather than passed on: a zone the cloud
// cannot resolve is a window that never opens, which there is indistinguishable from a home that
// reported nothing at all -- so it is better to report nothing honestly and let that one fault
// have one meaning.
//
// A machine that says UTC is reported as UTC. It is tempting to read that as "unconfigured" and
// answer "" instead, but a headless install genuinely on UTC is not a fault, and inventing that
// distinction would trade a schedule the operator can correct for a home that is never scheduled
// at all.
func localTimezone(root string) string {
	for _, name := range []string{os.Getenv("TZ"), zoneFromLocaltime(root), zoneFromEtcTimezone(root)} {
		if name == "" {
			continue
		}
		if _, err := time.LoadLocation(name); err != nil {
			continue
		}
		return name
	}
	return ""
}

// zoneFromLocaltime reads /etc/localtime's symlink and returns the part after "zoneinfo/".
func zoneFromLocaltime(root string) string {
	target, err := os.Readlink(filepath.Join(root, "etc", "localtime"))
	if err != nil {
		return "" // a regular file (a copied zone blob) carries no name -- fall through
	}
	target = filepath.ToSlash(target)
	i := strings.LastIndex(target, "zoneinfo/")
	if i < 0 {
		return ""
	}
	return strings.TrimPrefix(target[i+len("zoneinfo/"):], "posix/")
}

// zoneFromEtcTimezone reads Debian's /etc/timezone, one line holding the zone name.
func zoneFromEtcTimezone(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "etc", "timezone"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
