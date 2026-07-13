// Package pool implements the decentralized device pool: a full-mesh TCP
// network between all members, membership maintained by gossip (no
// coordinator), heartbeat-based failure detection, and last-writer-wins
// hand-off of "which device currently owns the shared cursor".
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"wifi-cursor/internal/discovery"
	"wifi-cursor/internal/id"
	"wifi-cursor/internal/protocol"
	"wifi-cursor/internal/rendezvous"
)

type Node struct {
	ID      string
	Name    string
	Addr    string // LAN address, reachable directly on the same network
	ScreenW int
	ScreenH int
	// PublicAddr is an internet-reachable fallback learned from a
	// rendezvous server, tried if Addr isn't dialable (different network).
	PublicAddr string
}

func (n Node) info() protocol.MemberInfo {
	return protocol.MemberInfo{ID: n.ID, Name: n.Name, Addr: n.Addr, ScreenW: n.ScreenW, ScreenH: n.ScreenH, PublicAddr: n.PublicAddr}
}

func fromInfo(m protocol.MemberInfo) Node {
	return Node{ID: m.ID, Name: m.Name, Addr: m.Addr, ScreenW: m.ScreenW, ScreenH: m.ScreenH, PublicAddr: m.PublicAddr}
}

// dialCandidates returns addresses worth trying to reach n, LAN first.
func dialCandidates(n Node) []string {
	var out []string
	if n.Addr != "" {
		out = append(out, n.Addr)
	}
	if n.PublicAddr != "" && n.PublicAddr != n.Addr {
		out = append(out, n.PublicAddr)
	}
	return out
}

// Handler receives events the cursor engine reacts to. Methods are called
// from pool-internal goroutines and must return quickly.
type Handler interface {
	OnFocusHandoff(msg protocol.FocusHandoff)
	OnInputEvent(fromID string, ev protocol.InputEvent)
	OnActiveChange(newActiveID string)
	OnRequestJump(fromID, targetID string)
	OnMembersChanged()
}

type peerConn struct {
	id  string
	c   net.Conn
	enc *protocol.Encoder
	mu  sync.Mutex
}

