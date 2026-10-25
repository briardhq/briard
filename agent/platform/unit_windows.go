package platform

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// The Windows side of the service-manager seam: a transient unit is a NAMED JOB OBJECT holding the
// unit's process tree, found again by name after the agent restarts -- the property the seam
// exists for.
//
// ⚠️ THE NAME LIVES AS LONG AS A HANDLE DOES, NOT AS LONG AS THE JOB. A process in a job keeps the
// job object alive but holds no handle to it, and the object manager drops a name when the last
// HANDLE closes. So a job made by the agent and joined by qemu outlives the agent nameless: the next
// agent's OpenJobObject finds nothing, reads the guest as stopped, and launches a second qemu over
// the first one's sockets (measured, on the rig). The unit's own process therefore INHERITS a handle
// to its job: the name lives exactly as long as the unit does, and the agent holds none.
//
// What it leaves off: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. With the unit holding the last handle it
// would only fire as the unit exits, so it would buy nothing; it is named here because it is the
// tidy-looking flag, and on any other handle arrangement it kills the guest at an agent restart.
//
// What it has no equivalent of: ExecStop. A job is killed, never asked to stop. On Linux the clean
// guest powerdown lives on the unit, so every stop of it -- the machine's, the agent's -- runs it;
// here the agent powers the guest down itself (PowerOffGuest) and this layer only ever kills.

// jobName is the kernel object a unit lives in. Global\ so the name is the same from any session --
// the agent runs as a service in session 0, and an operator's shell is not in it.
func jobName(unit string) string { return `Global\briard-unit-` + unit }

// Access rights on a job (winnt.h). The x/sys package defines the job's limit flags but not these.
const (
	jobQuery     = 0x0004 // JOB_OBJECT_QUERY
	jobTerminate = 0x0008 // JOB_OBJECT_TERMINATE
)

// OpenJobObjectW is not wrapped by x/sys/windows. CreateJobObject is, but it CREATES on a miss, and
// a query that conjures the thing it asks about cannot answer "not there".
var procOpenJobObjectW = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")

// errNoJob is the one failure that IS an answer: no job of that name, so no such unit.
var errNoJob = errors.New("no such job")

