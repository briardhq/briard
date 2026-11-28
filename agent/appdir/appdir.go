// Package appdir is what every app package does to the directory the product keeps for its app
// on the guest: install a file in it, and keep a secret in it for the boot.
//
// The directory is under /run, so every boot re-derives it, and a container may start while it
// is being written. It is a package of its own, below the registry (agent/apps), because the
// registry imports every app package and they cannot import it back.
package appdir

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Executor is the guest-side command runner Write needs.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
}

// Write installs content at path: written beside, given its owner and mode, and renamed over,
// so a container that starts mid-Prepare never reads or binds half a file. owner is a
// `uid:gid` for an app whose image drops to a user of its own, "" for the writer's.
func Write(ctx context.Context, x Executor, path string, content []byte, owner, mode string) error {
	tmp := path + ".new"
	if err := x.WriteFile(tmp, content); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	steps := [][]string{{"chmod", mode, tmp}, {"mv", "-f", tmp, path}}
	if owner != "" {
		steps = append([][]string{{"chown", owner, tmp}}, steps...)
	}
	for _, args := range steps {
		if out, err := x.Run(ctx, args[0], args[1:]...); err != nil {
			return fmt.Errorf("%s %s: %w: %s", args[0], path, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Secret is the boot's secret kept at path, n random bytes in hex: the one already written
// there, or a new one, which the caller then writes (fresh). Kept, never minted anew at each
// converge: converge restarts only the apps whose units changed, so a new value would leave a
// running app comparing against the old one. /run ends it with the boot.
func Secret(x interface {
	ReadFile(path string) ([]byte, error)
}, path string, n int) (secret string, fresh bool, err error) {
	if raw, err := x.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(raw)); len(k) == 2*n {
			return k, false, nil
		}
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("mint the secret for %s: %w", path, err)
	}
	return hex.EncodeToString(buf), true, nil
}
