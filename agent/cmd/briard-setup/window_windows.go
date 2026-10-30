package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// THE WINDOW is Windows' own task dialog -- the dialog an installer of Windows' shows: Briard's
// icon, a bold line, the step under it and a moving bar while the install runs, then the same
// window turned to how it ended. The install runs beside it and sends it each line; the dialog
// runs its own message loop on this thread until it is closed.
//
// THE TASK DIALOG IS COMMON CONTROLS 6, which a program gets only by asking for it in its
// manifest -- briard-setup.manifest, compiled into the program by the release build
// (briard-setup.rc). A build without it opens no window: it says so in a message box and installs
// nothing.
//
// THE INSTALL CANNOT BE STOPPED HALFWAY -- install.ps1 has no point to stop at that leaves
// nothing behind -- so while it runs the window's one button is disabled and closing it is refused.

var (
	procTaskDialogIndirect = windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect")
	procSendMessageW       = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageW")
)

const (
	tdfShowMarqueeProgressBar = 0x0400
	tdcbfCancelButton         = 0x0008
	tdcbfCloseButton          = 0x0020

	tdnCreated       = 0
	tdnButtonClicked = 2

	wmUser                   = 0x0400
	tdmNavigatePage          = wmUser + 101
	tdmSetProgressBarMarquee = wmUser + 107
	tdmSetElementText        = wmUser + 108
	tdmEnableButton          = wmUser + 111
	tdeContent               = 0

	tdErrorIcon = 0xFFFE // MAKEINTRESOURCE(-2)
	iconID      = 1      // briard.ico in briard-setup.rc
	idCancel    = 2
	idRestart   = 100
	idLater     = 101

	// The width the window keeps from page to page, in dialog units, so a step's text changes
	// its height at most and never its width.
	dialogWidth = 280
)

// dialog is the one setup window: its handle once it exists, and what each page's layout points
// into, kept reachable until the window is gone.
type dialog struct {
	hwnd    uintptr
	created chan struct{}
	once    sync.Once
	done    atomic.Bool // the install has ended: the window may close
	inst    uintptr
	keep    []any
}

var theDialog *dialog

// installWindow runs the install in the window, writing its output to log as well, and returns its
// exit code; on Restart now it restarts Windows, as install.ps1's own ending does.
func installWindow(log io.Writer, logPath string) int {
	if err := procTaskDialogIndirect.Find(); err != nil {
		tell("Briard Setup cannot open its window (" + err.Error() + "), so it installed nothing.\n\n" +
			"This build of it lacks its manifest; the published one has it.")
		return 1
	}
	runtime.LockOSThread() // the dialog's thread is the one that made it
	d := &dialog{created: make(chan struct{})}
	var h windows.Handle
	windows.GetModuleHandleEx(0, nil, &h)
	d.inst = uintptr(h)
	theDialog = d

	code := 1
	go func() {
		<-d.created
		lines := &lineWriter{d: d}
		code = runInstall(io.MultiWriter(log, lines))
		lines.flush()
		d.done.Store(true)
		d.navigate(ending(code, lines.tail, logPath))
	}()
	cfg := d.layout(installing)
	var pressed int32
	if hr, _, _ := procTaskDialogIndirect.Call(uintptr(unsafe.Pointer(&cfg[0])), uintptr(unsafe.Pointer(&pressed)), 0, 0); hr != 0 {
		tell(fmt.Sprintf("Briard Setup could not open its window (error 0x%08X), so it installed nothing.", hr))
		return 1
	}
	if code == 3010 && pressed == idRestart {
		restart()
	}
	return code
}

// callback is the dialog's: start the bar and disable the button when the window opens, and keep
// it open until the install has ended.
var callback = syscall.NewCallback(func(hwnd, msg, wparam, lparam, data uintptr) uintptr {
	d := theDialog
	switch msg {
	case tdnCreated:
		d.hwnd = hwnd
		sendMessage(hwnd, tdmSetProgressBarMarquee, 1, 30)
		sendMessage(hwnd, tdmEnableButton, idCancel, 0)
		d.once.Do(func() { close(d.created) })
	case tdnButtonClicked:
		if !d.done.Load() {
			return 1 // S_FALSE: the window stays
		}
	}
	return 0
})

