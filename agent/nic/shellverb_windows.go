package nic

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// THE BRIDGE WHERE NETSH HAS NO BRIDGE COMMANDS. Windows 10's `netsh bridge` context offers
// install/uninstall/set/show/dump and nothing that makes, changes or removes a bridge; netsh is
// still the way wherever it has the command. Without it, the bridge is reached the way a person
// reaches it -- the Network Connections folder's own commands ("Bridge Connections", "Add to
// Bridge", "Remove from Bridge") -- through the documented Shell interfaces (IShellFolder,
// IContextMenu), by the commands' language-independent verbs. COM by hand through the system
// DLLs: a dozen calls do not earn a library.
//
// Measured on Windows 10 22H2:
//   - createbridge binds the selection in its order, and the first member's binding creates the
//     multiplexor adapter the bridge stands on, whose device then installs. Binding a physical NIC
//     resets its link, and a reset during that install tears the adapter down under it: the
//     install fails, Windows shows "An unexpected error occurred while configuring the Network
//     Bridge", the other member is never bound, and every later attempt fails the same way until
//     a restart. Ethernet first failed that way in every traced trial; the TAP first -- whose link
//     never drops -- never did. So the TAP is always selected first.
//   - that error is a modal dialog, and createbridge returns only once it is dismissed: in
//     session 0 (a service, a SYSTEM task) no one can, so createbridge is refused there rather
//     than started.
//   - removefrombridge and addtobridge return within seconds in session 0, with no UI.
//   - removing a bridge's last member removes the bridge, which is how it is destroyed here: the
//     folder's Delete asks a question, and session 0 has no one to answer it.
//   - invoking a verb the menu does not carry blocks instead of failing, so a verb is looked up in
//     the menu first and invoked by its place there.

// netshBridgeCommands is the command list `netsh bridge ?` prints ("create   - Create a
// bridge..."), command names being the one part of that output netsh does not translate.
func netshBridgeCommands(out string) map[string]bool {
	cmds := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == "-" {
			cmds[f[0]] = true
		}
	}
	return cmds
}

var netshBridge = sync.OnceValue(func() map[string]bool {
	out, _ := exec.Command("netsh", "bridge", "?").CombinedOutput()
	return netshBridgeCommands(string(out))
})

// netshLacks reports whether netsh's bridge context was read and has no cmd. A list that could
// not be read answers false: netsh stays the way unless Windows says it has no such command.
func netshLacks(cmd string) bool { return lacks(netshBridge(), cmd) }

func lacks(cmds map[string]bool, cmd string) bool { return cmds["show"] && !cmds[cmd] }

// sessionZero reports whether this process runs where no desktop is: a service or a SYSTEM task.
func sessionZero() bool {
	var session uint32
	return windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) == nil && session == 0
}

// verbBound is how long a verb may take. Each returned within seconds when measured; one that has
// not returned in this long is one that never will.
const verbBound = 2 * time.Minute

