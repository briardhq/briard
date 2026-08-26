// Package platform holds the per-OS backends behind a common interface.
//
// The seam is cut by build tag, and Linux is the only backend that exists: unit_linux.go is the
// service manager (start a detached named transient unit, inspect it, stop it) and route_linux.go
// is iproute2. Their _windows.go counterparts return errors.ErrUnsupported.
//
// It takes TWO assertions to keep that true, because they catch different mistakes.
// `GOOS=windows go build ./...` catches a Linux-only SYMBOL (syscall.Statfs and its like); it
// cannot catch exec.Command("systemctl", ...), which compiles everywhere and fails only when
// someone runs it. That second half is seam_test.go, which reads string literals out of this
// package's shared files and fails on the mechanism names.
//
// Everything else here is deliberately OS-neutral and stays that way: the argv renderers
// (qemuArgs, launchArgs, routeReplaceArgs), the unit-state policy (what a reading MEANS and how
// long to wait for a name), the QMP protocol, snapshot.go and overlay.go -- the last two because
// qemu-img is the same program everywhere. So is the AF_UNIX control transport: Go supports unix
// sockets on Windows, so whether QEMU's own unix chardevs work there is a question about QEMU,
// not about this package.
//
// What a Windows backend owes -- including the three systemd features the SCM has no equivalent
// for -- is [V3b.27](c)'s paper pass, so it is decided once rather than re-derived here.
package platform
