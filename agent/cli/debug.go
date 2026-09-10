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
// It deliberately does NOT go through the agent's admin socket. That socket carries
// api.Directives, and the cloud enqueues directives too -- a debug-shell directive kind would
// hand the cloud (or anyone who took it) a root shell into every household, unilaterally. This
// talks straight to the local monitor instead, so the capability has no remote edge at all.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/syslog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"briard.io/agent/platform"
)

// defaultQMPSock mirrors ConfigFromEnv's QMP_SOCK default (agent/host/config.go), for the same
// reason defaultSock mirrors ADMIN_SOCK: the alternative is a shared const in shared/api, which
// would put a deployment path in the closed wire-contract package. Keep the two in step.
const defaultQMPSock = "/run/briard/qmp/guest.sock"

// consoleSockName is the debug console's socket, beside the monitor that arms it. DERIVED from
// the QMP path rather than configured separately, so there is one literal to keep in step and
// no way to point the two at different directories -- the 0700 one is the only place this may
// live.
const consoleSockName = "console.sock"

// escapeByte is Ctrl-] — the telnet/socat convention for "let me out". It is needed because the
// guest end is an autologin getty: `exit` just logs out and the getty hands you a fresh shell,
// so the session has no natural end and the terminal is in raw mode besides.
const escapeByte = 0x1d

func qmpSockDefault() string {
	if s := os.Getenv("QMP_SOCK"); s != "" {
		return s
	}
	return defaultQMPSock
}

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
	qmp := fs.String("qmp", qmpSockDefault(), "the guest's QMP monitor socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, "briard debug shell: takes no arguments\n")
		return 2
	}
	console := filepath.Join(filepath.Dir(*qmp), consoleSockName)

	audit := openAudit()
	defer audit.Close()

	// ARM. A failure here is almost always one of two things and the message says which: no
	// guest running (nothing listening on the monitor), or a guest launched by an agent old
	// enough not to give it a debug port at all (QEMU refuses to change a chardev it has no
	// record of).
	armCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := platform.DebugArm(armCtx, *qmp, console); err != nil {
		fmt.Fprintf(stderr, "briard debug shell: %v\n", err)
		fmt.Fprintf(stderr, "  (is a guest running? `briard logs` says. A guest launched by an\n"+
			"   older agent has no debug port and must be relaunched to gain one.)\n")
		return 1
	}
	audit.log("debug console ARMED on %s by uid=%d", console, os.Geteuid())

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
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dcancel()
		// The terminal is already restored by the time this runs (restore is deferred later,
		// so it unwinds first), which is why these are plain newlines and not raw-mode CRLF.
		if err := platform.DebugDisarm(dctx, *qmp); err != nil {
			// Loud, and on stderr as well as the journal: the operator is the only one who can
			// fix a node that stayed open, and they are about to walk away from the terminal.
			fmt.Fprintf(stderr, "\nbriard debug shell: FAILED TO DISARM: %v\n"+
				"  the console at %s is still open; relaunching the guest also closes it\n", err, console)
			audit.log("debug console DISARM FAILED on %s: %v", console, err)
			return
		}
		audit.log("debug console disarmed on %s", console)
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

// auditLog is the record that a node was opened. It goes to syslog rather than to the CLI's own
// stdout because the operator's terminal is exactly where it will not be found later: the whole
// point is that the next person to look at this node's journal can see that somebody was inside
// it, and when. Best-effort -- a node with no /dev/log still gets its shell.
type auditLog struct{ w *syslog.Writer }

func openAudit() auditLog {
	w, err := syslog.New(syslog.LOG_NOTICE|syslog.LOG_AUTH, "briard")
	if err != nil {
		return auditLog{}
	}
	return auditLog{w: w}
}

func (a auditLog) log(format string, args ...any) {
	if a.w == nil {
		return
	}
	_ = a.w.Notice(fmt.Sprintf(format, args...))
}

func (a auditLog) Close() {
	if a.w != nil {
		_ = a.w.Close()
	}
}