// shellVerb runs the Network Connections folder's command verb on the connections named, as one
// selection, and returns once Windows has finished it. A call that outlives verbBound is reported
// and left behind: a COM call cannot be cancelled, and the callers exit or carry on without it.
func shellVerb(ctx context.Context, verb string, names ...string) error {
	done := make(chan error, 1)
	go func() { done <- invokeVerb(verb, names) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(verbBound):
		return fmt.Errorf("Windows did not finish %q on %s within %s", verb, strings.Join(names, " + "), verbBound)
	}
}

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	shParseDisplayName = shell32.NewProc("SHParseDisplayName")
	shBindToObject     = shell32.NewProc("SHBindToObject")
	strRetToBufW       = windows.NewLazySystemDLL("shlwapi.dll").NewProc("StrRetToBufW")
	user32             = windows.NewLazySystemDLL("user32.dll")
	createPopupMenu    = user32.NewProc("CreatePopupMenu")
	destroyMenu        = user32.NewProc("DestroyMenu")
	getMenuItemCount   = user32.NewProc("GetMenuItemCount")
	getMenuItemID      = user32.NewProc("GetMenuItemID")

	iidShellFolder = windows.GUID{Data1: 0x000214e6, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidContextMenu = windows.GUID{Data1: 0x000214e4, Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// The Network Connections folder, by its shell CLSID.
const networkConnections = "::{7007ACC7-3202-11D1-AAD2-00805FC1270E}"

// The interfaces' methods by their place in the vtable, after IUnknown's three.
const (
	unkRelease            = 2
	sfEnumObjects         = 4
	sfGetUIObjectOf       = 10
	sfGetDisplayNameOf    = 11
	enumNext              = 3
	cmQueryContextMenu    = 3
	cmInvokeCommand       = 4
	cmGetCommandString    = 5
	shcontfFoldersNonFold = 0x20 | 0x40
	gcsVerbW              = 4
	cmicMaskNoAsync       = 0x100
)

// comObject is what an interface pointer points at: its first word is its vtable.
type comObject struct{ vtbl *[16]uintptr }

func (o *comObject) call(method int, args ...uintptr) (uintptr, error) {
	hr, _, _ := syscall.SyscallN(o.vtbl[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	if int32(hr) < 0 {
		return hr, fmt.Errorf("HRESULT %#08x", uint32(hr))
	}
	return hr, nil
}

func (o *comObject) release() { _, _ = o.call(unkRelease) }

// strRet is a STRRET: a type word and a union of a pointer, an offset and a MAX_PATH buffer.
type strRet struct {
	uType uint32
	_     uint32
	data  [260 * 2]byte
}

// cmInvokeCommandInfo is CMINVOKECOMMANDINFO, laid out as on amd64.
type cmInvokeCommandInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       uintptr
	lpParameters uintptr
	lpDirectory  uintptr
	nShow        int32
	dwHotKey     uint32
	hIcon        uintptr
}

func invokeVerb(verb string, names []string) error {
	// COM is per thread; this one is initialised for the call and given back after it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(windows.S_FALSE)) {
		return fmt.Errorf("COM: %w", err)
	}
	defer windows.CoUninitialize()

	folderName, _ := windows.UTF16PtrFromString(networkConnections)
	var folderID unsafe.Pointer
	var attrs uint32
	if hr, _, _ := shParseDisplayName.Call(uintptr(unsafe.Pointer(folderName)), 0, uintptr(unsafe.Pointer(&folderID)), 0, uintptr(unsafe.Pointer(&attrs))); int32(hr) < 0 {
		return fmt.Errorf("the Network Connections folder: HRESULT %#08x", uint32(hr))
	}
	defer windows.CoTaskMemFree(folderID)
	var folder *comObject
	if hr, _, _ := shBindToObject.Call(0, uintptr(folderID), 0, uintptr(unsafe.Pointer(&iidShellFolder)), uintptr(unsafe.Pointer(&folder))); int32(hr) < 0 {
		return fmt.Errorf("opening the Network Connections folder: HRESULT %#08x", uint32(hr))
	}
	defer folder.release()

	// The connections, by the name a person sees -- the adapter's name.
	var items *comObject
	if _, err := folder.call(sfEnumObjects, 0, shcontfFoldersNonFold, uintptr(unsafe.Pointer(&items))); err != nil || items == nil {
		return fmt.Errorf("listing Network Connections: %v", err)
	}
	defer items.release()
	picked := make([]uintptr, len(names))
	var all []unsafe.Pointer
	defer func() {
		for _, id := range all {
			windows.CoTaskMemFree(id)
		}
	}()
	for {
		var id unsafe.Pointer
		var n uint32
		if hr, _ := items.call(enumNext, 1, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&n))); hr != 0 || n == 0 {
			break
		}
		all = append(all, id)
		var sr strRet
		if _, err := folder.call(sfGetDisplayNameOf, uintptr(id), 0, uintptr(unsafe.Pointer(&sr))); err != nil {
			continue
		}
		var buf [260]uint16
		if hr, _, _ := strRetToBufW.Call(uintptr(unsafe.Pointer(&sr)), uintptr(id), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); int32(hr) < 0 {
			continue
		}
		for i, name := range names {
			if windows.UTF16ToString(buf[:]) == name {
				picked[i] = uintptr(id)
			}
		}
	}
	for i, id := range picked {
		if id == 0 {
			return fmt.Errorf("Network Connections lists no connection named %q", names[i])
		}
	}

	var menu *comObject
	if _, err := folder.call(sfGetUIObjectOf, 0, uintptr(len(picked)), uintptr(unsafe.Pointer(&picked[0])),
		uintptr(unsafe.Pointer(&iidContextMenu)), 0, uintptr(unsafe.Pointer(&menu))); err != nil {
		return fmt.Errorf("the commands for %s: %w", strings.Join(names, " + "), err)
	}
	defer menu.release()
	hmenu, _, _ := createPopupMenu.Call()
	if hmenu == 0 {
		return errors.New("CreatePopupMenu failed")
	}
	defer destroyMenu.Call(hmenu)
	if _, err := menu.call(cmQueryContextMenu, hmenu, 0, 1, 0x7fff, 0); err != nil {
		return fmt.Errorf("the commands for %s: %w", strings.Join(names, " + "), err)
	}
	count, _, _ := getMenuItemCount.Call(hmenu)
	for i := 0; i < int(int32(count)); i++ {
		id, _, _ := getMenuItemID.Call(hmenu, uintptr(i))
		if id == 0 || uint32(id) == 0xffffffff {
			continue
		}
		var buf [64]uint16
		if _, err := menu.call(cmGetCommandString, id-1, gcsVerbW, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); err != nil {
			continue
		}
		if windows.UTF16ToString(buf[:]) != verb {
			continue
		}
		ci := cmInvokeCommandInfo{fMask: cmicMaskNoAsync, lpVerb: id - 1, nShow: windows.SW_SHOWNORMAL}
		ci.cbSize = uint32(unsafe.Sizeof(ci))
		if _, err := menu.call(cmInvokeCommand, uintptr(unsafe.Pointer(&ci))); err != nil {
			return fmt.Errorf("%q on %s: %w", verb, strings.Join(names, " + "), err)
		}
		return nil
	}
	return fmt.Errorf("Windows offers no %q for %s", verb, strings.Join(names, " + "))
}
