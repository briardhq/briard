package main

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"briard.io/agent/host"
	"briard.io/agent/platform"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
)

// THE WIN32 HALF: one hidden window, which owns the notification-area icon and receives its
// clicks, a message loop on a locked thread, and a poll beside it. Everything the icon does is on
// the window's thread; the poll and the menu's actions run off it and post back.
//
// Through x/sys/windows and the system DLLs directly: the calls are a dozen, and a GUI library
// would be a dependency for the price of this file.

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	gdi32   = windows.NewLazySystemDLL("gdi32.dll")

	registerClassEx       = user32.NewProc("RegisterClassExW")
	createWindowEx        = user32.NewProc("CreateWindowExW")
	defWindowProc         = user32.NewProc("DefWindowProcW")
	destroyWindow         = user32.NewProc("DestroyWindow")
	getMessage            = user32.NewProc("GetMessageW")
	translateMessage      = user32.NewProc("TranslateMessage")
	dispatchMessage       = user32.NewProc("DispatchMessageW")
	postMessage           = user32.NewProc("PostMessageW")
	postQuitMessage       = user32.NewProc("PostQuitMessage")
	registerWindowMessage = user32.NewProc("RegisterWindowMessageW")
	createPopupMenu       = user32.NewProc("CreatePopupMenu")
	appendMenu            = user32.NewProc("AppendMenuW")
	setMenuDefaultItem    = user32.NewProc("SetMenuDefaultItem")
	trackPopupMenu        = user32.NewProc("TrackPopupMenu")
	destroyMenu           = user32.NewProc("DestroyMenu")
	setForegroundWindow   = user32.NewProc("SetForegroundWindow")
	getCursorPos          = user32.NewProc("GetCursorPos")
	createIconIndirect    = user32.NewProc("CreateIconIndirect")
	shellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	createDIBSection      = gdi32.NewProc("CreateDIBSection")
	createBitmap          = gdi32.NewProc("CreateBitmap")
	deleteObject          = gdi32.NewProc("DeleteObject")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmContextMenu = 0x007B
	wmApp         = 0x8000
	msgIcon       = wmApp + 1 // the icon's callback
	msgRead       = wmApp + 2 // the poll or an action has something to show

	ninSelect           = 0x0400
	ninKeySelect        = 0x0401
	ninBalloonUserClick = 0x0405

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4
	nifMessage    = 0x01
	nifIcon       = 0x02
	nifTip        = 0x04
	nifInfo       = 0x10
	nifShowTip    = 0x80
	niifInfo      = 0x01
	niifWarning   = 0x02
	notifyIconV4  = 4

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfSeparator = 0x0800
	tpmRight    = 0x0002
	tpmNoNotify = 0x0080
	tpmRetCmd   = 0x0100

	cmdOpen  = 1
	cmdStop  = 2
	cmdStart = 3
)

// pollEvery is how often the icon asks the agent; an action asks again as soon as it returns.
const pollEvery = 5 * time.Second

type notifyIconData struct {
	CbSize          uint32
	Wnd             windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msg struct {
	Wnd     windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	Private uint32
}

type tray struct {
	wnd      windows.HWND
	sock     string
	icons    [4]windows.Handle
	created  uint32 // the TaskbarCreated message: Explorer restarted, the icon must be added again
	wake     chan struct{}
	relaunch bool

	mu      sync.Mutex
	last    reading
	prev    *host.LocalStatus // the last answer, which a toast is news against
	pending string            // see reading.pending
	toastN  int               // a toast to show: the open count it is about; 0 none
	note    string            // a balloon to show: an action's failure, in words
}

var t *tray

func main() {
	runtime.LockOSThread()
	// One per session: a second start at logon -- the Run key and a reinstall's own start, say --
	// leaves the first in charge.
	name, _ := windows.UTF16PtrFromString(`Local\briard-tray`)
	mutex, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return
	}
	exe, _ := os.Executable()
	self, _ := os.Stat(exe)

	t = &tray{sock: host.ConfigFromEnv().AdminSock, wake: make(chan struct{}, 1)}
	for _, s := range []state{serving, degraded, stopped, working} {
		t.icons[s] = icon(art(s))
	}
	created, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := registerWindowMessage.Call(uintptr(unsafe.Pointer(created)))
	t.created = uint32(r)
	if err := t.window(); err != nil {
		return
	}
	t.last = reading{err: host.ErrNoAgent}
	t.add()
	go t.poll(exe, self)

	var m msg
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	// AN UPDATE REPLACED THIS PROGRAM: the new one takes over. The mutex goes first, or it would
	// find this one still holding the session.
	if t.relaunch {
		windows.CloseHandle(mutex)
		_ = exec.Command(exe).Start()
	}
}

