package host

import "errors"

// No FIFOs on Windows, so the hung-path test skips rather than lying. Keeping the file compiling
// under GOOS=windows is the point: a test that does not build is a seam that is not cut.
func mkFifo(string) error { return errors.ErrUnsupported }
