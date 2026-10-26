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

import (
	"fmt"
	"net/netip"
	"sort"

	"sdwan/controller/internal/keybinding"
)

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

// RRAddress names one route reflector. ID is for humans and session logs; Addr
// is the TLS endpoint the CPE dials ("host:port").
type RRAddress struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

// EnrollResponse is IDENTITY ONLY.
//
// Enrollment does exactly one thing: it binds this device to the tenant the
// claim code belongs to and issues the certificate that proves the binding.
// Everything else the device will eventually run with -- function ports,
// overlay MTU, hop-MAC key ring, reflector list, RR neighbours, tunnels -- is
// the tenant's configuration, and it reaches the device only after the
// customer configures that device in the console (the node-plan contract).
//
// The split is not cosmetic. When enrollment also pushed configuration, a
// device that had merely been associated with a tenant was already running a
// dataplane nobody had asked for, and every configuration change implied a
// re-enrollment. Instead: enroll → device shows up in the console → customer
// configures it → the plan is pushed. A device that enrolled and was never
// configured holds nothing but its identity and a heartbeat.
type EnrollResponse struct {
	TenantID string `json:"tenant_id"`
	NodeID   uint16 `json:"node_id"` // matches the URI SAN of the certificate below
	CertPEM  string `json:"cert_pem"`
	CAPEM    string `json:"ca_pem"`
	// TLSAddrs is controller reachability, not tenant configuration: an agent
	// that just enrolled has to know which of the controller's addresses to
	// use next. Everything a customer could configure lives in the node plan.
	TLSAddrs     []string `json:"tls_addrs"`
	HeartbeatSec int      `json:"heartbeat_sec"`
	Epoch        uint32   `json:"epoch"` // controller-assigned; devices never compute this themselves
}

// PKey is one entry of the tenant's hop-MAC key ring.
type PKey struct {
	HmacID uint16 `json:"hmac_id"`
	Key    string `json:"key"` // hex, 16 bytes (SR_PKEY_LEN)
}

// ---------------------------------------------------------------- tunnel MTU

// The per-packet bytes this dataplane puts on the wire ON TOP of the overlay
// packet. Every number here is measured against the lab dataplane (VPP 26.06 +
// sr_wg), not taken from a document -- and it lives in ONE place because a
// budget computed differently by the controller and the agent is a blackhole
// with two owners. test/scripts/test-mtu.sh re-measures these and asserts them.
//
//	UDP payload    = overlay + WGDataOverheadBytes                    (bare WG)
//	UDP payload    = overlay + WGDataOverheadBytes + SRHeaderBytes(N)  (SR path)
//	outer IPv4 len = UDP payload + OuterUnderlayBytes
//
// Two traces of the same ping, node1 -> node2 over a 2-segment path: an overlay
// packet of 136 B arrived as a 204 B UDP payload (136+32+36), and node2's reply
// -- plain WireGuard, no SR header -- left as a 168 B UDP payload (136+32).
const (
	// WGDataOverheadBytes is a WireGuard transport-data packet minus the packet
	// it carries. The protocol says 12 B header + 16 B tag = 28; VPP 26.06 puts
	// 32 on the wire. The excess is constant across overlay sizes (measured 64
	// through 176), so it is framing, not the kernel's 16-byte plaintext
	// padding. Use the measured number: a budget built on the protocol's 28
	// would let 4 bytes past the underlay MTU on every path.
	WGDataOverheadBytes = 32

	// SRFixedBytes is sr_header.h's SR_FIXED_HDR_LEN + SR_HMAC_LEN, and
	// SRSegmentBytes is one segment (IPv4 + port). Together they are the
	// plaintext header the design constraint puts in front of the ciphertext.
	SRFixedBytes   = 24
	SRSegmentBytes = 6
	MaxSRSegments  = 8 // SR_MAX_SEGS

	// OuterUnderlayBytes is the underlay's own IPv4 + UDP in front of the SR
	// header, i.e. what the underlay interface's MTU has to pay for first.
	OuterUnderlayBytes = 28
)

