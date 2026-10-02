package guestagent

import (
	"context"
	"fmt"
	"regexp"
)

// zoneinfoDir is where the image keeps the timezone database: NixOS links tzdata there whether or
// not a zone is configured. localtimePath is the link every reader of the local zone follows.
const (
	zoneinfoDir   = "/etc/zoneinfo"
	localtimePath = "/etc/localtime"
)

// zoneName is an IANA zone name as a path under zoneinfoDir: segments of letters, digits, `_`,
// `+` and `-`. It refuses `..`, an absolute path and anything a shell would read, before anything
// is run.
var zoneName = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

type timezoneRequest struct {
	Zone string `json:"zone"`
}

// setTimezone points /etc/localtime at the zone the host named.
//
// THE ZONE MUST EXIST IN THE IMAGE'S DATABASE, asked of the file rather than assumed: a link to
// nothing reads as UTC everywhere, which is the state this is meant to end, and an error says so.
//
// REPLACED, NEVER EDITED: the new link is made beside the old one and renamed over it, so a reader
// sees one zone or the other.
func setTimezone(ctx context.Context, x Executor, run func(string, ...string) error, zone string) error {
	if len(zone) > 64 || !zoneName.MatchString(zone) {
		return fmt.Errorf("%s: %q is not a timezone name", verbSetTimezone, zone)
	}
	target := zoneinfoDir + "/" + zone
	if _, err := x.Run(ctx, "test", "-f", target); err != nil {
		return fmt.Errorf("%s: %s is not in this image's timezone database", verbSetTimezone, zone)
	}
	tmp := localtimePath + ".briard"
	if err := run("ln", "-sfn", target, tmp); err != nil {
		return err
	}
	return run("mv", "-Tf", tmp, localtimePath)
}

// SetTimezone hands the guest the household's timezone (verbSetTimezone).
func (g *Client) SetTimezone(ctx context.Context, zone string) error {
	return g.c.Call(ctx, verbSetTimezone, timezoneRequest{Zone: zone}, nil)
}

// SupportsTimezone reports whether this guest can be told its timezone.
func (g *Client) SupportsTimezone() bool { return g.Supports(verbSetTimezone) }
