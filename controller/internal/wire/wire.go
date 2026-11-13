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
	Serial       string `json:"serial"`
	ClaimCode    string `json:"claim_code"`
	PublicKey    string `json:"public_key"` // base64(ed25519 public key)
	ProtocolVer  int    `json:"protocol_ver"`
	AgentVersion string `json:"agent_version"`
	// UDPRelfectorN is the agent's offer, not a request: it says how many
	// reflector addresses this device can actually probe (0 when the binary has
	// no probe socket at all). The controller answers with however many it runs,
	// capped at this number -- a symmetric-NAT verdict needs two DISTINCT
	// destinations, and a device that can only send one probe must not be told to
	// compare two.
	UDPRelfectorN int `json:"udp_reflectors"`
	// PreAuth is the index of a 预授权 record the tenant created in advance --
	// today its name, typed by whoever is standing at the rack. Optional: an
	// empty value is an ordinary enrollment. It is NOT a protocol-version change
	// (a controller that does not know the field behaves exactly as before), which
	// is why the response echoes what was actually bound -- the agent has to be
	// able to tell "no controller-side record" from "an old controller dropped my
	// argument".
	PreAuth string `json:"preauth,omitempty"`
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
// overlay MTU, hop-MAC key ring, RR neighbours, tunnels -- is the tenant's
// configuration, and it reaches the device only after the customer configures
// that device in the console (the node-plan contract).
//
// The reflector list is the deliberate exception, and it is an exception
// because of WHEN it is needed, not what it is: address discovery is what makes
// a device reachable at all, so a device that has enrolled and not yet been
// configured must still be able to ask. It is therefore the controller's own
// observation service (like TLSAddrs below), carries no tenant configuration,
// and is empty when the controller runs no reflectors.
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
	// Reflectors are the STUN endpoints ("ip:port") this device should probe to
	// learn its outer address. Empty = the controller runs no reflector service,
	// which is a real deployment (a lab, or a customer with public-address CPEs),
	// and the agent then reports no mapping rather than guessing an address.
	Reflectors []string `json:"reflectors,omitempty"`

	// PreAuthName echoes the 预授权 index this enrollment bound, if any. It exists
	// so the agent can prove the hand-over happened: an agent that asked for an
	// index and gets an empty echo did NOT receive the configuration waiting
	// behind it, which is otherwise invisible from the box.
	PreAuthName string `json:"preauth_name,omitempty"`
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
	UDPMapping   string `json:"udp_mapping"` // "ip:port" as observed by the reflector
	LocalPort    int    `json:"local_port"`  // the node's own local port (for symmetric-NAT analysis)
	SymmetricNAT bool   `json:"symmetric_nat"`
	// SymmetricKnown says whether SymmetricNAT is a measurement or just the zero
	// value. An agent that has not run the two-reflector comparison cannot tell,
	// and reporting false would let the planner build a tunnel that cannot work.
	// The controller only records the verdict when this is true.
	SymmetricKnown bool   `json:"symmetric_known,omitempty"`
	Reflector      string `json:"reflector_addr"` // which reflector produced this mapping
	// ProbeLocal is the LOCAL source address ("ip:port") the probe socket used
	// toward Reflector, read from the kernel's own route decision (it is not a
	// claim, and it is not in the STUN报文 anywhere). It exists because behind
	// NAT the observed address is nobody's interface: the only address on this
	// box that can be matched against the interface inventory -- the only way to
	// learn WHICH port the tunnel must leave from -- is the one the kernel chose
	// on the way out. Empty means "no probe ran"; a value that matches no
	// reported interface is a contradiction the planner must refuse, not smooth
	// over: it means the probe left through a path the dataplane does not own.
	ProbeLocal string `json:"probe_local,omitempty"`
	// Bindings is the device's whole keyring: one attestation per WG key slot,
	// index == slot number. It is a list and not one entry because a device holds
	// keybinding.WGKeySlots keys and a tunnel says which slot its peer uses -- a
	// peer cannot pick a slot it has never been given a key for. nil/empty means
	// "this agent does not report bindings at all", which is different from a
	// list shorter than the slot count (a stale binding set: refuse to build).
	Bindings []keybinding.Binding `json:"key_bindings,omitempty"`

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

