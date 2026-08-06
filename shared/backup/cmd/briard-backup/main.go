// Command briard-backup is a thin CLI over shared/backup: seal a home's sacred config
// to an encrypted blob, restore it, or mint a household keypair. The guest agent's
// backup.save/backup.restore verbs run the same shared/backup code in the product; this
// CLI exercises it standalone — for the nixosTest (which can't run the virtio-serial
// Manager in a lib.nix rig) and as an ops/escrow tool.
//
// Usage:
//
//	briard-backup keygen
//	    → prints "recipient: age1…\nidentity: AGE-SECRET-KEY-…"
//	briard-backup save --base DIR --dest FILE --recipient age1… [--include .storage ...]
//	briard-backup restore --base DIR --src FILE --identity-file FILE
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"briard.io/shared/backup"
)

// includeFlag collects repeated --include values (default: .storage + configuration.yaml).
type includeFlag []string

func (i *includeFlag) String() string     { return strings.Join(*i, ",") }
func (i *includeFlag) Set(v string) error { *i = append(*i, v); return nil }

func main() {
	if len(os.Args) < 2 {
		fatal("usage: briard-backup <keygen|save|restore> [flags]")
	}
	switch os.Args[1] {
	case "keygen":
		keygen()
	case "save":
		save(os.Args[2:])
	case "restore":
		restore(os.Args[2:])
	default:
		fatal("unknown subcommand %q (want keygen|save|restore)", os.Args[1])
	}
}

func keygen() {
	key, err := backup.GenerateKey()
	if err != nil {
		fatal("keygen: %v", err)
	}
	fmt.Printf("recipient: %s\nidentity: %s\n", key.Recipient, key.Identity)
}

func save(args []string) {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	base := fs.String("base", "", "root directory the includes are relative to")
	dest := fs.String("dest", "", "path to write the encrypted blob")
	recipient := fs.String("recipient", "", "age public recipient (age1…)")
	var includes includeFlag
	fs.Var(&includes, "include", "path under --base to back up (repeatable)")
	_ = fs.Parse(args)
	if *base == "" || *dest == "" || *recipient == "" {
		fatal("save: --base, --dest and --recipient are required")
	}
	if len(includes) == 0 {
		includes = includeFlag{".storage", "configuration.yaml"}
	}
	recip, err := backup.ParseRecipient(*recipient)
	if err != nil {
		fatal("save: %v", err)
	}
	f, err := os.Create(*dest)
	if err != nil {
		fatal("save: %v", err)
	}
	if err := backup.Save(*base, includes, recip, f); err != nil {
		f.Close()
		fatal("save: %v", err)
	}
	if err := f.Close(); err != nil {
		fatal("save: %v", err)
	}
	fmt.Printf("saved %s -> %s\n", includes.String(), *dest)
}

func restore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	base := fs.String("base", "", "root directory to extract into")
	src := fs.String("src", "", "encrypted blob to restore from")
	identityFile := fs.String("identity-file", "", "file holding the age identity (AGE-SECRET-KEY-…)")
	_ = fs.Parse(args)
	if *base == "" || *src == "" || *identityFile == "" {
		fatal("restore: --base, --src and --identity-file are required")
	}
	idBytes, err := os.ReadFile(*identityFile)
	if err != nil {
		fatal("restore: %v", err)
	}
	id, err := backup.ParseIdentity(strings.TrimSpace(string(idBytes)))
	if err != nil {
		fatal("restore: %v", err)
	}
	f, err := os.Open(*src)
	if err != nil {
		fatal("restore: %v", err)
	}
	defer f.Close()
	if err := backup.Load(f, id, *base); err != nil {
		fatal("restore: %v", err)
	}
	fmt.Printf("restored %s -> %s\n", *src, *base)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
