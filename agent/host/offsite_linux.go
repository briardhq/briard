package host

import "errors"

// The off-site check (backup.go) reads the Cloud Files API, which Linux has no counterpart of: no
// folder here is a sync client's in a way it can read, and the page says nothing about one.
func syncProvider(string) (string, error) { return "", errors.ErrUnsupported }

func inSync(string) error { return errors.ErrUnsupported }
