package platform

import "errors"

// ChownLike does nothing on Windows: a file in a person's folder is theirs through the folder's
// inherited ACL (a profile gives its owner Full Control on everything under it, whoever wrote it),
// so there is no owner to copy.
func ChownLike(dir, path string) error { return nil }

// OwnerUID has no Windows backend: ownership does not carry the Linux meaning there (ChownLike).
func OwnerUID(path string) (int, error) { return 0, errors.ErrUnsupported }
