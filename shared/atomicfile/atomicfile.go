// Package atomicfile writes a file atomically AND durably: a temp file in the same directory,
// fsynced, renamed into place, and the directory fsynced so the rename itself survives.
//
// WHY THIS EXISTS AS A PACKAGE, since the contract says to wait for three call sites. It had two --
// the cloud assignment cache and the service-manifest cache -- and `agent/cloud/cache.go` recorded
// the trigger in writing: "two call sites in two packages is the rule of three's second instance…
// The third one moves it." The mesh cache ([V3b.16b]) is the third, so this is that move, not a new
// guess about what might be reused later.
//
// WHY DURABLE AND NOT MERELY ATOMIC, which is the part that is easy to drop. Every caller here
// writes a fact whose ONLY copy is the file, read on a path that exists for a power cut. Atomic but
// unflushed means the write returns, the caller believes the fact is recorded, and a crash any time
// in the next commit interval takes it -- so the one boot the cache is for is the boot that finds it
// missing. That is [V3.23]'s defect verbatim. The directory fsync is [V3.23]'s too: on a first write
// the file is NEW, so the rename that publishes it lives only in the parent's dirent until the
// parent is flushed.
//
// It is deliberately three small functions and no type: callers own their paths, their permissions
// and their encoding. AGENTS §5's durable-write convention is what this implements, not a new one.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write puts data at path atomically and durably, creating the parent directory with dirPerm if it
// is missing. A failure at any step removes the temp file, so a failed write never leaves debris a
// later reader could mistake for content.
func Write(path string, data []byte, perm, dirPerm os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return err
		}
	}
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
	return SyncDir(filepath.Dir(path))
}

// SyncDir flushes a directory so a rename(2) into it is durable. Opening a directory read-only and
// fsyncing it is the portable spelling; a filesystem that refuses the open is reported rather than
// swallowed, since the caller's whole reason for being here is knowing the write landed.
func SyncDir(dir string) error {
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