// ------------------------------------------------------- customer-side config

// ConfigState is the DEVICE's own account of the customer-side configuration it
// runs. Every field is a claim by the device, and the controller must never
// derive "applied" from "published": those are two different facts, and a
// console that conflates them reports a change as done the moment someone
// clicked a button.
//
// Capable is the honest way to say "this agent was not started with -apply, so
// it will never install anything". Without it the controller could not tell a
// v0 device that ignores the channel from a healthy device that simply has
// nothing to install.
type ConfigState struct {
	Capable bool `json:"capable"`
	// Version is the version installed on disk (the store's current pointer),
	// not the version that was attempted last.
	Version int `json:"version,omitempty"`
	// Applying is true while an apply is in flight; ApplyingVersion is what it
	// is trying to reach. Reported so the page can say "正在应用 v4" instead of
	// showing a stale version with no explanation.
	Applying        bool   `json:"applying,omitempty"`
	ApplyingVersion int    `json:"applying_version,omitempty"`
	OK              bool   `json:"ok,omitempty"`              // last apply/boot succeeded
	AppliedAt       int64  `json:"applied_at_unix,omitempty"` // when that attempt ran
	Source          string `json:"source,omitempty"`          // apply | boot
	Error           string `json:"error,omitempty"`
	// Notes is the device's own "磁盘对、活的没跟上" list (已落盘、未热更 ...). Without
	// it a note-only apply would report as plain success, which is the one thing
	// §5.3 says must never be shown as done.
	Notes []string `json:"notes,omitempty"`
}

// ------------------------------------------------------ underlay delivery

// OverlayRouteTable is the VPP table the tunnel interfaces and the overlay
// routes live in. It is NOT a VPN: VPN N uses table N, and 250 is reserved for
// the transport domain (see test/lab/bringup.sh). The controller sends it in
// every bundle so the device never has to guess a table number.
const OverlayRouteTable uint32 = 250

// UnderlayPathIDBase keeps agent-managed SR path ids away from the ones a
// person types by hand (the lab's manual bringup uses 1..6). path_id is
// PathIDBase+peerNodeID, and one device pair carries one tunnel, so a single
// base is enough for every tunnel on a box.
const UnderlayPathIDBase uint16 = 1000

// UnderlayInstanceBase is the floor of WG user_instance values the agent owns.
// Instances at or above it may be adopted, reconfigured and deleted by the
// device's tunnel reconciler; anything below belongs to configuration a person
// typed and is never touched. The controller derives it the same way as the
// path id (1000+t7) so that a restarted device can recognise its own tunnels
// without a local state file -- VPP is the record.
const UnderlayInstanceBase uint32 = 1000

// RelayHop is one relay device on a tunnel's path, named the way the box has to
// dial it: the hop's outer address and its transit port, already joined. NodeID
// travels with it so a device's logs and its skip reasons can name the hop instead
// of a bare ip:port.
type RelayHop struct {
	NodeID   uint16 `json:"node_id"`
	Endpoint string `json:"endpoint"`
}

