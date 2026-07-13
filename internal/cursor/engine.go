// Package cursor is the state machine that turns raw local input plus pool
// membership into the "one cursor roaming across every screen" experience:
// edge-crossing hand-off, a manual focus hotkey, and relaying input to
// whichever device currently owns the cursor.
package cursor

import (
	"context"
	"sync"

	"wifi-cursor/internal/input"
	"wifi-cursor/internal/pool"
	"wifi-cursor/internal/protocol"
)

type Engine struct {
	pool    *pool.Pool
	backend input.Backend

	screenW, screenH int

	mu       sync.Mutex
	amActive bool
}

func New(p *pool.Pool, b input.Backend) (*Engine, error) {
	w, h, err := b.ScreenSize()
	if err != nil {
		return nil, err
	}
	return &Engine{pool: p, backend: b, screenW: w, screenH: h}, nil
}

var _ pool.Handler = (*Engine)(nil)

// Run installs the global mouse/keyboard hook and drives the engine until
// ctx is cancelled. Callers must only invoke Run after CreatePool/JoinPool
// has already succeeded: this is the one place the process starts touching
// system-wide input, so nothing runs before the user has actually entered a
// pool (no hook, no idle polling loop while just sitting at the prompt).
func (e *Engine) Run(ctx context.Context) error {
	events, hotkeys, err := e.backend.Start(ctx)
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.amActive = e.pool.IsActive()
	e.mu.Unlock()
	_ = e.backend.SetPassthrough(e.amActive)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			e.handleLocalEvent(ev)
		case hk, ok := <-hotkeys:
			if !ok {
				return nil
			}
			e.handleHotkey(hk)
		}
	}
}

func (e *Engine) handleLocalEvent(ev input.Event) {
	e.mu.Lock()
	active := e.amActive
	e.mu.Unlock()

	if active {
		if ev.Kind == input.Move {
			e.checkEdge(ev.X, ev.Y)
		}
		return // buttons/wheel are already handled natively by the OS
	}

	// Forwarding mode: this node isn't showing the cursor, but a human is
	// still touching its physical mouse — relay raw input to whoever is.
	e.pool.SendInputEvent(toProtocolEvent(ev))
}

func (e *Engine) checkEdge(x, y int) {
	var edge string
	switch {
	case x <= 0:
		edge = "left"
	case x >= e.screenW-1:
		edge = "right"
	default:
		return
	}
	target := e.neighbor(edge)
	if target == "" {
		return
	}
	ratio := clamp01(float64(y) / float64(maxInt(e.screenH-1, 1)))
	entryEdge := "left"
	if edge == "left" {
		entryEdge = "right"
	}
	e.performHandoff(target, entryEdge, ratio)
}

// neighbor finds the next/previous device in the pool's ring order, giving
// every node a consistent left/right screen without any coordination.
func (e *Engine) neighbor(edge string) string {
	ring := e.pool.Ring()
	if len(ring) < 2 {
		return ""
	}
	idx := -1
	for i, id := range ring {
		if id == e.pool.Self.ID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ""
	}
	n := len(ring)
	if edge == "right" {
		return ring[(idx+1)%n]
	}
	return ring[(idx-1+n)%n]
}

func (e *Engine) handleHotkey(hk input.HotkeyEvent) {
	ring := e.pool.Ring()
	if hk.Slot < 1 || hk.Slot > len(ring) {
		return
	}
	target := ring[hk.Slot-1]
	if target == e.pool.Self.ID {
		return
	}

	e.mu.Lock()
	amActive := e.amActive
	e.mu.Unlock()
	if amActive {
		e.performHandoff(target, "center", 0.5)
	} else {
		_ = e.pool.RequestJump(target)
	}
}

func (e *Engine) performHandoff(target, entryEdge string, ratio float64) {
	e.mu.Lock()
	if !e.amActive {
		e.mu.Unlock()
		return
	}
	e.amActive = false
	e.mu.Unlock()
	_ = e.backend.SetPassthrough(false)

	if err := e.pool.SendFocusHandoff(target, entryEdge, ratio); err != nil {
		// Target unreachable: reclaim the cursor rather than stranding it.
		e.mu.Lock()
		e.amActive = true
		e.mu.Unlock()
		_ = e.backend.SetPassthrough(true)
	}
}

// --- pool.Handler ---

func (e *Engine) OnFocusHandoff(msg protocol.FocusHandoff) {
	if msg.To != e.pool.Self.ID {
		return
	}
	e.mu.Lock()
	e.amActive = true
	e.mu.Unlock()

	x, y := e.entryPoint(msg.EntryEdge, msg.EntryRatio)
	_ = e.backend.WarpTo(x, y)
	_ = e.backend.SetPassthrough(true)
}

func (e *Engine) entryPoint(edge string, ratio float64) (int, int) {
	y := int(clamp01(ratio) * float64(maxInt(e.screenH-1, 1)))
	switch edge {
	case "left":
		return 1, y
	case "right":
		return e.screenW - 2, y
	default:
		return e.screenW / 2, e.screenH / 2
	}
}

func (e *Engine) OnInputEvent(fromID string, ev protocol.InputEvent) {
	e.mu.Lock()
	active := e.amActive
	e.mu.Unlock()
	if !active {
		return
	}
	_ = e.backend.Inject(fromProtocolEvent(ev))
}

func (e *Engine) OnActiveChange(newActiveID string) {
	amActive := newActiveID == e.pool.Self.ID
	e.mu.Lock()
	changed := e.amActive != amActive
	e.amActive = amActive
	e.mu.Unlock()
	if changed {
		_ = e.backend.SetPassthrough(amActive)
	}
}

func (e *Engine) OnRequestJump(fromID, targetID string) {
	e.mu.Lock()
	amActive := e.amActive
	e.mu.Unlock()
	if !amActive {
		return
	}
	e.performHandoff(targetID, "center", 0.5)
}

func (e *Engine) OnMembersChanged() {}

// --- conversions ---

func toProtocolEvent(ev input.Event) protocol.InputEvent {
	pe := protocol.InputEvent{}
	switch ev.Kind {
	case input.Move:
		pe.Kind = "move"
		pe.DX, pe.DY = int32(ev.DX), int32(ev.DY)
	case input.ButtonDown:
		pe.Kind = "down"
		pe.Button = buttonStr(ev.Button)
	case input.ButtonUp:
		pe.Kind = "up"
		pe.Button = buttonStr(ev.Button)
	case input.Wheel:
		pe.Kind = "wheel"
		pe.WheelDY = int32(ev.WheelDY)
	}
	return pe
}

func fromProtocolEvent(ev protocol.InputEvent) input.Event {
	e := input.Event{}
	switch ev.Kind {
	case "move":
		e.Kind = input.Move
		e.DX, e.DY = int(ev.DX), int(ev.DY)
	case "down":
		e.Kind = input.ButtonDown
		e.Button = parseButton(ev.Button)
	case "up":
		e.Kind = input.ButtonUp
		e.Button = parseButton(ev.Button)
	case "wheel":
		e.Kind = input.Wheel
		e.WheelDY = int(ev.WheelDY)
	}
	return e
}

func buttonStr(b input.Button) string {
	switch b {
	case input.Right:
		return "right"
	case input.Middle:
		return "middle"
	default:
		return "left"
	}
}

func parseButton(s string) input.Button {
	switch s {
	case "right":
		return input.Right
	case "middle":
		return input.Middle
	default:
		return input.Left
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
