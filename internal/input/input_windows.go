//go:build windows

// Windows backend: pure Go, no cgo. Uses a low-level mouse/keyboard hook
// (WH_MOUSE_LL / WH_KEYBOARD_LL) for absolute cursor position (while
// active) and buttons/wheel, SendInput to inject events back, and the Raw
// Input API (WM_INPUT) for relative mouse deltas while forwarding — see the
// comment on onRawInput for why that last part specifically isn't optional.
// No C compiler required.
package input

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	user32                            = syscall.NewLazyDLL("user32.dll")
	procSetWindowsHookExW             = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx                = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx           = user32.NewProc("UnhookWindowsHookEx")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostThreadMessageW            = user32.NewProc("PostThreadMessageW")
	procSendInput                     = user32.NewProc("SendInput")
	procSetCursorPos                  = user32.NewProc("SetCursorPos")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procShowCursor                    = user32.NewProc("ShowCursor")
	procClipCursor                    = user32.NewProc("ClipCursor")
	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procRegisterRawInputDevs          = user32.NewProc("RegisterRawInputDevices")
	procGetRawInputData               = user32.NewProc("GetRawInputData")
	procCreateCursor                  = user32.NewProc("CreateCursor")
	procSetSystemCursor               = user32.NewProc("SetSystemCursor")
	procSystemParametersInfoW         = user32.NewProc("SystemParametersInfoW")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")

	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
)

const (
	whMouseLL    = 14
	whKeyboardLL = 13
	hcAction     = 0

	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A

	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104

	wmInput = 0x00FF
	wmQuit  = 0x0012

	vkControlL = 0xA2
	vkControlR = 0xA3
	vkMenuL    = 0xA4
	vkMenuR    = 0xA5

	smCxScreen = 0
	smCyScreen = 1

	inputTypeMouse = 0

	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfWheel      = 0x0800

	ridInput       = 0x10000003
	ridevInputSink = 0x00000100
	riMouseUsage   = 0x02
	riUsagePageGen = 0x01
	rimTypeMouse   = 0

	spiSetCursors = 0x0057

	// wheelDelta: Windows always reports/expects wheel motion in multiples
	// of this per notch (WHEEL_DELTA). The wire protocol uses plain notch
	// counts instead, so both ends can inject in their own native units.
	wheelDelta = 120
)

// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == ((DPI_AWARENESS_CONTEXT)-4),
// a sentinel handle value rather than a real pointer (same idiom as
// HWND_MESSAGE below).
const dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)

// systemCursorIDs covers every OCR_* cursor Windows can display, so
// replacing all of them hides the pointer regardless of what it's hovering
// over (text field, resize handle, link, ...).
var systemCursorIDs = []uint32{
	32512, // OCR_NORMAL
	32513, // OCR_IBEAM
	32514, // OCR_WAIT
	32515, // OCR_CROSS
	32516, // OCR_UP
	32640, // OCR_SIZE
	32641, // OCR_ICON
	32642, // OCR_SIZENWSE
	32643, // OCR_SIZENESW
	32644, // OCR_SIZEWE
	32645, // OCR_SIZENS
	32646, // OCR_SIZEALL
	32648, // OCR_NO
	32649, // OCR_HAND
	32650, // OCR_APPSTARTING
	32651, // OCR_HELP
}

type point struct{ X, Y int32 }

