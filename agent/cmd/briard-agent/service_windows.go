package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"briard.io/agent/host"
	"briard.io/agent/platform"
	"briard.io/shared/sdnotify"
)

// runService runs the host agent under the Windows service manager when that is what started this
// process, and reports false when it is not (a `briard run` in a console runs in the foreground,
// exactly as on Linux).
func runService() bool {
	if in, err := svc.IsWindowsService(); err != nil || !in {
		return false
	}
	// Under the SCM a service's stderr goes nowhere. The log is a file, set once, so log.Printf
	// stays the one way the agent says anything.
	if w, err := openCappedLog(host.AgentLog()); err == nil {
		log.SetOutput(w)
	}
	if err := svc.Run(host.ServiceName, agentService{}); err != nil {
		log.Printf("service: %v", err)
	}
	return true
}

// runWarden runs the warden (host.Warden) under the service manager, logging to its own file. Only
// the service manager starts it: run by hand it would watch with nothing to restart what it ends.
func runWarden() int {
	if in, err := svc.IsWindowsService(); err != nil || !in {
		fmt.Fprintf(os.Stderr, "briard: -warden runs only as the %s service\n", host.WardenName)
		return 2
	}
	if w, err := openCappedLog(host.WardenLog()); err == nil {
		log.SetOutput(w)
	}
	if err := svc.Run(host.WardenName, wardenService{}); err != nil {
		log.Printf("warden service: %v", err)
		return 1
	}
	return 0
}

type wardenService struct{}

// Execute is RUNNING at once -- the warden has nothing to start -- and a stop or the machine's
// shutdown ends the watch. A warden that returns by itself failed, and says so to the recovery.
func (wardenService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.Warden(ctx, log.Printf) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			log.Printf("warden: %v", err)
			return false, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

type agentService struct{}

// Wait hints: how long the SCM is told the next status change may take. Start covers what the
// agent does before READY (identity, its config), and the warden ends a start that outlasts it
// (host.startLimit); stop is re-sent on a rolling checkpoint while the stop is still progressing.
const (
	startHint = 60 * time.Second
	stopHint  = 20 * time.Second
)

// Execute is the service's whole life, in the SCM's terms. RUNNING is reported when the agent
// reaches the call that sends READY=1 under systemd: "the agent started", never "the node is
// healthy" (sdnotify's package doc says why).
//
// The two ways down differ in the guest. A STOP (an agent restart or an update) finishes the act
// in flight and leaves the guest serving for the next agent to re-adopt, exactly as under
// systemd. A PRESHUTDOWN (the machine going down) is the agent's shutdown (host.ErrShutdown):
// the act aborts and the guest powers off cleanly before Run returns, inside the time Windows
// gives it. The agent's own `briard stop` is the same shutdown, arriving as Run returning nil by
// itself: a clean exit, which stays down.
//
// A resume costs the guest a clean restart: a guest that slept with the machine wakes with a
// stale clock and a lease the router may have given away, which a fresh boot never asks about.
// This only powers it off; the host already relaunches a stopped guest at once. It matters on a
// desktop because Fast Startup's "shut down" is a hibernate that sends no PRESHUTDOWN, so the
// power-on is a resume and arrives here.
func (agentService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: uint32(startHint / time.Millisecond)}
	if err := holdAwake(); err != nil {
		log.Printf("service: cannot hold the machine awake, it may sleep while serving: %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- runHost(ctx) }()
	qmp := host.ConfigFromEnv().QMPSock
	ready := sdnotify.Readied()
	for {
		select {
		case <-ready:
			status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptPreShutdown | svc.AcceptPowerEvent}
			ready = nil
		case err := <-done:
			// The agent returned by itself. An error is reported as a non-zero exit, which the SCM's
			// recovery actions treat as a failure once the service carries the failure-actions flag.
			if err != nil {
				log.Printf("host agent: %v", err)
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.PowerEvent:
				if c.EventType == pbtAPMResumeAutomatic {
					log.Printf("service: the machine resumed -- restarting the guest cleanly")
					go func() {
						sctx, scancel := context.WithTimeout(ctx, platform.GuestShutdownGrace+15*time.Second)
						defer scancel()
						if err := runGuestShutdown(sctx, qmp); err != nil {
							log.Printf("service: guest shutdown after resume: %v", err)
						}
					}()
				}
			case svc.Stop:
				log.Printf("service: stop -- the guest keeps running for the next agent to re-adopt")
				stopping(status, req, func() { cancel(nil); <-done })
				return false, 0
			case svc.PreShutdown:
				log.Printf("service: the machine is shutting down -- powering the guest off cleanly")
				stopping(status, req, func() { cancel(host.ErrShutdown); <-done })
				return false, 0
			}
		}
	}
}

// pbtAPMResumeAutomatic is the power event every resume sends, whoever woke the machine.
const pbtAPMResumeAutomatic = 0x12

var (
	procPowerCreateRequest = windows.NewLazySystemDLL("kernel32.dll").NewProc("PowerCreateRequest")
	procPowerSetRequest    = windows.NewLazySystemDLL("kernel32.dll").NewProc("PowerSetRequest")
)

// reasonContext is REASON_CONTEXT carrying a simple reason string; the blank field is the rest of
// its union.
type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
	_       [2]uintptr
}