func (t *tray) window() error {
	inst := windows.Handle(0)
	_ = windows.GetModuleHandleEx(0, nil, &inst)
	class, _ := windows.UTF16PtrFromString("BriardTray")
	wc := wndClassEx{WndProc: windows.NewCallback(wndProc), Instance: inst, ClassName: class}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := registerClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return err
	}
	// A hidden top-level window, never shown -- not a message-only one, which Explorer's
	// TaskbarCreated broadcast does not reach.
	title, _ := windows.UTF16PtrFromString("Briard")
	w, _, err := createWindowEx.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0,
		0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if w == 0 {
		return err
	}
	t.wnd = windows.HWND(w)
	return nil
}

func wndProc(wnd windows.HWND, m uint32, wp, lp uintptr) uintptr {
	switch {
	case m == msgIcon:
		switch lp & 0xffff {
		case ninSelect, ninKeySelect, ninBalloonUserClick:
			go t.open()
		case wmContextMenu:
			t.menu()
		}
		return 0
	case m == msgRead:
		t.show()
		return 0
	case m == t.created && m != 0:
		t.add()
		return 0
	case m == wmClose:
		destroyWindow.Call(uintptr(wnd))
		return 0
	case m == wmDestroy:
		t.notify(nimDelete, func(*notifyIconData) {})
		postQuitMessage.Call(0)
		return 0
	}
	r, _, _ := defWindowProc.Call(uintptr(wnd), uintptr(m), wp, lp)
	return r
}

// notify is one Shell_NotifyIcon call on the one icon, the data filled in by set.
func (t *tray) notify(op uintptr, set func(*notifyIconData)) {
	d := notifyIconData{Wnd: t.wnd, ID: 1}
	d.CbSize = uint32(unsafe.Sizeof(d))
	set(&d)
	shellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&d)))
}

func (t *tray) add() {
	t.mu.Lock()
	v := show(t.last)
	t.mu.Unlock()
	t.notify(nimAdd, func(d *notifyIconData) {
		d.Flags = nifMessage | nifIcon | nifTip | nifShowTip
		d.CallbackMessage = msgIcon
		d.Icon = t.icons[v.state]
		copyUTF16(d.Tip[:], v.tip)
	})
	t.notify(nimSetVersion, func(d *notifyIconData) { d.Version = notifyIconV4 })
}

// show puts the latest reading on the icon, and the toast or the note it brought, if any.
func (t *tray) show() {
	t.mu.Lock()
	v := show(t.last)
	n, note := t.toastN, t.note
	t.toastN, t.note = 0, ""
	t.mu.Unlock()
	t.notify(nimModify, func(d *notifyIconData) {
		d.Flags = nifIcon | nifTip | nifShowTip
		d.Icon = t.icons[v.state]
		copyUTF16(d.Tip[:], v.tip)
		switch {
		case note != "":
			d.Flags |= nifInfo
			d.InfoFlags = niifInfo
			copyUTF16(d.InfoTitle[:], "Briard")
			copyUTF16(d.Info[:], note)
		case n > 0:
			d.Flags |= nifInfo
			d.InfoFlags = niifWarning
			copyUTF16(d.InfoTitle[:], "Briard")
			copyUTF16(d.Info[:], alerts(n)+". Click to open Briard.")
		}
	})
}

