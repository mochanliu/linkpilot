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
//
// v3 (this tree): the shape follows the DATA PLANE, which is the settled half of
// this project. Three changes, all of them "a field meant something the
// dataplane does not do":
//   - `opcode_base` (POP=base+0, DELIVER=base+1) becomes two explicit ports.
//     The workbuddy convention numbered those two roles the other way round,
//     so a derived port was a coin flip; the plugin's two roles are also not a
//     POP/DELIVER pair. No implied arithmetic is left.
//   - `root_key` (E_root) becomes `p_keys`: the plugin's hop MAC is keyed by
//     `hmac_id` from a per-tenant ring (sr_hmac.h), not derived from a domain
//     root, so the field now names the thing the agent actually installs.
//   - `srou_ip` label addresses are gone. A path is composed from a peer's real
//     outer address, and the new /v1/observe path is what makes that address
//     known from the vantage point that has to send to it.
//
// A v2 agent sends none of these and expects the old ones; accepting it would
// produce exactly the v1 failure mode in a new costume ("it enrolled, so why is
// there no traffic"). Refusing is the point.
const ProtocolVersion = 3

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
// agent needs to operate. Note what is NOT here: any per-peer secret. The
// key ring below is the tenant-wide DOMAIN material, shared on purpose because
// every node must verify every other node's hop MAC; the pairwise tunnel key is
// the one thing the controller never sees and never issues.
type EnrollResponse struct {
	TenantID     string   `json:"tenant_id"`
	NodeID       uint16   `json:"node_id"` // matches the URI SAN of the certificate below
	CertPEM      string   `json:"cert_pem"`
	CAPEM        string   `json:"ca_pem"`
	TLSAddrs     []string `json:"tls_addrs"`     // all known controller addresses (HA: >1)
	UDPReflector []string `json:"udp_reflector"` // STUN-equivalent reflector addresses
	HeartbeatSec int      `json:"heartbeat_sec"`
	Epoch        uint32   `json:"epoch"` // controller-assigned; devices never compute this themselves

	// VpnPort and TransitPort are the tenant's UDP function ports, pushed rather
	// than configured per node. This is constraint (2) of the design -- the
	// destination port IS the dataplane's function-multiplexing key, so a node
	// that guesses it writes path labels pointing at a port nobody listens on,
	// and the symptom is "the tunnel came up and nothing flows".
	//
	// Two separate fields, not a base plus an offset: the two roles are not
	// arithmetic neighbours in this dataplane (51820 = deliver into the VPN,
	// 51821 = transit a segment and forward), and an earlier convention that
	// derived one from the other numbered them in opposite orders in the two
	// codebases. Anything a node has to compute is a thing it can compute
	// differently.
	VpnPort     uint16 `json:"vpn_port"`
	TransitPort uint16 `json:"transit_port"`

	// PKeys is the tenant's hop-MAC key ring, in the exact shape the dataplane
	// API takes (`sr-wg key set <hmac_id> <hex>`, 16-byte key per sr_hmac.h).
	//
	// It is tenant-wide and deliberately so: the SR header is signed by a node
	// the receiver may never have talked to (a transit hop), so the verifying key
	// cannot be pairwise. That is the same argument the old E_root field made --
	// this one just names the key the plugin actually consumes instead of a root
	// it would have to derive from. Rotation is NOT implemented: the ring holds a
	// single hmac_id, and when rotation lands the agent must install the new key
	// before any path references it, because the plugin refuses to delete a key a
	// path still cites.
	PKeys []PKey `json:"p_keys"`
}

// PKey is one entry of the tenant's hop-MAC key ring.
type PKey struct {
	HmacID uint16 `json:"hmac_id"`
	Key    string `json:"key"` // hex, 16 bytes (SR_PKEY_LEN)
}

// ------------------------------------------------------------------ runtime

