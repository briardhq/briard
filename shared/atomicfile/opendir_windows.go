package atomicfile

import (
	"os"

	"golang.org/x/sys/windows"
)

// openDir opens a directory for FlushFileBuffers, which on Windows refuses a read-only handle
// ("Access is denied") and flushes the directory's entries given a writable one. Backup semantics
// is what lets CreateFile open a directory at all.
func openDir(dir string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(h), dir), nil
}