func openJob(unit string, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(jobName(unit))
	if err != nil {
		return 0, err
	}
	r, _, e := procOpenJobObjectW.Call(uintptr(access), 0, uintptr(unsafe.Pointer(name)))
	if r == 0 {
		if errors.Is(e, windows.ERROR_FILE_NOT_FOUND) {
			return 0, errNoJob
		}
		return 0, fmt.Errorf("open job %s: %w", jobName(unit), e)
	}
	return windows.Handle(r), nil
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which x/sys/windows does not define.
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

// activeProcesses counts what is still running in the unit's job. errNoJob when there is no job.
func activeProcesses(unit string) (uint32, error) {
	h, err := openJob(unit, jobQuery)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var info jobAccounting
	if err := windows.QueryInformationJobObject(h, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0, fmt.Errorf("query job %s: %w", jobName(unit), err)
	}
	return info.ActiveProcesses, nil
}

// startTransient runs the command as a named job that outlives this process. The process is created
// SUSPENDED and joined to the job before it runs a single instruction, so nothing it starts can
// escape the job by racing the assignment.
func startTransient(_ context.Context, args []string) ([]byte, error) {
	t, err := parseTransient(args)
	if err != nil {
		return nil, err
	}
	// systemd-run refuses a name that is loaded; so does this, rather than joining a second qemu
	// to a job that already holds one.
	if n, err := activeProcesses(t.unit); err == nil && n > 0 {
		return nil, fmt.Errorf("unit %s is already running (%d processes in its job)", t.unit, n)
	}
	name, err := windows.UTF16PtrFromString(jobName(t.unit))
	if err != nil {
		return nil, err
	}
	// Inheritable: the unit's process holds the handle that keeps the name (the file header says why).
	jsa := windows.SecurityAttributes{InheritHandle: 1}
	jsa.Length = uint32(unsafe.Sizeof(jsa))
	job, err := windows.CreateJobObject(&jsa, name)
	if err != nil {
		return nil, fmt.Errorf("create job %s: %w", jobName(t.unit), err)
	}
	defer windows.CloseHandle(job)

	out, err := unitOutput(t.log)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(out)
	// Only these two are inherited, by name: a bare bInheritHandles=TRUE would hand the child every
	// inheritable handle this process happens to hold at that instant.
	inherit := [2]windows.Handle{out, job}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inherit[0]), unsafe.Sizeof(inherit)); err != nil {
		return nil, fmt.Errorf("handle list: %w", err)
	}
	si := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdOutput: out,
			StdErr:    out,
		},
		ProcThreadAttributeList: attrs.List(),
	}
	cmd, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(t.exec))
	if err != nil {
		return nil, err
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP |
		windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcess(nil, cmd, nil, nil, true, flags, nil, nil, &si.StartupInfo, &pi); err != nil {
		return nil, fmt.Errorf("start %s: %w", t.exec[0], err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return nil, fmt.Errorf("join %s to job %s: %w", t.exec[0], jobName(t.unit), err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		return nil, fmt.Errorf("resume %s: %w", t.exec[0], err)
	}
	return nil, nil
}

// unitOutput opens the unit's log for appending, inheritable, shared so a reader can open it while
// the unit writes. No log named is NUL: the SCM keeps no journal to fall back on.
func unitOutput(path string) (windows.Handle, error) {
	if path == "" {
		path = "NUL"
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	sa := windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(sa))
	h, err := windows.CreateFile(p, windows.FILE_APPEND_DATA,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, &sa,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, fmt.Errorf("open unit log %s: %w", path, err)
	}
	return h, nil
}

// unitShow answers the properties the shared policy asks, in systemd's vocabulary, from the job:
// a job with processes is active, one without is at rest, and no job is a unit nobody knows. The
// identifiers systemd keeps for a unit's file and invocation do not exist here and read as "".
func unitShow(unit, prop string) (string, error) {
	n, err := activeProcesses(unit)
	missing := errors.Is(err, errNoJob)
	if err != nil && !missing {
		return "", err
	}
	switch prop {
	case "ActiveState":
		if n > 0 {
			return "active", nil
		}
		return "inactive", nil
	case "LoadState":
		if missing {
			return "not-found", nil
		}
		return "loaded", nil
	}
	return "", nil
}

func unitIsActive(_ context.Context, unit string) bool {
	n, err := activeProcesses(unit)
	return err == nil && n > 0
}

// unitResetFailed has nothing to clear: a job with no processes and no handles is already gone.
func unitResetFailed(string) {}

// unitKill is `systemctl kill --signal=SIGKILL`: every process in the job, now.
func unitKill(unit string) {
	h, err := openJob(unit, jobTerminate)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateJobObject(h, 1)
}

// unitStopGrace bounds the wait for a killed job to empty. Termination is asynchronous; a process
// in the middle of an uninterruptible kernel call finishes it first.
const unitStopGrace = 30 * time.Second

// unitStop kills the job and waits for it to empty -- a job has no ExecStop to run first (the file
// header says where the clean stop lives instead).
func unitStop(unit string) ([]byte, error) {
	unitKill(unit)
	deadline := time.Now().Add(unitStopGrace)
	for {
		n, err := activeProcesses(unit)
		if errors.Is(err, errNoJob) || (err == nil && n == 0) {
			return nil, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("unit %s still has %d processes %s after the kill (%v)", unit, n, unitStopGrace, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// unitDisable has no file to unlink: a job enables nothing.
func unitDisable(string) ([]byte, error) { return nil, errors.ErrUnsupported }

// unitsReload has no unit files to re-read.
func unitsReload() ([]byte, error) { return nil, nil }

// unitTransient is true of every unit here: a job is the manager's own object, with no file of ours.
func unitTransient(string) bool { return true }

func unitStamp(string) string { return "" }

// serviceStopBound bounds a stop asked without patience: the candidate a revert stops, whose own
// stop is seconds. serviceStartBound is the Linux agent unit's TimeoutStartSec, so a start that
// fails there fails here too.
const (
	serviceStopBound  = 2 * time.Minute
	serviceStartBound = 60 * time.Second
)

// StopService stops a service through its ORDINARY stop path and waits, with patience, for it to
// finish what it is doing: the service manager's STOP, then a poll until the service reports
// STOPPED. Past the patience -- or at once, for a service still starting, which takes no stop --
// its process is ended. patience 0 is a plain stop, bounded by serviceStopBound. The
// self-update's stop: the agent drains its act lane on STOP, so "stopped" means "stopped between
// acts".
//
// ⚠️ AN ENDED PROCESS IS A FAILURE to the service manager, whose recovery starts the service
// again 5 s later. Every caller swaps and starts within that, and a recovery start that finds the
// service running is a no-op.
func StopService(ctx context.Context, name string, patience time.Duration, logf func(string, ...any)) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer s.Close()
	bound := patience
	if bound == 0 {
		bound = serviceStopBound
	}
	if st, err := s.Query(); err == nil && st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		logf("%s took no stop (%v); ending its process", name, err)
		bound = 0
	}
	if stopped, err := waitStopped(ctx, s, bound); stopped || err != nil {
		return err
	}
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("ask %s its state: %w", name, err)
	}
	if bound > 0 {
		logf("%s did not stop within %s; ending its process", name, bound)
	}
	if st.ProcessId != 0 {
		if err := EndProcess(st.ProcessId); err != nil {
			return fmt.Errorf("end %s's process %d: %w", name, st.ProcessId, err)
		}
	}
	stopped, err := waitStopped(ctx, s, unitStopGrace)
	if err == nil && !stopped {
		err = fmt.Errorf("%s has not stopped %s after its process was ended", name, unitStopGrace)
	}
	return err
}

// waitStopped polls a service until it reports STOPPED or bound passes.
func waitStopped(ctx context.Context, s *mgr.Service, bound time.Duration) (bool, error) {
	deadline := time.Now().Add(bound)
	for {
		st, err := s.Query()
		if err != nil {
			return false, fmt.Errorf("ask %s its state: %w", s.Name, err)
		}
		if st.State == svc.Stopped {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// openService opens a service holding only the rights asked for, through a connection to the
// service manager that asks only to connect: what the install user does unelevated -- `briard
// start`, the wait after `briard stop` -- holds no more than the service grants that account, and
// mgr.Connect's all-access is an administrator's alone.
func openService(name string, access uint32) (*mgr.Service, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, fmt.Errorf("the service manager: %w", err)
	}
	defer windows.CloseServiceHandle(m)
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenService(m, p, access)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	return &mgr.Service{Name: name, Handle: h}, nil
}

// StartService starts a service and returns once it reported RUNNING -- for the agent, READY --
// or with why it did not: it stopped again while starting, or was still starting at
// serviceStartBound. A service already running counts as started.
func StartService(ctx context.Context, name string) error {
	s, err := openService(name, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start %s: %w", name, err)
	}
	deadline := time.Now().Add(serviceStartBound)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("ask %s its state: %w", name, err)
		}
		switch st.State {
		case svc.Running:
			return nil
		case svc.Stopped:
			return fmt.Errorf("start %s: it stopped while starting (exit code %d)", name, st.Win32ExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("start %s: still starting after %s", name, serviceStartBound)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// EndProcess ends a process outright, as a kill does on Linux.
func EndProcess(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

// guestExitBound bounds the wait for qemu's process to exit once its monitor has gone quiet.
const guestExitBound = 30 * time.Second

// PowerOffGuest stops the guest cleanly and returns once its process has exited: the powerdown
// over the monitor, then the wait for the guest's job to empty. A QUIET MONITOR IS NOT AN EXITED
// QEMU: qemu closes its socket on its way out, and a guest launched by an agent that stopped mid
// bring-up has no monitor up yet at all. A caller may go on to delete or replace files that process
// holds, so the kill is only the last resort past the bound. A job is killed, never stopped
// (unitStop), so the clean half is this function's own.
func PowerOffGuest(ctx context.Context, qmpSock string, logf func(string, ...any)) error {
	gctx, cancel := context.WithTimeout(ctx, GuestShutdownGrace+15*time.Second)
	defer cancel()
	if err := ShutdownVM(gctx, qmpSock, GuestShutdownGrace); err != nil {
		return err
	}
	if !unitIsActive(ctx, GuestUnit) {
		return nil
	}
	logf("waiting for the guest's process to exit")
	deadline := time.Now().Add(guestExitBound)
	for unitIsActive(ctx, GuestUnit) {
		if time.Now().After(deadline) {
			logf("the guest's process has not exited %s after its powerdown; ending it", guestExitBound)
			return RemoveUnit(GuestUnit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// SystemState is "" here: the service manager tells the agent a shutdown is a shutdown
// (PRESHUTDOWN), so nothing asks.
func SystemState(context.Context) string { return "" }

// ServiceStopped reports whether a service is down and staying down: STOPPED by a clean exit. A
// non-zero exit is a failure, which the service's recovery starts again 5 s later.
func ServiceStopped(name string) (bool, error) {
	s, err := openService(name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false, err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return false, fmt.Errorf("ask %s its state: %w", name, err)
	}
	return stoppedStatus(st), nil
}

// stoppedStatus is ServiceStopped's reading of a service's status.
func stoppedStatus(st svc.Status) bool {
	return st.State == svc.Stopped && st.Win32ExitCode == 0 && st.ServiceSpecificExitCode == 0
}