// DefaultTunnelMTU is the overlay MTU a tunnel gets when the controller pushes
// none. It is chosen against the WORST case this dataplane can produce -- a
// 1500-byte underlay and the maximum 8 segments -- whose exact budget is 1368:
//
//	1360 + 32 + (24 + 6*8) + 28 = 1492 <= 1500
//
// so a tenant that says nothing still cannot emit an oversized frame, on any
// path length, and there are 8 bytes left for the unexpected. It is NOT safe on
// a smaller underlay: 1450 (PPPoE, some CGNATs) budgets 1318 at 8 segments --
// which is exactly why this is a pushed field and not only a constant.
const DefaultTunnelMTU uint16 = 1360

// The legal range for a pushed TunnelMTU. Both ends belong to the contract, not
// to the agent: the controller must not be able to hand out a value its own
// agent refuses to start with, and a pushed MTU is a config the CPE writes into
// VPP without asking anyone.
//
// 576 is the smallest IPv4 reassembly size a conforming host guarantees -- below
// it the tunnel cannot carry a legal packet at all. 9000 is a jumbo frame; above
// it the number is simply wrong. The bounds exist because VPP validates neither:
// it records whatever it is told, and `set interface mtu packet 0` is accepted
// too (measured -- it clears all four slots, i.e. cancels the MTU record). A bad
// value therefore fails silently and surfaces much later as "this tunnel
// blackholes big packets".
const (
	MinTunnelMTU uint16 = 576
	MaxTunnelMTU uint16 = 9000
)

// TCPOverheadBytes is what a TCP segment carries in front of its payload in an
// IPv4 packet: 20 B IP + 20 B TCP.
const TCPOverheadBytes = 40

// MPLSLabelBytes is the MPLS shim header a VPN-VRF route pushes: 20-bit label +
// 3-bit TC + 1-bit S + 8-bit TTL = exactly 4 bytes. It is budgeted
// unconditionally because a terminal behind a 1500-byte LAN cannot tell whether
// its flow will land in a VRF, and an under-clamped MSS only shows up as
// "big packets vanish" much later.
const MPLSLabelBytes = 4

// MSSOverheadBytes is everything that rides in front of a TCP payload inside
// the tunnel: the IPv4 header, the TCP header and the VPN label.
const MSSOverheadBytes = TCPOverheadBytes + MPLSLabelBytes

// TCPMSSForTunnelMTU is the MSS a CPE must clamp TCP to on a tunnel of this
// overlay MTU (2026-10-24 ruling: automatic, MTU - 44 since the VPN label
// layer was added).
//
// Why clamp at all: a LAN terminal sits behind a 1500-byte interface and
// negotiates MSS 1460, while the tunnel carries at most mtu bytes. VPP does
// carry such a segment -- it fragments it at the WireGuard output (measured
// 2026-10-22) -- but every piece then gets its own SR/WG header and the outer
// fragments land on the underlay, the shape middleboxes drop. Clamping the MSS
// the terminal sees stops it from producing those segments in the first place.
//
// In this lab the tunnel MTU already reaches the terminal because the kernel
// route carries `mtu 1360`; a real LAN does not, so the CPE's clamp is the only
// thing that tells both TCP endpoints the truth. test/scripts/test-mtu.sh
// re-measures the clamp on the wire (ASSERT J).
func TCPMSSForTunnelMTU(mtu uint16) uint16 {
	if mtu <= MSSOverheadBytes {
		return 0
	}
	return mtu - MSSOverheadBytes
}

// SRHeaderBytes is the on-wire length of the SR header for a path with segCount
// segments (0 means "no SR header at all", i.e. plain WireGuard direct).
func SRHeaderBytes(segCount uint8) int {
	if segCount == 0 {
		return 0
	}
	return SRFixedBytes + int(segCount)*SRSegmentBytes
}

// TunnelMTUBudget answers: given an underlay interface MTU and the worst
// segment count a path on this tunnel may carry, how large may an overlay
// packet be before the OUTER frame overruns the underlay?
//
// Over budget the packet is still not refused -- VPP cuts the OVERLAY packet at
// the tunnel MTU instead (measured 2026-10-22: overlay 1436 through a wg0
// recorded at 1360 left as two WireGuard packets carrying inner fragments
// 1356 + 100, each with its own SR header, outer 1452 + 196, delivered). That
// is why the budget matters rather than "the dataplane handles it anyway": a
// tunnel configured ABOVE its budget puts the fragments on the underlay, where
// they are the real hazard -- middleboxes drop lone UDP fragments, and although
// the hop MAC protects the SR header, a header split across fragments is only
// deliverable if the receiver reassembles before the UDP dispatch. VPP does
// reassemble here (Decap_hmac_fail stayed 0 through the fragmented probe), but
// a production WAN path is not obliged to. So the budget is what keeps the
// tunnel from silently turning into "works until someone sends a big packet".
// Returns 0 when no overlay packet can fit (the caller must then refuse the
// configuration rather than install a tunnel that can only fragment).
func TunnelMTUBudget(underlayMTU uint16, segCount uint8) uint16 {
	budget := int(underlayMTU) - OuterUnderlayBytes - WGDataOverheadBytes - SRHeaderBytes(segCount)
	if budget < 0 {
		return 0
	}
	return uint16(budget)
}

