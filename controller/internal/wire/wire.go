// Package wire defines the controller <-> CPE agent protocol types.
//
// This file is the single source of truth for the control-plane protocol.
// Both the server and the agent import it, so a field can never drift
// between the two sides (the same discipline that the SRoU shim header
// enforces in the dataplane, applied to the control plane).
//
// Transport note (M-C0): these types are transported as JSON over mTLS.
// Moving to gRPC/protobuf later is a transport swap, not a redesign —
// the fields and their semantics stay exactly as defined here.
package wire

import "sdwan/controller/internal/keybinding"

// ProtocolVersion lets the controller refuse agents it cannot talk to,
// instead of failing in some confusing way later.
//
// v2 (phase 2 / W4): the controller now mediates KEY BINDINGS and issues the
// tenant's E_root. Version 1 agents send no binding, so a controller that
// silently accepted them would leave every peer unauthenticated-and-derivable
// (i.e. an O-key computed from unverified key material) -- the failure would
// look like "the tunnel works but I cannot tell you why". Refusing is the point.
const ProtocolVersion = 2

// ------------------------------------------------------------------ enrollment

// EnrollRequest is sent by a device that has never talked to this
// controller before. It carries NO credential other than the claim code:
// the device's Ed25519 public key is what the controller will certify.
//
// Note what is NOT here: the X25519 key binding. A binding names the device's
// (tenant, node_id), and at first enrollment the device does not know either --
// the claim code resolves to a tenant, and the node id is assigned by this
// request. The binding therefore rides the authenticated heartbeat path, where
// both are known. Trying to squeeze it in here would mean signing a message
// containing values the signer had not been told yet.
type EnrollRequest struct {
	Serial        string `json:"serial"`
	ClaimCode     string `json:"claim_code"`
	PublicKey     string `json:"public_key"` // base64(ed25519 public key)
	ProtocolVer   int    `json:"protocol_ver"`
	AgentVersion  string `json:"agent_version"`
	UDPRelfectorN int    `json:"udp_reflectors"` // how many reflector addrs the agent can probe
}

// EnrollResponse hands back the device certificate plus everything the
// agent needs to operate. Note that the agent never receives any secret of
// the tenant other than RootKey, which is the DOMAIN key: deliberately shared,
// because every node of a tenant must be able to verify every other node's hop
// MAC. It is the O-key (per pair) that the controller never sees.
type EnrollResponse struct {
	TenantID     string   `json:"tenant_id"`
	NodeID       uint16   `json:"node_id"` // 2 bytes: the same ID the SRoU v4 signer field uses
	CertPEM      string   `json:"cert_pem"`
	CAPEM        string   `json:"ca_pem"`
	TLSAddrs     []string `json:"tls_addrs"`     // all known controller addresses (HA: >1)
	UDPReflector []string `json:"udp_reflector"` // STUN-equivalent reflector addresses
	HeartbeatSec int      `json:"heartbeat_sec"`
	Epoch        uint32   `json:"epoch"` // controller-assigned; devices never compute this themselves
	// RootKey is the tenant's E_root as hex (16 bytes). It is the ONLY way the
	// dataplane's D-key domain is established: without it the hop MAC cannot be
	// verified at all, and `no_key` would be permanent rather than a transit
	// node's normal fate.
	RootKey string `json:"root_key"`
	// OpcodeBase is the tenant's instruction-port base (doc 23). Pushed rather
	// than configured, for the same reason the node id is: it is tenant-wide
	// truth, and a node that guesses it writes labels pointing at ports nobody
	// listens on.
	OpcodeBase uint16 `json:"opcode_base"`
	// SrouIP is this node's LABEL address: the value other nodes write into
	// their seg[] when they point at it. It is assigned from a per-tenant pool
	// rather than configured, which is the last step of the Q1 argument -- the
	// label address is a NAME, so nothing about it should need operator input.
	// (An operator who needs a specific value can still pass --srou-ip; the
	// agent then keeps it across re-enrollment, same as the opcode ports.)
	SrouIP string `json:"srou_ip"`
}

// ------------------------------------------------------------------ runtime

// MappingReport tells the controller the public UDP mapping the node
// observed for its tunnel socket. This is the STUN result being registered
// in the rendezvous table — the controller does NOT trust the node to know
// its own public address, it trusts the reflector's observation that the
// node is relaying.
//
// Binding re-announces the node's X25519 key binding. It rides here as well as
// on the heartbeat because this call is what the agent makes immediately after
// enrollment: putting it only on the heartbeat would leave a window (up to one
// heartbeat interval) in which the node is visible to its peers with no key
// material, and they would derive nothing.
type MappingReport struct {
	UDPMapping   string              `json:"udp_mapping"` // "ip:port" as observed by the reflector
	LocalPort    int                 `json:"local_port"`  // the node's own local port (for symmetric-NAT analysis)
	SymmetricNAT bool                `json:"symmetric_nat"`
	Reflector    string              `json:"reflector_addr"` // which reflector produced this mapping
	Binding      *keybinding.Binding `json:"key_binding,omitempty"`
}