func (p *peerConn) send(msgType string, payload interface{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enc.Send(msgType, payload)
}

// Pool tracks membership of a single device pool from this node's point of
// view. There is no central server: every member keeps its own copy of the
// state below, and they converge via gossip.
type Pool struct {
	ID   string
	Self Node

	handler Handler
	ln      net.Listener
	rv      *rendezvous.Client // set once connected to a rendezvous server, nil otherwise

	mu            sync.RWMutex
	members       map[string]Node
	conns         map[string]*peerConn
	dialing       map[string]bool
	lastSeen      map[string]time.Time
	activeID      string
	activeVersion uint64
}

// New allocates a pool bound to no ID yet; call CreatePool or JoinPool next.
func New(name string, screenW, screenH int) *Pool {
	self := Node{ID: id.NodeID(), Name: name, ScreenW: screenW, ScreenH: screenH}
	return &Pool{
		Self:     self,
		members:  map[string]Node{self.ID: self},
		conns:    map[string]*peerConn{},
		dialing:  map[string]bool{},
		lastSeen: map[string]time.Time{},
	}
}

func (p *Pool) SetHandler(h Handler) { p.handler = h }

func (p *Pool) listen() error {
	ln, err := net.Listen("tcp4", ":0")
	if err != nil {
		return err
	}
	_, ip, err := discovery.LocalInterface()
	if err != nil {
		ln.Close()
		return err
	}
	p.ln = ln
	p.mu.Lock()
	p.Self.Addr = fmt.Sprintf("%s:%d", ip.String(), ln.Addr().(*net.TCPAddr).Port)
	p.members[p.Self.ID] = p.Self
	p.mu.Unlock()
	return nil
}

// CreatePool starts a brand-new pool with self as its only, active member.
// If rvAddr is non-empty, the pool ID is registered with that rendezvous
// server so devices outside this LAN can find and join it; if the server is
// unreachable, CreatePool falls back to a locally-generated ID (LAN-only).
func (p *Pool) CreatePool(rvAddr string) (string, error) {
	if err := p.listen(); err != nil {
		return "", err
	}
	if rvAddr != "" {
		if rc, err := rendezvous.Dial(rvAddr, 5*time.Second); err == nil {
			if poolID, err := rc.Create(p.Self.info()); err == nil {
				p.ID = poolID
				p.rv = rc
			} else {
				rc.Close()
			}
		}
	}
	if p.ID == "" {
		p.ID = id.PoolID()
	}
	p.mu.Lock()
	p.activeID = p.Self.ID
	p.activeVersion = 1
	p.mu.Unlock()
	return p.ID, nil
}

// JoinPool locates an existing pool by ID and connects to it. It tries LAN
// multicast discovery first (fast, no dependency on the internet or a
// third party); if that finds nothing and rvAddr is set, it falls back to
// asking a rendezvous server. Either way, once connected this node also
// registers with the rendezvous server (when configured) so it is itself
// discoverable by future joiners regardless of how it found the pool.
// Pool traffic always stays a direct connection between the two peers.
func (p *Pool) JoinPool(ctx context.Context, disc *discovery.Conn, rvAddr, poolID string) error {
	if err := p.listen(); err != nil {
		return err
	}
	p.ID = poolID

	connected := false
	var lastErr error

	found := disc.FindPool(ctx, poolID, p.Self.ID, p.Self.Name, 3*time.Second)
	for _, f := range found {
		c, err := net.DialTimeout("tcp4", f.TCPAddr, 4*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		if err := p.handshakeDial(c); err != nil {
			lastErr = err
			continue
		}
		connected = true
		break
	}

	if rvAddr != "" {
		rc, err := rendezvous.Dial(rvAddr, 5*time.Second)
		if err != nil {
			if !connected {
				return fmt.Errorf("пул %s не найден в локальной сети; сервер обнаружения %s недоступен: %w", poolID, rvAddr, err)
			}
		} else {
			peers, err := rc.Join(poolID, p.Self.info())
			if err != nil {
				rc.Close()
				if !connected {
					return fmt.Errorf("пул %s не найден ни в локальной сети, ни на сервере обнаружения: %w", poolID, err)
				}
			} else {
				p.rv = rc
				if !connected {
					for _, peer := range peers {
						n := fromInfo(peer.MemberInfo)
						n.PublicAddr = peer.PublicAddr
						for _, addr := range dialCandidates(n) {
							c, err := net.DialTimeout("tcp4", addr, 4*time.Second)
							if err != nil {
								lastErr = err
								continue
							}
							if err := p.handshakeDial(c); err != nil {
								lastErr = err
								continue
							}
							connected = true
							break
						}
						if connected {
							break
						}
					}
				}
			}
		}
	}

	if !connected {
		if lastErr == nil {
			lastErr = errors.New("устройство не отвечает")
		}
		if rvAddr == "" {
			return fmt.Errorf("пул %s не найден в локальной сети, а сервер обнаружения не задан: %w", poolID, lastErr)
		}
		return fmt.Errorf("пул %s найден, но не удалось подключиться напрямую ни к одному участнику (вероятно, NAT/файрвол блокирует входящие): %w", poolID, lastErr)
	}
	return nil
}

// Start launches all background loops: TCP accept, gossip, heartbeat, mesh
// maintenance, LAN discovery (announcing + answering lookups) and, if a
// rendezvous server was used, its push-notification/keepalive loops.
func (p *Pool) Start(ctx context.Context, disc *discovery.Conn) {
	go p.acceptLoop(ctx)
	go p.gossipLoop(ctx)
	go p.heartbeatLoop(ctx)
	go p.meshMaintenanceLoop(ctx)
	go disc.AnswerRequests(ctx, p.Self.ID, p.Self.Name, func() string { return p.ID }, func() string { return p.Self.Addr })
	go disc.AnnouncePresence(ctx, p.Self.ID, p.Self.Name, func() string { return p.ID }, p.Self.Addr, 2*time.Second)
	if p.rv != nil {
		go p.rv.Listen(
			func(peer protocol.RVPeer) {
				mi := peer.MemberInfo
				mi.PublicAddr = peer.PublicAddr
				p.mergeMembers([]protocol.MemberInfo{mi})
			},
			func(string) {}, // heartbeat/mesh maintenance already reap departed peers
		)
		go p.rv.Keepalive(ctx, 20*time.Second)
	}
}

// Leave gracefully notifies peers and tears down the network.
func (p *Pool) Leave() {
	p.mu.RLock()
	peers := make([]*peerConn, 0, len(p.conns))
	for _, pc := range p.conns {
		peers = append(peers, pc)
	}
	self := p.Self.ID
	p.mu.RUnlock()
	for _, pc := range peers {
		_ = pc.send(protocol.TypeBye, struct {
			NodeID string `json:"node_id"`
		}{self})
		pc.c.Close()
	}
	if p.ln != nil {
		p.ln.Close()
	}
	if p.rv != nil {
		p.rv.Close()
	}
}

// --- handshake ---

func (p *Pool) handshakeDial(c net.Conn) error {
	enc := protocol.NewEncoder(c)
	dec := protocol.NewDecoder(c)

	if err := enc.Send(protocol.TypeHello, protocol.Hello{Self: p.Self.info(), PoolID: p.ID}); err != nil {
		c.Close()
		return err
	}
	env, err := dec.Recv()
	if err != nil {
		c.Close()
		return err
	}
	if env.Type != protocol.TypeHello {
		c.Close()
		return errors.New("unexpected handshake reply")
	}
	var hello protocol.Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		c.Close()
		return err
	}
	peer := fromInfo(hello.Self)
	p.mu.Lock()
	p.members[peer.ID] = peer
	p.mu.Unlock()

	env2, err := dec.Recv()
	if err != nil {
		c.Close()
		return err
	}
	if env2.Type == protocol.TypeMemberSync {
		var ms protocol.MemberSync
		if json.Unmarshal(env2.Payload, &ms) == nil {
			p.mergeMembers(ms.Members)
			p.adoptActive(ms.ActiveID, ms.Version)
		}
	}

	pc := p.registerConn(peer.ID, c, enc)
	if p.handler != nil {
		p.handler.OnMembersChanged()
	}
	go p.readLoop(pc, dec)
	return nil
}

func (p *Pool) acceptLoop(ctx context.Context) {
	go func() {
		<-ctx.Done()
		p.ln.Close()
	}()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handleAccept(c)
	}
}

