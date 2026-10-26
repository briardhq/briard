package host

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// THE START MENU SHORTCUT, written through Windows' own shell-link object (IShellLinkW) rather than
// WScript.Shell, which cannot set a shortcut's properties: the one this needs is the
// AppUserModelID, which ties the tray's process to the shortcut so that Windows titles its
// notifications with the shortcut's name and icon instead of the program's file name. COM by hand
// through the system DLLs, as the tray's Win32 calls are -- a dozen calls do not earn a library.

var (
	ole32            = windows.NewLazySystemDLL("ole32.dll")
	coCreateInstance = ole32.NewProc("CoCreateInstance")

	clsidShellLink   = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW    = windows.GUID{Data1: 0x000214f9, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPersistFile   = windows.GUID{Data1: 0x0000010b, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPropertyStore = windows.GUID{Data1: 0x886d8eeb, Data2: 0x8cf2, Data3: 0x4446, Data4: [8]byte{0x8d, 0x02, 0xcd, 0xba, 0x1d, 0xbd, 0xcf, 0x99}}
	// PKEY_AppUserModel_ID.
	pkeyAppUserModelID = propertyKey{windows.GUID{Data1: 0x9f4c2855, Data2: 0x9f79, Data3: 0x4b39, Data4: [8]byte{0xa8, 0xd0, 0xe1, 0xd4, 0x2d, 0xe1, 0xd5, 0xf3}}, 5}
)

// The interfaces' methods by their place in the vtable, after IUnknown's three.
const (
	slSetDescription      = 7
	slSetWorkingDirectory = 9
	slSetArguments        = 11
	slSetIconLocation     = 17
	slSetPath             = 20
	psSetValue            = 6
	psCommit              = 7
	pfSave                = 6
	unkQueryInterface     = 0
	unkRelease            = 2

	vtLPWSTR = 31
)

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// propVariant is a PROPVARIANT holding a VT_LPWSTR, laid out as Windows' 24-byte union on amd64.
type propVariant struct {
	vt       uint16
	reserved [3]uint16
	val      uintptr
	pad      uintptr
}

// comObject is what an interface pointer points at: its first word is its vtable.
type comObject struct{ vtbl *[32]uintptr }

func (o *comObject) call(method int, args ...uintptr) error {
	hr, _, _ := syscall.SyscallN(o.vtbl[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	if int32(hr) < 0 {
		return fmt.Errorf("HRESULT %#08x", uint32(hr))
	}
	return nil
}

func (o *comObject) query(iid *windows.GUID) (*comObject, error) {
	var p *comObject
	if err := o.call(unkQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&p))); err != nil {
		return nil, err
	}
	return p, nil
}

func (o *comObject) release() { _ = o.call(unkRelease) }

// writeShortcut writes lnk: target run with args in dir, the icon, a description, and the
// AppUserModelID appID.
func writeShortcut(lnk, target, args, dir, icon, desc, appID string) error {
	// COM is per thread; this one is initialised for the call and given back after it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && err != syscall.Errno(windows.S_FALSE) {
		return fmt.Errorf("COM: %w", err)
	}
	defer windows.CoUninitialize()

	var link *comObject
	if hr, _, _ := coCreateInstance.Call(uintptr(unsafe.Pointer(&clsidShellLink)), 0, windows.CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&link))); int32(hr) < 0 {
		return fmt.Errorf("the shell-link object: HRESULT %#08x", uint32(hr))
	}
	defer link.release()
	// The strings handed over as bare addresses stay referenced until every call has returned.
	var keep []*uint16
	defer runtime.KeepAlive(&keep)
	str := func(s string) uintptr {
		u, _ := windows.UTF16PtrFromString(s)
		keep = append(keep, u)
		return uintptr(unsafe.Pointer(u))
	}
	for _, step := range []struct {
		what string
		err  error
	}{
		{"path", link.call(slSetPath, str(target))},
		{"arguments", link.call(slSetArguments, str(args))},
		{"working directory", link.call(slSetWorkingDirectory, str(dir))},
		{"icon", link.call(slSetIconLocation, str(icon), 0)},
		{"description", link.call(slSetDescription, str(desc))},
	} {
		if step.err != nil {
			return fmt.Errorf("the shortcut's %s: %w", step.what, step.err)
		}
	}

	store, err := link.query(&iidPropertyStore)
	if err != nil {
		return fmt.Errorf("the shortcut's properties: %w", err)
	}
	defer store.release()
	v := propVariant{vt: vtLPWSTR, val: str(appID)}
	if err := store.call(psSetValue, uintptr(unsafe.Pointer(&pkeyAppUserModelID)), uintptr(unsafe.Pointer(&v))); err != nil {
		return fmt.Errorf("the shortcut's AppUserModelID: %w", err)
	}
	if err := store.call(psCommit); err != nil {
		return fmt.Errorf("the shortcut's properties: %w", err)
	}

	file, err := link.query(&iidPersistFile)
	if err != nil {
		return err
	}
	defer file.release()
	if err := file.call(pfSave, str(lnk), 1); err != nil {
		return fmt.Errorf("saving %s: %w", lnk, err)
	}
	return nil
}
