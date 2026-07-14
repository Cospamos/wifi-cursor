// Package rendezvous is the client side of the VPS signaling server: it
// lets a pool be created/joined across the internet, where LAN UDP
// multicast can't reach. Once peers are introduced, all pool traffic still
// goes directly between them — this connection is only ever used for
// discovery/introduction, never for relaying cursor input.
package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"wifi-cursor/internal/protocol"
)

type Client struct {
	conn net.Conn
	enc  *protocol.Encoder
	dec  *protocol.Decoder
}

func Dial(addr string, timeout time.Duration) (*Client, error) {
	c, err := net.DialTimeout("tcp4", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &Client{conn: c, enc: protocol.NewEncoder(c), dec: protocol.NewDecoder(c)}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Create registers a brand-new pool and returns its server-assigned ID.
// passHash is protocol.HashPassword(password); empty means no password.
func (c *Client) Create(self protocol.MemberInfo, passHash string) (string, error) {
	if err := c.enc.Send(protocol.RVCreate, protocol.RVCreateReq{Self: self, PassHash: passHash}); err != nil {
		return "", err
	}
	msg, err := c.recvRegistered()
	if err != nil {
		return "", err
	}
	return msg.PoolID, nil
}

// Join registers self as a member of an existing pool and returns its
// current members (not including self). Fails if the pool has a password
// and passHash doesn't match it.
func (c *Client) Join(poolID string, self protocol.MemberInfo, passHash string) ([]protocol.RVPeer, error) {
	if err := c.enc.Send(protocol.RVJoin, protocol.RVJoinReq{PoolID: poolID, Self: self, PassHash: passHash}); err != nil {
		return nil, err
	}
	msg, err := c.recvRegistered()
	if err != nil {
		return nil, err
	}
	return msg.Peers, nil
}

func (c *Client) recvRegistered() (protocol.RVRegisteredMsg, error) {
	env, err := c.dec.Recv()
	if err != nil {
		return protocol.RVRegisteredMsg{}, err
	}
	if env.Type == protocol.RVError {
		var e protocol.RVErrorMsg
		_ = json.Unmarshal(env.Payload, &e)
		if e.Message == "" {
			e.Message = "сервер обнаружения отклонил запрос"
		}
		return protocol.RVRegisteredMsg{}, errors.New(e.Message)
	}
	var msg protocol.RVRegisteredMsg
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		return protocol.RVRegisteredMsg{}, err
	}
	return msg, nil
}

// Listen streams events pushed by the server until the connection closes:
// peers joining/leaving, and relay offers (someone else couldn't reach this
// node directly and asked the server to broker a relay session instead).
// Meant to run in its own goroutine for the lifetime of pool membership.
func (c *Client) Listen(onJoined func(protocol.RVPeer), onLeft func(nodeID string), onRelayOffer func(protocol.RVRelayOfferMsg)) {
	for {
		env, err := c.dec.Recv()
		if err != nil {
			return
		}
		switch env.Type {
		case protocol.RVPeerJoined:
			var e protocol.RVPeerEvent
			if json.Unmarshal(env.Payload, &e) == nil && onJoined != nil {
				onJoined(e.Peer)
			}
		case protocol.RVPeerLeft:
			var e protocol.RVPeerLeftMsg
			if json.Unmarshal(env.Payload, &e) == nil && onLeft != nil {
				onLeft(e.NodeID)
			}
		case protocol.RVRelayOffer:
			var e protocol.RVRelayOfferMsg
			if json.Unmarshal(env.Payload, &e) == nil && onRelayOffer != nil {
				onRelayOffer(e)
			}
		}
	}
}

// RequestRelay asks the server to broker a relay session with another pool
// member, identified by token (caller-generated, used to pair up both legs
// on the relay port). Used as a fallback when a direct dial to that member
// failed.
func (c *Client) RequestRelay(toNodeID, token string) error {
	return c.enc.Send(protocol.RVRelayRequest, protocol.RVRelayRequestMsg{ToNodeID: toNodeID, Token: token})
}

// DialRelay connects to the server's relay data-plane port and joins the
// given session token. Once the matching leg (the other peer, or this node
// answering an RVRelayOffer) also joins with the same token, the server
// pipes bytes between them and the returned conn behaves like a direct
// connection to that peer.
func DialRelay(relayAddr, token string, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("tcp4", relayAddr, timeout)
	if err != nil {
		return nil, err
	}
	if err := protocol.NewEncoder(c).Send(protocol.RVRelayJoin, protocol.RVRelayJoinMsg{Token: token}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Keepalive periodically pings the server so the control connection isn't
// reaped as idle, until ctx is cancelled.
func (c *Client) Keepalive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.enc.Send(protocol.RVPing, struct{}{}) != nil {
				return
			}
		}
	}
}