func (p *Pool) handleAccept(c net.Conn) {
	dec := protocol.NewDecoder(c)
	enc := protocol.NewEncoder(c)

	env, err := dec.Recv()
	if err != nil || env.Type != protocol.TypeHello {
		c.Close()
		return
	}
	var hello protocol.Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		c.Close()
		return
	}
	if hello.PoolID != p.ID {
		c.Close()
		return
	}
	peer := fromInfo(hello.Self)
	p.mu.Lock()
	p.members[peer.ID] = peer
	p.mu.Unlock()

	if err := enc.Send(protocol.TypeHello, protocol.Hello{Self: p.Self.info(), PoolID: p.ID}); err != nil {
		c.Close()
		return
	}

	pc := p.registerConn(peer.ID, c, enc)
	if p.handler != nil {
		p.handler.OnMembersChanged()
	}

	p.mu.RLock()
	snapshot := p.snapshotMemberSync()
	p.mu.RUnlock()
	if err := pc.send(protocol.TypeMemberSync, snapshot); err != nil {
		return
	}

	p.broadcastGossip() // let the rest of the mesh know about the newcomer right away
	go p.readLoop(pc, dec)
}

func (p *Pool) registerConn(peerID string, c net.Conn, enc *protocol.Encoder) *peerConn {
	pc := &peerConn{id: peerID, c: c, enc: enc}
	p.mu.Lock()
	p.conns[peerID] = pc
	p.lastSeen[peerID] = time.Now()
	p.mu.Unlock()
	return pc
}

func (p *Pool) readLoop(pc *peerConn, dec *protocol.Decoder) {
	defer p.dropConn(pc.id)
	for {
		env, err := dec.Recv()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.lastSeen[pc.id] = time.Now()
		p.mu.Unlock()
		p.dispatch(pc.id, env)
	}
}

func (p *Pool) dropConn(peerID string) {
	p.mu.Lock()
	if pc, ok := p.conns[peerID]; ok {
		pc.c.Close()
	}
	delete(p.conns, peerID)
	delete(p.members, peerID)
	delete(p.lastSeen, peerID)
	wasActive := p.activeID == peerID
	p.mu.Unlock()

	if p.handler != nil {
		p.handler.OnMembersChanged()
	}
	if wasActive {
		p.electActive()
	}
}

// --- message dispatch ---

