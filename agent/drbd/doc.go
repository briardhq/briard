// Package drbd generates the drbd-reactor config and ordered units and
// reads their status into NodeStatus. It never drives the failover lifecycle —
// drbd-reactor inside the VM does.
package drbd