// TunnelSpec is one tunnel as ONE end has to build it. Both ends receive their
// own copy: the peer's overlay address, the peer's listening port and the outer
// address to dial are mirrored.
//
// Everything here is derived by the controller from facts the devices reported
// (interface inventory, reflector observation) or authenticated (the peer's
// certificate and key binding). None of it is a policy the device may
// second-guess.
type TunnelSpec struct {
	TunnelID   string `json:"tunnel_id"`
	PeerNodeID uint16 `json:"peer_node_id"`
	PeerSerial string `json:"peer_serial,omitempty"` // for the box's own log lines

	// Instance is the WG user_instance. VPP rejects a duplicate instance (-31),
	// so it is derived from the tunnel's never-reused numeric suffix: 1000+t7.
	Instance uint32 `json:"instance"`
	// PathIDBase: path_id = base + PeerNodeID (see UnderlayPathIDBase).
	PathIDBase uint16 `json:"path_id_base"`

	// Iface is the physical port this tunnel leaves through, by NAME. Indices
	// are renumbered by VPP restarts; names are not.
	Iface string `json:"iface"`
	// SrcIP is the encapsulation source address, set only when the controller
	// could prove which of the device's own addresses the egress port owns
	// (it matches the reflector's observed address). Empty means "pick the
	// egress port's own address on the box".
	SrcIP string `json:"src_ip,omitempty"`

	// ListenPort is this end's own tunnel port; PeerEndpoint is where the peer
	// is dialed ("ip:port", already carrying the peer's listen port unless the
	// operator typed an explicit port).
	ListenPort   uint16 `json:"listen_port"`
	PeerEndpoint string `json:"peer_endpoint"`

	// The overlay /32s. LocalOverlay is installed on this end's tunnel
	// interface (in table OverlayRouteTable); PeerOverlay is routed to it.
	LocalOverlay string `json:"local_overlay"`
	PeerOverlay  string `json:"peer_overlay"`

	// Color is the tunnel's SD-WAN color (0..7): it decides the VPP preference
	// (10+color) of the FIB paths that resolve through this tunnel's next hop.
	// It is a POINTER on purpose, not a default-0 uint8: color 0 is a real
	// color ("best"), while an old controller that never sends the field is a
	// different fact altogether -- the device must be able to tell them apart,
	// because installing every path at preference 0 silently flattens the
	// primary/backup ordering (G-2, docs/g2-tunnel-table-derivation-plan.md §4.1).
	// nil means "the controller predates the color field": the agent refuses to
	// derive the sr-fib tunnel table rather than guess.
	Color *uint8 `json:"color,omitempty"`

	// Peer identity: the certificate and the attested key binding travel with
	// the tunnel so the device authenticates the peer itself instead of
	// trusting the controller's word (see agent.Link.authenticate).
	//
	// Slot is which of the peer's keybinding.WGKeySlots keys THIS tunnel uses,
	// and PeerBinding is that one slot's attestation -- not the peer's whole
	// keyring. The two must agree (binding.Slot == Slot) or the device would
	// install a key the signature never covered. A device picks its own key for
	// the same slot number, so the pair's n-th tunnel is n-th key on both ends.
	PeerCertPEM string              `json:"peer_cert_pem"`
	Slot        uint8               `json:"slot"`
	PeerBinding *keybinding.Binding `json:"peer_binding,omitempty"`

	// Relays is the relay chain, in traversal order: Relays[0] is the FIRST box
	// the packet touches, and the peer is the destination behind all of them.
	// Empty means a direct tunnel. Each Endpoint is the hop's outer address plus
	// its transit port ("ip:port").
	//
	// The agent puts these into the path's segment list REVERSED, because segment
	// 0 is the destination; the order here is the order the operator read.
	Relays []RelayHop `json:"relays,omitempty"`

	// MTU is the overlay MTU; 0 means "not known yet" and the device falls back
	// to DefaultTunnelMTU. UnderlayMTU is the egress port's real link MTU, 0
	// when neither end has reported it.
	MTU         uint16 `json:"mtu"`
	UnderlayMTU uint16 `json:"underlay_mtu,omitempty"`

	// Weight 是这条隧道在颜色表里的分担权重（VPP fib_path.weight，u8）。
	// 0 = 默认 1；Dynamic 开着时它是 agent 动态档位的**基数上限**（agent 只在
	// 1..Weight 之间调）。不是指针：weight 0 与"老控制器没发过"是同一个事实
	// （都是等价分担），不需要区分 —— 这一点与 Color 不同，颜色 0 是真实
	// 颜色而"没发过颜色"会整表拒派生。
	// Dynamic 是"动态 weight"开关：开 = agent 按探针 SLA 调档（丢包优先、
	// 四档、非线性，迟滞 3 拍 × 5s，owner 2026-11-13 拍）。
	Weight  uint8 `json:"weight,omitempty"`
	Dynamic bool  `json:"dynamic,omitempty"`
}

// TunnelSkip is a configured tunnel that could not be delivered to this device,
// with the reason. It is NOT an error: one peer without a certificate must not
// stop every other tunnel on the box from converging. The agent keeps an
// already-installed skipped tunnel running (a momentarily stale observation is
// not a reason to tear down a working link) and reports the reason.
type TunnelSkip struct {
	TunnelID   string `json:"tunnel_id"`
	PeerNodeID uint16 `json:"peer_node_id"`
	Reason     string `json:"reason"`
}

