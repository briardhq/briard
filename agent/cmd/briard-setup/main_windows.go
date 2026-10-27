package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TWO WAYS TO RUN, ONE INSTALL. Double-clicked, it opens a console that shows the install as it
// goes and, when the restart is owed, asks whether to restart now. With /S -- the Store's silent
// switch -- nothing opens but the UAC prompt and it never restarts Windows. Either way it ends with
// the script's exit code, 3010 when the restart is owed (the Store shows its own restart message
// for it), 0 when not, and anything else is a failure. The whole output also goes to
// %TEMP%\briard-setup.log.
//
// Not elevated, it relaunches itself elevated and returns that run's code: a program with no
// manifest resource asks for UAC this way, as install.ps1 does.

// exitCancelled is ERROR_CANCELLED: the UAC prompt was declined.
const exitCancelled = 1223

func main() { os.Exit(run()) }

func run() int {
	silent := false
	for _, a := range os.Args[1:] {
		if !strings.EqualFold(a, "/S") {
			return usage(a)
		}
		silent = true
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return relaunchElevated(strings.Join(os.Args[1:], " "))
	}
	logPath := filepath.Join(os.TempDir(), "briard-setup.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return 1
	}
	defer lf.Close()
	out := io.Writer(lf)
	var con *os.File
	if !silent {
		if con = console(); con != nil {
			defer con.Close()
			out = io.MultiWriter(con, lf)
		}
	}
	code := runInstall(out)
	if con != nil {
		switch code {
		case 3010:
			fmt.Fprint(con, "\nRestart now? [Y/n] ")
			if a := strings.TrimSpace(readLine()); a == "" || strings.EqualFold(a[:1], "y") {
				// install.ps1's own restart: Restart, never a shutdown -- Fast Startup's resume is
				// not a start.
				exec.Command(filepath.Join(os.Getenv("SystemRoot"), `System32\shutdown.exe`),
					"/r", "/t", "5", "/c", "Briard: restarting once to finish the install").Run()
				return code
			}
			fmt.Fprintln(con, "Restart Windows when you are ready (Restart, not Shut down): Briard starts by itself afterwards.")
		case 0:
		default:
			fmt.Fprintf(con, "\nbriard: the install did not finish. Everything above is also in %s.\n", logPath)
		}
		fmt.Fprint(con, "\nPress Enter to close this window.")
		readLine()
	}
	return code
}

// readLine reads one line from the console.
func readLine() string {
	in, err := os.Open("CONIN$")
	if err != nil {
		return ""
	}
	defer in.Close()
	s, _ := bufio.NewReader(in).ReadString('\n')
	return s
}

// runInstall unpacks what this program carries and runs install.ps1: the carried arm's, or
// stable's, fetched and verified (prepare).
func runInstall(out io.Writer) int {
	fail := func(err error) int { fmt.Fprintf(out, "briard: ERROR: %v\n", err); return 1 }
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	f, err := os.Open(self)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	z, err := payload(f, fi.Size())
	if err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp("", "briard-setup-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	fmt.Fprintln(out, "briard: unpacking the install ...")
	if err := extract(z, dir); err != nil {
		return fail(err)
	}
	env, err := prepare(context.Background(), dir, channel, func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) })
	if err != nil {
		return fail(err)
	}
	// By its full path: an elevated program does not take powershell.exe from whatever PATH says.
	ps := filepath.Join(os.Getenv("SystemRoot"), `System32\WindowsPowerShell\v1.0\powershell.exe`)
	cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(dir, "install.ps1"), "-NoRestart")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	// Its output comes through this program, so it needs no window of its own in either mode.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func usage(arg string) int {
	if con := console(); con != nil {
		fmt.Fprintf(con, "briard-setup: unknown argument %q (the only one is /S, a silent install)\n", arg)
		con.Close()
	}
	return 87 // ERROR_INVALID_PARAMETER
}

var (
	procAllocConsole    = windows.NewLazySystemDLL("kernel32.dll").NewProc("AllocConsole")
	procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
)

// console opens a console window for this GUI-subsystem program; nil when there is none to open.
func console() *os.File {
	if r, _, _ := procAllocConsole.Call(); r == 0 {
		return nil
	}
	con, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return con
}

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size       uint32
	mask       uint32
	hwnd       uintptr
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instApp    uintptr
	idList     uintptr
	class      *uint16
	keyClass   uintptr
	hotKey     uint32
	icon       uintptr
	process    windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x40
	seeMaskNoAsync        = 0x100
)

// relaunchElevated runs this program again through UAC with args, waits, and returns its code.
func relaunchElevated(args string) int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(self)
	params, _ := windows.UTF16PtrFromString(args)
	sei := shellExecuteInfo{mask: seeMaskNoCloseProcess | seeMaskNoAsync, verb: verb, file: file, parameters: params, show: windows.SW_SHOWNORMAL}
	sei.size = uint32(unsafe.Sizeof(sei))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return exitCancelled
		}
		return 1
	}
	defer windows.CloseHandle(sei.process)
	if _, err := windows.WaitForSingleObject(sei.process, windows.INFINITE); err != nil {
		return 1
	}
	var code uint32
	if err := windows.GetExitCodeProcess(sei.process, &code); err != nil {
		return 1
	}
	return int(code)
}