func (t *tray) menu() {
	t.mu.Lock()
	v := show(t.last)
	t.mu.Unlock()
	m, _, _ := createPopupMenu.Call()
	defer destroyMenu.Call(m)
	item := func(flags uintptr, id int, text string) {
		p, _ := windows.UTF16PtrFromString(text)
		appendMenu.Call(m, flags, uintptr(id), uintptr(unsafe.Pointer(p)))
	}
	item(mfString, cmdOpen, "Open Briard")
	setMenuDefaultItem.Call(m, cmdOpen, 0)
	if v.open > 0 {
		item(mfString, cmdOpen, alerts(v.open))
	}
	appendMenu.Call(m, mfSeparator, 0, 0)
	switch v.action {
	case "stop":
		item(mfString, cmdStop, "Stop Briard")
	case "start":
		item(mfString, cmdStart, "Start Briard")
	default:
		item(mfString|mfGrayed, 0, v.tip)
	}
	var pt struct{ X, Y int32 }
	getCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// The window must be in front for the menu to close when the person clicks elsewhere, and the
	// null message after it is the documented end of that dance.
	setForegroundWindow.Call(uintptr(t.wnd))
	cmd, _, _ := trackPopupMenu.Call(m, tpmRight|tpmRetCmd|tpmNoNotify, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(t.wnd), 0)
	postMessage.Call(uintptr(t.wnd), wmNull, 0, 0)
	switch cmd {
	case cmdOpen:
		go t.open()
	case cmdStop:
		go t.act("stop")
	case cmdStart:
		go t.act("start")
	}
}

// open asks the agent for a one-time link to the Briard page and opens it in this person's
// browser -- the tray is what runs as them, which is why it opens the link `briard open` prints.
func (t *tray) open() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"name": displayName(), "username": os.Getenv("USERNAME"), "language": language()})
	o, err := host.Submit(ctx, t.sock, api.Directive{Kind: api.DirectiveDashboard, Payload: string(payload)})
	switch {
	case errors.Is(err, fs.ErrPermission):
		t.say("Briard was installed by another account on this computer, and only that account can open it here.")
	case errors.Is(err, host.ErrNoAgent):
		t.say("Briard is not running. Start it from this icon's menu.")
	case err != nil:
		t.say("Briard did not answer: " + err.Error())
	case o.State != api.OutcomeDone:
		t.say(o.Detail)
	default:
		link, _ := windows.UTF16PtrFromString(o.Detail)
		verb, _ := windows.UTF16PtrFromString("open")
		if err := windows.ShellExecute(windows.Handle(t.wnd), verb, link, nil, nil, windows.SW_SHOWNORMAL); err != nil {
			t.say("Could not open the browser: " + err.Error())
		}
	}
}

// act is Stop Briard or Start Briard: `briard stop`'s directive, `briard start`'s service start.
// With nobody answering, a stop does nothing more: the elevated `briard stop` is the one that
// powers an orphaned VM off.
func (t *tray) act(what string) {
	t.mu.Lock()
	t.pending = what
	t.mu.Unlock()
	t.poke()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var err error
	if what == "stop" {
		var o api.DirectiveOutcome
		if o, err = host.Submit(ctx, t.sock, api.Directive{Kind: api.DirectiveStop}); err == nil && o.State != api.OutcomeDone {
			err = errors.New(o.Detail)
		}
	} else {
		err = host.Start(ctx, func(string, ...any) {})
	}
	if err != nil {
		t.mu.Lock()
		t.pending = ""
		t.mu.Unlock()
		t.say("Briard did not " + what + ": " + err.Error())
	}
	t.poke()
}