type msllhookstruct struct {
	Pt          point
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type kbdllhookstruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type mouseInputC struct {
	Dx, Dy      int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type inputRecord struct {
	Type uint32
	Mi   mouseInputC
}

type msgStruct struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type rawInputDevice struct {
	UsagePage  uint16
	Usage      uint16
	Flags      uint32
	HwndTarget uintptr
}

// rawInputHeader mirrors RAWINPUTHEADER.
type rawInputHeader struct {
	Type   uint32
	Size   uint32
	Device uintptr
	WParam uintptr
}

// rawMouse mirrors RAWMOUSE (only the fields we need; the button union is
// read as two USHORTs, which is safe since we never use ulButtons here).
type rawMouse struct {
	Flags       uint16
	_pad        uint16
	ButtonFlags uint16
	ButtonData  uint16
	RawButtons  uint32
	LastX       int32
	LastY       int32
	ExtraInfo   uint32
}

// rawInputMouse mirrors RAWINPUT for the mouse-data case (header + RAWMOUSE
// union member; we never touch keyboard/HID data so we don't model those).
type rawInputMouse struct {
	Header rawInputHeader
	Mouse  rawMouse
}

type winBackend struct {
	events  chan Event
	hotkeys chan HotkeyEvent

	mu                 sync.Mutex
	threadID           uint32
	mouseHook, keyHook uintptr
	msgWnd             uintptr

	screenW, screenH int32
	centerX, centerY int32

	passthrough atomic.Bool
	ctrlDown    atomic.Bool
	altDown     atomic.Bool

	// wheelAccum carries the remainder when a WM_MOUSEWHEEL delta doesn't
	// divide evenly by wheelDelta - see onRawMouse. Only ever touched from
	// the hook callback, which always runs on the single message-loop
	// thread, so no lock needed.
	wheelAccum int32
}

// NewBackend constructs the platform input backend.
func NewBackend() (Backend, error) {
	// Without declaring DPI awareness, Windows silently virtualizes both
	// GetSystemMetrics and hook-delivered coordinates for this process to a
	// 96-DPI-equivalent space, and that virtualization can round
	// differently depending on monitor layout/scaling - a plausible source
	// of the screen bounds (and therefore edge-trigger position) not lining
	// up exactly the same on both sides of the screen. Opting in to
	// per-monitor DPI awareness makes every coordinate we read or compare
	// against a real physical pixel, consistently.
	procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2)

	b := &winBackend{}
	b.passthrough.Store(true)
	if _, _, err := b.ScreenSize(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *winBackend) ScreenSize() (int, int, error) {
	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if w == 0 || h == 0 {
		return 0, 0, errors.New("GetSystemMetrics failed")
	}
	b.screenW, b.screenH = int32(w), int32(h)
	b.centerX, b.centerY = int32(w)/2, int32(h)/2
	return int(w), int(h), nil
}

func (b *winBackend) Start(ctx context.Context) (<-chan Event, <-chan HotkeyEvent, error) {
	b.events = make(chan Event, 256)
	b.hotkeys = make(chan HotkeyEvent, 16)

	ready := make(chan error, 1)
	go b.runMessageLoop(ready)
	if err := <-ready; err != nil {
		return nil, nil, err
	}

	go func() {
		<-ctx.Done()
		b.stop()
	}()
	return b.events, b.hotkeys, nil
}

// runMessageLoop must own an OS thread for the whole hook lifetime: Windows
// delivers WH_*_LL callbacks and WM_INPUT only to the thread that installed
// them, and only while that thread is pumping messages.
func (b *winBackend) runMessageLoop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadId.Call()
	b.mu.Lock()
	b.threadID = uint32(tid)
	b.mu.Unlock()

	hInstance, _, _ := procGetModuleHandleW.Call(0)

	wndProc := syscall.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
		if msg == wmInput {
			b.onRawInput(lparam)
			return 0
		}
		ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return ret
	})

	className, _ := syscall.UTF16PtrFromString("WifiCursorRawInputWnd")
	wc := wndClassExW{
		WndProc:   wndProc,
		Instance:  hInstance,
		ClassName: className,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		ready <- fmt.Errorf("RegisterClassExW: %w", err)
		return
	}

	const hwndMessage = ^uintptr(2) // HWND_MESSAGE == (HWND)-3
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0,
		0,
		0, 0, 0, 0,
		hwndMessage,
		0,
		hInstance,
		0,
	)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowExW: %w", err)
		return
	}
	b.msgWnd = hwnd

	rid := rawInputDevice{UsagePage: riUsagePageGen, Usage: riMouseUsage, Flags: ridevInputSink, HwndTarget: hwnd}
	if ok, _, err := procRegisterRawInputDevs.Call(uintptr(unsafe.Pointer(&rid)), 1, unsafe.Sizeof(rid)); ok == 0 {
		ready <- fmt.Errorf("RegisterRawInputDevices: %w", err)
		return
	}

	mouseCB := syscall.NewCallback(func(nCode int, wParam, lParam uintptr) uintptr {
		if nCode == hcAction {
			if b.onRawMouse(uint32(wParam), (*msllhookstruct)(unsafe.Pointer(lParam))) {
				return 1 // swallow: don't let it also act locally while forwarding
			}
		}
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	})
	h1, _, e1 := procSetWindowsHookExW.Call(whMouseLL, mouseCB, 0, 0)
	if h1 == 0 {
		ready <- fmt.Errorf("SetWindowsHookExW(mouse): %w", e1)
		return
	}

	keyCB := syscall.NewCallback(func(nCode int, wParam, lParam uintptr) uintptr {
		if nCode == hcAction {
			b.onRawKey(uint32(wParam), (*kbdllhookstruct)(unsafe.Pointer(lParam)))
		}
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	})
	h2, _, e2 := procSetWindowsHookExW.Call(whKeyboardLL, keyCB, 0, 0)
	if h2 == 0 {
		procUnhookWindowsHookEx.Call(h1)
		ready <- fmt.Errorf("SetWindowsHookExW(keyboard): %w", e2)
		return
	}
	b.mouseHook, b.keyHook = h1, h2
	ready <- nil

	var m msgStruct
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	procUnhookWindowsHookEx.Call(h1)
	procUnhookWindowsHookEx.Call(h2)
}

