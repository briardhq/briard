// Package drbd renders a resource's `.res` and its drbd-reactor promoter snippet,
// names the storage seam's devices, and parses `drbdsetup status` into the node's
// quorum view. It never drives the failover lifecycle — drbd-reactor inside the VM does.
package drbd
