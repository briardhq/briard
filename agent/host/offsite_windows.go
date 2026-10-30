package host

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The off-site check's two reads (backup.go), from the Cloud Files API that OneDrive -- and other
// sync clients since -- is built on: the client registers its folder as a sync root under a name
// of its own, and keeps per file whether the copy on disk is the one it holds. Both are reads of
// what the client recorded; neither opens a file's data, so a file the client moved to the cloud
// is not downloaded to be asked about.
var (
	cldapi                      = windows.NewLazySystemDLL("cldapi.dll") // the library behind cfapi.h
	procCfGetSyncRootInfoByPath = cldapi.NewProc("CfGetSyncRootInfoByPath")
	procCfGetPlaceholderInfo    = cldapi.NewProc("CfGetPlaceholderInfo")
)

const (
	cfSyncRootInfoProvider  = 2    // CF_SYNC_ROOT_INFO_PROVIDER
	cfPlaceholderInfoBasic  = 0    // CF_PLACEHOLDER_INFO_BASIC
	cfInSyncStateInSync     = 1    // CF_IN_SYNC_STATE_IN_SYNC
	cfMaxProviderNameLength = 255  // CF_MAX_PROVIDER_NAME_LENGTH, and CF_MAX_PROVIDER_VERSION_LENGTH
	cfMaxFileIdentityLength = 4096 // CF_PLACEHOLDER_MAX_FILE_IDENTITY_LENGTH
)

// cfSyncRootProviderInfo is CF_SYNC_ROOT_PROVIDER_INFO.
type cfSyncRootProviderInfo struct {
	ProviderStatus  uint32
	ProviderName    [cfMaxProviderNameLength + 1]uint16
	ProviderVersion [cfMaxProviderNameLength + 1]uint16
}

// cfPlaceholderBasicInfo is CF_PLACEHOLDER_BASIC_INFO, with room for the longest file identity.
type cfPlaceholderBasicInfo struct {
	PinState           uint32
	InSyncState        uint32
	FileID             int64
	SyncRootFileID     int64
	FileIdentityLength uint32
	FileIdentity       [cfMaxFileIdentityLength]byte
}

// errNotUploaded is a file its sync client holds but has not uploaded yet.
var errNotUploaded = errors.New("not uploaded yet")

// offsiteCanTell: this host reads whether a folder is a sync client's (the page's off-site line).
const offsiteCanTell = true

// syncProvider names the sync client whose sync root holds dir, by the name it registered.
func syncProvider(dir string) (string, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", err
	}
	if err := procCfGetSyncRootInfoByPath.Find(); err != nil {
		return "", err
	}
	var info cfSyncRootProviderInfo
	var n uint32
	hr, _, _ := procCfGetSyncRootInfoByPath.Call(uintptr(unsafe.Pointer(p)), cfSyncRootInfoProvider,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), uintptr(unsafe.Pointer(&n)))
	if hr != 0 {
		return "", hresult(hr)
	}
	return windows.UTF16ToString(info.ProviderName[:]), nil
}

// inSync is nil when path's sync client has uploaded it: a placeholder, in sync. A file the client
// has not taken up yet is no placeholder at all (ERROR_NOT_A_CLOUD_FILE), which is the state of
// every file written while nobody is signed in to run OneDrive.
func inSync(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := procCfGetPlaceholderInfo.Find(); err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info cfPlaceholderBasicInfo
	var n uint32
	hr, _, _ := procCfGetPlaceholderInfo.Call(uintptr(h), cfPlaceholderInfoBasic,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), uintptr(unsafe.Pointer(&n)))
	if hr != 0 {
		return hresult(hr)
	}
	if info.InSyncState != cfInSyncStateInSync {
		return errNotUploaded
	}
	return nil
}

// hresult is a failed HRESULT as an error: the Win32 error inside it when it carries one, which
// is how the Cloud Files API reports its own.
func hresult(hr uintptr) error {
	if hr&0xffff0000 == 0x80070000 {
		return windows.Errno(hr & 0xffff)
	}
	return fmt.Errorf("HRESULT %#x", uint32(hr))
}
