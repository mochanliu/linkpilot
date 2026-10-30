// Package route implements the control-plane route exchange between CPEs and
// the controller-side route reflectors (RRs).
//
// 协议形似 BGP，但只保留本系统需要的那部分：CPE 与两个 RR 建邻居，把自己 VRF
// 内的内侧路由发给 RR；RR 按 VPN 隧道拓扑反射给对端 CPE；CPE 用 VRF 的
// import RT 决定是否收进 "bgp" 路由表。
//
// 与 BGP 的差异都是刻意的：
//
//   - 一条 TCP/TLS 连接就是一个邻居，v1 只承载 IPv4 单播前缀，不做 capability
//     协商。版本在 hello 里显式交换，对不上就断，不存在"猜对方支持什么"。
//
//   - 路由身份是 (发起设备, VRF, prefix)，**不带下一跳**。下一跳只对接收方
//     有意义：同一个前缀发给一千个对端，每个对端该用的下一跳都不一样。所以
//     隧道邻接单独宣告（Tunnel），RR 只决定"发给谁"，收端用自己的隧道表把
//     "这条路由来自设备 N" 解析成本地的一条或多条路径。一台 hub 有一千条隧道
//     时，它的前缀里不会因此背上一千个地址。
//
//   - 路由器只交换**声明式快照**：会话建立时双方各发一份全量，之后 announce/
//     withdraw 增量。没有 BGP 的 best-path 计算，也没有 MED/LP；RR 存的是
//     adj-rib-in，CPE 存的是按 import RT 过滤后的 bgp 表。
//
//   - 所有标识必须**规范形式**（netip 的 String()、RT 的 "ASN:value" 无前导
//     零），接收侧因此可以用精确字符串比较，不会出现 "10.0.0.0/24" 与
//     "10.0.0.0/24 " 这种看起来一样、算起来不同的两份路由。
package route

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// ProtocolVersion is exchanged in hello. A mismatch is fatal for the
	// session: half-understanding an update is worse than not having a session.
	//
	// v2 changed a route from one path to a prefix with a set of next hops, and
	// withdrawals from a path key to a prefix key. v3 moves the next hops off the
	// route entirely: a device announces its tunnel table once, and every receiver
	// resolves the path locally. There is no compatibility mode: both ends are
	// ours, and a mixed pair would be talking about different tables while
	// looking healthy.
	ProtocolVersion = 3

	// DefaultHoldSec is how long either side waits for any message before
	// declaring the session dead. Keepalive goes out at a third of it.
	DefaultHoldSec = 30

	// MaxLineBytes bounds one JSONL frame. A route table is small today, but the
	// reader must not be the place where a malformed frame turns into OOM.
	MaxLineBytes = 4 << 20
	// MaxRoutesPerMessage bounds one announce/withdraw frame. Beyond this an
	// update is applied in chunks by the sender, not by growing the frame.
	MaxRoutesPerMessage = 8192
	// MaxRTsPerRoute bounds the RT list on one route.
	MaxRTsPerRoute = 16
	// MaxTunnelsPerDevice bounds one device's tunnel table. A hub with a thousand
	// spokes is the scale this split exists for, so the cap is per device, not per
	// route; the frame limit still applies on top of it.
	MaxTunnelsPerDevice = 4096
	// MaxVRFID is the largest VRF this protocol can name: the field is uint8 in
	// Route.VRFID, RouteKey.VRFID and VRFConfig.ID. A VPN id above it cannot be
	// expressed at all, so a controller that configures 导通 must refuse it rather
	// than render a route.json the device rejects. internal/fib carries the same
	// bound for its table-number formula.
	MaxVRFID uint8 = 255
)

// MsgType is the envelope discriminator. Unknown types are protocol errors,
// not ignored: silently dropping a message we do not understand is how two
// implementations end up disagreeing about the table without anyone noticing.
type MsgType string

