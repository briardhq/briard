package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

type agentService struct{}

// Wait hints: how long the SCM is told the next status change may take. Start covers what the
// agent does before READY (identity, disks, a staged qemu's smoke test); stop is re-sent on a
// rolling checkpoint while the stop is still making progress.
const (
	startHint = 60 * time.Second
	stopHint  = 20 * time.Second
)

// Execute is the service's whole life, in the SCM's terms.
//
// RUNNING is reported when the agent reaches the call that sends READY=1 under systemd -- "the
// agent started", never "the node is healthy" (sdnotify's package doc says why).
//
// STOP and PRESHUTDOWN are the two ways down, and the difference between them is the guest. A stop
// is an agent restart or an update: the guest keeps serving and the next agent re-adopts it,
// exactly as under systemd. A preshutdown is the machine going down, and is the one moment the
// guest must be powered off cleanly -- on Linux that is the guest unit's ExecStop, because there
// the agent cannot tell the two apart; here the SCM tells it.
//
// A resume is the third event, and it costs the guest a clean restart: a guest that slept with the
// machine wakes with a stale clock and a lease the router may have given away, and a fresh boot
// asks neither question. Powering it off is all this does -- a stopped guest is the case the host
// already relaunches at once. Fast Startup is why this matters on a desktop: its "shut down" is a
// hibernate that sends no PRESHUTDOWN, so the power-on is a resume and arrives here.
func (agentService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: uint32(startHint / time.Millisecond)}
	if err := holdAwake(); err != nil {
		log.Printf("service: cannot hold the machine awake, it may sleep while serving: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
				stopping(status, req, func() { cancel(); <-done })
				return false, 0
			case svc.PreShutdown:
				log.Printf("service: the machine is shutting down -- powering the guest off cleanly")
				stopping(status, req, func() {
					cancel()
					<-done
					sctx, scancel := context.WithTimeout(context.Background(), platform.GuestShutdownGrace+15*time.Second)
					defer scancel()
					if err := runGuestShutdown(sctx, qmp); err != nil {
						log.Printf("service: guest shutdown: %v", err)
						return
					}
					log.Printf("service: the guest powered off cleanly")
				})
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
const awakeReason = "Briard is serving your home's services from this PC"

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

// elevateHint is the refusal when it does not.
const elevateHint = "run as administrator"

// elevated reports whether this process holds an administrator's full token -- what an install or
// an uninstall needs, the way Linux needs root.
func elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// What `briard uninstall` names: the install tree, the data it keeps, and how to run it as needed.
const (
	installTree      = `C:\ProgramData\briard`
	dataTree         = `C:\ProgramData\briard\state`
	uninstallElevate = "run it in a terminal opened as administrator"
)

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

// idYes is what MessageBox returns for Yes.
const idYes = 6

// confirm asks text as a Yes/No question in a window.
func confirm(text string) bool {
	return messageBox(text, windows.MB_YESNO|windows.MB_ICONQUESTION) == idYes
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
