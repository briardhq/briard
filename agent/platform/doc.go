// Package platform holds the per-OS backends behind a common interface.
//
// The seam is cut by build tag, two arms each. unit_linux.go is systemd (start a detached named
// transient unit, inspect it, stop it) and unit_windows.go is a named Job Object found again by
// name; route_linux.go is iproute2 and route_windows.go is a stub, since the private link it
// routes over does not exist on the bridge substrate Windows uses; alloc_*.go reserve a disk's
// bytes; owner_*.go say who a file belongs to.
//
// It takes TWO assertions to keep the cut honest, because they catch different mistakes.
// `GOOS=windows go build ./...` catches a Linux-only SYMBOL (syscall.Statfs and its like); it
// cannot catch exec.Command("systemctl", ...), which compiles everywhere and fails only when
// someone runs it. That second half is internal/arch's TestHostOSCallsStayBehindTheSeam, which
// reads string literals out of every shared file of the per-OS packages and fails on the
// mechanism names.
//
// Everything else here is deliberately OS-neutral and stays that way: the argv renderers
// (qemuArgs, launchArgs, routeReplaceArgs), the unit-state policy (what a reading MEANS and how
// long to wait for a name), the QMP protocol, and the one rendering of a transient unit that the
// Windows arm reads back (transient.go). So is the AF_UNIX control transport: Go supports unix
// sockets on Windows, and so does the shipped qemu.
package platform