const (
	MsgHello     MsgType = "hello"     // CPE -> RR: version + claimed device id
	MsgHelloOK   MsgType = "hello_ok"  // RR -> CPE: version, rr id, hold time
	MsgTunnels   MsgType = "tunnels"   // CPE -> RR: this device's whole tunnel table
	MsgAnnounce  MsgType = "announce"  // either direction: add/replace routes
	MsgWithdraw  MsgType = "withdraw"  // either direction: remove exact keys
	MsgEOR       MsgType = "eor"       // end of the sender's initial snapshot
	MsgKeepalive MsgType = "keepalive" // liveness only
	MsgError     MsgType = "error"     // sent before the sender closes
)

// Envelope is the single frame shape on the wire. One struct rather than a
// union keeps the codec small; the Type decides which fields are meaningful,
// and every handler validates that the fields it reads are present.
type Envelope struct {
	Type MsgType `json:"type"`

	// hello / hello_ok
	Version  int    `json:"version,omitempty"`
	DeviceID uint16 `json:"device_id,omitempty"` // hello: claimed identity
	RRID     string `json:"rr_id,omitempty"`     // hello_ok
	HoldSec  int    `json:"hold_sec,omitempty"`  // hello_ok

	// tunnels
	//
	// A pointer so an explicitly empty table ({"tunnels":[]}) is distinguishable
	// from a frame that forgot the field. "I have no tunnels" is a valid answer
	// and must still arrive: the RR does not send a route snapshot to a peer
	// before it knows that peer's table, otherwise the peer would get an empty
	// EOR'd table and then all of it again as updates.
	Tunnels *[]Tunnel `json:"tunnels,omitempty"`

	// announce / withdraw
	Routes []Route    `json:"routes,omitempty"`
	Keys   []RouteKey `json:"keys,omitempty"`

	// error
	Message string `json:"message,omitempty"`
}

// Route is one prefix advertised by its origin device. It carries no next hop:
// the path to the origin belongs to the receiver's tunnel table, not to the
// prefix, and the same announce is reflected unchanged to every adjacent peer.
//
// DeviceID is the origin, and it is NOT allowed to differ from the session's
// certificate identity -- a CPE can only ever speak for itself. The RR checks
// that on every announce/withdraw; without it one node could poison another's
// routes by simply claiming its id in JSON.
type Route struct {
	DeviceID uint16   `json:"device_id"`
	VRFID    uint8    `json:"vrf_id"`
	Prefix   string   `json:"prefix"` // canonical CIDR, IPv4
	RTs      []string `json:"rts"`    // export RTs of the origin VRF, canonical "ASN:value"
}

// Tunnel is one VPN tunnel as its owning device sees it: the peer it reaches and
// the remote overlay address it sends to. The local interface name stays on the
// device -- the RR has no use for it and no business knowing it.
//
// The table replaces the previous one atomically, and it is the device's whole
// adjacency opinion. Two devices are adjacent only when both announce each
// other: a one-way entry has no return path, so the RR never reflects a route
// over it.
type Tunnel struct {
	Peer    uint16 `json:"peer_device_id"`
	NextHop string `json:"next_hop"` // canonical IPv4
}

// RouteKey identifies one route: an origin's opinion about one prefix.
// Withdrawals and the RR's adj-rib-in are keyed by it, so a next-hop-set or
// RT-only change replaces the route instead of creating a second one.
type RouteKey struct {
	DeviceID uint16 `json:"device_id"`
	VRFID    uint8  `json:"vrf_id"`
	Prefix   string `json:"prefix"`
}

func (r Route) Key() RouteKey {
	return RouteKey{DeviceID: r.DeviceID, VRFID: r.VRFID, Prefix: r.Prefix}
}

// Equal compares everything the origin asserts about a prefix. The RR uses it to
// decide whether an announce that lands on an existing key is a no-op or a
// replacement.
func (r Route) Equal(o Route) bool {
	if r.Key() != o.Key() || len(r.RTs) != len(o.RTs) {
		return false
	}
	for i := range r.RTs {
		if r.RTs[i] != o.RTs[i] {
			return false
		}
	}
	return true
}

