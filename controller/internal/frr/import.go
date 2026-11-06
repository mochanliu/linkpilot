package frr

import (
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// resyncQuiet is how long the replay has to stay silent before it counts as
	// finished. zebra pushes the dump in batches over one connection, so a gap
	// this wide means there is nothing left queued.
	resyncQuiet = 500 * time.Millisecond
	// resyncDeadline bounds the window when the replay never goes quiet (a very
	// large RIB, or zebra busy). Past it we adopt what arrived and let the rest
	// stream in normally.
	resyncDeadline = 10 * time.Second
	// linkRecovery is the same window opened for a kernel link event (VPP
	// restart, lcp/vpp0 down). Measured on lab-cpe3 (2026-11-06): the customer
	// eBGP session came back 15s after the link event, while sr-frr's poll loop
	// would have published "0 prefixes" at second 3 -- and that publication is
	// what let sr-route start exporting the box's own loopback, which bgpd then
	// re-originated at weight 32768 and shadowed the real customer path (same
	// measurement chain as PlanFrom, vmlab-rebuild-plan.md §9.19).
	// 30s is two recovery intervals of margin; it is the price of not telling
	// the whole network "this site has nothing" while the session is still
	// re-establishing.
	linkRecovery = 30 * time.Second
)

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

	// The resync window. While it is open the answers are stale ∪ pending: the
	// last good set stays published and the replay fills pending alongside it.
	// Closing it adopts pending alone, which is the only moment a prefix can be
	// withdrawn because of a reconnect.
	stale   map[ImportKey]sourceSet
	pending map[ImportKey]sourceSet
	resync  bool
	seen    bool // at least one notify arrived on the new connection
	started time.Time
	last    time.Time
	// deadline is how long the open window may run; it depends on what opened
	// it (a ZAPI reconnect or a kernel link event, see linkRecovery).
	deadline time.Duration
	// reason names what opened the window, for the log line at close.
	reason string
	// settled records that at least one window has closed, i.e. the answers
	// below are backed by a replay we watched to completion. Until then the
	// exporter must not add anything (main.go freezes injection on this).
	settled bool

	// vpnOf maps a ZAPI header vrf_id (kernel VRF ifindex) to the VPN. Unknown
	// ids are dropped and counted, never folded into VPN 0.
	vpnOf   func(vrfID uint32) (uint8, bool)
	unknown map[uint32]struct{}
	badVRF  uint64

	Now  func() time.Time
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
		Now:     time.Now,
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
	if im.resync {
		set = im.pending
		im.seen = true
		im.last = im.Now()
	}
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

// BeginResync opens the window after a (re)connect. The old set is kept, not
// cleared: the replay on the new connection will rebuild the whole view, and
// publishing the empty set in the meantime tells sr-route -- and every remote
// site through the RR -- that this box has no customer prefixes. Measured on
// lab-cpe1: that publication is visible for ~4.5s after a zebra restart.
//
// The cost is bounded and lands on the safe side: a prefix the customer really
// dropped during the window stays visible until the window closes, at most
// resyncDeadline.
func (im *Importer) BeginResync() {
	im.beginWindow(resyncDeadline, "重连")
}

// BeginLinkWindow opens the same window for a kernel link event (VPP restart
// tears down lcp/vpp0, which takes the customer's eBGP session with it). The
// session is gone but the routes it carried are still what this box should
// claim -- measured recovery is 15s (2026-11-06, lab-cpe3), so publishing the
// empty set at second 3 both flaps the whole network and permanently shadows
// the customer path once sr-route starts exporting the prefix itself.
func (im *Importer) BeginLinkWindow() {
	im.beginWindow(linkRecovery, "链路事件")
}

func (im *Importer) beginWindow(deadline time.Duration, reason string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if !im.resync {
		im.stale = im.routes
	}
	// A second reconnect before the first window closed keeps the original
	// stale set: it is still the last thing the device really had.
	im.pending = map[ImportKey]sourceSet{}
	im.resync = true
	im.seen = false
	im.started = im.Now()
	im.last = im.started
	im.deadline = deadline
	im.reason = reason
}

