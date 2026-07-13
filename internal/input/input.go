// Package input defines the platform-independent capture/inject contract.
// The actual implementation lives in input_windows.go / input_linux.go,
// selected at compile time by GOOS build tags.
package input

import "context"

type EventKind int

const (
	Move EventKind = iota
	ButtonDown
	ButtonUp
	Wheel
)

type Button int

const (
	Left Button = iota
	Right
	Middle
)

// Event is a single local hardware input sample.
//
// X, Y are the absolute cursor position and are meaningful while this
// node's cursor is in passthrough mode (edge detection reads them). DX, DY
// are the relative motion since the previous Move event and are what gets
// forwarded to a remote active node while passthrough is disabled — the
// backend recenters the real cursor internally so the user can keep pushing
// the mouse indefinitely without hitting a physical screen edge.
type Event struct {
	Kind    EventKind
	X, Y    int
	DX, DY  int
	Button  Button
	WheelDY int
}

// HotkeyEvent fires when the global focus-jump combo (Ctrl+Alt+1..9) is
// pressed, letting the user jump straight to a device instead of dragging
// the cursor across every screen edge in between.
type HotkeyEvent struct {
	Slot int // 1-9
}

// Backend is the platform-specific capture/inject implementation.
type Backend interface {
	ScreenSize() (w, h int, err error)

	// Start begins listening for global mouse/keyboard input. Events and
	// hotkeys are delivered on the returned channels until ctx is cancelled.
	Start(ctx context.Context) (events <-chan Event, hotkeys <-chan HotkeyEvent, err error)

	// WarpTo moves the local OS cursor to an absolute screen position.
	WarpTo(x, y int) error

	// SetPassthrough(true) lets the local cursor move normally (this node
	// is active). SetPassthrough(false) hides/pins it and switches Start's
	// channel to emitting recentered relative deltas instead (this node is
	// forwarding input to whichever node is active).
	SetPassthrough(enabled bool) error

	// Inject applies a remote event locally: used by whichever node is
	// currently active to render input forwarded from another member.
	Inject(ev Event) error

	Close() error
}