// step shows one line of the install's output under the bold line.
func (d *dialog) step(line string) {
	if s := step(line); s != "" {
		t, _ := windows.UTF16FromString(s)
		sendMessage(d.hwnd, tdmSetElementText, tdeContent, uintptr(unsafe.Pointer(&t[0])))
		runtime.KeepAlive(t)
	}
}

// navigate turns the window to p.
func (d *dialog) navigate(p page) {
	cfg := d.layout(p)
	sendMessage(d.hwnd, tdmNavigatePage, 0, uintptr(unsafe.Pointer(&cfg[0])))
	runtime.KeepAlive(cfg)
}

// layout is p as a TASKDIALOGCONFIG: commctrl.h declares it packed to one byte, which no Go
// struct can be, so it is written field by field, in its order, into a byte slice.
func (d *dialog) layout(p page) []byte {
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	ptr := func(v uintptr) { binary.Write(&b, binary.LittleEndian, uint64(v)) }
	str := func(s string) uintptr {
		if s == "" {
			return 0
		}
		t, _ := windows.UTF16FromString(s)
		d.keep = append(d.keep, t)
		return uintptr(unsafe.Pointer(&t[0]))
	}
	flags, common, icon := uint32(0), uint32(tdcbfCloseButton), uintptr(iconID)
	var buttons []byte
	switch {
	case p.running:
		flags, common = tdfShowMarqueeProgressBar, tdcbfCancelButton
	case p.restart:
		common = 0
		for _, bt := range []struct {
			id   int32
			text string
		}{{idRestart, "Restart now"}, {idLater, "Later"}} {
			var e bytes.Buffer
			binary.Write(&e, binary.LittleEndian, bt.id)
			binary.Write(&e, binary.LittleEndian, uint64(str(bt.text)))
			buttons = append(buttons, e.Bytes()...)
		}
		d.keep = append(d.keep, buttons)
	}
	if p.failed {
		icon = tdErrorIcon
	}
	u32(160)                       // cbSize
	ptr(0)                         // hwndParent
	ptr(d.inst)                    // hInstance
	u32(flags)                     // dwFlags
	u32(common)                    // dwCommonButtons
	ptr(str("Briard Setup"))       // pszWindowTitle
	ptr(icon)                      // pszMainIcon: a resource of hInstance, by its id
	ptr(str(p.instruction))        // pszMainInstruction
	ptr(str(p.content))            // pszContent
	u32(uint32(len(buttons) / 12)) // cButtons
	if len(buttons) > 0 {
		ptr(uintptr(unsafe.Pointer(&buttons[0]))) // pButtons
	} else {
		ptr(0)
	}
	u32(idRestart)        // nDefaultButton (a page without it takes its first)
	u32(0)                // cRadioButtons
	ptr(0)                // pRadioButtons
	u32(0)                // nDefaultRadioButton
	ptr(0)                // pszVerificationText
	ptr(str(p.details))   // pszExpandedInformation
	ptr(str(detailsHide)) // pszExpandedControlText
	ptr(str(detailsShow)) // pszCollapsedControlText
	ptr(0)                // pszFooterIcon
	ptr(0)                // pszFooter
	ptr(callback)         // pfCallback
	ptr(0)                // lpCallbackData
	u32(dialogWidth)      // cxWidth
	cfg := b.Bytes()
	d.keep = append(d.keep, cfg)
	return cfg
}

const (
	detailsShow = "Show details"
	detailsHide = "Hide details"
)

func sendMessage(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

// lineWriter hands the window the install's output a line at a time, and keeps its last lines
// for the page a failure ends on.
type lineWriter struct {
	d    *dialog
	part []byte
	tail []string
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.part = append(w.part, b...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			return len(b), nil
		}
		w.line(string(w.part[:i]))
		w.part = w.part[i+1:]
	}
}

func (w *lineWriter) flush() {
	if len(w.part) > 0 {
		w.line(string(w.part))
		w.part = nil
	}
}

func (w *lineWriter) line(l string) {
	l = strings.TrimRight(l, "\r ")
	if strings.TrimSpace(l) == "" {
		return
	}
	w.d.step(l)
	if w.tail = append(w.tail, l); len(w.tail) > detailLines {
		w.tail = w.tail[1:]
	}
}