// UnderlayBundle is the whole tunnel view for one device: the tunnels it builds,
// the tunnels it cannot build yet, its relay port, and the tenant's hop-MAC key
// ring.
//
// It is a FULL SNAPSHOT, not a delta: reconciliation is "make the dataplane look
// like this", which is the only shape that survives a lost fetch or a crashed
// device. Version is the controller's underlay generation (underlay.State.Gen),
// bumped on every tunnel/path/settings mutation.
type UnderlayBundle struct {
	TenantID    string `json:"tenant_id"`
	NodeID      uint16 `json:"node_id"`
	Version     int    `json:"version"`
	GeneratedAt int64  `json:"generated_at_unix"`

	OverlayTable uint32 `json:"overlay_table"`
	// TransitPort is this device's relay port; 0 = it relays nothing today.
	TransitPort uint16 `json:"transit_port,omitempty"`

	// Telemetry is the tenant's measurement policy for every tunnel it runs,
	// copied out of the published underlay settings. It travels with the bundle
	// because these are the numbers the console shows and edits: without them the
	// agent has nothing to program and falls back to the defaults below, which is
	// how a tenant's edit ended up changing nothing on the box.
	Telemetry TelemetryPolicy `json:"telemetry"`

	PKeys   []PKey       `json:"p_keys"`
	Tunnels []TunnelSpec `json:"tunnels"`
	Skipped []TunnelSkip `json:"skipped"`
}

// TelemetryPolicy is the per-tenant part of the telemetry configuration: the
// probe interval and the BFD timers, in the units the console uses (ms) for the
// probe and VPP's native unit (us) for BFD. Zero means "use the default next to
// each field in TunnelTelemetry", so an older controller that renders no policy
// still gets the same sessions the defaults describe.
type TelemetryPolicy struct {
	ProbeIntervalMS uint32 `json:"probe_interval_ms,omitempty"`
	BFDMinTxUS      uint32 `json:"bfd_min_tx_us,omitempty"`
	BFDMinRxUS      uint32 `json:"bfd_min_rx_us,omitempty"`
	BFDDetectMult   uint8  `json:"bfd_detect_mult,omitempty"`
}

// TunnelState is the DEVICE's account of the tunnel configuration it runs, the
// same arrangement as ConfigState: the controller never derives "applied" from
// "published". Tunnels is how many links the device currently has installed, a
// number that came from the box rather than from the controller's intent.
type TunnelState struct {
	Capable         bool   `json:"capable"`
	Version         int    `json:"version,omitempty"` // bundle version installed
	Applying        bool   `json:"applying,omitempty"`
	ApplyingVersion int    `json:"applying_version,omitempty"`
	OK              bool   `json:"ok,omitempty"`
	AppliedAt       int64  `json:"applied_at_unix,omitempty"`
	Error           string `json:"error,omitempty"`
	Tunnels         int    `json:"tunnels,omitempty"`
	Skipped         int    `json:"skipped,omitempty"`
}

// Heartbeat is the liveness + re-registration signal. It carries the
// mapping again because a NAT rebinding must be able to travel on the
// heartbeat path alone (no separate re-registration round trip).
type HeartbeatRequest struct {
	NodeID     uint16               `json:"node_id"`
	UDPMapping string               `json:"udp_mapping"`
	Uptime     int64                `json:"uptime_sec"`
	Bindings   []keybinding.Binding `json:"key_bindings,omitempty"`
	// Config is what this device runs right now. nil means "this agent does not
	// implement the config channel" (an older binary), which the controller must
	// not confuse with Capable=false ("newer binary, started without -apply").
	Config *ConfigState `json:"config,omitempty"`
	// Ifaces is this box's own interface inventory, read from the dataplane.
	// nil means "not reported" (an agent started without -vpp-api, or a VPP that
	// could not be reached) -- an EMPTY non-nil list means "the dataplane really
	// has no interfaces", which is a different fact and must be declarable.
	//
	// The addresses are the INTERFACE's own, not the NAT-translated outer source;
	// those differ behind a NAT, and only the controller holds both facts (the
	// reflector's observation is reported on /v1/observe). Matching them is how
	// the underlay egress port gets identified by measurement instead of by a
	// naming convention.
	Ifaces *[]IfaceInfo `json:"ifaces,omitempty"`
	// Tunnel is what this device runs on the underlay channel. nil means "this
	// agent has no tunnel applier at all" (started without -apply, or with no
	// vpp api socket); Capable=false means "newer binary, channel switched off".
	// The distinction matters because only the first one is an old agent.
	Tunnel *TunnelState `json:"tunnel,omitempty"`
	// DiagBusy reports "a remote-diagnosis command is still running here". The
	// controller holds back the next command while this is set, so commands
	// execute one at a time and a slow ping never swallows a newer traceroute.
	DiagBusy bool `json:"diag_busy,omitempty"`
}