// Settled reports whether a window has ever closed, i.e. whether the answers
// are backed by a replay we watched to completion. The export side refuses to
// add prefixes before the first one settles (main.go) -- without that, a boot
// where zebra answers last would inject the box's own prefixes first and
// shadow them for good.
func (im *Importer) Settled() bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.settled
}

// Tick closes the window once the replay has been quiet long enough, or the
// deadline is reached. The poll loop calls it before reading Report; nothing
// else needs to run on a timer.
func (im *Importer) Tick(now time.Time) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if !im.resync {
		return
	}
	expired := now.Sub(im.started) >= im.deadline
	if !expired {
		// No notify yet is not "the replay was empty": the dump may still be in
		// flight (a full table takes seconds), and closing on that guess would
		// withdraw every prefix. Only silence *after* the replay started counts.
		if !im.seen || now.Sub(im.last) < resyncQuiet {
			return
		}
	}
	kept, dropped := 0, 0
	for k := range im.stale {
		if _, ok := im.pending[k]; ok {
			kept++
			continue
		}
		dropped++
	}
	im.routes = im.pending
	im.stale, im.pending, im.resync = nil, nil, false
	im.settled = true
	if expired {
		if !im.seen {
			im.Logf("sr-frr: %s后 %s 没收到任何回放，按空集合收尾（撤掉旧前缀 %d 条）", im.reason, im.deadline, dropped)
			return
		}
		// Adopting a half-filled replay here can withdraw prefixes that still
		// exist. They come back as the replay keeps arriving, one poll later.
		im.Logf("sr-frr: %s对账到 %s 上限仍未回放完，按已收到的 %d 条收尾（%d 条旧前缀可能被误撤，回放继续到达时会加回）",
			im.reason, im.deadline, len(im.routes), dropped)
		return
	}
	im.Logf("sr-frr: %s对账完成：保留 %d 条，撤掉 %d 条", im.reason, kept, dropped)
}

// visibleLocked is what the customer side is currently believed to hold: the
// settled set plus, while a window is open, the last good set and the replay.
// Callers must hold im.mu.
func (im *Importer) visibleLocked() map[ImportKey]sourceSet {
	visible := map[ImportKey]sourceSet{}
	add := func(set map[ImportKey]sourceSet) {
		for k, src := range set {
			dst := visible[k]
			if dst == nil {
				dst = sourceSet{}
				visible[k] = dst
			}
			for t := range src {
				dst[t] = struct{}{}
			}
		}
	}
	add(im.routes)
	if im.resync {
		add(im.stale)
		add(im.pending)
	}
	return visible
}

// OwnedByCustomer groups the visible set by VPN, before the whitelist and the
// anti-loop filter. The export side gates injections on it (PlanFrom): a prefix
// the customer already routes must never get a K route of its own, because that
// K route would shadow the customer path and stop the very notifications that
// put it in this answer, and the deadlock is measured (2026-11-06, lab-cpe3).
//
// Its blind spot is what ZAPI never told us: a prefix learned from a source we
// did not subscribe to is invisible here. The frozen first round in main.go
// covers the boot race; the subscription set has to name every source the
// customer actually uses.
func (im *Importer) OwnedByCustomer() map[uint8]map[netip.Prefix]struct{} {
	im.mu.Lock()
	defer im.mu.Unlock()
	visible := im.visibleLocked()
	out := make(map[uint8]map[netip.Prefix]struct{}, len(visible))
	for k := range visible {
		set := out[k.VPN]
		if set == nil {
			set = map[netip.Prefix]struct{}{}
			out[k.VPN] = set
		}
		set[k.Prefix] = struct{}{}
	}
	return out
}

// Report applies the current rules to the visible set: selected source, the
// VPN's whitelist, and the anti-loop intersection with what this VPN exports.
// Everything is computed at read time, so removing a source or a prefix from a
// whitelist takes effect on the next poll without waiting for zebra.
func (im *Importer) Report(rules []ImportRule, exported map[uint8]map[netip.Prefix]struct{}) ([]ReportedPrefix, ReportStats) {
	im.mu.Lock()
	visible := im.visibleLocked()
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
