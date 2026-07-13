// Package discovery finds other wifi-cursor instances on the local network
// over UDP multicast: periodic presence beacons, and direct "who has pool
// ID X" lookups answered by any current member of that pool.
package discovery

import (
	"context"
	"net"
	"sync"
	"time"

	"wifi-cursor/internal/protocol"
)

// groupIP is a locally-scoped (site-local) multicast address, safe to reuse
// without colliding with well-known multicast services.
const groupIP = "239.255.42.99"

func groupAddr() *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(groupIP), Port: protocol.UDPPort}
}

// Received pairs a decoded discovery message with the sender's address, so a
// response can be unicast directly back to it.
type Received struct {
	Msg  protocol.Discover
	Addr *net.UDPAddr
}

// Conn is a single shared multicast socket for the process. All discovery
// traffic (announcing, answering pool lookups, scanning) fans out from one
// readLoop through subscriber channels, avoiding the need to bind the same
// multicast port more than once.
type Conn struct {
	pc   *net.UDPConn
	mu   sync.Mutex
	subs map[int]chan Received
	next int
}

func Open() (*Conn, error) {
	pc, err := net.ListenMulticastUDP("udp4", nil, groupAddr())
	if err != nil {
		return nil, err
	}
	_ = pc.SetReadBuffer(1 << 16)
	c := &Conn{pc: pc, subs: make(map[int]chan Received)}
	go c.readLoop()
	return c, nil
}

func (c *Conn) Close() error { return c.pc.Close() }

func (c *Conn) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, addr, err := c.pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		d, err := protocol.ParseDiscover(buf[:n])
		if err != nil {
			continue
		}
		r := Received{Msg: d, Addr: addr}
		c.mu.Lock()
		for _, ch := range c.subs {
			select {
			case ch <- r:
			default: // slow subscriber: drop rather than block the read loop
			}
		}
		c.mu.Unlock()
	}
}

func (c *Conn) subscribe() (int, chan Received) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	ch := make(chan Received, 32)
	c.subs[id] = ch
	return id, ch
}

func (c *Conn) unsubscribe(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.subs[id]; ok {
		delete(c.subs, id)
		close(ch)
	}
}

func (c *Conn) Broadcast(d protocol.Discover) error {
	return protocol.SendDiscover(c.pc, groupAddr(), d)
}

func (c *Conn) Reply(to *net.UDPAddr, d protocol.Discover) error {
	return protocol.SendDiscover(c.pc, to, d)
}

// AnnouncePresence periodically broadcasts a beacon until ctx is cancelled.
// poolID is polled on every tick so the beacon always reflects the node's
// current pool membership (empty string before joining/creating one).
func (c *Conn) AnnouncePresence(ctx context.Context, nodeID, name string, poolID func() string, tcpAddr string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_ = c.Broadcast(protocol.Discover{
			Kind:    protocol.DiscoverKindPresence,
			PoolID:  poolID(),
			NodeID:  nodeID,
			Name:    name,
			TCPAddr: tcpAddr,
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AnswerRequests listens until ctx is cancelled and unicasts a response to
// anyone looking up a pool this node currently belongs to.
func (c *Conn) AnswerRequests(ctx context.Context, nodeID, name string, myPoolID func() string, tcpAddr func() string) {
	id, ch := c.subscribe()
	defer c.unsubscribe(id)
	for {
		select {
		case <-ctx.Done():
			return
		case r, ok := <-ch:
			if !ok {
				return
			}
			pid := myPoolID()
			if r.Msg.Kind == protocol.DiscoverKindRequest && pid != "" && r.Msg.PoolID == pid {
				_ = c.Reply(r.Addr, protocol.Discover{
					Kind:    protocol.DiscoverKindResponse,
					PoolID:  pid,
					NodeID:  nodeID,
					Name:    name,
					TCPAddr: tcpAddr(),
				})
			}
		}
	}
}

// FindPool broadcasts a lookup request for poolID and collects distinct
// responder addresses for the given time window.
func (c *Conn) FindPool(ctx context.Context, poolID, nodeID, name string, window time.Duration) []protocol.Discover {
	id, ch := c.subscribe()
	defer c.unsubscribe(id)

	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	_ = c.Broadcast(protocol.Discover{Kind: protocol.DiscoverKindRequest, PoolID: poolID, NodeID: nodeID, Name: name})

	var found []protocol.Discover
	seen := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return found
		case r, ok := <-ch:
			if !ok {
				return found
			}
			if r.Msg.Kind == protocol.DiscoverKindResponse && r.Msg.PoolID == poolID && !seen[r.Msg.NodeID] {
				seen[r.Msg.NodeID] = true
				found = append(found, r.Msg)
			}
		}
	}
}

// ScanPresence passively collects presence beacons for the given window, so
// a joining user can see which pools are currently active nearby.
func (c *Conn) ScanPresence(ctx context.Context, window time.Duration) map[string]protocol.Discover {
	id, ch := c.subscribe()
	defer c.unsubscribe(id)

	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	out := make(map[string]protocol.Discover)
	for {
		select {
		case <-ctx.Done():
			return out
		case r, ok := <-ch:
			if !ok {
				return out
			}
			if r.Msg.Kind == protocol.DiscoverKindPresence && r.Msg.PoolID != "" {
				out[r.Msg.PoolID] = r.Msg
			}
		}
	}
}
