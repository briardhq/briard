package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"briard.io/shared/api"
)

// registerTimeout bounds the boot-time Register so a slow or unreachable cloud never
// stalls the node's boot -- identity is the *source* from the cloud but never a runtime
// dependency for boot (degrade-to-local).
const registerTimeout = 5 * time.Second

// LoadAssignment reads the cached Assignment from path. ok is false when the file is
// absent (the never-registered case); a malformed file is a real error.
func LoadAssignment(path string) (a api.Assignment, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return api.Assignment{}, false, nil
	}
	if err != nil {
		return api.Assignment{}, false, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return api.Assignment{}, false, fmt.Errorf("assignment cache %s: %w", path, err)
	}
	return a, true, nil
}

// SaveAssignment writes a to path atomically (temp file + rename), so a crash mid-write
// never leaves a torn cache on the cold-boot path -- simplicity here is a reliability
// property.
func SaveAssignment(path string, a api.Assignment) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Resolve determines this node's Assignment at boot. It registers with the cloud and
// caches the result; if the cloud is unreachable it cold-boots from the cache
// (degrade-to-local -- the cloud is the source of identity, never a runtime dependency
// for boot). With neither available (a first boot during an outage) it returns a local
// default so the node still boots, its tenant unresolved until it can register.
//
// A nil client (standalone) or an empty cachePath (no persistence) each just skip that
// step. Register is bounded so a wedged cloud can't stall boot.
func Resolve(ctx context.Context, client CloudClient, cachePath string, info api.NodeInfo, logf func(string, ...any)) api.Assignment {
	if client != nil {
		rctx, cancel := context.WithTimeout(ctx, registerTimeout)
		a, err := client.Register(rctx, info)
		cancel()
		if err == nil {
			if cachePath != "" {
				if err := SaveAssignment(cachePath, a); err != nil {
					logf("assignment cache write failed: %v", err) // non-fatal
				}
			}
			logf("registered with cloud: tenant=%s role=%s", a.Tenant, a.Role)
			return a
		}
		logf("cloud register failed (%v); cold-booting identity from cache", err)
	}
	if cachePath != "" {
		switch a, ok, err := LoadAssignment(cachePath); {
		case err != nil:
			logf("assignment cache read failed: %v", err)
		case ok:
			logf("cold-booted identity from cache: tenant=%s role=%s", a.Tenant, a.Role)
			return a
		}
	}
	logf("no cloud, no cache: booting with unresolved tenant (role=%s)", info.Role)
	return api.Assignment{Role: info.Role}
}
