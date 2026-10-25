package host

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// socketSDDL reads a socket file's DACL as itself, not through the reparse point it is.
func socketSDDL(t *testing.T, sock string) string {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(sock)
	h, err := windows.CreateFile(p, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// The door's DACL after the bind: the install user may write -- connect -- and nothing inherited
// widens it. Without an install user the bind's inherited DACL is left as it is. Run on a Windows
// host (the cross-compiled test binary), as whoever: the SID is only data here.
func TestRestrictDoorAdmitsTheInstallUser(t *testing.T) {
	const user = "S-1-5-21-1-2-3-1001"
	for _, tc := range []struct {
		owner string
		want  []string
		not   []string
	}{
		{owner: user, want: []string{"D:P", "(A;;FA;;;SY)", "(A;;FA;;;BA)", "(A;;FW;;;" + user + ")"}, not: []string{"ID;"}},
		{owner: "", want: []string{"ID;"}, not: []string{"D:P", user}},
	} {
		sock := filepath.Join(t.TempDir(), "admin.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := restrictDoor(sock, tc.owner); err != nil {
			t.Fatalf("owner %q: %v", tc.owner, err)
		}
		got := socketSDDL(t, sock)
		ln.Close()
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("owner %q: %s lacks %s", tc.owner, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("owner %q: %s carries %s", tc.owner, got, n)
			}
		}
	}
	// A SID that does not parse shuts the door rather than writing a DACL nobody chose.
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := restrictDoor(sock, "S-1-5-21)(A;;FA;;;WD"); err == nil {
		t.Errorf("an unparseable install user was taken: %s", socketSDDL(t, sock))
	}
}

// A connect the DACL refused reads as a refusal, never as "no agent"; anything else is left alone.
func TestDoorRefusalIsAPermissionError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", windows.WSAEACCES)}
	if !errors.Is(doorRefusal(refused), fs.ErrPermission) {
		t.Errorf("WSAEACCES is not a permission error: %v", doorRefusal(refused))
	}
	absent := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", windows.WSAECONNREFUSED)}
	if errors.Is(doorRefusal(absent), fs.ErrPermission) {
		t.Errorf("a refused connection reads as a permission error: %v", doorRefusal(absent))
	}
}

// The install names the account that ran it -- this test's own token -- in config.env.
func TestInstallConfigNamesTheInstallUser(t *testing.T) {
	t.Setenv("BRIARD_BACKUP_DIR", "") // no folder made in this account's Documents
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if u.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		t.Skip("run as SYSTEM: there is no install user to name")
	}
	r := installRoots{prefix: t.TempDir()}
	if err := r.writeConfig(context.Background(), r.bundleLines(""), quiet); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.prefix, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "\nINSTALL_USER_SID=" + u.User.Sid.String() + "\n"; !strings.Contains(string(b), want) {
		t.Errorf("config.env lacks %q:\n%s", want, b)
	}
}