// IfaceInfo is one entry of the device's interface inventory. Every field is a
// reading from the dataplane (core's sw_interface_details / ip_address_details),
// never a configuration claim: this is what the box HAS, not what it was told.
type IfaceInfo struct {
	Name      string `json:"name"`
	SWIfIndex uint32 `json:"sw_if_index"`
	// MTU is the L3 MTU (`show interface`'s MTU column, mtu[0]). This is the
	// number an underlay MTU budget must be computed from -- VPP's link_mtu for
	// an af-packet port is the default 9000 and does not describe the NIC.
	MTU     uint32   `json:"mtu"`
	AdminUp bool     `json:"admin_up"`
	LinkUp  bool     `json:"link_up"`
	Addrs   []string `json:"addrs,omitempty"` // each "a.b.c.d/len"
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
	// ConfigVersion is the customer-side version the tenant has PUBLISHED for
	// this device; 0 means nothing was ever published. The device pulls the
	// bundle over /v1/config/bundle when this differs from what it runs, which
	// is what makes the channel work behind NAT: the controller never dials the
	// box.
	ConfigVersion int `json:"config_version,omitempty"`
	// TunnelVersion is the tenant's underlay generation for this device; 0 means
	// no underlay configuration exists yet (nothing published, no tunnels). The
	// device pulls /v1/underlay/bundle when this differs from what it runs.
	TunnelVersion int `json:"tunnel_version,omitempty"`
	// Reflectors re-advertises the STUN endpoints on every beat, not just at
	// enrollment: a device that enrolled before the operator started the
	// reflector service, or a restarted agent that has an identity and so never
	// enrolls again, must still find out. The agent keeps one probe socket for
	// its lifetime, so a changed list is re-probed rather than appended to.
	Reflectors []string `json:"reflectors,omitempty"`
	// Diag is one queued remote-diagnosis command, delivered here instead of a
	// separate pull because the beat is already the reliable downlink. nil =
	// nothing pending (or the agent said it is busy). Delivery is pop-once:
	// the controller marks the command dispatched when it hands it over, and a
	// box that dies before executing it just leaves a "no result" row the
	// console re-runs.
	Diag *DiagCommand `json:"diag,omitempty"`
}

// ------------------------------------------------------------------ rendezvous

