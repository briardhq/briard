package main

import (
	"context"
	"encoding/json"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"briard.io/agent/host"
	"briard.io/shared/api"
)

var (
	mpr                   = windows.NewLazySystemDLL("mpr.dll")
	wnetAddConnection2    = mpr.NewProc("WNetAddConnection2W")
	wnetGetConnection     = mpr.NewProc("WNetGetConnectionW")
	wnetCancelConnection2 = mpr.NewProc("WNetCancelConnection2W")
)

// netResource is NETRESOURCEW, what WNetAddConnection2 is told to connect.
type netResource struct {
	scope, kind, displayType, usage uint32
	localName, remoteName           *uint16
	comment, provider               *uint16
}

const (
	resourceTypeDisk = 1 // RESOURCETYPE_DISK
	// errConnectionUnavail is WNetGetConnection's answer for a letter Windows remembers a share
	// for and has not connected: the letter is spoken for all the same.
	errConnectionUnavail = 1201
)

// driveConn is the share a letter names, "" when it names none. A local disk names none.
func driveConn(letter string) string {
	l, err := windows.UTF16PtrFromString(letter)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := wnetGetConnection.Call(uintptr(unsafe.Pointer(l)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	switch r {
	case 0:
		return windows.UTF16ToString(buf)
	case errConnectionUnavail:
		if s := windows.UTF16ToString(buf); s != "" {
			return s
		}
		return "?" // spoken for, by a share Windows did not name
	}
	return ""
}

// driveFree reports a letter that names nothing: no disk, and no share.
func driveFree(letter string) bool {
	mask, err := windows.GetLogicalDrives()
	if err != nil || len(letter) == 0 || letter[0] < 'A' || letter[0] > 'Z' {
		return false
	}
	return mask&(1<<(letter[0]-'A')) == 0 && driveConn(letter) == ""
}

// driveAdd maps m for this logon session. Not remembered across logons and never asking: the
// icon maps it again at the next logon, with whatever address and password hold then, and a
// mapping that fails is tried again at the next poll, with no dialog.
func driveAdd(m driveMap, user, password string) bool {
	local, err1 := windows.UTF16PtrFromString(m.letter)
	remote, err2 := windows.UTF16PtrFromString(m.remote)
	u, err3 := windows.UTF16PtrFromString(user)
	p, err4 := windows.UTF16PtrFromString(password)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return false
	}
	nr := netResource{kind: resourceTypeDisk, localName: local, remoteName: remote}
	r, _, _ := wnetAddConnection2.Call(uintptr(unsafe.Pointer(&nr)), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(u)), 0)
	return r == 0
}

// driveCancel disconnects a letter, open files or not: it is called when the address moved, and
// what was open there is at an address that no longer answers.
func driveCancel(letter string) bool {
	l, err := windows.UTF16PtrFromString(letter)
	if err != nil {
		return false
	}
	r, _, _ := wnetCancelConnection2.Call(uintptr(unsafe.Pointer(l)), 0, 1)
	return r == 0
}

// driveStep asks the agent where the drive is and carries out what driveNext decides. Off the
// poll and one at a time: Windows takes a while to give up on an address that does not answer,
// and the icon must go on reading the node meanwhile. An agent that does not know the directive
// fails it, and nothing is mapped.
func (t *tray) driveStep() {
	if !t.driving.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer t.driving.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		o, err := host.Submit(ctx, t.sock, api.Directive{Kind: api.DirectiveDrive})
		if err != nil || o.State != api.OutcomeDone {
			return
		}
		var d host.LocalDrive
		if json.Unmarshal([]byte(o.Detail), &d) != nil {
			return
		}
		// t.drive is this goroutine's alone: `driving` admits one at a time.
		keep, cancelLetter, add := driveNext(d, t.drive, driveConn, driveFree)
		t.drive = keep
		if cancelLetter != "" {
			if !driveCancel(cancelLetter) {
				return
			}
			t.drive = driveMap{}
		}
		if add.letter != "" && driveAdd(add, d.User, d.Password) {
			t.drive = add
		}
	}()
}