// awakeReason is what `powercfg /requests` shows against the agent, so a user asking why the PC
// does not sleep is told by name.
const awakeReason = "Briard is serving your home's apps from this PC"

// holdAwake keeps the machine out of idle sleep for the rest of the process's life: a
// system-required power request, released by the process's exit. Idle sleep only -- a closed lid,
// the power button and a chosen Sleep are the user's acts under the power plan, and no request
// overrides them.
func holdAwake() error {
	reason, err := windows.UTF16PtrFromString(awakeReason)
	if err != nil {
		return err
	}
	rc := reasonContext{flags: 1 /* POWER_REQUEST_CONTEXT_SIMPLE_STRING */, reason: reason}
	h, _, err := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc)))
	if windows.Handle(h) == windows.InvalidHandle {
		return fmt.Errorf("PowerCreateRequest: %w", err)
	}
	if ok, _, err := procPowerSetRequest.Call(h, 1 /* PowerRequestSystemRequired */); ok == 0 {
		windows.CloseHandle(windows.Handle(h))
		return fmt.Errorf("PowerSetRequest: %w", err)
	}
	return nil
}

// stopping runs work while telling the SCM, on a rolling checkpoint, that the stop is progressing.
// A stop that has stopped progressing stops checkpointing, and the SCM's wait hint is what gives
// up on it.
func stopping(status chan<- svc.Status, req <-chan svc.ChangeRequest, work func()) {
	finished := make(chan struct{})
	go func() { work(); close(finished) }()
	cp := uint32(1)
	pending := func() svc.Status {
		return svc.Status{State: svc.StopPending, CheckPoint: cp, WaitHint: uint32(stopHint / time.Millisecond)}
	}
	status <- pending()
	tick := time.NewTicker(stopHint / 4)
	defer tick.Stop()
	for {
		select {
		case <-finished:
			return
		case <-tick.C:
			cp++
			status <- pending()
		case c := <-req:
			if c.Cmd == svc.Interrogate {
				status <- pending()
			}
		}
	}
}

// logMax is the size past which the agent's log rolls to `.1`. One generation, so the log costs at
// most twice this and the previous stretch is still there to read.
const logMax = 16 << 20

// cappedLog is the agent's log file on Windows: appended, rolled to `.1` past logMax.
type cappedLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	n    int64
}

func openCappedLog(path string) (*cappedLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	c := &cappedLog{path: path}
	return c, c.open()
}

func (c *cappedLog) open() error {
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	c.f, c.n = f, fi.Size()
	return nil
}

