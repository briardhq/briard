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

// SaveAssignment writes a to path atomically AND durably (temp file + fsync + rename +
// directory fsync), so a crash mid-write never leaves a torn cache on the cold-boot path --
// simplicity here is a reliability property.
//
// The fsync is not belt-and-braces. This file exists for exactly one event: booting when the
// cloud cannot be reached. Atomic-but-unflushed means the write returns, the caller believes the
// identity is recorded, and a power cut any time in the next commit interval takes it -- so the
// one boot the cache is for is the boot that finds it missing. That is [V3.23]'s defect verbatim,
// and the reason the directory is fsynced too is [V3.23]'s as well: on a first registration the
// file is NEW, so the rename that publishes it lives only in the parent's dirent until the parent
// is flushed.
func SaveAssignment(path string, a api.Assignment) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return writeDurable(path, b, 0o644)
}

// writeDurable writes data to path atomically and durably: a temp file in the same directory,
// fsynced, renamed into place, and the directory fsynced so the rename itself survives.
//
// Deliberately NOT shared with agent/host's identical need (the service-manifest cache): two call
// sites in two packages is the rule of three's second instance, and a `shared/atomicfile` for it
// would be the abstraction the contract says to wait on. The third one moves it.
func writeDurable(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil { // durable BEFORE the rename publishes it
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir flushes a directory so a rename(2) into it is durable. Opening a directory read-only
// and fsyncing it is the portable spelling; a filesystem that refuses the open is reported rather
// than swallowed, since the caller's whole reason for being here is knowing the write landed.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
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
