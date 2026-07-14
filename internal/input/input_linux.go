//go:build linux

// Linux backend: built on robotgo (inject) and gohook (capture), both
// backed by X11 (XTest/XRecord). Requires a running X server and, to build,
// a C toolchain plus libx11-dev/libxtst-dev/libxkbcommon-dev (see README).
package input

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-vgo/robotgo"
	hook "github.com/robotn/gohook"
)

// warpGrace is how long after we programmatically recenter the cursor that
// we treat incoming move events as possibly being the echo of that
// recenter rather than genuine motion. Time-based rather than "wait for an
// event whose position exactly matches the recenter target": X11 may or
// may not report XWarpPointer-induced motion through the same path gohook
// uses to capture real hardware motion, and a match-based flag that never
// gets cleared would silently wedge forwarding shut forever.
const warpGrace = 30 * time.Millisecond

type linuxBackend struct {
	events  chan Event
	hotkeys chan HotkeyEvent

	mu               sync.Mutex
	lastX, lastY     int32
	screenW, screenH int32
	centerX, centerY int32
	warpUntil        time.Time

	passthrough atomic.Bool

	endOnce sync.Once
}

// NewBackend constructs the platform input backend.
func NewBackend() (Backend, error) {
	b := &linuxBackend{}
	b.passthrough.Store(true)
	if _, _, err := b.ScreenSize(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *linuxBackend) ScreenSize() (int, int, error) {
	w, h := robotgo.GetScreenSize()
	if w == 0 || h == 0 {
		return 0, 0, errors.New("robotgo.GetScreenSize failed")
	}
	b.screenW, b.screenH = int32(w), int32(h)
	b.centerX, b.centerY = int32(w)/2, int32(h)/2
	return w, h, nil
}

func (b *linuxBackend) Start(ctx context.Context) (<-chan Event, <-chan HotkeyEvent, error) {
	b.events = make(chan Event, 256)
	b.hotkeys = make(chan HotkeyEvent, 16)

	hook.Register(hook.MouseMove, []string{}, func(e hook.Event) {
		b.onMove(int(e.X), int(e.Y))
	})
	hook.Register(hook.MouseDown, []string{}, func(e hook.Event) {
		b.onButton(e.Button, true)
	})
	hook.Register(hook.MouseUp, []string{}, func(e hook.Event) {
		b.onButton(e.Button, false)
	})
	hook.Register(hook.MouseWheel, []string{}, func(e hook.Event) {
		b.emit(Event{Kind: Wheel, WheelDY: int(e.Rotation)})
	})
	for slot := 1; slot <= 9; slot++ {
		s := slot
		hook.Register(hook.KeyDown, []string{strconv.Itoa(s), "ctrl", "alt"}, func(e hook.Event) {
			select {
			case b.hotkeys <- HotkeyEvent{Slot: s}:
			default:
			}
		})
	}

	evChan := hook.Start()
	done := hook.Process(evChan)
	go func() {
		<-ctx.Done()
		b.end()
		<-done
	}()

	return b.events, b.hotkeys, nil
}

func (b *linuxBackend) onMove(x, y int) {
	if b.passthrough.Load() {
		b.emit(Event{Kind: Move, X: x, Y: y})
		return
	}

	b.mu.Lock()
	dx, dy := int32(x)-b.lastX, int32(y)-b.lastY
	b.lastX, b.lastY = int32(x), int32(y)
	inGrace := time.Now().Before(b.warpUntil)
	b.mu.Unlock()

	if inGrace {
		// Likely the echo of our own recenter (or arrived too soon after it
		// to trust the delta); don't forward it and don't re-warp again yet.
		return
	}
	if dx != 0 || dy != 0 {
		b.emit(Event{Kind: Move, DX: int(dx), DY: int(dy)})
	}
	b.mu.Lock()
	b.warpUntil = time.Now().Add(warpGrace)
	b.mu.Unlock()
	robotgo.Move(int(b.centerX), int(b.centerY))
}

func (b *linuxBackend) onButton(btnCode uint16, down bool) {
	btn := Left
	switch btnCode {
	case hook.MouseMap["right"]:
		btn = Right
	case hook.MouseMap["center"]:
		btn = Middle
	}
	kind := ButtonUp
	if down {
		kind = ButtonDown
	}
	b.emit(Event{Kind: kind, Button: btn})
}

func (b *linuxBackend) emit(ev Event) {
	select {
	case b.events <- ev:
	default:
	}
}

func (b *linuxBackend) WarpTo(x, y int) error {
	robotgo.Move(x, y)
	b.mu.Lock()
	b.lastX, b.lastY = int32(x), int32(y)
	b.mu.Unlock()
	return nil
}

// SetPassthrough(false) recenters the cursor on every move so it can travel
// indefinitely, matching the Windows backend's trick. Unlike Windows, this
// build has no reliable cross-desktop-environment API for hiding/clipping
// the system cursor, so it stays visible and will visibly jump back to the
// screen center while this node is forwarding input elsewhere.
func (b *linuxBackend) SetPassthrough(enabled bool) error {
	if b.passthrough.Swap(enabled) == enabled {
		return nil
	}
	if !enabled {
		b.mu.Lock()
		b.lastX, b.lastY = b.centerX, b.centerY
		b.warpUntil = time.Now().Add(warpGrace)
		b.mu.Unlock()
		robotgo.Move(int(b.centerX), int(b.centerY))
	}
	return nil
}

func (b *linuxBackend) Inject(ev Event) error {
	switch ev.Kind {
	case Move:
		robotgo.MoveRelative(ev.DX, ev.DY)
	case ButtonDown:
		return robotgo.MouseDown(buttonName(ev.Button))
	case ButtonUp:
		return robotgo.MouseUp(buttonName(ev.Button))
	case Wheel:
		robotgo.Scroll(0, ev.WheelDY)
	}
	return nil
}

func buttonName(b Button) string {
	switch b {
	case Right:
		return "right"
	case Middle:
		return "center"
	default:
		return "left"
	}
}

func (b *linuxBackend) end() {
	b.endOnce.Do(func() {
		hook.End()
	})
}

func (b *linuxBackend) Close() error {
	b.end()
	return nil
}
