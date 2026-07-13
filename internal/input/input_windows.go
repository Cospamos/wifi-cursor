//go:build windows

// Windows backend: pure Go, no cgo. Uses a low-level mouse/keyboard hook
// (WH_MOUSE_LL / WH_KEYBOARD_LL) to capture global input and SendInput to
// inject it back, so the tool builds with nothing but the stock Go
// toolchain (no C compiler required).
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
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procSendInput           = user32.NewProc("SendInput")
	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procShowCursor          = user32.NewProc("ShowCursor")
	procClipCursor          = user32.NewProc("ClipCursor")

	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
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

	wmQuit = 0x0012
)

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

type winBackend struct {
	events  chan Event
	hotkeys chan HotkeyEvent

	mu                 sync.Mutex
	threadID           uint32
	mouseHook, keyHook uintptr
	lastX, lastY       int32

	screenW, screenH int32
	centerX, centerY int32

	passthrough   atomic.Bool
	expectingWarp atomic.Bool
	ctrlDown      atomic.Bool
	altDown       atomic.Bool
}

// NewBackend constructs the platform input backend.
func NewBackend() (Backend, error) {
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
// delivers WH_*_LL callbacks only to the thread that installed them, and
// only while that thread is pumping messages.
func (b *winBackend) runMessageLoop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadId.Call()
	b.mu.Lock()
	b.threadID = uint32(tid)
	b.mu.Unlock()

	mouseCB := syscall.NewCallback(func(nCode int, wParam, lParam uintptr) uintptr {
		if nCode == hcAction {
			b.onRawMouse(uint32(wParam), (*msllhookstruct)(unsafe.Pointer(lParam)))
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

func (b *winBackend) onRawMouse(wParam uint32, info *msllhookstruct) {
	switch wParam {
	case wmMouseMove:
		if b.passthrough.Load() {
			b.emit(Event{Kind: Move, X: int(info.Pt.X), Y: int(info.Pt.Y)})
			return
		}
		if b.expectingWarp.Load() {
			if info.Pt.X == b.centerX && info.Pt.Y == b.centerY {
				b.expectingWarp.Store(false)
			}
			b.mu.Lock()
			b.lastX, b.lastY = info.Pt.X, info.Pt.Y
			b.mu.Unlock()
			return
		}
		b.mu.Lock()
		dx, dy := info.Pt.X-b.lastX, info.Pt.Y-b.lastY
		b.lastX, b.lastY = info.Pt.X, info.Pt.Y
		b.mu.Unlock()
		if dx != 0 || dy != 0 {
			b.emit(Event{Kind: Move, DX: int(dx), DY: int(dy)})
		}
		b.expectingWarp.Store(true)
		procSetCursorPos.Call(uintptr(b.centerX), uintptr(b.centerY))
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
		delta := int16(info.MouseData >> 16)
		b.emit(Event{Kind: Wheel, WheelDY: int(delta)})
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

func (b *winBackend) WarpTo(x, y int) error {
	r, _, err := procSetCursorPos.Call(uintptr(int32(x)), uintptr(int32(y)))
	if r == 0 {
		return err
	}
	b.mu.Lock()
	b.lastX, b.lastY = int32(x), int32(y)
	b.mu.Unlock()
	return nil
}

func (b *winBackend) SetPassthrough(enabled bool) error {
	if b.passthrough.Swap(enabled) == enabled {
		return nil
	}
	if enabled {
		procClipCursor.Call(0)
		procShowCursor.Call(1)
		return nil
	}
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	b.mu.Lock()
	b.lastX, b.lastY = pt.X, pt.Y
	b.mu.Unlock()
	rect := struct{ Left, Top, Right, Bottom int32 }{b.centerX, b.centerY, b.centerX + 1, b.centerY + 1}
	procClipCursor.Call(uintptr(unsafe.Pointer(&rect)))
	procSetCursorPos.Call(uintptr(b.centerX), uintptr(b.centerY))
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
		sendMouseInput(mouseInputC{MouseData: uint32(int32(ev.WheelDY)), DwFlags: mouseeventfWheel})
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
