// Rendezvous protocol: spoken over a plain TCP connection (same
// Envelope/Encoder/Decoder framing as the LAN mesh) between a client and a
// rendezvous server, so pools can be found across the internet and not just
// on the same LAN/multicast domain.
package protocol

const (
	RVCreate     = "rv_create"      // client -> server: register a new pool
	RVJoin       = "rv_join"        // client -> server: join an existing pool by ID
	RVRegistered = "rv_registered"  // server -> client: reply to RVCreate/RVJoin
	RVError      = "rv_error"       // server -> client: request failed (e.g. unknown pool)
	RVPeerJoined = "rv_peer_joined" // server -> client: pushed when another member joins later
	RVPeerLeft   = "rv_peer_left"   // server -> client: pushed when a member disconnects
	RVPing       = "rv_ping"
	RVPong       = "rv_pong"
)

// RVPeer is a pool member as known to the rendezvous server.
type RVPeer struct {
	MemberInfo
	// PublicAddr is the address the server observed this member's control
	// connection come from — reachable across the internet only if the
	// member's NAT/router happens to allow inbound to that port.
	PublicAddr string `json:"public_addr"`
}

type RVCreateReq struct {
	Self MemberInfo `json:"self"`
}

type RVJoinReq struct {
	PoolID string     `json:"pool_id"`
	Self   MemberInfo `json:"self"`
}

// RVRegisteredMsg is the reply to both RVCreate and RVJoin: the pool ID
// (server-assigned for RVCreate) and the current member list (not
// including the caller).
type RVRegisteredMsg struct {
	PoolID string   `json:"pool_id"`
	Peers  []RVPeer `json:"peers"`
}

type RVErrorMsg struct {
	Message string `json:"message"`
}

type RVPeerEvent struct {
	Peer RVPeer `json:"peer"`
}

type RVPeerLeftMsg struct {
	NodeID string `json:"node_id"`
}
