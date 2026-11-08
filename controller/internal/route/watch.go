package route

// Watch is the event-push counterpart of AllRIB. It exists because the
// alternative -- sr-fib polling /status every poll_sec -- adds that whole
// interval on top of every real change; the owner's phrasing for #276 was
// "as instantaneous as FRR's redistribute". FRR gets that from a single
// long-lived ZAPI socket, so we mirror the shape that matters:
//
//   - Per-event delivery: one entry per affected (target table, source key)
//     pair (redistribute.c::redistribute_update / _delete).
//   - 10 ms coalescing (zebra_router.h::ZEBRA_RIB_PROCESS_HOLD_TIME): a
//     burst of thousands of per-announce calls becomes a single write.
//   - Diff on top of a "last sent view" -- not on top of the raw
//     ribUpdate -- because two of the three gates that make a prefix
//     visible (tunnel next-hop list, imported-RTs table) are *read-time*
//     state and mutate without going through loc.process. A peer's
//     next-hop set can grow while the path stays resolvable: nothing in
//     the Loc-RIB changes, but what the FIB should install does.
//   - Drop-on-backpressure: if the reader can't keep up, the hub closes
//     the subscription and the reader re-attaches for a fresh INIT. This
//     is zebra's "write queue overflow closes the client socket" policy
//     and it keeps us on the right side of AGENTS.md §1.3 -- no
//     optimistically-held stale view while we wait for the reader to
//     catch up.
//
// One subscriber at a time. The consumer today is sr-fib on the same box,
// and adding a second one means either duplicating `prev` per subscriber or
// pretending we can share it while one is behind -- neither is worth the
// complexity while there is only one reader.

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// coalesceWindow is the number of milliseconds a batch of edits waits for
// additional ones before flushing. FRR's ZEBRA_RIB_PROCESS_HOLD_TIME is
// 10; matching it keeps the shape we're claiming to copy honest.
const coalesceWindow = 10 * time.Millisecond

// watchBufferDepth is the reader-side buffer, in batches (not events). A
// single batch can already hold the whole view (worst case: thousands of
// routes on an INIT), so one slot in flight plus one waiting is plenty;
// beyond that we would rather drop the reader than hide a growing state
// gap behind a queue.
const watchBufferDepth = 1

// OpUpsert / OpWithdraw are the two operations a RouteEvent can carry.
const (
	OpUpsert   = "upsert"
	OpWithdraw = "withdraw"
)

// RouteEvent is one observable change to a (target table, source route)
// pair. Same-key upserts with an identical NextHops list are dropped by
// the diff; a key that stops appearing goes out as a withdraw, and the
// reader does not need to remember the old next hops to reconcile.
type RouteEvent struct {
	Op       string   `json:"op"`
	Table    uint8    `json:"table"`
	DeviceID uint16   `json:"device_id"`
	VRFID    uint8    `json:"vrf_id"`
	Prefix   string   `json:"prefix"`
	NextHops []string `json:"next_hops,omitempty"`
}

// WatchBatch is one flush. Init marks the whole-view snapshot a new
// subscriber receives on attach; the wire format for the two is the same
// so the reader parses one type, but the semantics differ: Init replaces
// the reader's state, a later batch edits it.
type WatchBatch struct {
	Init   bool         `json:"init"`
	Events []RouteEvent `json:"events"`
}

// viewKey is one observable entry. Not a RouteKey -- a single source route
// fans out to every local table that imports its RTs, and those are the
// units sr-fib reconciles against.
type viewKey struct {
	Table    uint8
	DeviceID uint16
	VRFID    uint8
	Prefix   string
}

type viewEntry struct {
	NextHops []string
}

type view map[viewKey]viewEntry

// Watch is one active subscription. The hub owns the channel; readers
// consume Events and call Close on their way out. A closed Events channel
// means the hub either dropped this subscriber (backpressure) or the
// client went away -- either way, close the response and let the client
// reconnect to get a fresh INIT.
type Watch struct {
	Events chan WatchBatch

	hub  *WatchHub
	once sync.Once
}

