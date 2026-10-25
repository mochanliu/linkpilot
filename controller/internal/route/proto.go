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
//   - 路由身份是 (发起设备, VRF, prefix)，一条路由带一组 next_hop。一台 CPE
//     每条 VPN 隧道各有一个本端 overlay 地址，所以同一个内侧前缀的所有路径
//     放在一条路由里宣告；RR 原样反射，不替任何一端改写下一跳。哪些路径最终
//     可被数据面使用，是数据面选路阶段的事。
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
	// withdrawals from a path key to a prefix key. There is no v1 compatibility
	// mode: both ends are ours and a mixed pair would be talking about different
	// tables while looking healthy.
	ProtocolVersion = 2

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
	// MaxNextHopsPerRoute bounds the next-hop set on one route. One CPE has one
	// overlay address per tunnel, so this is really a cap on tunnels per device.
	MaxNextHopsPerRoute = 16
)

// MsgType is the envelope discriminator. Unknown types are protocol errors,
// not ignored: silently dropping a message we do not understand is how two
// implementations end up disagreeing about the table without anyone noticing.
type MsgType string

const (
	MsgHello     MsgType = "hello"     // CPE -> RR: version + claimed device id
	MsgHelloOK   MsgType = "hello_ok"  // RR -> CPE: version, rr id, hold time
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

	// announce / withdraw
	Routes []Route    `json:"routes,omitempty"`
	Keys   []RouteKey `json:"keys,omitempty"`

	// error
	Message string `json:"message,omitempty"`
}

// Route is one prefix advertised by its origin device, carrying every path that
// origin has for it.
//
// DeviceID is the origin, and it is NOT allowed to differ from the session's
// certificate identity -- a CPE can only ever speak for itself. The RR checks
// that on every announce/withdraw; without it one node could poison another's
// routes by simply claiming its id in JSON.
//
// NextHops is the origin's local overlay address on each of its tunnels: one
// route covers all of them, and the set is the whole opinion. An announce
// replaces the set; a withdraw removes the prefix entirely.
type Route struct {
	DeviceID uint16   `json:"device_id"`
	VRFID    uint8    `json:"vrf_id"`
	Prefix   string   `json:"prefix"`    // canonical CIDR, IPv4
	NextHops []string `json:"next_hops"` // canonical IPv4, strictly ascending
	RTs      []string `json:"rts"`       // export RTs of the origin VRF, canonical "ASN:value"
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

// Equal compares everything, including the next-hop set and RTs. The RR uses it
// to decide whether an announce that lands on an existing key is a no-op or a
// replacement.
func (r Route) Equal(o Route) bool {
	if r.Key() != o.Key() || len(r.RTs) != len(o.RTs) || len(r.NextHops) != len(o.NextHops) {
		return false
	}
	for i := range r.RTs {
		if r.RTs[i] != o.RTs[i] {
			return false
		}
	}
	for i := range r.NextHops {
		if r.NextHops[i] != o.NextHops[i] {
			return false
		}
	}
	return true
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
	if len(r.NextHops) == 0 {
		return fmt.Errorf("route %s: at least one next_hop is required", r.Prefix)
	}
	if len(r.NextHops) > MaxNextHopsPerRoute {
		return fmt.Errorf("route %s: %d next_hops exceeds %d", r.Prefix, len(r.NextHops), MaxNextHopsPerRoute)
	}
	for i, nh := range r.NextHops {
		if err := validateNextHop(nh); err != nil {
			return fmt.Errorf("route %s: %w", r.Prefix, err)
		}
		if i > 0 && r.NextHops[i-1] >= nh {
			return fmt.Errorf("route %s: next_hops must be strictly ascending and unique (got %q after %q)",
				r.Prefix, nh, r.NextHops[i-1])
		}
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
	case MsgHello, MsgHelloOK, MsgAnnounce, MsgWithdraw, MsgEOR, MsgKeepalive, MsgError:
	default:
		return Envelope{}, fmt.Errorf("route: unknown message type %q", env.Type)
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
