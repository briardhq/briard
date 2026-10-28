package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A STAND-IN SYNC CLIENT: the test registers a sync root of its own with the Cloud Files API --
// the call OneDrive makes for its folder -- and takes files up as placeholders itself, in sync or
// not, so the off-site check's two reads run against the real API with no Microsoft account. What
// it cannot stand in for is the client's own meaning of in sync, nor another user's profile read
// as SYSTEM; those need a real OneDrive.

var (
	procCfRegisterSyncRoot     = cldapi.NewProc("CfRegisterSyncRoot")
	procCfUnregisterSyncRoot   = cldapi.NewProc("CfUnregisterSyncRoot")
	procCfConvertToPlaceholder = cldapi.NewProc("CfConvertToPlaceholder")
)

const (
	standInName                  = "Briard stand-in"
	cfHydrationPolicyFull        = 2 // CF_HYDRATION_POLICY_FULL
	cfPopulationPolicyAlwaysFull = 3 // CF_POPULATION_POLICY_ALWAYS_FULL: no callbacks to answer
	cfConvertFlagMarkInSync      = 1 // CF_CONVERT_FLAG_MARK_IN_SYNC
)

// cfSyncRegistration is CF_SYNC_REGISTRATION.
type cfSyncRegistration struct {
	StructSize             uint32
	ProviderName           *uint16
	ProviderVersion        *uint16
	SyncRootIdentity       uintptr
	SyncRootIdentityLength uint32
	FileIdentity           uintptr
	FileIdentityLength     uint32
	ProviderID             windows.GUID
}

// cfSyncPolicies is CF_SYNC_POLICIES.
type cfSyncPolicies struct {
	StructSize                          uint32
	HydrationPrimary, HydrationModifier uint16
	PopulationPrimary, PopulationMod    uint16
	InSync                              uint32
	HardLink                            uint32
	PlaceholderManagement               uint32
}

// standIn registers a fresh directory as the stand-in client's sync root, unregistered when the
// test ends.
func standIn(t *testing.T) string {
	t.Helper()
	if err := procCfRegisterSyncRoot.Find(); err != nil {
		t.Fatalf("no Cloud Files API: %v", err)
	}
	root := t.TempDir()
	rp, _ := windows.UTF16PtrFromString(root)
	name, _ := windows.UTF16PtrFromString(standInName)
	version, _ := windows.UTF16PtrFromString("1.0")
	id := []byte("root")
	reg := cfSyncRegistration{ProviderName: name, ProviderVersion: version,
		SyncRootIdentity: uintptr(unsafe.Pointer(&id[0])), SyncRootIdentityLength: uint32(len(id)),
		ProviderID: windows.GUID{Data1: 0xb184a, Data2: 0xb1a2, Data3: 0xd0ff}}
	reg.StructSize = uint32(unsafe.Sizeof(reg))
	pol := cfSyncPolicies{HydrationPrimary: cfHydrationPolicyFull, PopulationPrimary: cfPopulationPolicyAlwaysFull}
	pol.StructSize = uint32(unsafe.Sizeof(pol))
	if hr, _, _ := procCfRegisterSyncRoot.Call(uintptr(unsafe.Pointer(rp)), uintptr(unsafe.Pointer(&reg)), uintptr(unsafe.Pointer(&pol)), 0); hr != 0 {
		t.Fatalf("register the stand-in's sync root: %v", hresult(hr))
	}
	t.Cleanup(func() {
		if hr, _, _ := procCfUnregisterSyncRoot.Call(uintptr(unsafe.Pointer(rp))); hr != 0 {
			t.Errorf("unregister the stand-in's sync root: %v", hresult(hr))
		}
	})
	return root
}

// takeUp makes path one of the stand-in's placeholders: uploaded when inSync.
func takeUp(t *testing.T, path string, inSync bool) {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var flags uintptr
	if inSync {
		flags = cfConvertFlagMarkInSync
	}
	id := []byte(filepath.Base(path))
	if hr, _, _ := procCfConvertToPlaceholder.Call(uintptr(h), uintptr(unsafe.Pointer(&id[0])), uintptr(len(id)), flags, 0, 0); hr != 0 {
		t.Fatalf("take %s up: %v", path, hresult(hr))
	}
}

// TestTheOffsiteReadsAreTheSyncClients: the folder's client by the name it registered, and per file
// whether it has it uploaded -- a file it has not taken up and one it holds but has not uploaded
// are both no, and only an all-uploaded night reads as synced.
func TestTheOffsiteReadsAreTheSyncClients(t *testing.T) {
	root := standIn(t)
	if name, err := syncProvider(t.TempDir()); name != "" {
		t.Errorf("a folder outside every sync root read as %q's", name)
	} else {
		t.Logf("outside a sync root: %v", err)
	}
	night := time.Now().Add(-time.Minute)
	folder := func(name string) string {
		dir := filepath.Join(root, name)
		writeAt(t, dir, "config", night.Add(-time.Hour))
		writeAt(t, dir, "data/ab/pack", night.Add(time.Second))
		writeAt(t, dir, "snapshots/snap", night.Add(2*time.Second))
		return dir
	}

	all := folder("uploaded")
	if name, err := syncProvider(all); name != standInName {
		t.Fatalf("the stand-in's folder read as %q's (%v)", name, err)
	}
	pack, snap := filepath.Join(all, "data", "ab", "pack"), filepath.Join(all, "snapshots", "snap")
	if err := inSync(pack); !errors.Is(err, windows.ERROR_NOT_A_CLOUD_FILE) {
		t.Errorf("a file the client has not taken up: %v, want ERROR_NOT_A_CLOUD_FILE", err)
	}
	takeUp(t, pack, false)
	if err := inSync(pack); !errors.Is(err, errNotUploaded) {
		t.Errorf("a placeholder not in sync: %v, want not uploaded", err)
	}
	takeUp(t, snap, true)
	if err := inSync(snap); err != nil {
		t.Errorf("a placeholder in sync: %v", err)
	}
	if a := offsiteRead(all, night, syncProvider, inSync); a.synced || a.in != standInName || a.files != 2 || a.uploaded != 1 {
		t.Errorf("one of the night's files not uploaded: %+v", a)
	}

	other := folder("all")
	takeUp(t, filepath.Join(other, "data", "ab", "pack"), true)
	takeUp(t, filepath.Join(other, "snapshots", "snap"), true)
	if a := offsiteRead(other, night, syncProvider, inSync); !a.synced || a.files != 2 {
		t.Errorf("every file of the night uploaded: %+v", a)
	}
	if err := os.WriteFile(filepath.Join(other, "snapshots", "late"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a := offsiteRead(other, night, syncProvider, inSync); a.synced {
		t.Errorf("a file written after the client took the rest up read as synced: %+v", a)
	}
}