func (p *Pool) dispatch(fromID string, env protocol.Envelope) {
	switch env.Type {
	case protocol.TypePing:
		if pc := p.peer(fromID); pc != nil {
			_ = pc.send(protocol.TypePong, struct{}{})
		}
	case protocol.TypePong:
		// lastSeen already refreshed in readLoop
	case protocol.TypeMemberSync:
		var m protocol.MemberSync
		if json.Unmarshal(env.Payload, &m) == nil {
			p.mergeMembers(m.Members)
			p.adoptActive(m.ActiveID, m.Version)
		}
	case protocol.TypeActiveChange:
		var a protocol.ActiveChange
		if json.Unmarshal(env.Payload, &a) == nil {
			p.adoptActive(a.ActiveID, a.Version)
		}
	case protocol.TypeFocusHandoff:
		var f protocol.FocusHandoff
		if json.Unmarshal(env.Payload, &f) == nil {
			p.adoptActive(f.To, f.Version)
			if p.handler != nil {
				p.handler.OnFocusHandoff(f)
			}
		}
	case protocol.TypeRequestJump:
		var r protocol.RequestJump
		if json.Unmarshal(env.Payload, &r) == nil && p.handler != nil {
			p.handler.OnRequestJump(fromID, r.To)
		}
	case protocol.TypeInputEvent:
		var ev protocol.InputEvent
		if json.Unmarshal(env.Payload, &ev) == nil && p.handler != nil {
			p.handler.OnInputEvent(fromID, ev)
		}
	case protocol.TypeBye:
		p.dropConn(fromID)
	}
}

func (p *Pool) mergeMembers(list []protocol.MemberInfo) {
	var toDial []Node
	p.mu.Lock()
	for _, m := range list {
		if m.ID == p.Self.ID {
			continue
		}
		n := fromInfo(m)
		if _, exists := p.members[m.ID]; !exists {
			toDial = append(toDial, n)
		}
		p.members[m.ID] = n
	}
	p.mu.Unlock()

	if p.handler != nil {
		p.handler.OnMembersChanged()
	}
	for _, n := range toDial {
		p.maybeDial(n)
	}
}

// maybeDial applies a deterministic tie-break (lower ID dials) so two nodes
// that simultaneously learn about each other don't open duplicate
// connections. meshMaintenanceLoop dials the other direction as a fallback
// if that never happens (e.g. the lower-ID side crashed mid-handshake).
func (p *Pool) maybeDial(n Node) {
	if p.Self.ID >= n.ID {
		return
	}
	p.forceDial(n)
}

func (p *Pool) forceDial(n Node) {
	p.mu.Lock()
	_, connected := p.conns[n.ID]
	if connected || p.dialing[n.ID] {
		p.mu.Unlock()
		return
	}
	p.dialing[n.ID] = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.dialing, n.ID)
			p.mu.Unlock()
		}()
		for _, addr := range dialCandidates(n) {
			c, err := net.DialTimeout("tcp4", addr, 4*time.Second)
			if err != nil {
				continue
			}
			if p.handshakeDial(c) == nil {
				return
			}
		}
	}()
}

func (p *Pool) adoptActive(newID string, version uint64) {
	p.mu.Lock()
	if p.activeVersion != 0 && version <= p.activeVersion {
		p.mu.Unlock()
		return
	}
	changed := newID != p.activeID
	p.activeID = newID
	p.activeVersion = version
	p.mu.Unlock()
	if changed && p.handler != nil {
		p.handler.OnActiveChange(newID)
	}
}

// electActive is called after the currently active member disappears.
// Every surviving node runs the same deterministic rule (lowest remaining
// ID wins), so they converge on the same new active member without a vote.
func (p *Pool) electActive() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.members))
	for mid := range p.members {
		ids = append(ids, mid)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		p.mu.Unlock()
		return
	}
	newID := ids[0]
	p.activeVersion++
	p.activeID = newID
	version := p.activeVersion
	p.mu.Unlock()

	if p.handler != nil {
		p.handler.OnActiveChange(newID)
	}
	p.broadcastActiveChange(newID, version)
}

func (p *Pool) broadcastActiveChange(id string, version uint64) {
	p.mu.RLock()
	peers := make([]*peerConn, 0, len(p.conns))
	for _, pc := range p.conns {
		peers = append(peers, pc)
	}
	p.mu.RUnlock()
	for _, pc := range peers {
		_ = pc.send(protocol.TypeActiveChange, protocol.ActiveChange{ActiveID: id, Version: version})
	}
}