// Close releases the slot. Idempotent, and safe to call after the hub already
// dropped this subscription -- shut() is what both paths go through.
func (w *Watch) Close() { w.hub.release(w) }

// shut ends the reader's side exactly once. Both the reader (Close) and the
// hub (drop on backpressure, CloseHub on shutdown) can be the one that gets
// here first, and a second close of the channel would panic.
func (w *Watch) shut() { w.once.Do(func() { close(w.Events) }) }

// WatchHub is one hub per Client; it is created lazily because a Client
// that never serves /status (i.e. the unit tests) shouldn't pay for one.
type WatchHub struct {
	client *Client

	mu     sync.Mutex
	sub    *Watch
	dirty  bool
	timer  *time.Timer
	prev   view
	closed bool
}

var errWatchBusy = errors.New("route watch: another subscriber is active")

// WatchSubscribe hands the caller the single allowed subscription and
// schedules an immediate INIT. Fails if a subscriber is already present.
func (c *Client) WatchSubscribe() (*Watch, error) {
	c.mu.Lock()
	h := c.watch
	if h == nil {
		h = &WatchHub{client: c}
		c.watch = h
	}
	c.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("route watch: hub closed")
	}
	if h.sub != nil {
		return nil, errWatchBusy
	}
	w := &Watch{
		Events: make(chan WatchBatch, watchBufferDepth),
		hub:    h,
	}
	h.sub = w
	// prev==nil is what makes the next flush an INIT, and dirty is what makes
	// that flush happen at all: a subscriber that arrives while the Loc-RIB is
	// quiet still has to be handed the table it is going to diff against.
	h.prev = nil
	h.dirty = true
	h.scheduleLocked()
	return w, nil
}

// CloseHub refuses further subscriptions. Called from Client.Run's
// shutdown path so an attached reader gets a clean close instead of
// hanging on a channel nobody writes to again.
func (h *WatchHub) CloseHub() {
	h.mu.Lock()
	h.closed = true
	sub := h.sub
	h.sub = nil
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.prev = nil
	h.mu.Unlock()
	if sub != nil {
		sub.shut()
	}
}

// release is the reader-initiated counterpart of CloseHub.
func (h *WatchHub) release(w *Watch) {
	h.mu.Lock()
	if h.sub == w {
		h.sub = nil
		h.prev = nil
	}
	h.mu.Unlock()
	w.shut()
}

// scheduleLocked arms the flush timer if nothing is pending. Callers hold
// h.mu.
func (h *WatchHub) scheduleLocked() {
	if h.timer != nil || h.closed {
		return
	}
	h.timer = time.AfterFunc(coalesceWindow, h.fire)
}

// notifyWatchLocked is how the rest of the client tells the hub "something
// moved". Every Loc-RIB mutation site -- and every site that changes one of
// the read-time gates the view is rendered through -- calls it while holding
// c.mu. It is a no-op when nobody has ever subscribed, so a Client built by
// a test pays nothing for this.
func (c *Client) notifyWatchLocked() {
	c.watch.markDirty()
}

// markDirty is called by every mutation site while it holds c.mu. Taking
// h.mu inside c.mu is the one lock order this file permits; fire() never
// takes c.mu while holding h.mu, so no cycle exists.
func (h *WatchHub) markDirty() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.dirty = true
	h.scheduleLocked()
	h.mu.Unlock()
}

