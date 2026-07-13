// Package protocol defines the wire messages exchanged between pool
// members (TCP mesh, newline-delimited JSON) and the UDP discovery beacons
// used to find peers on the local network.
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// UDPPort is used for both presence beacons and pool lookup requests.
const UDPPort = 47990

// Message types carried inside Envelope.Payload.
const (
	TypeHello        = "hello"         // handshake, sent by dialer right after connect
	TypeMemberSync   = "member_sync"   // anti-entropy: gossiped known-member list
	TypePing         = "ping"
	TypePong         = "pong"
	TypeBye          = "bye"           // graceful leave
	TypeActiveChange = "active_change" // gossiped: who currently owns the cursor
	TypeFocusHandoff = "focus_handoff" // sent directly to the node that should become active
	TypeRequestJump  = "request_jump"  // hotkey: ask the current active node to hand off to a target
	TypeInputEvent   = "input_event"   // forwarded raw mouse activity, sent to the active node
)

// Envelope wraps every message sent over a pool TCP connection.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type MemberInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Addr    string `json:"addr"` // LAN host:port TCP address others should dial
	ScreenW int    `json:"screen_w"`
	ScreenH int    `json:"screen_h"`
	// PublicAddr is an internet-reachable fallback address, filled in by a
	// rendezvous server from the connection it saw this member on. Empty
	// for members only ever discovered over LAN multicast.
	PublicAddr string `json:"public_addr,omitempty"`
}

type Hello struct {
	Self   MemberInfo `json:"self"`
	PoolID string     `json:"pool_id"`
}

type MemberSync struct {
	Members  []MemberInfo `json:"members"`
	ActiveID string       `json:"active_id"`
	// Version is a monotonically increasing counter for the active-node
	// assignment, used so a stale gossip message can never override a
	// newer handoff (last-writer-wins by version, then by ActiveID).
	Version uint64 `json:"version"`
}

type ActiveChange struct {
	ActiveID string `json:"active_id"`
	Version  uint64 `json:"version"`
}

type FocusHandoff struct {
	From        string  `json:"from"`
	To          string  `json:"to"`
	EntryEdge   string  `json:"entry_edge"` // "left", "right" or "center"
	EntryRatio  float64 `json:"entry_ratio"`
	Version     uint64  `json:"version"`
}

type RequestJump struct {
	To string `json:"to"`
}

type InputEvent struct {
	Kind    string `json:"kind"` // "move", "down", "up", "wheel"
	DX      int32  `json:"dx,omitempty"`
	DY      int32  `json:"dy,omitempty"`
	Button  string `json:"button,omitempty"` // "left", "right", "middle"
	WheelDY int32  `json:"wheel_dy,omitempty"`
}

// Encoder writes framed envelopes to a connection.
type Encoder struct {
	w io.Writer
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

func (e *Encoder) Send(msgType string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env := Envelope{Type: msgType, Payload: body}
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = e.w.Write(line)
	return err
}

// Decoder reads framed envelopes from a connection.
type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: bufio.NewReader(r)} }

func (d *Decoder) Recv() (Envelope, error) {
	line, err := d.r.ReadBytes('\n')
	if err != nil {
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	return env, nil
}

// --- UDP discovery ---

const (
	DiscoverKindPresence = "presence" // periodic beacon: "I run wifi-cursor"
	DiscoverKindRequest  = "request"  // "who has pool ID X?"
	DiscoverKindResponse = "response" // direct unicast reply with a TCP dial address
)

type Discover struct {
	Kind    string `json:"kind"`
	PoolID  string `json:"pool_id,omitempty"`
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	TCPAddr string `json:"tcp_addr,omitempty"`
}

func SendDiscover(conn net.PacketConn, addr net.Addr, d Discover) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = conn.WriteTo(body, addr)
	return err
}

func ParseDiscover(b []byte) (Discover, error) {
	var d Discover
	err := json.Unmarshal(b, &d)
	return d, err
}
