// Package platform holds the per-OS backends behind a common interface.
//
// The seam is cut by build tag, two arms each: unit_linux.go is systemd (start a detached named
// transient unit, inspect it, stop it), unit_windows.go a named Job Object found again by name;
// route_linux.go is iproute2 and route_windows.go a stub, since the private link it routes over
// does not exist on the bridge substrate Windows uses; alloc_*.go reserve a disk's bytes;
// owner_*.go say who a file belongs to.
//
// Two assertions keep the cut honest, because they catch different mistakes. `GOOS=windows go
// build ./...` catches a Linux-only symbol (syscall.Statfs and its like), not
// exec.Command("systemctl", ...), which compiles everywhere and fails only when run. The other
// half is internal/arch's TestHostOSCallsStayBehindTheSeam, which reads string literals out of
// every shared file of the per-OS packages and fails on the mechanism names.
//
// Everything else here is deliberately OS-neutral and stays that way: the argv renderers
// (qemuArgs, launchArgs, routeReplaceArgs), the unit-state policy (what a reading means and how
// long to wait for a name), the QMP protocol, the one rendering of a transient unit that the
// Windows arm reads back (transient.go), and the AF_UNIX control transport (Go supports unix
// sockets on Windows, and so does the shipped qemu).
package platform