// ------------------------------------------------------------------ runtime

// MaxTunnelEndpoints bounds one node's declared tunnel table. It is the same
// order as the route protocol's MaxTunnelsPerDevice but deliberately a separate
// constant: the two guard different stores (this controller's registry and one
// RR session's table), and the number a node announces to its RR may be a
// subset of what its dataplane holds.
const MaxTunnelEndpoints = 4096

// TunnelEndpoint is one tunnel as the declaring node's own dataplane sees it:
// the peer it reaches and the OVERLAY address of the declaring node on that
// tunnel.
//
// It is declared, not assigned. The address belongs to an interface the node
// built, and the controller has no way to invent it. It exists because the
// route protocol (v3) puts no next hop on a prefix: a next hop only means
// something to the RECEIVER, so each side has to know "which address of MINE
// does my peer send to". When X declares {peer: R, local_ip: 192.168.99.3},
// the controller shows 192.168.99.3 to R as X's address toward R, and R then
// resolves "routes learned from X via 192.168.99.3" without X ever carrying a
// next hop on its prefixes.
//
// Parallel tunnels to the same peer are legal: two entries, two addresses.
// Same peer AND same address is one tunnel declared twice and is collapsed.
type TunnelEndpoint struct {
	PeerNodeID uint16 `json:"peer_node_id"`
	LocalIP    string `json:"local_ip"` // canonical IPv4; this node's own overlay /32 on that tunnel
}

