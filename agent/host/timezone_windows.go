package host

import (
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// localTimezone returns the machine's IANA zone name, or "" if it cannot be established. root is
// unused: Windows keeps the zone in the registry, not in a file under a root.
//
// The registry names a WINDOWS zone ("GTB Standard Time"), which nothing outside Windows reads.
// ICU, which Windows ships as icu.dll, maps it to IANA with CLDR's table. Without a region, ICU
// answers with the zone's representative city (Europe/Bucharest for Athens too). By definition a
// Windows zone groups IANA zones that keep the same clock, so the times are right and only the
// city's name may differ.
//
// THE NAME IS NOT LOADED HERE, unlike on Linux. Windows has no zone database Go can read, so
// time.LoadLocation would refuse every name, and ICU answers only with canonical IANA names
// anyway. The guest still refuses a zone its own database lacks. The host's own schedules use
// time.Local, which Go builds from the same registry key.
//
// TZ is not read: on Windows it does not set this process's clock either.
func localTimezone(string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	win, _, err := k.GetStringValue("TimeZoneKeyName")
	if err != nil {
		return ""
	}
	return ianaZone(win)
}

var icuWindowsIDToIANA = windows.NewLazySystemDLL("icu.dll").NewProc("ucal_getTimeZoneIDForWindowsID")

// ianaZone maps a Windows zone ID to its IANA name through ICU, or "" when ICU is missing or
// knows no such zone.
//
// int32_t ucal_getTimeZoneIDForWindowsID(const UChar* winid, int32_t len, const char* region,
// UChar* id, int32_t idCapacity, UErrorCode* status)
func ianaZone(win string) string {
	if win == "" || icuWindowsIDToIANA.Find() != nil {
		return ""
	}
	in, err := windows.UTF16FromString(win)
	if err != nil {
		return ""
	}
	var out [64]uint16
	var status int32 // U_ZERO_ERROR; ICU reports a failure as a positive code
	r, _, _ := icuWindowsIDToIANA.Call(
		uintptr(unsafe.Pointer(&in[0])), uintptr(len(in)-1), 0,
		uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), uintptr(unsafe.Pointer(&status)))
	n := int32(r)
	if status > 0 || n <= 0 || int(n) > len(out) {
		return ""
	}
	return windows.UTF16ToString(out[:n])
}
