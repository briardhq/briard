package cli

// `briard debug shell` — a root shell on the guest's second serial port.
//
// UNDOCUMENTED ON PURPOSE. It is dispatched from Main ahead of the `commands` table rather than
// being a row in it, so it appears in no help, no listing and no completion; `briard help debug`
// says the command does not exist, which is the honest answer for a verb that is not part of the
// product. The way anyone learns it is a support answer that names it.
//
// That is a policy, not a boundary, and the difference matters. The gate underneath is QMP,
// whose socket lives in a 0700 root directory, and a caller who can reach QMP can already reset
// this VM and dump its RAM. Nothing here withholds a capability from anyone who has that; what
// it withholds is the SUGGESTION that poking at the guest is a supported way to run the product.
// The product's answers to "something is wrong" are `alerts`, `logs` and `rescue`.
//
// WHAT KEEPS THIS SAFE IS NOT THE LOCK. The guest is a disposable appliance ([B.86]): its OS
// moves by image swap and `briard rescue` rebuilds it, so whatever an operator edits in there is
// erased at the next update, and only the replicated data volume survives. A shell here is a
// window, not a place to keep anything.
//
// ARM AND DISARM ARE THE AGENT'S ([B.142a]), submitted over the admin socket as directive kinds
// the cloud's down-channel is refused (host.localOnlyKinds). This client keeps only what is
// inherently the operator's: the terminal, the escape key, and the guarantee that every path out
// of here disarms. The agent owns the act so that it owns the RECORD -- its log reaches the
// journal, where an interactive process's stderr never could.
//
// What that gate does NOT do is stop a remote root shell, and the earlier claim that it did is
// withdrawn: arming publishes a socket inside the 0700 root QMP directory, which nothing remote
// can reach and no directive proxies. A cloud able to arm could leave a door open, not walk
// through one -- walking through needs host root, which is already QMP, which is already this
// guest's RAM. What the gate says is that the cloud has no business changing local debug state.
//
// The trade it costs: with the agent's observe loop wedged, no console. That is deliberate. The
// user this product is built for should be deterred from operating on an already-degraded node,
// and anyone equipped to drive QMP by hand is not that user.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"briard.io/shared/api"
)

// escapeByte is Ctrl-] — the telnet/socat convention for "let me out". It is needed because the
// guest end is an autologin getty: `exit` just logs out and the getty hands you a fresh shell,
// so the session has no natural end and the terminal is in raw mode besides.
const escapeByte = 0x1d

// disarmTimeout bounds the cleanup submit. Unlike `submit`'s deliberately unbounded wait (an
// upgrade legitimately runs for minutes), this one runs while the operator waits to get their
// terminal back, and the op behind it is a single monitor call.
const disarmTimeout = 20 * time.Second

// runDebug dispatches `briard debug <subcommand>`. One subcommand today; the level exists so
// that if a second debugging tool is ever wanted it lands here rather than growing the
// supported surface.
func runDebug(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "shell" {
		fmt.Fprint(stderr, "usage: briard debug shell\n")
		return 2
	}
	return runDebugShell(ctx, args[1:], stdout, stderr)
}