// snapshotMemberSync assumes the caller already holds at least a read lock.
func (p *Pool) snapshotMemberSync() protocol.MemberSync {
	list := make([]protocol.MemberInfo, 0, len(p.members))
	for _, n := range p.members {
		list = append(list, n.info())
	}
	return protocol.MemberSync{Members: list, ActiveID: p.activeID, Version: p.activeVersion}
}

func (p *Pool) broadcastGossip() {
	p.mu.RLock()
	snap := p.snapshotMemberSync()
	peers := make([]*peerConn, 0, len(p.conns))
	for _, pc := range p.conns {
		peers = append(peers, pc)
	}
	p.mu.RUnlock()
	for _, pc := range peers {
		_ = pc.send(protocol.TypeMemberSync, snap)
	}
}

// --- background loops ---

func (p *Pool) gossipLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.broadcastGossip()
		}
	}
}

func (p *Pool) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.RLock()
			peers := make([]*peerConn, 0, len(p.conns))
			for _, pc := range p.conns {
				peers = append(peers, pc)
			}
			now := time.Now()
			var stale []string
			for pid, seen := range p.lastSeen {
				if now.Sub(seen) > 7*time.Second {
					stale = append(stale, pid)
				}
			}
			p.mu.RUnlock()

			for _, pc := range peers {
				_ = pc.send(protocol.TypePing, struct{}{})
			}
			for _, pid := range stale {
				p.dropConn(pid)
			}
		}
	}
}

// meshMaintenanceLoop dials any known member we still lack a connection to.
// It is the fallback path for the tie-break in maybeDial and for
// connections lost to transient network hiccups.
func (p *Pool) meshMaintenanceLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.RLock()
			var missing []Node
			for mid, n := range p.members {
				if mid == p.Self.ID {
					continue
				}
				if _, ok := p.conns[mid]; !ok {
					missing = append(missing, n)
				}
			}
			p.mu.RUnlock()
			for _, n := range missing {
				p.forceDial(n)
			}
		}
	}
}

func (p *Pool) peer(id string) *peerConn {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.conns[id]
}

// --- public accessors used by the cursor engine ---

// Ring returns member IDs in a stable, deterministic cyclic order (equal on
// every node without coordination) used to find "the next/previous screen".
func (p *Pool) Ring() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := make([]string, 0, len(p.members))
	for mid := range p.members {
		ids = append(ids, mid)
	}
	sort.Strings(ids)
	return ids
}

func (p *Pool) Member(id string) (Node, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n, ok := p.members[id]
	return n, ok
}

func (p *Pool) Members() []Node {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Node, 0, len(p.members))
	for _, n := range p.members {
		out = append(out, n)
	}
	return out
}

func (p *Pool) ActiveID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeID
}

func (p *Pool) IsActive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeID == p.Self.ID
}

// SendFocusHandoff makes `to` the new active (cursor-owning) member. Only
// meaningful to call while this node is itself active.
func (p *Pool) SendFocusHandoff(to, entryEdge string, entryRatio float64) error {
	p.mu.Lock()
	p.activeVersion++
	version := p.activeVersion
	p.activeID = to
	pc := p.conns[to]
	p.mu.Unlock()

	if pc == nil {
		return fmt.Errorf("нет соединения с устройством %s", to)
	}
	msg := protocol.FocusHandoff{From: p.Self.ID, To: to, EntryEdge: entryEdge, EntryRatio: entryRatio, Version: version}
	if err := pc.send(protocol.TypeFocusHandoff, msg); err != nil {
		return err
	}
	p.broadcastActiveChange(to, version)
	return nil
}

// RequestJump asks for `target` to become active. If this node is already
// active it performs the handoff directly; otherwise it asks whichever node
// currently is.
func (p *Pool) RequestJump(target string) error {
	p.mu.RLock()
	amActive := p.activeID == p.Self.ID
	activePC := p.conns[p.activeID]
	p.mu.RUnlock()

	if amActive {
		return p.SendFocusHandoff(target, "center", 0.5)
	}
	if activePC == nil {
		return errors.New("текущее активное устройство недоступно")
	}
	return activePC.send(protocol.TypeRequestJump, protocol.RequestJump{To: target})
}

// SendInputEvent forwards a locally-captured raw input event to whichever
// member currently owns the cursor.
func (p *Pool) SendInputEvent(ev protocol.InputEvent) {
	p.mu.RLock()
	pc := p.conns[p.activeID]
	p.mu.RUnlock()
	if pc != nil {
		_ = pc.send(protocol.TypeInputEvent, ev)
	}
}
