package guestfirmware

import "syscall"

func setHostname(name string) error { return syscall.Sethostname([]byte(name)) }
