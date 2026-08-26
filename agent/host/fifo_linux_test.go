package host

import "syscall"

func mkFifo(path string) error { return syscall.Mkfifo(path, 0o644) }