// Heartbeat is the liveness + re-registration signal. It carries the
// mapping again because a NAT rebinding must be able to travel on the
// heartbeat path alone (no separate re-registration round trip).
type HeartbeatRequest struct {
	NodeID     uint16              `json:"node_id"`
	UDPMapping string              `json:"udp_mapping"`
	Uptime     int64               `json:"uptime_sec"`
	Binding    *keybinding.Binding `json:"key_binding,omitempty"`
}

// HeartbeatResponse returns the controller-assigned epoch and the current
// peer count, so an agent can notice "my tenant view just changed" even if
// its watch stream is momentarily disconnected.
type HeartbeatResponse struct {
	Epoch     uint32 `json:"epoch"`
	PeerCount int    `json:"peer_count"`
	StaleMe   bool   `json:"stale_me"` // true if the registry thinks this node lapsed
	// BindingErr is a NON-FATAL report: the controller stored the node but could
	// not authenticate the key binding it sent. The node stays registered (its
	// mapping is still useful for rendezvous) but peers will not be able to
	// derive an O-key toward it, so the agent must surface this loudly rather
	// than leaving "tunnel is up but no traffic" unexplained.
	BindingErr string `json:"binding_err,omitempty"`
}

// ------------------------------------------------------------------ rendezvous

// PeerInfo is one entry of the endpoint table, as seen by one tenant.
//
// CertPEM and Binding are relayed so the RECEIVER can authenticate them itself.
// The controller authenticates the binding too (it would otherwise relay
// garbage), but that check is a convenience, not the guarantee: the guarantee
// is that the relying party verifies against the CA it has pinned, and can do
// so even if this controller is lying. Relaying the certificate rather than a
// pre-extracted public key is deliberate -- it lets the receiver re-do the
// whole chain check instead of trusting a digest.
type PeerInfo struct {
	NodeID     uint16 `json:"node_id"`
	Serial     string `json:"serial"`
	UDPMapping string `json:"udp_mapping"`
	// SrouIP is the peer's LABEL address (doc 24 §3.2): the value we write into
	// seg[] when we point at it. Relayed here so a node can program its
	// dataplane for a peer without any hand-configured address.
	SrouIP string `json:"srou_ip,omitempty"`
	// Reachability verdict: "unknown" | "punching" | "ready" | "relay"
	Reach       string              `json:"reach"`
	LastSeen    int64               `json:"last_seen_unix"`
	AgentVer    string              `json:"agent_version"`
	IsSelf      bool                `json:"is_self"`
	Stale       bool                `json:"stale"`
	SymmetricNA bool                `json:"symmetric_nat"`
	CertPEM     string              `json:"cert_pem,omitempty"`
	Binding     *keybinding.Binding `json:"key_binding,omitempty"`
	// KeyGen lets a receiver tell a rotation from a replay without parsing the
	// binding, and makes key generation visible in the registry view.
	KeyGen uint32 `json:"key_gen,omitempty"`
}

// WatchEvent is one line of the push stream. Types:
//
//	snapshot — full tenant view, sent once on connect
//	update   — a peer appeared or changed (mapping change lands here)
//	stale    — a peer lapsed; peers should stop using its mapping
//	removed  — a peer was decommissioned
//
// TenantID is explicit and MUST be checked by the consumer. Node IDs are
// unique only within a tenant, so a bare node ID in an event is ambiguous
// across tenants — that ambiguity caused a real cross-tenant leak in the
// first version of the broadcaster. Identity travels with the message.
type WatchEvent struct {
	Type     string   `json:"type"`
	TenantID string   `json:"tenant_id"`
	Peer     PeerInfo `json:"peer"`
	Epoch    uint32   `json:"epoch"`
	SentAt   int64    `json:"sent_at_unix"`
	Message  string   `json:"message,omitempty"`
}

// ReachReport lets a node tell the controller the outcome of a hole-punch
// attempt, so the controller can escalate to a relay — and so the dashboard
// can show why a tunnel is on the relay path.
type ReachReport struct {
	PeerNodeID uint16 `json:"peer_node_id"`
	Result     string `json:"result"` // "ready" | "failed" | "relay"
	RTTms      int    `json:"rtt_ms,omitempty"`
}
