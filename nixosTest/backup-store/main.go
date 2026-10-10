// Command backup-store is a TEST HELPER (nixosTest/backup.nix), not a product binary.
//
// It serves the REAL backup store -- agent/host's, the one the host agent serves the guest on the
// private link -- with no agent around it, so an agent-less rig can back its guest up into a real
// folder through the code that ships.
//
//	backup-store <folder> <listen-ip> <client-ip>
package main

import (
	"context"
	"log"
	"os"

	"briard.io/agent/host"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: backup-store <folder> <listen-ip> <client-ip>")
	}
	host.ServeBackupStore(context.Background(), os.Args[1], os.Args[2], os.Args[3], log.Printf)
}