func (b *winBackend) stop() {
	b.mu.Lock()
	tid := b.threadID
	b.mu.Unlock()
	if tid != 0 {
		procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	}
}

// onRawMouse handles the low-level hook: absolute position while active
// (passthrough, for edge detection) - movement is never blocked, ClipCursor
// already confines it while forwarding. Buttons/wheel are always captured
// for forwarding, and now also *swallowed* (return true) while forwarding
// is active, so a scroll/click doesn't also act on this machine at the same
// time it's forwarded - previously they reached the local desktop
// unconditionally since ClipCursor doesn't do anything to stop that.
func (b *winBackend) onRawMouse(wParam uint32, info *msllhookstruct) (swallow bool) {
	switch wParam {
	case wmMouseMove:
		if b.passthrough.Load() {
			b.emit(Event{Kind: Move, X: int(info.Pt.X), Y: int(info.Pt.Y)})
		}
		// Forwarding-mode deltas come from Raw Input (onRawInput) instead of
		// from this hook's absolute position — see onRawInput for why.
		return false
	case wmLButtonDown:
		b.emit(Event{Kind: ButtonDown, Button: Left})
	case wmLButtonUp:
		b.emit(Event{Kind: ButtonUp, Button: Left})
	case wmRButtonDown:
		b.emit(Event{Kind: ButtonDown, Button: Right})
	case wmRButtonUp:
		b.emit(Event{Kind: ButtonUp, Button: Right})
	case wmMButtonDown:
		b.emit(Event{Kind: ButtonDown, Button: Middle})
	case wmMButtonUp:
		b.emit(Event{Kind: ButtonUp, Button: Middle})
	case wmMouseWheel:
		// Not every device reports clean multiples of wheelDelta (120) per
		// notch - precision touchpads and some "smooth scroll" mice send
		// finer deltas (e.g. 40) to support inertial scrolling. A plain
		// delta/wheelDelta division truncates those to 0 and the scroll is
		// silently lost, which on a device that never happens to send a
		// full 120 at once means it never forwards *any* scroll at all.
		// Accumulate instead, so partial notches carry over and eventually
		// add up to a real one.
		delta := int16(info.MouseData >> 16)
		b.wheelAccum += int32(delta)
		notches := b.wheelAccum / wheelDelta
		b.wheelAccum -= notches * wheelDelta
		if notches != 0 {
			b.emit(Event{Kind: Wheel, WheelDY: int(notches)})
		}
	default:
		return false
	}
	return !b.passthrough.Load()
}

// onRawInput handles WM_INPUT: while forwarding (not active), this is the
// *only* correct source for movement deltas. The alternative — diffing
// absolute cursor position between WH_MOUSE_LL events — is what this node
// also uses while forwarding is off, but it fundamentally cannot work here:
// SetPassthrough(false) confines the cursor to a 1x1 ClipCursor rect so it
// stays out of the way, and a clipped cursor's *reported position* is
// clipped too, so every diff comes out ~0 no matter how far the mouse
// actually moves. Raw Input reports the HID device's motion directly,
// bypassing cursor position (and therefore ClipCursor) entirely.
func (b *winBackend) onRawInput(lparam uintptr) {
	if b.passthrough.Load() {
		return
	}
	var buf [64]byte
	size := uint32(len(buf))
	r, _, _ := procGetRawInputData.Call(
		lparam,
		ridInput,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		unsafe.Sizeof(rawInputHeader{}),
	)
	if int32(r) <= 0 {
		return
	}
	ri := (*rawInputMouse)(unsafe.Pointer(&buf[0]))
	if ri.Header.Type != rimTypeMouse {
		return
	}
	if ri.Mouse.LastX != 0 || ri.Mouse.LastY != 0 {
		b.emit(Event{Kind: Move, DX: int(ri.Mouse.LastX), DY: int(ri.Mouse.LastY)})
	}
}