func (c *cappedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n+int64(len(p)) > logMax {
		c.f.Close()
		_ = os.Rename(c.path, c.path+".1")
		if err := c.open(); err != nil {
			return 0, err
		}
	}
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

// elevateHint is the refusal when elevated is false.
const elevateHint = "run as administrator"

// elevated reports whether this process holds an administrator's full token -- what an install or
// an uninstall needs, the way Linux needs root.
func elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// What `briard uninstall` names: the install tree, the data it keeps, and how to run it as needed.
const (
	installTree      = `C:\ProgramData\briard`
	dataTree         = `C:\ProgramData\briard\state`
	uninstallElevate = "run it in a terminal opened as administrator"
	systemUnits      = "Windows services"
)

// exitCancelled is ERROR_CANCELLED: the UAC prompt was declined.
const exitCancelled = 1223

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

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

// uninstallElevated runs `briard uninstall args` again through UAC, as the installer elevates
// itself, waits for it, and returns its exit code. This is how a package manager's quiet uninstall
// works from a terminal that is not elevated: winget runs the quiet form unelevated, whatever the
// install needed. The elevated run has a console of its own, hidden -- the quiet form shows no
// window -- so what it says is not seen here; its outcome is.
func uninstallElevated(args []string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "briard uninstall: %v\n", err)
		return 1
	}
	params := []string{"uninstall"}
	for _, a := range args {
		params = append(params, windows.EscapeArg(a))
	}
	fmt.Println("briard: asking for administrator rights to uninstall ...")
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(self)
	p, _ := windows.UTF16PtrFromString(strings.Join(params, " "))
	sei := shellExecuteInfo{mask: seeMaskNoCloseProcess | seeMaskNoAsync, verb: verb, file: file, parameters: p, show: windows.SW_HIDE}
	sei.size = uint32(unsafe.Sizeof(sei))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			fmt.Fprintln(os.Stderr, "briard uninstall: not uninstalled: the administrator prompt was declined")
			return exitCancelled
		}
		fmt.Fprintf(os.Stderr, "briard uninstall: could not ask for administrator rights (%v); %s\n", err, uninstallElevate)
		return 1
	}
	defer windows.CloseHandle(sei.process)
	var code uint32
	if _, err := windows.WaitForSingleObject(sei.process, windows.INFINITE); err != nil {
		code = 1
	} else if err := windows.GetExitCodeProcess(sei.process, &code); err != nil {
		code = 1
	}
	if code != 0 {
		fmt.Fprintf(os.Stderr, "briard uninstall: the uninstall did not finish (exit %d); %s to see why\n", code, uninstallElevate)
		return int(code)
	}
	fmt.Println("briard: uninstalled")
	return 0
}

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// windowed reports whether this process is alone in a console Windows opened for it -- started
// from Installed apps or a double-click, where text printed into that console vanishes with it at
// exit -- on a desktop someone can see. A shell shares its console with what it runs, and session 0
// (a service, a SYSTEM task) has no desktop to show a window on, nor anyone to close it.
func windowed() bool {
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) != nil || session == 0 {
		return false
	}
	var ids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}

// noQuickEdit turns QuickEdit off in the console this process has to itself (windowed). With it
// on, a click in the window starts a selection, and every write to the console blocks until the
// selection ends -- a stray click stops the program at its next line of output, with nothing on
// screen to say so. Best-effort: a console that refuses keeps its mode.
func noQuickEdit() {
	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if windows.GetConsoleMode(in, &mode) != nil {
		return
	}
	_ = windows.SetConsoleMode(in, mode&^windows.ENABLE_QUICK_EDIT_MODE|windows.ENABLE_EXTENDED_FLAGS)
}

var (
	procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	procShowWindow       = windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow")
)

// hideConsole hides the console this process has to itself (windowed): the quiet uninstall a
// package manager starts is a console program Windows gives a window of its own, and the quiet
// form shows none.
func hideConsole() {
	if h, _, _ := procGetConsoleWindow.Call(); h != 0 {
		procShowWindow.Call(h, windows.SW_HIDE)
	}
}

// idYes is what MessageBox returns for Yes.
const idYes = 6

// confirm asks text as a Yes/No question in a window.
func confirm(text string) bool {
	return messageBox(text, windows.MB_YESNO|windows.MB_ICONQUESTION) == idYes
}

// confirmDelete asks text as a Yes/No warning whose default is No: the answer a stray Enter gives
// must not be the one that deletes.
func confirmDelete(text string) bool {
	return messageBox(text, windows.MB_YESNO|windows.MB_ICONWARNING|windows.MB_DEFBUTTON2) == idYes
}

var procTaskDialogIndirect = windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect")

// The task dialog's numbers, from commctrl.h.
const (
	tdfAllowDialogCancellation = 0x0008 // Esc and the close box answer Cancel
	tdfVerificationFlagChecked = 0x0100 // the checkbox starts ticked
	tdcbfCancelButton          = 0x0008
	idUninstall                = 100 // the one button of ours; Cancel is Windows' own
	iconID                     = 1   // the icon, ../briard.rc
)