// MappingReport is a node describing ITSELF: what the reflectors saw, what its
// own addresses are, which ports its dataplane is actually listening on, and its
// key binding.
//
// Two witnesses, and they answer different questions.
//
// UDPMapping is the STUN/reflector answer: "the address a third party saw your
// packet come from". It is what a peer needs in order to send you your FIRST
// packet, and the controller treats it as observation, not as the node's claim
// (the node does not get to assert its own public address; it reports what the
// reflector told it).
//
// The observations on /v1/observe are the other witness: "the address a peer
// actually saw when you talked to IT". Under a symmetric NAT those two answers
// differ, and only the second one is a path back to that peer. So the reflector
// mapping bootstraps the first contact and the observation path maintains the
// ones that matter. A node can therefore be reachable through this report and
// unreachable through that one, which is a real state of the world and not a
// bug to average away.
//
// LocalAddrs is the node's own address set (interface addresses, no ports). It
// exists so the controller can attribute an observed outer source to a node by
// matching against what that node itself declared over an authenticated channel.
// It is declared here and not at enrollment because enrollment is the one
// request with no certificate behind it: an identity claim needs an identity.
// Binding re-announces the node's attested X25519 key. It rides here as well as
// on the heartbeat because this call is what the agent makes immediately after
// enrollment: putting it only on the heartbeat would leave a window (up to one
// heartbeat interval) in which the node is visible to its peers with no key
// material, and they could not build a peer entry for it at all.
//
// What the binding's X25519Pub IS, stated once: it is this node's WireGuard
// STATIC public key -- the same 32 bytes the agent installs into the dataplane
// and the same bytes a peer passes to `wireguard peer add ... public-key`. The
// device's Ed25519 identity key signs it, so a peer can tell "this is node 7's
// tunnel key" from "the controller made this up" (see internal/keybinding for
// what that does and does not protect: it stops key substitution in transit, it
// does not stop an attacker who owns the tenant CA).
type MappingReport struct {
	UDPMapping   string              `json:"udp_mapping"` // "ip:port" as observed by the reflector
	LocalPort    int                 `json:"local_port"`  // the node's own local port (for symmetric-NAT analysis)
	SymmetricNAT bool                `json:"symmetric_nat"`
	Reflector    string              `json:"reflector_addr"` // which reflector produced this mapping
	Binding      *keybinding.Binding `json:"key_binding,omitempty"`

	// VpnPort / TransitPort are what this node's dataplane REALLY listens on,
	// which is not necessarily what enrollment pushed: a node that could not
	// bind the requested port rebinds (`sr_wg_rebind_ports`) and must then tell
	// peers where to find it. Zero means "use the tenant's pushed values".
	VpnPort     uint16   `json:"vpn_port,omitempty"`
	TransitPort uint16   `json:"transit_port,omitempty"`
	LocalAddrs  []string `json:"local_addrs,omitempty"` // each "ip" or "ip/prefix"
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
	// mapping is still useful for rendezvous) but peers cannot install a WireGuard
	// peer for it, so the agent must surface this loudly rather than leaving
	// "tunnel is up but no traffic" unexplained.
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
	// VpnPort / TransitPort are where this peer says it listens. A peer that
	// rebound (it could not bind the pushed port) is only reachable at the port
	// IT advertises, not the one the tenant was issued.
	VpnPort     uint16 `json:"vpn_port,omitempty"`
	TransitPort uint16 `json:"transit_port,omitempty"`
	// ObservedMapping, ObservedAgeSec and ObservedVerified are filled PER READER,
	// like IsSelf: "the outer source this peer's traffic had when I received it".
	// There is no tenant-wide answer to that question -- under a symmetric NAT two
	// observers of the same node see different ports -- so the value a node acts
	// on must be its own. Empty means "I have never seen that peer".
	ObservedMapping  string `json:"observed_mapping,omitempty"`
	ObservedAgeSec   uint64 `json:"observed_age_sec,omitempty"`
	ObservedVerified bool   `json:"observed_verified,omitempty"`
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
//	ping     — stream keepalive, carries no peer
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

// -------------------------------------------------- dataplane observations

// Observation is one row of a node's dataplane observation table
// (vpp-plugin/sr_obs.h; dumped over the API as sr_wg_obs_dump).
//
// Read it as a statement about a packet that was RECEIVED: "a UDP packet whose
// outer source was src_ip:src_port arrived at my port dst_port". There is no
// identity in it, and there cannot be: the SR header's MAC covers everything
// from ver_flags through the segments and NOT the outer IP header
// (vpp-plugin/sr_hmac.c), so even verified=true only proves that some holder of
// a key from this tenant's ring signed a well-formed header -- not that the
// header arrived from the address written here.
//
// Attribution is therefore the controller's job, and it does it the only honest
// way available: match the address against what each node declared about itself
// over its authenticated channel, and refuse to guess when two nodes could both
// have sent it. The alternative -- "nearest match wins" -- turns a lab artifact
// (a bridge that rewrites sources, a shared NAT) into a confident wrong answer
// in the registry.
type Observation struct {
	SrcIP   string `json:"src_ip"`
	SrcPort uint16 `json:"src_port"`
	// DstPort names WHICH ROLE answered: the tenant's VPN port or its transit
	// port. The destination UDP port is the dataplane's function-multiplexing key,
	// so this is what separates "a peer reached my tunnel" from "a peer was
	// transiting through me" -- two facts a node needs kept apart, not merged
	// into one address book.
	DstPort  uint16 `json:"dst_port"`
	RxIf     string `json:"rx_if,omitempty"`
	AgeSec   uint64 `json:"age_sec"`
	Verified bool   `json:"verified"`
	Hits     uint64 `json:"hits"`
}

// ObserveReport carries a node's observation table to the controller.
//
// Why this exists at all: the address a node must SEND to in order to reach a
// peer is not the address that peer thinks it has -- it is the source address
// the peer's packets had as seen from here. Under a symmetric NAT that value is
// per-destination, so only the receiver of the first packet can know it. The
// dataplane records it (sr_obs), the agent dumps it here, and the controller
// turns it into an identity ("that was node 3") and hands it back to the
// observer, which is the only party with a use for it.
type ObserveReport struct {
	NodeID uint16 `json:"node_id"`
	// Reset says the table was cleared before this dump, so a source missing
	// from it really is gone. Without that flag the controller could only ever
	// learn additions, and a mapping that stopped working would keep being
	// advertised as though it were current.
	Reset        bool          `json:"reset"`
	Observations []Observation `json:"observations"`
}

// Attribution is one line of the controller's answer: what the reporter saw,
// and which node it belongs to. NodeID is 0 when the controller cannot say, and
// Reason is then the sentence the agent logs -- "nobody claimed that address"
// and "two nodes claimed it" are different failures with different fixes.
type Attribution struct {
	Mapping string `json:"mapping"` // "src_ip:src_port" exactly as the reporter saw it
	DstPort uint16 `json:"dst_port,omitempty"`
	NodeID  uint16 `json:"node_id,omitempty"`
	Reason  string `json:"reason"`
}

// ObserveResponse is the synchronous answer to an ObserveReport. The reporter
// acts on this; it does not wait for a watch event, because what changed is its
// OWN view and nobody else's.
type ObserveResponse struct {
	Epoch        uint32        `json:"epoch"`
	Accepted     int           `json:"accepted"`
	Attributed   []Attribution `json:"attributed,omitempty"`
	Unattributed []Attribution `json:"unattributed,omitempty"`
}
