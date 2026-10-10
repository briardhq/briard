package platform

import "errors"

// ChownLike has no Windows backend: what a file in a person's folder inherits there is an ACL
// question the design settles, not a uid to copy.
func ChownLike(dir, path string) error { return errors.ErrUnsupported }