// PeerInfo is one entry of the endpoint table, as seen by one tenant.
//
// CertPEM and Bindings are relayed so the RECEIVER can authenticate them itself.
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
	Reach       string               `json:"reach"`
	LastSeen    int64                `json:"last_seen_unix"`
	AgentVer    string               `json:"agent_version"`
	IsSelf      bool                 `json:"is_self"`
	Stale       bool                 `json:"stale"`
	SymmetricNA bool                 `json:"symmetric_nat"`
	CertPEM     string               `json:"cert_pem,omitempty"`
	Bindings    []keybinding.Binding `json:"key_bindings,omitempty"`
	// Bindings is the peer's whole keyring, index == slot number: a reader that
	// wants to talk to this peer has to be told every key it might meet, because
	// which slot a tunnel uses is decided per tunnel pair, not per peer. Each
	// entry carries its own key_gen, so a receiver tells a rotation from a replay
	// without a separate counter list.
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
//   - Reachability comes from VPP's own BFD. Its verdict is a timer rule, not a
//     measurement: a session goes down after detect_mult × the negotiated
//     receive interval of silence —— 50 ms × 3 = 150 ms once both ends are Up.
//     The wall clock is coarser: while a session is Init/Down VPP sends at about
//     1 s (measured 2026-11-05, CPE1 `show bfd sessions` printed Transmit
//     1000000), which is what turned the 150 ms rule into the ~1.5 s a dead
//     interface actually took to be reported (measured 2026-10-24).
//     BFD exposes NO quality metrics at all -- neither
//     bfd_udp_session_details nor the session struct carries rtt, jitter or loss
//     (measured 2026-10-24).
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
	// BFD defaults are owner-mandated: 300 ms × 3 = 900 ms detection window.
	// #286 measured that 50 ms × 3 (150 ms) has no headroom on a single-thread
	// VPP that also runs WG crypto, tap punting and CLI on vpp_main -- BFD
	// sessions flapped with zero packet loss. The window must survive a busy
	// main thread; faster convergence is a per-tenant setting, not a default.
	DefaultBFDMinTxUS    uint32 = 300000
	DefaultBFDMinRxUS    uint32 = 300000
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
// The exception is the first frame after the agent starts sampling a tunnel:
// there is no earlier sample to subtract, so WindowSec is 0 and those four
// numbers are the device's counters since the probe session was created. The
// consumer has to know, because a windowless delta read as a delta makes the
// dashboard count one tunnel's whole life into a single second.
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

	// HasProbe says whether this device has a probe session for the tunnel at
	// all. Reachability-only rows (BFD present, probe never installed -- a half
	// applied tunnel, a plugin that refused the set) must display quality as
	// "未上报", not as 0% loss and 0 ms: a zero is a measurement, absence is not.
	HasProbe bool `json:"has_probe,omitempty"`

	ProbeTx   uint64 `json:"probe_tx"`
	ProbeRx   uint64 `json:"probe_rx"`
	ProbeLost uint64 `json:"probe_lost"`
	ProbeLate uint64 `json:"probe_late"`
	// LossPermille is the plugin's sliding-window loss rate (lost / (replied +
	// lost) over the last SR_PROBE_WINDOW samples), so it is bounded by
	// construction and "nothing lost" is exactly 0. It is NOT
	// probe_lost / probe_tx of this report's window: a probe sent near the end of
	// a window is declared lost in a later one (timeout = 3 intervals), so that
	// ratio only has a numerator in the window where it counts, and it can exceed
	// 100%.
	LossPermille uint32 `json:"loss_permille"`

	RTTms    float64 `json:"rtt_ms"`     // most recent reply
	MinRTTms float64 `json:"min_rtt_ms"` // lifetime minimum
	AvgRTTms float64 `json:"avg_rtt_ms"` // lifetime mean
	MaxRTTms float64 `json:"max_rtt_ms"` // lifetime maximum
	JitterMs float64 `json:"jitter_ms"`  // RFC3550 EWMA, current

	// Per-tunnel traffic, from VPP's per-interface combined counters read off
	// the stats shared-memory segment (no data-plane change: every tunnel is
	// its own WG interface). DELTAS over CounterSec, same window discipline as
	// the probe counters; the first frame after the agent starts counting a
	// tunnel is cumulative (CounterSec = 0), like ProbeTx above.
	//
	// HasCounters=false means the data plane gave nothing this window (old
	// agent binary, stats segment unreadable) -- which is NOT zero traffic,
	// and must render as "未上报", never as 0 B/s. The counters count overlay
	// bytes (inner packets after decap / before encap), so WG+SR+UDP overhead
	// is excluded on purpose: this is what the customer's traffic did.
	HasCounters bool   `json:"has_counters,omitempty"`
	CounterSec  uint32 `json:"counter_sec,omitempty"`
	RxPackets   uint64 `json:"rx_packets,omitempty"`
	RxBytes     uint64 `json:"rx_bytes,omitempty"`
	TxPackets   uint64 `json:"tx_packets,omitempty"`
	TxBytes     uint64 `json:"tx_bytes,omitempty"`
}