// fire runs on the timer goroutine. It snapshots the view under c.mu, then
// diffs and delivers under h.mu.
//
// The send is inside h.mu on purpose even though it is a non-blocking one:
// release() and CloseHub() clear the slot and then close the channel, and a
// close that lands between "pick the subscriber" and "send to it" would make
// this goroutine panic on a reader that merely hung up. Under h.mu, either the
// reader is still the current subscriber -- then its channel is still open --
// or it is not, and there is nothing to send to.
func (h *WatchHub) fire() {
	h.mu.Lock()
	h.timer = nil
	if !h.dirty || h.sub == nil {
		h.mu.Unlock()
		return
	}
	h.dirty = false
	h.mu.Unlock()

	h.client.mu.Lock()
	now := h.client.viewLocked()
	h.client.mu.Unlock()

	h.mu.Lock()
	sub := h.sub
	if sub == nil {
		h.mu.Unlock()
		return
	}
	init := h.prev == nil
	events := diffView(h.prev, now)
	h.prev = now
	if !init && len(events) == 0 {
		// A quiet flush is not an event: an internal edit that never reached a
		// visible surface must not look like a change to the reader.
		h.mu.Unlock()
		h.rearmIfDirty()
		return
	}
	select {
	case sub.Events <- WatchBatch{Init: init, Events: events}:
		h.mu.Unlock()
		h.rearmIfDirty()
	default:
		// Backpressure: this reader is not keeping up. Close it; it will
		// reconnect and receive a fresh INIT. Same policy as zebra's
		// "close the client on write-queue overflow", and the only one
		// that doesn't hide a state gap behind a growing queue.
		h.dropLocked(sub)
		h.mu.Unlock()
		sub.shut()
	}
}

func (h *WatchHub) rearmIfDirty() {
	h.mu.Lock()
	if h.dirty && h.sub != nil {
		h.scheduleLocked()
	}
	h.mu.Unlock()
}

// dropLocked frees the subscriber's slot. Callers hold h.mu and must shut the
// watch after letting the lock go.
func (h *WatchHub) dropLocked(w *Watch) {
	if h.sub == w {
		h.sub = nil
		h.prev = nil
	}
}

// viewLocked renders the current observable view -- the same set of
// (table, source-route) rows AllRIB exposes, keyed the way the reader
// needs them. Must be called with c.mu held.
func (c *Client) viewLocked() view {
	out := make(view, c.loc.len())
	c.loc.eachBest(func(k RouteKey, p *path) {
		nh := c.nextHops[p.route.DeviceID]
		for _, t := range c.importTargets(p.route.RTs) {
			out[viewKey{Table: t, DeviceID: k.DeviceID, VRFID: k.VRFID, Prefix: k.Prefix}] =
				viewEntry{NextHops: append([]string(nil), nh...)}
		}
	})
	return out
}

// diffView returns the changes needed to move prev to now. prev==nil
// means "no previous view" and produces an all-upsert set -- that's the
// INIT payload. Empty result means "nothing observable changed", which is
// the common case when an internal edit didn't reach a visible surface.
func diffView(prev, now view) []RouteEvent {
	events := make([]RouteEvent, 0, len(now))
	for k, e := range now {
		old, existed := prev[k]
		if existed && equalStrings(old.NextHops, e.NextHops) {
			continue
		}
		events = append(events, RouteEvent{
			Op:       OpUpsert,
			Table:    k.Table,
			DeviceID: k.DeviceID,
			VRFID:    k.VRFID,
			Prefix:   k.Prefix,
			NextHops: e.NextHops,
		})
	}
	for k := range prev {
		if _, exists := now[k]; exists {
			continue
		}
		events = append(events, RouteEvent{
			Op:       OpWithdraw,
			Table:    k.Table,
			DeviceID: k.DeviceID,
			VRFID:    k.VRFID,
			Prefix:   k.Prefix,
		})
	}
	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.DeviceID != b.DeviceID {
			return a.DeviceID < b.DeviceID
		}
		if a.VRFID != b.VRFID {
			return a.VRFID < b.VRFID
		}
		if a.Prefix != b.Prefix {
			return a.Prefix < b.Prefix
		}
		return a.Op < b.Op
	})
	return events
}
