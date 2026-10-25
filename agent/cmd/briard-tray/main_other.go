//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The tray is Windows' alone; on Linux the CLI is the whole host console.
func main() {
	fmt.Fprintln(os.Stderr, "briard-tray runs on Windows only")
	os.Exit(1)
}
