//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The installer is Windows' alone; Linux installs with install.sh.
func main() {
	fmt.Fprintln(os.Stderr, "briard-setup runs on Windows only")
	os.Exit(1)
}
