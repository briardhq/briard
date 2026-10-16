package host

import (
	"context"
	"os/exec"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	powrprof                   = windows.NewLazySystemDLL("powrprof.dll")
	procGetPwrCapabilities     = powrprof.NewProc("GetPwrCapabilities")
	procPowerGetActiveScheme   = powrprof.NewProc("PowerGetActiveScheme")
	procPowerReadACValueIndex  = powrprof.NewProc("PowerReadACValueIndex")
	procCallNtPowerInformation = powrprof.NewProc("CallNtPowerInformation")
	procQueryInterruptTime     = windows.NewLazySystemDLL("kernelbase.dll").NewProc("QueryInterruptTime")

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

// POWER_INFORMATION_LEVEL: the interrupt-time count at the last wake and at the last sleep.
const (
	powerLastWakeTime  = 14
	powerLastSleepTime = 15
)

// lastSleep reads the machine's last sleep and wake as wall-clock times. Windows keeps both as the
// interrupt-time count (100 ns ticks since boot, sleep included) at the moment, so each is placed
// by its distance from the count now. No sleep since boot is ok with zero times (an answer, and the
// one the first reading after a boot primes on); not ok only when the reads fail or disagree.
func lastSleep() (slept, woke time.Time, ok bool) {
	read := func(level uintptr) (uint64, bool) {
		var v uint64
		r, _, _ := procCallNtPowerInformation.Call(level, 0, 0, uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
		return v, r == 0
	}
	s, okS := read(powerLastSleepTime)
	w, okW := read(powerLastWakeTime)
	switch {
	case !okS || !okW:
		return time.Time{}, time.Time{}, false
	case s == 0 && w == 0:
		return time.Time{}, time.Time{}, true
	case s == 0 || w == 0 || w < s:
		return time.Time{}, time.Time{}, false
	}
	var now uint64
	procQueryInterruptTime.Call(uintptr(unsafe.Pointer(&now)))
	wall := time.Now()
	at := func(t uint64) time.Time { return wall.Add(-time.Duration(now-t) * 100) }
	return at(s), at(w), true
}

// fastStartup reports whether the machine came back from its last sleep by a power-on: a Shut down
// with Fast Startup on hibernates the kernel, the agent with it, and the boot that resumes it logs
// Kernel-Boot 27 with boot type 1 (a cold boot is 0, a chosen Hibernate's resume 2). A read that
// fails is no: the alert then says sleep, which was the hibernate's truth anyway.
func fastStartup(since time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := "/q:*[System[Provider[@Name='Microsoft-Windows-Kernel-Boot'] and EventID=27 and TimeCreated[@SystemTime>='" +
		since.UTC().Format("2006-01-02T15:04:05.000Z") + "']]]"
	out, err := exec.CommandContext(ctx, "wevtutil", "qe", "System", q, "/rd:true", "/c:1", "/f:xml").Output()
	return err == nil && strings.Contains(string(out), "<Data Name='BootType'>1</Data>")
}
