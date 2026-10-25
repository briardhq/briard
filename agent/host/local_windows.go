package host

import (
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sys/windows"
)

// restrictDoor sets the admin socket's DACL: SYSTEM, Administrators, and -- when this node has
// one -- the install user's account, which may connect and nothing more. Windows AF_UNIX checks
// connect against the socket FILE's DACL and needs write on it, as Linux needs write on the inode
// (measured: read alone is refused, FILE_GENERIC_WRITE admits). No grant on run\ is needed: every
// token holds the privilege to bypass traverse checking, so the socket's own DACL is the only gate,
// and the one that keeps every other account out.
//
// Protected, so nothing inherited widens it. With no install user (a SYSTEM install) the bind's
// inherited DACL -- run\'s, SYSTEM and Administrators -- already says this, and it is left alone.
// The socket file is a reparse point, which a plain open would try to follow: it is opened as
// itself.
func restrictDoor(sock, owner string) error {
	if owner == "" {
		return nil
	}
	sid, err := windows.StringToSid(owner)
	if err != nil {
		return fmt.Errorf("the install user %q: %w", owner, err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FW;;;" + sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(sock)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.WRITE_DAC|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// doorRefusal makes a connect the socket's DACL refused read as one: Windows says WSAEACCES,
// which, unlike Linux's EACCES, is not fs.ErrPermission -- and a refusal must never read as "no
// agent" (Stop's fallback would then go after a node this account may not touch).
func doorRefusal(err error) error {
	if errors.Is(err, windows.WSAEACCES) {
		return fmt.Errorf("%w: %w", fs.ErrPermission, err)
	}
	return err
}
