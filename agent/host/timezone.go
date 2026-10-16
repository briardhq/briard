package host

import "context"

// LOCAL TIMEZONE DETECTION -- what the node tells the cloud at registration so
// its home can be given a local-time update window.
//
// It reads the HOST's configuration, which is the right place for it twice over: the host is
// where a human installed the machine and set its clock (logic lives on the host by default), and the
// guest is a NixOS appliance whose zone is ours, not the household's.
//
// Go's own time.Local is deliberately not used. It resolves to a *zone offset* -- Local.String()
// is "Local" and a formatted time carries an abbreviation like "EEST" -- and neither is an IANA
// name. Abbreviations are ambiguous (IST is Ireland, Israel and India) and an offset is not a
// zone at all: it cannot say when the clocks change, which is the entire question the window
// predicate asks (the cloud-side window predicate).
//
// Each OS keeps its zone somewhere else, so localTimezone has one arm per OS.

// timezoneSetter is the slice of the guest the timezone push costs.
type timezoneSetter interface {
	SetTimezone(ctx context.Context, zone string) error
	SupportsTimezone() bool
}

// pushTimezone hands the guest the household's zone at bring-up, a node-scoped fact the host holds
// and the guest keeps only for the boot it was pushed into.
//
// IT WARNS AND NEVER FAILS: a guest on UTC shows a household its History in the wrong hours, which
// is worth a log line and not worth a node that will not come up. A host naming no zone, and a
// guest too old to be told one, leave the guest on UTC the same way.
func pushTimezone(ctx context.Context, g timezoneSetter, zone string, logf func(string, ...any)) {
	switch {
	case zone == "":
		logf("timezone: this host names no timezone; the guest stays on UTC")
	case !g.SupportsTimezone():
		logf("timezone: this guest cannot be told its timezone (%s); it stays on UTC", zone)
	default:
		if err := g.SetTimezone(ctx, zone); err != nil {
			logf("timezone: %v; the guest stays on UTC", err)
			return
		}
		logf("timezone: the guest runs on %s", zone)
	}
}
