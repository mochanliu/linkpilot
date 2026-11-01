package frr

import (
	"log"
	"net/netip"
	"sort"
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
)

// Importer 维护"从客户侧学到"的前缀集合：BGP 再分发通知里的 ADD 收下，
// DEL 撤掉。集合语义是"每个前缀的最新选中路由"。
type Importer struct {
	mu     sync.Mutex
	routes map[netip.Prefix]struct{}

	// The resync window. While it is open the answers are stale ∪ pending: the
	// last good set stays published and the replay fills pending alongside it.
	// Closing it adopts pending alone, which is the only moment a prefix can be
	// withdrawn because of a reconnect.
	stale   map[netip.Prefix]struct{}
	pending map[netip.Prefix]struct{}
	resync  bool
	seen    bool // at least one notify arrived on the new connection
	started time.Time
	last    time.Time

	Now  func() time.Time
	Logf func(format string, args ...any)
}

func NewImporter() *Importer {
	return &Importer{
		routes: map[netip.Prefix]struct{}{},
		Now:    time.Now,
		Logf:   log.Printf,
	}
}

// Handle 处理一条 zebra 再分发通知。
func (im *Importer) Handle(ev RouteEvent) {
	if !ev.Prefix.IsValid() || !ev.Prefix.Addr().Is4() {
		return
	}
	p := ev.Prefix.Masked()
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.resync {
		im.apply(im.pending, p, ev)
		im.seen = true
		im.last = im.Now()
		return
	}
	im.apply(im.routes, p, ev)
}

func (im *Importer) apply(set map[netip.Prefix]struct{}, p netip.Prefix, ev RouteEvent) {
	if ev.Add {
		set[p] = struct{}{}
		return
	}
	delete(set, p)
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
	im.mu.Lock()
	defer im.mu.Unlock()
	if !im.resync {
		im.stale = im.routes
	}
	// A second reconnect before the first window closed keeps the original
	// stale set: it is still the last thing the device really had.
	im.pending = map[netip.Prefix]struct{}{}
	im.resync = true
	im.seen = false
	im.started = im.Now()
	im.last = im.started
}

// Tick closes the window once the replay has been quiet long enough, or the
// deadline is reached. The poll loop calls it before reading Prefixes; nothing
// else needs to run on a timer.
func (im *Importer) Tick(now time.Time) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if !im.resync {
		return
	}
	expired := now.Sub(im.started) >= resyncDeadline
	if !expired {
		// No notify yet is not "the replay was empty": the dump may still be in
		// flight (a full table takes seconds), and closing on that guess would
		// withdraw every prefix. Only silence *after* the replay started counts.
		if !im.seen || now.Sub(im.last) < resyncQuiet {
			return
		}
	}
	kept, dropped := 0, 0
	for p := range im.stale {
		if _, ok := im.pending[p]; ok {
			kept++
			continue
		}
		dropped++
	}
	im.routes = im.pending
	im.stale, im.pending, im.resync = nil, nil, false
	if expired {
		if !im.seen {
			im.Logf("sr-frr: 重连后 %s 没收到任何回放，按空集合收尾（撤掉旧前缀 %d 条）", resyncDeadline, dropped)
			return
		}
		// Adopting a half-filled replay here can withdraw prefixes that still
		// exist. They come back as the replay keeps arriving, one poll later.
		im.Logf("sr-frr: 重连对账到 %s 上限仍未回放完，按已收到的 %d 条收尾（%d 条旧前缀可能被误撤，回放继续到达时会加回）",
			resyncDeadline, len(im.routes), dropped)
		return
	}
	im.Logf("sr-frr: 重连对账完成：保留 %d 条，撤掉 %d 条", kept, dropped)
}

// Prefixes 返回排好序的前缀快照。
func (im *Importer) Prefixes() []netip.Prefix {
	im.mu.Lock()
	set := im.routes
	if im.resync {
		set = map[netip.Prefix]struct{}{}
		for p := range im.stale {
			set[p] = struct{}{}
		}
		for p := range im.pending {
			set[p] = struct{}{}
		}
	}
	out := make([]netip.Prefix, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	im.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Len 返回当前对外可见的集合大小（日志用）。
func (im *Importer) Len() int {
	return len(im.Prefixes())
}
