package frr

import (
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strings"
	"sync"
)

// 导入侧没有任何"等一拍再看"的窗口：答案永远是当前这份集合。链路事件、zebra
// 重连、白名单改动都直接反映到下一次上报，代价写在 Importer.Reset 上。
// 硬规则见 AGENTS.md 第 3 条（2026-11-07 定案：不许维持旧集合）。

// ImportRule is one (VPN) CPE-SDWAN row after the tenant's prefix-list has been
// expanded by the controller. Sources are route types (RouteConnected...); an
// empty Allow list matches nothing -- CPE-SDWAN always names a list, and fail
// closed is the only safe default for a whitelist.
type ImportRule struct {
	VPN     uint8
	Sources []uint8
	Allow   []AllowEntry
}

// AllowEntry is one permit line. FRR prefix-list semantics: with neither ge nor
// le the entry matches exactly that prefix length; ge/le widen it.
type AllowEntry struct {
	Prefix netip.Prefix
	GE     int
	LE     int
}

func (e AllowEntry) allows(p netip.Prefix) bool {
	if p.Bits() < e.Prefix.Bits() || !e.Prefix.Contains(p.Addr()) {
		return false
	}
	if e.GE == 0 && e.LE == 0 {
		return p.Bits() == e.Prefix.Bits()
	}
	if e.GE > 0 && p.Bits() < e.GE {
		return false
	}
	if e.LE > 0 && p.Bits() > e.LE {
		return false
	}
	return true
}

// Allows reports whether the row's whitelist permits p. No entries = nothing.
func (r ImportRule) Allows(p netip.Prefix) bool {
	for _, e := range r.Allow {
		if e.allows(p) {
			return true
		}
	}
	return false
}

// WantsSource reports whether this row selected the zebra route type.
func (r ImportRule) WantsSource(t uint8) bool {
	for _, s := range r.Sources {
		if s == t {
			return true
		}
	}
	return false
}

