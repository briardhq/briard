package guestagent

import "errors"

// The guest is Linux forever -- nothing in this package ever runs on Windows. It is compiled
// there only because the host packages import it for the wire types, so the one syscall it makes
// needs a stub rather than an exemption.
func setHostname(string) error { return errors.ErrUnsupported }
