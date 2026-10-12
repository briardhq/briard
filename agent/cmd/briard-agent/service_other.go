//go:build !windows

package main

// runService is false off Windows: systemd runs the agent as an ordinary process.
func runService() bool { return false }
