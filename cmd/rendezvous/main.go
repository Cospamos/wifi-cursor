// Command rendezvous is the VPS-hosted signaling server: it lets wifi-cursor
// pools be found and joined across the internet, where LAN UDP multicast
// can't reach. It only ever brokers introductions — the actual pool traffic
// (cursor/mouse events) stays direct peer-to-peer between clients, exactly
// as on a LAN; this process never sees it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"wifi-cursor/internal/id"
	"wifi-cursor/internal/protocol"
)

type client struct {
	nodeID string
	conn   net.Conn
	enc    *protocol.Encoder
	mu     sync.Mutex
}

func (c *client) send(msgType string, payload interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Send(msgType, payload)
}

type poolState struct {
	mu       sync.Mutex
	passHash string
	members  map[string]protocol.RVPeer
	clients  map[string]*client
}

type server struct {
	mu    sync.Mutex
	pools map[string]*poolState
}

func newServer() *server {
	return &server{pools: make(map[string]*poolState)}
}

func (s *server) newPoolID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		pid := id.PoolID()
		if _, exists := s.pools[pid]; !exists {
			return pid
		}
	}
}

func (s *server) getPool(poolID string) (*poolState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	return p, ok
}

func (s *server) getOrCreatePool(poolID string) *poolState {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		p = &poolState{members: map[string]protocol.RVPeer{}, clients: map[string]*client{}}
		s.pools[poolID] = p
	}
	return p
}

func (s *server) dropPoolIfEmpty(poolID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		return
	}
	p.mu.Lock()
	empty := len(p.members) == 0
	p.mu.Unlock()
	if empty {
		delete(s.pools, poolID)
	}
}

func main() {
	addr := flag.String("addr", ":47990", "TCP listen address (control/signaling)")
	relayAddr := flag.String("relay-addr", ":47991", "TCP listen address (relay data plane, fallback for NATed pairs)")
	flag.Parse()

	ln, err := net.Listen("tcp4", *addr)
	if err != nil {
		log.Fatal(err)
	}
	relayLn, err := net.Listen("tcp4", *relayAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wifi-cursor rendezvous server listening on %s (control) and %s (relay)", *addr, *relayAddr)

	hub := newRelayHub()
	go hub.serve(relayLn)

	srv := newServer()
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		go srv.handleConn(c)
	}
}

func (s *server) handleConn(c net.Conn) {
	defer c.Close()
	enc := protocol.NewEncoder(c)
	dec := protocol.NewDecoder(c)

	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	env, err := dec.Recv()
	if err != nil {
		return
	}

	publicAddr := c.RemoteAddr().String()

	var poolID string
	var self protocol.MemberInfo
	var passHash string
	newPool := false

	switch env.Type {
	case protocol.RVCreate:
		var req protocol.RVCreateReq
		if json.Unmarshal(env.Payload, &req) != nil {
			return
		}
		self = req.Self
		passHash = req.PassHash
		poolID = s.newPoolID()
		newPool = true

	case protocol.RVJoin:
		var req protocol.RVJoinReq
		if json.Unmarshal(env.Payload, &req) != nil {
			return
		}
		self = req.Self
		poolID = req.PoolID
		existing, exists := s.getPool(poolID)
		if !exists {
			_ = enc.Send(protocol.RVError, protocol.RVErrorMsg{Message: fmt.Sprintf("пул %s не найден", poolID)})
			return
		}
		existing.mu.Lock()
		wantHash := existing.passHash
		existing.mu.Unlock()
		if wantHash != "" && req.PassHash != wantHash {
			_ = enc.Send(protocol.RVError, protocol.RVErrorMsg{Message: "неверный пароль пула"})
			return
		}

	default:
		return
	}

	pool := s.getOrCreatePool(poolID)
	if newPool {
		pool.mu.Lock()
		pool.passHash = passHash
		pool.mu.Unlock()
	}
	me := protocol.RVPeer{MemberInfo: self, PublicAddr: publicAddr}

	pool.mu.Lock()
	peers := make([]protocol.RVPeer, 0, len(pool.members))
	for _, m := range pool.members {
		peers = append(peers, m)
	}
	notify := make([]*client, 0, len(pool.clients))
	for _, cl := range pool.clients {
		notify = append(notify, cl)
	}
	pool.members[me.ID] = me
	myClient := &client{nodeID: me.ID, conn: c, enc: enc}
	pool.clients[me.ID] = myClient
	pool.mu.Unlock()

	if err := enc.Send(protocol.RVRegistered, protocol.RVRegisteredMsg{PoolID: poolID, Peers: peers}); err != nil {
		return
	}
	for _, cl := range notify {
		_ = cl.send(protocol.RVPeerJoined, protocol.RVPeerEvent{Peer: me})
	}
	log.Printf("pool %s: %s joined from %s (%d other member(s))", poolID, me.ID, publicAddr, len(peers))

	defer func() {
		pool.mu.Lock()
		delete(pool.members, me.ID)
		delete(pool.clients, me.ID)
		remaining := make([]*client, 0, len(pool.clients))
		for _, cl := range pool.clients {
			remaining = append(remaining, cl)
		}
		pool.mu.Unlock()
		for _, cl := range remaining {
			_ = cl.send(protocol.RVPeerLeft, protocol.RVPeerLeftMsg{NodeID: me.ID})
		}
		s.dropPoolIfEmpty(poolID)
		log.Printf("pool %s: %s left", poolID, me.ID)
	}()

	_ = c.SetReadDeadline(time.Time{})
	for {
		env, err := dec.Recv()
		if err != nil {
			return
		}
		switch env.Type {
		case protocol.RVPing:
			_ = myClient.send(protocol.RVPong, struct{}{})
		case protocol.RVRelayRequest:
			var req protocol.RVRelayRequestMsg
			if json.Unmarshal(env.Payload, &req) != nil {
				continue
			}
			pool.mu.Lock()
			target, ok := pool.clients[req.ToNodeID]
			pool.mu.Unlock()
			if ok {
				_ = target.send(protocol.RVRelayOffer, protocol.RVRelayOfferMsg{FromNodeID: me.ID, Token: req.Token})
			}
		}
	}
}
