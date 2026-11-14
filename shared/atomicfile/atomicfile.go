// Package atomicfile writes a file atomically AND durably: a temp file in the same directory,
// fsynced, renamed into place, and the directory fsynced so the rename itself survives.
//
// WHY DURABLE AND NOT MERELY ATOMIC, which is the part that is easy to drop. Every caller writes a
// fact whose ONLY copy is the file, read on a path that exists for a power cut. Atomic but
// unflushed means the write returns, the caller believes the fact is recorded, and a crash in the
// next commit interval takes it -- so the one boot the file is for is the boot that finds it
// missing. The directory fsync is part of the same guarantee: on a first write the file is NEW, so
// the rename that publishes it lives only in the parent's dirent until the parent is flushed.
//
// It is deliberately a few small functions and no type: callers own their paths, their permissions
// and their encoding. This is the durable-write convention, not a second one.
package atomicfile

import (
	"io/fs"
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

// SyncDir flushes a directory so a rename(2) into it is durable. How a directory is opened for that
// is per OS (openDir); a filesystem that refuses the open is reported rather than swallowed, since
// the caller's whole reason for being here is knowing the write landed.
func SyncDir(dir string) error {
	d, err := openDir(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// SyncTree flushes root and, when it is a directory, every regular file and directory under it,
// so a tree that is about to be renamed into place is on disk before the rename can be. root may
// be a single file. Symlinks are not followed: a link's own entry is flushed with its directory.
//
// This is for bytes that arrive by bulk (an unpacked tarball, a decompressed image) rather than
// through Write, and it has the same reason to exist: a rename that publishes an unflushed file
// can reach the disk before the file does, so a power cut leaves a verified name over lost bytes.
func SyncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		open := openFile
		if d.IsDir() {
			open = openDir
		}
		f, err := open(p)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}