// ValidateTunnels enforces the canonical table contract: non-zero peers,
// canonical next hops, at most MaxTunnelsPerDevice entries, and strict order by
// (peer, next hop) so two tables that describe the same tunnels compare equal
// without any downstream sorting.
func ValidateTunnels(ts []Tunnel) error {
	if len(ts) > MaxTunnelsPerDevice {
		return fmt.Errorf("route: %d tunnels exceeds %d", len(ts), MaxTunnelsPerDevice)
	}
	for i, t := range ts {
		if t.Peer == 0 {
			return fmt.Errorf("tunnel #%d: peer_device_id must be non-zero", i)
		}
		if err := validateNextHop(t.NextHop); err != nil {
			return fmt.Errorf("tunnel to device %d: %w", t.Peer, err)
		}
		if i > 0 {
			prev := ts[i-1]
			if prev.Peer > t.Peer || (prev.Peer == t.Peer && prev.NextHop >= t.NextHop) {
				return fmt.Errorf("tunnels must be strictly ascending by (peer_device_id, next_hop): %d/%s follows %d/%s",
					t.Peer, t.NextHop, prev.Peer, prev.NextHop)
			}
		}
	}
	return nil
}

// SortedTunnels returns a deterministic copy in the wire's canonical order.
// Several tunnels may lead to the same peer (parallel paths); they differ by
// next hop and all of them are usable at once.
func SortedTunnels(in []Tunnel) []Tunnel {
	out := append([]Tunnel(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Peer != out[j].Peer {
			return out[i].Peer < out[j].Peer
		}
		return out[i].NextHop < out[j].NextHop
	})
	return out
}

// Validate enforces the wire's canonical-form contract. Passing a route from
// the network through this is what makes exact string comparisons safe
// everywhere downstream (RT import, keys, dedupe).
func (r Route) Validate() error {
	if r.DeviceID == 0 {
		return errors.New("route: device_id must be non-zero")
	}
	if err := validatePrefix(r.Prefix); err != nil {
		return fmt.Errorf("route %s: %w", r.Prefix, err)
	}
	if len(r.RTs) == 0 {
		return fmt.Errorf("route %s: at least one RT is required (it can never be imported otherwise)", r.Prefix)
	}
	if len(r.RTs) > MaxRTsPerRoute {
		return fmt.Errorf("route %s: %d RTs exceeds %d", r.Prefix, len(r.RTs), MaxRTsPerRoute)
	}
	seen := make(map[string]bool, len(r.RTs))
	for _, rt := range r.RTs {
		canon, err := CanonicalRT(rt)
		if err != nil {
			return fmt.Errorf("route %s: %w", r.Prefix, err)
		}
		if canon != rt {
			return fmt.Errorf("route %s: RT %q is not canonical (want %q)", r.Prefix, rt, canon)
		}
		if seen[rt] {
			return fmt.Errorf("route %s: duplicate RT %q", r.Prefix, rt)
		}
		seen[rt] = true
	}
	return nil
}

// Validate checks a withdrawal key. No RTs are involved: removing a prefix is
// identified by where it came from, not by how it was exported.
func (k RouteKey) Validate() error {
	if k.DeviceID == 0 {
		return errors.New("route key: device_id must be non-zero")
	}
	if err := validatePrefix(k.Prefix); err != nil {
		return fmt.Errorf("route key %s: %w", k.Prefix, err)
	}
	return nil
}

func validatePrefix(s string) error {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return fmt.Errorf("bad prefix %q: %w", s, err)
	}
	if !p.Addr().Is4() {
		return fmt.Errorf("prefix %q is not IPv4 (v1 carries IPv4 unicast only)", s)
	}
	if p.Masked().String() != s {
		return fmt.Errorf("prefix %q is not canonical (want %q)", s, p.Masked())
	}
	return nil
}

func validateNextHop(s string) error {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return fmt.Errorf("bad next_hop %q: %w", s, err)
	}
	if !a.Is4() {
		return fmt.Errorf("next_hop %q is not IPv4", s)
	}
	if a.IsUnspecified() || a.IsMulticast() {
		return fmt.Errorf("next_hop %q is not a unicast address", s)
	}
	if a.String() != s {
		return fmt.Errorf("next_hop %q is not canonical (want %q)", s, a.String())
	}
	return nil
}

