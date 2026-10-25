// The node's way down, and `briard stop` / `briard start`.
//
// TWO STOPS, BECAUSE THEY HAVE OPPOSITE PRIORITIES. A STOP -- an update, a restart, a service
// manager's stop -- swaps the agent under a guest that keeps serving: the act in flight finishes
// and the guest is never touched (acts.go, stopAfter). A SHUTDOWN brings the node down: the loop
// and its act are cancelled at once -- the act is worthless, its guest is about to leave, and an
// install keeps its stage and rollback point while a directive with no outcome is re-delivered --
// then the guest is powered off cleanly, then the agent returns. Draining at a shutdown cost both
// ways: the wait outlasted the time a machine gives its services, so the halt power-cut the guest
// the drain was keeping, and the still-running loop saw its guest leave and relaunched it.
//
// One way down, three triggers, all a context cancelled with ErrShutdown as its cause: the Windows
// PRESHUTDOWN, a SIGTERM while systemd reports the machine stopping (StopCause), and the local
// stop directive, which is `briard stop`. A clean exit stays down on both systems -- the agent unit
// restarts on failure only, and the service manager's recovery acts on a non-zero exit -- so down
// needs nothing disabled, and the next boot brings briard back.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"time"

	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
)

// ErrShutdown is the cause the agent's context is cancelled with when the node is going down
// rather than the agent restarting.
var ErrShutdown = errors.New("briard is shutting down")

// shuttingDown reports whether ctx ended as a shutdown.
func shuttingDown(ctx context.Context) bool { return errors.Is(context.Cause(ctx), ErrShutdown) }

// StopCause is what a SIGTERM means, given the service manager's word for where the machine is
// (platform.SystemState): a shutdown while systemd is stopping -- it answers "stopping" for the
// whole of a shutdown or a reboot, the moment it signals the agent included -- and an ordinary
// stop otherwise, nil. The signal is the same for both; only the manager knows which it sent.
func StopCause(systemState string) error {
	if systemState == "stopping" {
		return ErrShutdown
	}
	return nil
}

// shutdownBound bounds the shutdown's own work, the guest's clean powerdown: inside the two
// minutes Windows gives a preshutdown (preshutdownBudget), and leased, so the watchdog counts it.
const shutdownBound = platform.GuestShutdownGrace + 45*time.Second

// shutDown is the end of Run on a shutdown: the loop has returned, so nothing is left to relaunch
// the guest, and the guest is powered off cleanly -- the same line on both systems
// (platform.PowerOffGuest).
func (cfg Config) shutDown(ctx context.Context, logf func(string, ...any)) {
	sctx, cancel := cfg.beat.budget(context.WithoutCancel(ctx), shutdownBound)
	defer cancel()
	logf("shutting down: powering the guest off cleanly")
	if err := platform.PowerOffGuest(sctx, cfg.QMPSock, logf); err != nil {
		logf("shutting down: the guest's powerdown: %v", err)
		return
	}
	logf("shutting down: the guest is powered off")
}

// applyStop is the stop directive, on the loop: it answers with what the shutdown aborts, then
// starts it. Never refused "busy" -- a refusal would need a force flag -- and refused only while a
// stop is already draining, which ends with the guest still running.
func (cfg Config) applyStop(d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	kind, stopping := cfg.acts.state()
	if stopping || cfg.down == nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed,
			Detail: "the agent is already stopping and leaves the VM running; run `briard stop` again once it has stopped"}
	}
	detail := "briard is stopping; its VM powers off cleanly"
	if kind != "" {
		detail = "aborted the " + kind + " in flight; " + detail
	}
	logf("stop: asked on the admin socket -- %s", detail)
	cfg.down(ErrShutdown)
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: detail}
}

// ErrNoAgent is a Submit that reached no agent: nothing answers on the admin socket.
// ErrNoOutcome is one whose agent hung up without answering.
var (
	ErrNoAgent   = errors.New("nothing answers on the admin socket")
	ErrNoOutcome = errors.New("the agent closed the connection without reporting an outcome")
)

// Submit sends one directive to the running agent over its admin socket and blocks for the
// outcome: the local door's client (serveLocal is its server), and every injector's. No deadline
// of its own -- an install legitimately runs for minutes -- so a caller wanting one puts it on ctx.
func Submit(ctx context.Context, sock string, d api.Directive) (api.DirectiveOutcome, error) {
	var zero api.DirectiveOutcome
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return zero, fmt.Errorf("%w: %w", ErrNoAgent, doorRefusal(err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(d); err != nil {
		return zero, fmt.Errorf("submitting the directive: %w", err)
	}
	var o api.DirectiveOutcome
	if err := json.NewDecoder(conn).Decode(&o); err != nil {
		if errors.Is(err, io.EOF) {
			return zero, ErrNoOutcome
		}
		return zero, fmt.Errorf("reading the outcome: %w", err)
	}
	return o, nil
}

// stopWait bounds the wait for the agent's service to stop once the agent has started its
// shutdown: the guest's powerdown, then the exit.
const stopWait = shutdownBound + 30*time.Second

// Stop is `briard stop`, and an uninstall's first step: the node down -- the act in flight
// aborted and named, the guest powered off cleanly, the agent's service stopped -- until `briard
// start` or the next boot. THE FALLBACK IS PART OF THE VERB: an agent that is not there to ask
// (crashed, crash-looping, half installed) may have left a qemu running, so with nobody answering
// the service is stopped through the manager and the guest powered off from here (stopNode).
func (cfg Config) Stop(ctx context.Context, say func(string, ...any)) error {
	o, err := Submit(ctx, cfg.AdminSock, api.Directive{Kind: api.DirectiveStop})
	switch {
	case errors.Is(err, ErrNoAgent) && !errors.Is(err, fs.ErrPermission):
		say("the agent does not answer (%v); stopping briard through the service manager", err)
		if err := stopNode(ctx, cfg.QMPSock, say); err != nil {
			return err
		}
	case errors.Is(err, ErrNoOutcome):
		say("the agent hung up without answering; waiting for it to stop")
		if err := waitStopped(ctx); err != nil {
			return err
		}
	case err != nil:
		return err
	case o.State != api.OutcomeDone:
		return errors.New(o.Detail)
	default:
		say("%s", o.Detail)
		if err := waitStopped(ctx); err != nil {
			return err
		}
	}
	return nil
}

// waitStopped waits, bounded, for the agent's service to be down and staying down.
func waitStopped(ctx context.Context) error {
	deadline := time.Now().Add(stopWait)
	for {
		down, err := platform.ServiceStopped(selfupdate.DefaultAgentUnit)
		if err == nil && down {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("asking whether briard stopped: %w", err)
			}
			return fmt.Errorf("briard's service has not stopped %s after the agent began its shutdown", stopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Start is `briard start`: the agent's service started, which brings the guest back. It returns
// once the agent reported that it started, never once the node serves -- the guest's bring-up
// comes after.
func Start(ctx context.Context, say func(string, ...any)) error {
	say("starting briard")
	if err := StartAgent(ctx, say); err != nil {
		return err
	}
	say("briard is running and starting its VM; `briard doctor` says when your apps serve again")
	return nil
}