// say shows a note in a balloon.
func (t *tray) say(s string) {
	t.mu.Lock()
	t.note = s
	t.mu.Unlock()
	postMessage.Call(uintptr(t.wnd), msgRead, 0, 0)
}

func (t *tray) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// poll reads the node on its tick, or at once when poked, and posts what it read. It also notices
// an update that replaced this program, once the update has committed.
func (t *tray) poll(exe string, self os.FileInfo) {
	l := selfupdate.New("", "")
	for {
		r := t.read(l)
		t.mu.Lock()
		switch {
		case t.pending == "stop" && r.err != nil && r.down,
			t.pending == "start" && r.err == nil:
			t.pending = ""
		}
		r.pending = t.pending
		if toast(t.prev, r) {
			t.toastN = r.status.Open
		}
		if r.err == nil && r.refusal == "" {
			s := r.status
			t.prev = &s
		}
		t.last = r
		t.mu.Unlock()
		postMessage.Call(uintptr(t.wnd), msgRead, 0, 0)
		if now, err := os.Stat(exe); err == nil && self != nil && !os.SameFile(self, now) && !exists(l.PrevPath()) {
			t.relaunch = true
			postMessage.Call(uintptr(t.wnd), wmClose, 0, 0)
			return
		}
		select {
		case <-t.wake:
		case <-time.After(pollEvery):
		}
	}
}

func (t *tray) read(l selfupdate.Layout) reading {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var r reading
	o, err := host.Submit(ctx, t.sock, api.Directive{Kind: api.DirectiveStatus})
	switch {
	case err != nil:
		r.err = err
		r.down, _ = platform.ServiceStopped(selfupdate.DefaultAgentUnit)
		r.update = exists(l.NextPath()) || exists(l.PrevPath())
	case o.State != api.OutcomeDone:
		r.refusal = o.Detail
	default:
		if err := json.Unmarshal([]byte(o.Detail), &r.status); err != nil {
			r.refusal = "Briard answered in a way this icon does not read: " + err.Error()
		}
	}
	return r
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// displayName and language are what Home Assistant's first user is created with when the page is
// opened before anyone has set it up -- `briard open`'s account details, read the Windows way.
func displayName() string {
	u := os.Getenv("USERNAME")
	if u == "" {
		return ""
	}
	return strings.ToUpper(u[:1]) + u[1:]
}

func language() string {
	if l, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME); err == nil && len(l) > 0 && len(l[0]) >= 2 {
		return strings.ToLower(l[0][:2])
	}
	return "en"
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = append(u[:len(dst)-1], 0)
	}
	copy(dst, u)
}

// icon makes a Windows icon of an image (art), for Windows to scale.
func icon(img *image.NRGBA) windows.Handle {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	hdr := struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPerMeter, YPerMeter   int32
		ClrUsed, ClrImportant  uint32
	}{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	hdr.Size = uint32(unsafe.Sizeof(hdr))
	var bits unsafe.Pointer
	color, _, _ := createDIBSection.Call(0, uintptr(unsafe.Pointer(&hdr)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if color == 0 || bits == nil {
		return 0
	}
	// The DIB is BGRA, top-down, row after row as the image's Pix is.
	px := unsafe.Slice((*byte)(bits), w*h*4)
	for i := 0; i < len(px); i += 4 {
		p := img.Pix[i : i+4]
		px[i], px[i+1], px[i+2], px[i+3] = p[2], p[1], p[0], p[3]
	}
	mask, _, _ := createBitmap.Call(uintptr(w), uintptr(h), 1, 1, 0)
	info := struct {
		Icon               int32
		XHotspot, YHotspot uint32
		Mask, Color        windows.Handle
	}{Icon: 1, Mask: windows.Handle(mask), Color: windows.Handle(color)}
	hicon, _, _ := createIconIndirect.Call(uintptr(unsafe.Pointer(&info)))
	deleteObject.Call(mask)
	deleteObject.Call(color)
	return windows.Handle(hicon)
}