// SourceName is the tenant-facing name of a zebra route type.
func SourceName(t uint8) string {
	switch t {
	case RouteConnected:
		return "connected"
	case RouteStatic:
		return "static"
	case RouteOSPF:
		return "ospf"
	case RouteBGP:
		return "bgp"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

// ParseSource maps a rule's source name to the zebra type measured in S-0.
func ParseSource(name string) (uint8, bool) {
	switch name {
	case "connected":
		return RouteConnected, true
	case "static":
		return RouteStatic, true
	case "ospf":
		return RouteOSPF, true
	case "bgp":
		return RouteBGP, true
	default:
		return 0, false
	}
}

// ImportKey is one prefix in one VPN. The source set is what lets a withdraw of
// the BGP copy keep the connected copy (zebra notifies per route type).
type ImportKey struct {
	VPN    uint8
	Prefix netip.Prefix
}

func (k ImportKey) String() string {
	return fmt.Sprintf("vpn%d %s", k.VPN, k.Prefix)
}

type sourceSet map[uint8]struct{}

// ReportedPrefix is one line of PUT /local-prefixes.
type ReportedPrefix struct {
	VPN    uint8
	Prefix netip.Prefix
}

// ReportStats is what /status shows. LoopDropped is the size of the current
// intersection with this VPN's export set (S-0 proved K shadows BGP, but the
// intersection is written down rather than relied on).
type ReportStats struct {
	Tracked     int
	Reported    int
	LoopDropped int
	UnknownVRF  uint64
}

// Importer maintains "what the customer side told us", keyed by (VPN, prefix)
// with the set of route types that produced it.
type Importer struct {
	mu     sync.Mutex
	routes map[ImportKey]sourceSet

	// vpnOf maps a ZAPI header vrf_id (kernel VRF ifindex) to the VPN. Unknown
	// ids are dropped and counted, never folded into VPN 0.
	vpnOf   func(vrfID uint32) (uint8, bool)
	unknown map[uint32]struct{}
	badVRF  uint64

	Logf func(format string, args ...any)
}

// NewImporter takes the ifindex→VPN mapping measured in S-0. A nil mapper keeps
// only the default VRF (id 0 → VPN 0).
func NewImporter(vpnOf func(vrfID uint32) (uint8, bool)) *Importer {
	if vpnOf == nil {
		vpnOf = func(vrfID uint32) (uint8, bool) { return 0, vrfID == 0 }
	}
	return &Importer{
		routes:  map[ImportKey]sourceSet{},
		vpnOf:   vpnOf,
		unknown: map[uint32]struct{}{},
		Logf:    log.Printf,
	}
}

// Handle processes one zebra redistribute notification. The header's vrf_id is
// the route's VRF ifindex; the payload's type is its source.
func (im *Importer) Handle(ev RouteEvent) {
	if !ev.Prefix.IsValid() || !ev.Prefix.Addr().Is4() {
		return
	}
	vpn, ok := im.vpnOf(ev.VRFID)
	if !ok {
		im.mu.Lock()
		im.badVRF++
		first := false
		if _, seen := im.unknown[ev.VRFID]; !seen {
			im.unknown[ev.VRFID] = struct{}{}
			first = true
		}
		im.mu.Unlock()
		if first {
			im.Logf("sr-frr: 再分发通知里的 vrf_id %d 没有对应的 VPN，丢弃（不并到 VPN 0）", ev.VRFID)
		}
		return
	}
	k := ImportKey{VPN: vpn, Prefix: ev.Prefix.Masked()}

	im.mu.Lock()
	defer im.mu.Unlock()
	set := im.routes
	if ev.Add {
		src := set[k]
		if src == nil {
			src = sourceSet{}
			set[k] = src
		}
		src[ev.Type] = struct{}{}
		return
	}
	if src := set[k]; src != nil {
		delete(src, ev.Type)
		if len(src) == 0 {
			delete(set, k)
		}
	}
}

// Reset drops the whole set. The caller uses it on every new ZAPI connection,
// before subscribing: what the customer side has is what that connection says,
// so a reconnect rebuilds the view from the dump instead of carrying the old
// one forward.
//
// The consequence, stated because it is the price of the rule: between the
// reset and the replay landing, a poll publishes a shorter -- possibly empty
// -- list, and remote sites withdraw those prefixes until it arrives. Measured
// on lab-cpe1, the old window existed exactly to hide that ~4.5s; as of
// 2026-11-07 hiding it is not allowed, because a prefix kept alive on the
// strength of a dead session is the transient blackhole, not the dump.
func (im *Importer) Reset() {
	im.mu.Lock()
	defer im.mu.Unlock()
	if len(im.routes) > 0 {
		im.Logf("sr-frr: 重连，清空本地前缀集（%d 条），等新连接的回放重建", len(im.routes))
	}
	im.routes = map[ImportKey]sourceSet{}
}

// snapshotLocked is a copy of what the customer side currently holds. Report
// walks it after releasing the lock, so the copy has to be its own.
func (im *Importer) snapshotLocked() map[ImportKey]sourceSet {
	out := make(map[ImportKey]sourceSet, len(im.routes))
	for k, src := range im.routes {
		cp := make(sourceSet, len(src))
		for t := range src {
			cp[t] = struct{}{}
		}
		out[k] = cp
	}
	return out
}

// Report applies the current rules to the live set: selected source, the VPN's
// whitelist, and the anti-loop intersection with what this VPN exports.
// Everything is computed at read time, so removing a source or a prefix from a
// whitelist takes effect on the next poll without waiting for zebra.
func (im *Importer) Report(rules []ImportRule, exported map[uint8]map[netip.Prefix]struct{}) ([]ReportedPrefix, ReportStats) {
	im.mu.Lock()
	visible := im.snapshotLocked()
	stats := ReportStats{Tracked: len(visible), UnknownVRF: im.badVRF}
	im.mu.Unlock()

	byVPN := make(map[uint8]ImportRule, len(rules))
	for _, r := range rules {
		byVPN[r.VPN] = r
	}
	out := make([]ReportedPrefix, 0, len(visible))
	for k, src := range visible {
		rule, ok := byVPN[k.VPN]
		if !ok {
			continue
		}
		wanted := false
		for t := range src {
			if rule.WantsSource(t) {
				wanted = true
				break
			}
		}
		if !wanted || !rule.Allows(k.Prefix) {
			continue
		}
		if set := exported[k.VPN]; set != nil {
			if _, loop := set[k.Prefix]; loop {
				stats.LoopDropped++
				continue
			}
		}
		out = append(out, ReportedPrefix{VPN: k.VPN, Prefix: k.Prefix})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VPN != out[j].VPN {
			return out[i].VPN < out[j].VPN
		}
		return out[i].Prefix.String() < out[j].Prefix.String()
	})
	stats.Reported = len(out)
	return out, stats
}

// Signature is a stable form of a reported set, used to avoid pointless PUTs.
func Signature(ps []ReportedPrefix) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(fmt.Sprintf("%d|%s\n", p.VPN, p.Prefix))
	}
	return b.String()
}