// MetricsReport carries one sampling window of in-tunnel telemetry. There is no
// controller-side delta bookkeeping: the agent owns the window, so a report is
// self-contained and a lost report does not corrupt the next one.
type MetricsReport struct {
	NodeID  uint16          `json:"node_id"`
	Tunnels []TunnelMetrics `json:"tunnels"`
	// Selections 是动态策略行**此刻**的选路结果（sla.Selector 的状态快照）。
	// 与 Tunnels 同一节拍上报：控制台策略页的"当前颜色"要的是和 SLA 数字同一
	// 时刻的事实，分成两条消息只会造出"数字说这条好、颜色说走的另一条"的裂缝。
	// nil = 这台盒子没有动态策略（或版本太老），存侧按"没有上报"处理。
	Selections []PolicySelection `json:"selections,omitempty"`
}

// PolicySelection 是一台盒子上一条动态策略行的选路状态。
type PolicySelection struct {
	RowID int    `json:"row_id"` // 与第 7 段的行号同一个号
	VPN   uint32 `json:"vpn"`
	// Color 是当前选中的颜色（0 = 兜底进主表）。Reason 是一句给人看的话
	// （为什么选中/为什么没切），控制台原样展示 —— 选路组件的解释是排障时
	// 唯一便宜的证据。
	Color     uint8  `json:"color"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt int64  `json:"updated_at_unix,omitempty"`
}

// NodeTelemetry is one node's most recent sampling window as the controller
// holds it. AgeSec is filled at READ time -- a stored age would start lying the
// moment it was written.
type NodeTelemetry struct {
	NodeID  uint16          `json:"node_id"`
	SeenAt  int64           `json:"seen_at_unix"`
	AgeSec  uint64          `json:"age_sec"`
	Tunnels []TunnelMetrics `json:"tunnels"`
	// Selections 是这台盒子上动态策略行的当前选路（可能为空：没有动态策略，
	// 或盒子的 agent 还是旧版本）。nil 与空列表在这里是同一个意思 —— 页面把
	// 没上报的行显示成"未上报"，不发明一个颜色。
	Selections []PolicySelection `json:"selections,omitempty"`
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

// ------------------------------------------------------------------ remote diagnostics

// DiagCommand is one remote-diagnosis job the controller hands a device on its
// heartbeat. The tool set is a fixed whitelist (see the agent's executor): a
// diagnostic is something the box runs on the operator's behalf, not a shell.
// Output is capped on both sides and the whole thing is ephemeral -- a pending
// command that outlives a controller restart is dropped, because nobody is
// watching the console for its result anymore.
//
// The channel reuses the config-push shape on purpose: the controller never
// dials the box (it is behind NAT), so the command rides the heartbeat down and
// the result comes back as its own POST.
type DiagCommand struct {
	ID     string `json:"id"`               // diag_<seq>, unique per controller process
	Tool   string `json:"tool"`             // ping | traceroute | vpp-show
	Target string `json:"target,omitempty"` // host/IP for ping & traceroute
	// Count for ping (1..10). Zero is stored as the default on issue.
	Count int `json:"count,omitempty"`
	// Iface is an optional source interface for ping/traceroute (kernel name,
	// e.g. a lcp TAP); validated by charset only -- argv exec has no shell to
	// inject into, and the box reports "unknown iface" from ping itself.
	Iface string `json:"iface,omitempty"`
	// VppShow is the exact vppctl command for tool=vpp-show; the agent accepts
	// only prefixes from its own whitelist.
	VppShow  string `json:"vpp_show,omitempty"`
	IssuedBy string `json:"issued_by,omitempty"`
	IssuedAt int64  `json:"issued_at_unix"`
}

// DiagResult is the box's answer to one DiagCommand. OK=false covers both "the
// tool ran and told us the target is unreachable" (ExitErr) and "the tool
// itself could not run" (Err) -- Output and Err together always say which.
type DiagResult struct {
	NodeID uint16 `json:"node_id"`
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	OK     bool   `json:"ok"`
	// Output is the combined stdout/stderr, capped (the agent truncates, and
	// the controller re-caps before storing: neither side trusts the other).
	Output string `json:"output,omitempty"`
	// Err is the reason the tool could not run or was killed (timeout, missing
	// binary, whitelist refusal). Empty when the tool ran to completion.
	Err      string `json:"err,omitempty"`
	DurMS    int64  `json:"dur_ms,omitempty"`
	Finished int64  `json:"finished_at_unix"`
}
