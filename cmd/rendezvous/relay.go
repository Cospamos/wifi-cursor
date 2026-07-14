package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"wifi-cursor/internal/protocol"
)

// relayHub pairs two raw TCP legs that present the same one-time token,
// then pipes bytes between them until either side disconnects. It's the
// fallback data path for a pool pair that couldn't reach each other with a
// direct dial (both behind NAT, no port forwarding) - everything else in
// the pool still connects directly and never touches this.
type relayHub struct {
	mu       sync.Mutex
	sessions map[string]*relaySession
}

type relaySession struct {
	mu     sync.Mutex
	first  *relayLeg
	paired chan struct{}
}

type relayLeg struct {
	conn net.Conn
	br   *bufio.Reader
}

func newRelayHub() *relayHub {
	return &relayHub{sessions: make(map[string]*relaySession)}
}

func (h *relayHub) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Println("relay accept:", err)
			continue
		}
		go h.handleConn(c)
	}
}

func (h *relayHub) handleConn(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var env protocol.Envelope
	if json.Unmarshal(line, &env) != nil || env.Type != protocol.RVRelayJoin {
		c.Close()
		return
	}
	var msg protocol.RVRelayJoinMsg
	if json.Unmarshal(env.Payload, &msg) != nil || msg.Token == "" {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	leg := &relayLeg{conn: c, br: br}

	h.mu.Lock()
	sess, exists := h.sessions[msg.Token]
	if !exists {
		sess = &relaySession{paired: make(chan struct{})}
		h.sessions[msg.Token] = sess
	}
	h.mu.Unlock()

	sess.mu.Lock()
	if sess.first == nil {
		sess.first = leg
		sess.mu.Unlock()
		select {
		case <-sess.paired:
			// the other leg's goroutine is now piping; nothing left to do here
		case <-time.After(20 * time.Second):
			h.mu.Lock()
			if h.sessions[msg.Token] == sess {
				delete(h.sessions, msg.Token)
			}
			h.mu.Unlock()
			c.Close()
		}
		return
	}
	peer := sess.first
	sess.mu.Unlock()

	h.mu.Lock()
	delete(h.sessions, msg.Token)
	h.mu.Unlock()
	close(sess.paired)

	log.Printf("relay: paired session %s", msg.Token)
	pipeLegs(leg, peer)
}

func pipeLegs(a, b *relayLeg) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a.conn, b.br)
		_ = a.conn.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b.conn, a.br)
		_ = b.conn.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}