// NormaliseTunnelEndpoints validates one node's declared table and returns it
// in canonical form: dotted-quad IPv4, ascending by (peer, address), exact
// duplicates collapsed.
//
// self is the declaring node. A tunnel to yourself is refused rather than
// dropped: it can only come from a bug, and a silently dropped entry would
// leave the registry row looking healthy.
func NormaliseTunnelEndpoints(self uint16, in []TunnelEndpoint) ([]TunnelEndpoint, error) {
	if len(in) > MaxTunnelEndpoints {
		return nil, fmt.Errorf("%d tunnel endpoints exceeds %d", len(in), MaxTunnelEndpoints)
	}
	out := make([]TunnelEndpoint, 0, len(in))
	for i, t := range in {
		if t.PeerNodeID == 0 {
			return nil, fmt.Errorf("tunnel endpoint #%d: peer_node_id must be non-zero", i)
		}
		if t.PeerNodeID == self {
			return nil, fmt.Errorf("tunnel endpoint #%d: a tunnel to this node itself", i)
		}
		a, err := netip.ParseAddr(t.LocalIP)
		if err != nil {
			return nil, fmt.Errorf("tunnel endpoint to node %d: bad local_ip %q: %w", t.PeerNodeID, t.LocalIP, err)
		}
		if !a.Is4() {
			return nil, fmt.Errorf("tunnel endpoint to node %d: local_ip %q is not IPv4", t.PeerNodeID, t.LocalIP)
		}
		if a.IsUnspecified() || a.IsMulticast() {
			return nil, fmt.Errorf("tunnel endpoint to node %d: local_ip %q is not a unicast address", t.PeerNodeID, t.LocalIP)
		}
		out = append(out, TunnelEndpoint{PeerNodeID: t.PeerNodeID, LocalIP: a.String()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PeerNodeID != out[j].PeerNodeID {
			return out[i].PeerNodeID < out[j].PeerNodeID
		}
		return out[i].LocalIP < out[j].LocalIP
	})
	deduped := make([]TunnelEndpoint, 0, len(out))
	for i, t := range out {
		if i > 0 && t == out[i-1] {
			continue
		}
		deduped = append(deduped, t)
	}
	return deduped, nil
}

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

	// Tunnels is this node's whole tunnel table: for every tunnel its dataplane
	// holds, the peer and THIS node's own overlay address on it (see
	// TunnelEndpoint). nil means "not reported" (an agent that does not fill it,
	// or a report that only refreshes liveness); an EMPTY non-nil list means "I
	// have no tunnels", which is a real state and must be declarable -- otherwise
	// a torn-down tunnel would sit in the registry forever and peers would keep
	// resolving a next hop to an interface that no longer exists.
	Tunnels *[]TunnelEndpoint `json:"tunnels,omitempty"`
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
	// OverlayIPs are this peer's own overlay addresses on the tunnels it built
	// TOWARD THE READER -- not a node-wide address list. Filled per reader (like
	// ObservedMapping), so what a reader receives is exactly the set of next hops
	// it may use for routes learned from this peer, one per parallel tunnel.
	//
	// This is the controller-side half of protocol v3: prefixes carry no next
	// hop, so the receiver resolves "routes from X" against these addresses from
	// its own tunnel table. An empty list means the peer has not declared a
	// tunnel toward this reader (yet), and routes from it cannot be installed.
	OverlayIPs []string `json:"overlay_ips,omitempty"`
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

// -------------------------------------------------- in-tunnel telemetry

// The telemetry split (settled 2026-10-18) exists because the two dataplane
// mechanisms answer different questions and only one of them has numbers:
//
//   - Reachability comes from VPP's own BFD. It detects a dead tunnel in about
//     a second at the lab's 50 ms interval, but exposes NO quality metrics at
//     all -- neither bfd_udp_session_details nor the session struct carries rtt,
//     jitter or loss (measured 2026-10-24).
//   - Quality comes from the plugin's own probe (sr_wg_probe, UDP 51829): one
//     packet type, echoed by the peer, giving RTT, RFC3550 jitter and a sliding
//     window loss ratio. There is deliberately no bidirectional STAMP.
//
// The two tables do NOT share a session key, so the agent joins them and the
// northbound only ever sees one row per tunnel:
//   - the probe is per WG interface (sw_if_index), one session per tunnel;
//   - BFD is a MULTIHOP session (sw_if_index = ~0) addressed by (local, peer).
//     Interface-bound BFD must not be used: on a WG interface it replaces the
//     overlay route's forwarding DPO with dpo-drop whenever the peer has no
//     session, taking the tunnel down in both directions (measured 2026-10-24).
// TunnelMetrics therefore carries sw_if_index as the identity while the agent
// does the (local_ip, peer_ip) lookup internally.

// Telemetry defaults live in the contract, not in the agent: the controller must
// be able to say "you get 300 ms" and have the agent install exactly that, and
// the two sides must agree on what a zero field means.
const (
	DefaultProbeIntervalMS uint32 = 300
	// ProbeTimeoutMult is the plugin's rule for TimeoutMS=0: three intervals.
	ProbeTimeoutMult uint32 = 3
	// BFD defaults match the lab session that detected a dead WG interface in
	// 1.5 s (2026-10-24). Detection time is detect_mult * max(tx, rx).
	DefaultBFDMinTxUS    uint32 = 50000
	DefaultBFDMinRxUS    uint32 = 50000
	DefaultBFDDetectMult uint8  = 3
)

// TunnelTelemetry is the controller's per-tunnel telemetry configuration. It is
// pushed, not derived: the probe interval is a controller policy knob (default
// 300 ms) and BFD timers decide how fast a dead tunnel is noticed, which is also
// a policy choice, not something an agent can guess from local state.
type TunnelTelemetry struct {
	// SwIfIndex names the tunnel's WG interface. The controller learns it from
	// the agent's mapping report; it is not derived from node_id.
	SwIfIndex  uint32 `json:"sw_if_index"`
	PeerNodeID uint16 `json:"peer_node_id,omitempty"`
	PeerIP     string `json:"peer_ip"`
	// LocalIP is the local overlay /32 on this tunnel. Probe could auto-pick the
	// interface's first IPv4, but BFD cannot: bfd_udp_add needs a concrete local
	// address, and on a multi-tunnel node "first address of the interface" is
	// exactly the kind of implicit choice that ends up binding the wrong /32.
	LocalIP string `json:"local_ip"`

	// IntervalMS is the probe interval; 0 means DefaultProbeIntervalMS (300).
	// TimeoutMS 0 means "3 intervals" (the plugin's own default). A non-zero
	// timeout shorter than the interval is refused: the probe would be declared
	// lost before its reply could possibly arrive.
	IntervalMS uint32 `json:"interval_ms,omitempty"`
	TimeoutMS  uint32 `json:"timeout_ms,omitempty"`

	// BFD timers in microseconds (VPP's native unit). Zero means the lab default
	// (50 ms / 50 ms / x3). VPP does not clamp these: a 1000 us session is
	// accepted as-is, so the values travel explicitly rather than being implied.
	BFDMinTxUS    uint32 `json:"bfd_min_tx_us,omitempty"`
	BFDMinRxUS    uint32 `json:"bfd_min_rx_us,omitempty"`
	BFDDetectMult uint8  `json:"bfd_detect_mult,omitempty"`
}

// TunnelMetrics is one tunnel's merged telemetry row, as reported by the agent.
//
// ProbeTx/Rx/Lost/Late are DELTAS over WindowSec, not lifetime counters: a
// dashboard that plots a lifetime counter shows a flat line forever, and a
// controller that recomputes deltas from two reports cannot tell a counter
// reset (session re-created) from real traffic. Window failures are therefore
// computed by the only party that samples both ends of the window.
//
// RTT/jitter values are snapshots, not window aggregates: min/avg/max are the
// probe session's lifetime scope (the plugin tracks them that way) and jitter is
// its current RFC3550 EWMA. That difference is stated in the field comments
// rather than hidden behind a single "rtt" number with ambiguous meaning.
type TunnelMetrics struct {
	SwIfIndex  uint32 `json:"sw_if_index"`
	PeerNodeID uint16 `json:"peer_node_id,omitempty"`
	PeerIP     string `json:"peer_ip"`
	// Up is the BFD verdict. It is the only reachability signal here; a probe
	// that stops replying is reported as loss, never as "down", because loss on
	// a live path and a dead path need different operator responses.
	Up       bool   `json:"up"`
	BFDState string `json:"bfd_state"` // "up" | "init" | "down" | "admin-down" | "absent"

	IntervalMS uint32 `json:"interval_ms"`
	WindowSec  uint32 `json:"window_sec"`

	ProbeTx   uint64 `json:"probe_tx"`
	ProbeRx   uint64 `json:"probe_rx"`
	ProbeLost uint64 `json:"probe_lost"`
	ProbeLate uint64 `json:"probe_late"`
	// LossPermille is the window loss ratio (probe_lost / probe_tx * 1000).
	// When no probe was sent in the window it falls back to the plugin's
	// sliding-window value, which still describes the recent past.
	LossPermille uint32 `json:"loss_permille"`

	RTTms    float64 `json:"rtt_ms"`     // most recent reply
	MinRTTms float64 `json:"min_rtt_ms"` // lifetime minimum
	AvgRTTms float64 `json:"avg_rtt_ms"` // lifetime mean
	MaxRTTms float64 `json:"max_rtt_ms"` // lifetime maximum
	JitterMs float64 `json:"jitter_ms"`  // RFC3550 EWMA, current
}

// MetricsReport carries one sampling window of in-tunnel telemetry. There is no
// controller-side delta bookkeeping: the agent owns the window, so a report is
// self-contained and a lost report does not corrupt the next one.
type MetricsReport struct {
	NodeID  uint16          `json:"node_id"`
	Tunnels []TunnelMetrics `json:"tunnels"`
}

// NodeTelemetry is one node's most recent sampling window as the controller
// holds it. AgeSec is filled at READ time -- a stored age would start lying the
// moment it was written.
type NodeTelemetry struct {
	NodeID  uint16          `json:"node_id"`
	SeenAt  int64           `json:"seen_at_unix"`
	AgeSec  uint64          `json:"age_sec"`
	Tunnels []TunnelMetrics `json:"tunnels"`
}

// TelemetryView is the northbound read of a tenant's in-tunnel telemetry.
//
// Rows are NOT joined across nodes, even when both ends report the same tunnel:
// each end has its own BFD session and its own probe counters, and when a link
// breaks the two ends usually say different things (one sees loss, the other
// sees down). That disagreement IS the diagnosis, so the controller keeps the
// two rows side by side instead of inventing a merged verdict.
type TelemetryView struct {
	TenantID string          `json:"tenant_id"`
	Self     uint16          `json:"self"`
	Nodes    []NodeTelemetry `json:"nodes"`
}
