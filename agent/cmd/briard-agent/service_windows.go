package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"

	"briard.io/agent/host"
	"briard.io/agent/platform"
	"briard.io/shared/sdnotify"
)

// serviceName is the agent's name in the service manager.
const serviceName = "briard-agent"

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
	if err := svc.Run(serviceName, agentService{}); err != nil {
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
func (agentService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: uint32(startHint / time.Millisecond)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runHost(ctx) }()
	qmp := host.ConfigFromEnv().QMPSock
	ready := sdnotify.Readied()
	for {
		select {
		case <-ready:
			status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptPreShutdown}
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
