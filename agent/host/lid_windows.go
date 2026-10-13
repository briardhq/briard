package host

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	powrprof                  = windows.NewLazySystemDLL("powrprof.dll")
	procGetPwrCapabilities    = powrprof.NewProc("GetPwrCapabilities")
	procPowerGetActiveScheme  = powrprof.NewProc("PowerGetActiveScheme")
	procPowerReadACValueIndex = powrprof.NewProc("PowerReadACValueIndex")

	// GUID_SYSTEM_BUTTON_SUBGROUP and GUID_LIDCLOSE_ACTION: the power plan's "when I close the lid".
	subButtons = windows.GUID{Data1: 0x4f971e89, Data2: 0xeebd, Data3: 0x4455, Data4: [8]byte{0xa8, 0xde, 0x9e, 0x59, 0x04, 0x0e, 0x73, 0x47}}
	lidClose   = windows.GUID{Data1: 0x5ca83367, Data2: 0x6e45, Data3: 0x459f, Data4: [8]byte{0xa2, 0x7b, 0x47, 0x6b, 0x1d, 0x01, 0xc9, 0x36}}
)

// lidAction reads what closing the lid does while plugged in, under the active plan: "stays" for
// Do nothing, "sleeps" for sleep, hibernate or shut down, "" when there is no lid or no answer.
func lidAction() string {
	// SYSTEM_POWER_CAPABILITIES opens with three BOOLEANs: power button, sleep button, lid. The
	// buffer is larger than the struct, so only the leading byte the answer needs is ours to read.
	var caps [256]byte
	if ok, _, _ := procGetPwrCapabilities.Call(uintptr(unsafe.Pointer(&caps[0]))); ok == 0 || caps[2] == 0 {
		return ""
	}
	var scheme *windows.GUID
	if r, _, _ := procPowerGetActiveScheme.Call(0, uintptr(unsafe.Pointer(&scheme))); r != 0 || scheme == nil {
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(scheme)))
	var action uint32
	if r, _, _ := procPowerReadACValueIndex.Call(0, uintptr(unsafe.Pointer(scheme)),
		uintptr(unsafe.Pointer(&subButtons)), uintptr(unsafe.Pointer(&lidClose)), uintptr(unsafe.Pointer(&action))); r != 0 {
		return ""
	}
	if action == 0 {
		return "stays"
	}
	return "sleeps"
}