func runDebugShell(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("briard debug shell", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", sockDefault(), "the agent's admin socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard debug shell: takes no arguments\n")
		return 2
	}

	// ARM. The failures worth naming are the three the operator can act on, and they are now
	// three rather than two: no agent (or not root -- `submit` says which), no guest running,
	// or a guest launched by an agent old enough not to give it a debug port at all (QEMU
	// refuses to change a chardev it has no record of). There is no -qmp here any more: the
	// agent derives the console path from its own monitor, which is what keeps the socket in
	// the 0700 root directory with no second setting able to move it.
	o, err := submit(ctx, *sock, api.Directive{Kind: api.DirectiveDebugArm})
	if err != nil {
		fmt.Fprintf(stderr, "briard debug shell: %v\n", err)
		return 1
	}
	if o.State != api.OutcomeDone {
		fmt.Fprintf(stderr, "briard debug shell: %s\n", o.Detail)
		fmt.Fprintf(stderr, "  (is a guest running? `briard logs` says. A guest launched by an\n"+
			"   older agent has no debug port and must be relaunched to gain one.)\n")
		return 1
	}
	console := o.Detail

	// DISARM ON EVERY PATH OUT, including the signals a terminal delivers, because an armed node
	// is a node left open. The one hole is SIGKILL, which nothing can close from in here -- the
	// socket's existence is what makes that state visible afterwards.
	disarmed := false
	disarm := func() {
		if disarmed {
			return
		}
		disarmed = true
		// A FRESH CONTEXT, NOT ctx: the usual reason we are unwinding is that ctx was cancelled,
		// and cleanup that inherits the cancellation is cleanup that does not run.
		dctx, dcancel := context.WithTimeout(context.Background(), disarmTimeout)
		defer dcancel()
		// The terminal is already restored by the time this runs (restore is deferred later,
		// so it unwinds first), which is why these are plain newlines and not raw-mode CRLF.
		//
		// Loud on stderr for BOTH failure shapes -- the submit never landing, and the agent
		// reporting it could not disarm. The operator is the only one who can fix a node that
		// stayed open and is about to walk away from the terminal; the agent has already written
		// its own side to the journal.
		left := func(reason string) {
			fmt.Fprintf(stderr, "\nbriard debug shell: FAILED TO DISARM: %s\n"+
				"  the console at %s is still open; relaunching the guest also closes it\n", reason, console)
		}
		od, err := submit(dctx, *sock, api.Directive{Kind: api.DirectiveDebugDisarm})
		if err != nil {
			left(err.Error())
			return
		}
		if od.State != api.OutcomeDone {
			left(od.Detail)
		}
	}
	defer disarm()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	conn, err := dialConsole(ctx, console)
	if err != nil {
		fmt.Fprintf(stderr, "briard debug shell: %v\n", err)
		return 1
	}
	defer conn.Close()

	fmt.Fprint(stdout, banner)

	// RAW MODE VIA stty, not a terminal library, to keep this from adding a module dependency
	// for one verb nobody is meant to use. Degrading to line mode is survivable (doubled echo,
	// no Ctrl-C to the guest, no tab completion) where refusing to open at all is not, so a
	// missing or unhappy stty is a warning and not an error.
	restore, err := rawMode()
	if err != nil {
		fmt.Fprintf(stderr, "briard debug shell: no raw terminal (%v); line-mode only\n", err)
	}
	defer restore()

	guestDone := make(chan struct{})
	go func() { defer close(guestDone); _, _ = io.Copy(stdout, conn) }()
	inputDone := make(chan struct{})
	go func() { defer close(inputDone); relayInput(conn) }()

	select {
	case <-guestDone: // QEMU hung up: the guest stopped, or someone else disarmed us
	case <-inputDone: // the operator pressed the escape key, or stdin ended
	case <-sigs:
	case <-ctx.Done():
	}
	// The deferred restore + disarm run here. A goroutine still blocked on os.Stdin.Read is
	// fine: it dies with the process, and nothing it could still write matters.
	return 0
}

const banner = `
!! You are about to enter the guest OS as root. This is not a supported way to
!! operate a Briard node, and there is nothing in here you are meant to change.
!!
!! Anything you edit in the guest's own filesystem is TEMPORARY: the OS moves by
!! whole-image swap and ` + "`briard rescue`" + ` rebuilds it, so your change is erased at
!! the next update, silently and without warning you first. Services, mounts and
!! network here are managed by the agent, which will fight you for them and win.
!!
!! You can break this node from in here in ways ` + "`briard alerts`" + ` will not explain.

Press Enter for a prompt (the shell's own is already in the log, not on the wire).
Ctrl-] to leave — that also closes the console again.

`

// dialConsole connects to the socket QEMU has just started serving. Briefly retried because arm
// and listen are not the same instant: the monitor answers when the chardev has been changed,
// and the listening socket appears a hair later.
func dialConsole(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to the armed console at %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// relayInput copies the terminal to the guest until the escape byte, which is consumed rather
// than forwarded. Not io.Copy, because the escape is the only way out of a raw-mode session
// whose far end is an autologin getty that never closes.
func relayInput(conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for i, b := range chunk {
				if b == escapeByte {
					_, _ = conn.Write(chunk[:i])
					return
				}
			}
			if _, werr := conn.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// rawMode puts the terminal into raw mode and returns the undo. When stdin is not a terminal
// (a pipe, a test) it is a no-op pair rather than an error: there is nothing to make raw.
func rawMode() (func(), error) {
	noop := func() {}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return noop, nil
	}
	saved, err := stty("-g")
	if err != nil {
		return noop, err
	}
	if _, err := stty("raw", "-echo"); err != nil {
		return noop, err
	}
	return func() { _, _ = stty(saved) }, nil
}

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("stty %v: %w", args, err)
	}
	return string(trimNewline(out)), nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// The record that a node was opened is the AGENT's now ([B.142a]) -- host.applyDebugConsole logs
// it, and systemd puts the agent's stderr in the journal. What stood here was a syslog writer,
// which existed only because an interactive process's stderr is the operator's terminal and so
// reaches no journal at all; it also pinned `log/syslog`, which has no Windows build and broke
// the GOOS seam for every package in this module.