func (b *winBackend) onRawKey(wParam uint32, info *kbdllhookstruct) {
	down := wParam == wmKeyDown || wParam == wmSysKeyDown
	switch info.VkCode {
	case vkControlL, vkControlR:
		b.ctrlDown.Store(down)
	case vkMenuL, vkMenuR:
		b.altDown.Store(down)
	default:
		if down && b.ctrlDown.Load() && b.altDown.Load() && info.VkCode >= 0x31 && info.VkCode <= 0x39 {
			slot := int(info.VkCode - 0x30)
			select {
			case b.hotkeys <- HotkeyEvent{Slot: slot}:
			default:
			}
		}
	}
}

func (b *winBackend) emit(ev Event) {
	select {
	case b.events <- ev:
	default:
	}
}

// hideSystemCursors replaces every system cursor resource with a fully
// transparent one. ShowCursor(FALSE) alone isn't reliable here: it only
// affects the calling thread's display counter, and the actually-visible
// cursor is composited based on the foreground window, which belongs to
// some other process/thread entirely - so a background hook thread calling
// ShowCursor often has no visible effect at all. Replacing the shared
// system cursor resources works regardless of which window has focus.
func hideSystemCursors() {
	and := [2]byte{0xFF, 0xFF} // 1x1 monochrome cursor, fully transparent
	xor := [2]byte{0x00, 0x00}
	for _, id := range systemCursorIDs {
		h, _, _ := procCreateCursor.Call(0, 0, 0, 1, 1, uintptr(unsafe.Pointer(&and[0])), uintptr(unsafe.Pointer(&xor[0])))
		if h != 0 {
			// SetSystemCursor takes ownership of h; a fresh cursor is needed
			// per ID rather than reusing one handle across multiple calls.
			procSetSystemCursor.Call(h, uintptr(id))
		}
	}
}

// restoreSystemCursors reloads the user's normal cursor scheme, undoing
// hideSystemCursors.
func restoreSystemCursors() {
	procSystemParametersInfoW.Call(spiSetCursors, 0, 0, 0)
}

func (b *winBackend) WarpTo(x, y int) error {
	r, _, err := procSetCursorPos.Call(uintptr(int32(x)), uintptr(int32(y)))
	if r == 0 {
		return err
	}
	return nil
}

func (b *winBackend) SetPassthrough(enabled bool) error {
	if b.passthrough.Swap(enabled) == enabled {
		return nil
	}
	if enabled {
		procClipCursor.Call(0)
		restoreSystemCursors()
		procShowCursor.Call(1)
		return nil
	}
	rect := struct{ Left, Top, Right, Bottom int32 }{b.centerX, b.centerY, b.centerX + 1, b.centerY + 1}
	procClipCursor.Call(uintptr(unsafe.Pointer(&rect)))
	procSetCursorPos.Call(uintptr(b.centerX), uintptr(b.centerY))
	hideSystemCursors()
	procShowCursor.Call(0)
	return nil
}

func (b *winBackend) Inject(ev Event) error {
	switch ev.Kind {
	case Move:
		sendMouseInput(mouseInputC{Dx: int32(ev.DX), Dy: int32(ev.DY), DwFlags: mouseeventfMove})
	case ButtonDown:
		sendMouseInput(mouseInputC{DwFlags: buttonFlag(ev.Button, true)})
	case ButtonUp:
		sendMouseInput(mouseInputC{DwFlags: buttonFlag(ev.Button, false)})
	case Wheel:
		sendMouseInput(mouseInputC{MouseData: uint32(int32(ev.WheelDY) * wheelDelta), DwFlags: mouseeventfWheel})
	}
	return nil
}

func buttonFlag(btn Button, down bool) uint32 {
	switch btn {
	case Right:
		if down {
			return mouseeventfRightDown
		}
		return mouseeventfRightUp
	case Middle:
		if down {
			return mouseeventfMiddleDown
		}
		return mouseeventfMiddleUp
	default:
		if down {
			return mouseeventfLeftDown
		}
		return mouseeventfLeftUp
	}
}

func sendMouseInput(mi mouseInputC) {
	rec := inputRecord{Type: inputTypeMouse, Mi: mi}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&rec)), unsafe.Sizeof(rec))
}

func (b *winBackend) Close() error {
	b.stop()
	return nil
}