// confirmUninstall asks the windowed uninstall's question in Windows' task dialog -- a bold line,
// the text, Uninstall and Cancel, and a checkbox with check as its label, ticked to start with when
// checked -- and reports whether to go ahead and whether the box was ticked. The task dialog is
// Common Controls 6, which the agent's manifest asks for (../briard.manifest); a build without it
// asks the plain Yes/No question, the checkbox's answer left as it came.
func confirmUninstall(instruction, content, check string, checked bool) (ok, ticked bool) {
	if procTaskDialogIndirect.Find() != nil {
		return confirm(instruction + "\n\n" + content), checked
	}
	var keep [][]uint16
	str := func(s string) uint64 {
		t, _ := windows.UTF16FromString(s)
		keep = append(keep, t)
		return uint64(uintptr(unsafe.Pointer(&t[0])))
	}
	var inst windows.Handle
	windows.GetModuleHandleEx(0, nil, &inst)
	var button bytes.Buffer // TASKDIALOG_BUTTON, packed: nButtonID, pszButtonText
	binary.Write(&button, binary.LittleEndian, int32(idUninstall))
	binary.Write(&button, binary.LittleEndian, str("Uninstall"))
	btn := button.Bytes()
	flags := uint32(tdfAllowDialogCancellation)
	if checked {
		flags |= tdfVerificationFlagChecked
	}
	// TASKDIALOGCONFIG: commctrl.h declares it packed to one byte, which no Go struct can be, so it
	// is written field by field, in its order.
	var b bytes.Buffer
	for _, f := range []any{
		uint32(160),                              // cbSize
		uint64(0),                                // hwndParent
		uint64(inst),                             // hInstance
		flags,                                    // dwFlags
		uint32(tdcbfCancelButton),                // dwCommonButtons
		str("Briard"),                            // pszWindowTitle
		uint64(iconID),                           // pszMainIcon: a resource of hInstance, by its id
		str(instruction),                         // pszMainInstruction
		str(content),                             // pszContent
		uint32(1),                                // cButtons
		uint64(uintptr(unsafe.Pointer(&btn[0]))), // pButtons
		int32(idUninstall),                       // nDefaultButton
		uint32(0),                                // cRadioButtons
		uint64(0),                                // pRadioButtons
		int32(0),                                 // nDefaultRadioButton
		str(check),                               // pszVerificationText
		uint64(0),                                // pszExpandedInformation
		uint64(0),                                // pszExpandedControlText
		uint64(0),                                // pszCollapsedControlText
		uint64(0),                                // pszFooterIcon
		uint64(0),                                // pszFooter
		uint64(0),                                // pfCallback
		uint64(0),                                // lpCallbackData
		uint32(0),                                // cxWidth: Windows' own
	} {
		binary.Write(&b, binary.LittleEndian, f)
	}
	cfg := b.Bytes()
	var pressed, tick int32
	hr, _, _ := procTaskDialogIndirect.Call(uintptr(unsafe.Pointer(&cfg[0])),
		uintptr(unsafe.Pointer(&pressed)), 0, uintptr(unsafe.Pointer(&tick)))
	runtime.KeepAlive(keep)
	runtime.KeepAlive(btn)
	runtime.KeepAlive(cfg)
	if hr != 0 {
		return confirm(instruction + "\n\n" + content), checked
	}
	return pressed == idUninstall, tick != 0
}

// tell shows text in a window until it is closed.
func tell(text string, failed bool) {
	icon := uint32(windows.MB_ICONINFORMATION)
	if failed {
		icon = windows.MB_ICONERROR
	}
	messageBox(text, windows.MB_OK|icon)
}

// messageBox shows text, unwrapped -- it is written for an 80-column console, and a window wraps
// it itself -- and returns the button pressed.
func messageBox(text string, style uint32) int32 {
	text = strings.NewReplacer("\n\n", "\n\n", "\n", " ").Replace(text)
	t, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return 0
	}
	caption, _ := windows.UTF16PtrFromString("Briard")
	ret, _ := windows.MessageBox(0, t, caption, style|windows.MB_SETFOREGROUND)
	return ret
}

// updateStop and updateStart are the self-update's two service verbs (runUpdate), with the warden
// as one more entry around the agent: stopped first, so it neither counts the agent's quiet as a
// wedge nor runs the program about to be replaced, and brought to the agent in place last
// (host.StartWarden). A warden that does not stop or start is logged, never the update's failure.
func updateStop(ctx context.Context, patience time.Duration) error {
	if err := host.StopWarden(ctx, log.Printf); err != nil {
		log.Printf("update: stopping the warden: %v", err)
	}
	return platform.StopService(ctx, host.ServiceName, patience, log.Printf)
}

// updateStart starts the agent to RUNNING, then the warden. A failed start carries the agent's own
// last lines since the start, which is what its revert reports.
func updateStart(ctx context.Context) error {
	mark := int64(0)
	if fi, err := os.Stat(host.AgentLog()); err == nil {
		mark = fi.Size()
	}
	err := host.StartAgent(ctx, log.Printf)
	if err != nil {
		if tail := logTail(host.AgentLog(), mark, 5); tail != "" {
			err = fmt.Errorf("%w: %s", err, tail)
		}
	}
	return err
}

// logTail is the last n lines written to path past offset (all of it, when the log rolled), on
// one line.
func logTail(path string, offset int64, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if offset <= int64(len(b)) {
		b = b[offset:]
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}