// CanonicalRT parses an "ASN:value" route target and returns its canonical
// spelling. Config files may use leading zeros; the wire may not.
func CanonicalRT(s string) (string, error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", fmt.Errorf("bad RT %q: want ASN:value", s)
	}
	if strings.IndexByte(s[i+1:], ':') >= 0 {
		return "", fmt.Errorf("bad RT %q: want exactly one ':'", s)
	}
	asn, err := strconv.ParseUint(s[:i], 10, 32)
	if err != nil {
		return "", fmt.Errorf("bad RT %q: ASN is not a 32-bit unsigned integer", s)
	}
	val, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil {
		return "", fmt.Errorf("bad RT %q: value is not a 32-bit unsigned integer", s)
	}
	return fmt.Sprintf("%d:%d", asn, val), nil
}

// WriteEnvelope emits one JSONL frame. Callers serialize writes per connection.
func WriteEnvelope(w io.Writer, env Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("route: encode %s: %w", env.Type, err)
	}
	if len(b) > MaxLineBytes {
		return fmt.Errorf("route: %s frame is %d bytes, limit %d", env.Type, len(b), MaxLineBytes)
	}
	b = append(b, '\n')
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// ReadEnvelope reads one JSONL frame, enforcing the size limit before the
// payload can grow past it.
func ReadEnvelope(br *bufio.Reader) (Envelope, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxLineBytes {
			return Envelope{}, fmt.Errorf("route: frame exceeds %d bytes", MaxLineBytes)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Envelope{}, fmt.Errorf("route: decode frame: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, errors.New("route: frame has no type")
	}
	switch env.Type {
	case MsgHello, MsgHelloOK, MsgTunnels, MsgAnnounce, MsgWithdraw, MsgEOR, MsgKeepalive, MsgError:
	default:
		return Envelope{}, fmt.Errorf("route: unknown message type %q", env.Type)
	}
	if env.Tunnels != nil && len(*env.Tunnels) > MaxTunnelsPerDevice {
		return Envelope{}, fmt.Errorf("route: %d tunnels exceeds %d per device", len(*env.Tunnels), MaxTunnelsPerDevice)
	}
	if len(env.Routes) > MaxRoutesPerMessage {
		return Envelope{}, fmt.Errorf("route: %d routes exceeds %d per message", len(env.Routes), MaxRoutesPerMessage)
	}
	if len(env.Keys) > MaxRoutesPerMessage {
		return Envelope{}, fmt.Errorf("route: %d keys exceeds %d per message", len(env.Keys), MaxRoutesPerMessage)
	}
	return env, nil
}

// SortedRoutes returns a deterministic copy, so snapshots and test assertions
// do not depend on map iteration order.
func SortedRoutes(in []Route) []Route {
	out := append([]Route(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		if out[i].VRFID != out[j].VRFID {
			return out[i].VRFID < out[j].VRFID
		}
		if out[i].Prefix != out[j].Prefix {
			return out[i].Prefix < out[j].Prefix
		}
		// The key is the whole identity: equal keys are the same route.
		return false
	})
	return out
}

func SortedKeys(in []RouteKey) []RouteKey {
	out := append([]RouteKey(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		if out[i].VRFID != out[j].VRFID {
			return out[i].VRFID < out[j].VRFID
		}
		if out[i].Prefix != out[j].Prefix {
			return out[i].Prefix < out[j].Prefix
		}
		// The key is the whole identity: equal keys are the same route.
		return false
	})
	return out
}

// HoldDuration is the shared interpretation of HoldSec. Zero means default;
// values below three seconds are refused because keepalive is hold/3 and a
// session that sends sub-second keepalives is a busy loop, not a protocol.
func HoldDuration(sec int) (time.Duration, error) {
	if sec == 0 {
		sec = DefaultHoldSec
	}
	if sec < 3 || sec > 3600 {
		return 0, fmt.Errorf("route: hold_sec must be 3..3600 (got %d)", sec)
	}
	return time.Duration(sec) * time.Second, nil
}
