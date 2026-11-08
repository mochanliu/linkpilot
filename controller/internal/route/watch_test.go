package route

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// watchFixture is a running client plus the cancel that stops it. The tests
// here need to stop the client inside the test body (that is the point of one
// of them), so startClient's t.Cleanup-only shutdown is not enough.
type watchFixture struct {
	c      *Client
	rr     *scriptedRR
	cancel context.CancelFunc
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	rr := newScriptedRR()
	cfg := testClientConfig()
	cfg.Dial = rr.dial
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.SetLocalPrefixes(testLocalPrefixes); err != nil {
		t.Fatalf("SetLocalPrefixes: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	})
	return &watchFixture{c: c, rr: rr, cancel: cancel}
}

func (f *watchFixture) subscribe(t *testing.T) *Watch {
	t.Helper()
	w, err := f.c.WatchSubscribe()
	if err != nil {
		t.Fatalf("WatchSubscribe: %v", err)
	}
	t.Cleanup(w.Close)
	return w
}

// unreadDepth reports how many batches the hub is holding for its current
// subscriber, or -1 when the slot is empty -- which is also how these tests see
// that the hub dropped a reader that would not drain. fire() sends while it
// holds h.mu, so reading the buffer under the same lock is the only way to ask
// this without racing the flush.
func unreadDepth(c *Client) int {
	c.mu.Lock()
	h := c.watch
	c.mu.Unlock()
	if h == nil {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sub == nil {
		return -1
	}
	return len(h.sub.Events)
}

// nextBatch waits for one flush. The budget is generous because a batch only
// arrives after the 10 ms coalesce window and a loaded test machine can stretch
// that; nothing here is measuring the window itself.
func nextBatch(t *testing.T, w *Watch) WatchBatch {
	t.Helper()
	select {
	case b, ok := <-w.Events:
		if !ok {
			t.Fatal("watch channel closed")
		}
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("no watch batch arrived")
		return WatchBatch{}
	}
}

// wantQuiet asserts the stream stays silent for d. Kept short on purpose: the
// claim is "no batch for this change", and waiting long enough to be sure also
// waits long enough to hide a slow CI failure.
func wantQuiet(t *testing.T, w *Watch, d time.Duration, what string) {
	t.Helper()
	select {
	case b := <-w.Events:
		t.Fatalf("%s: got batch %+v", what, b.Events)
	case <-time.After(d):
	}
}

// eventLine is the compact form the expectations are written in:
// "upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2]".
func eventLine(e RouteEvent) string {
	s := e.Op + " t" + strconv.Itoa(int(e.Table)) +
		" d" + strconv.Itoa(int(e.DeviceID)) +
		" v" + strconv.Itoa(int(e.VRFID)) + " " + e.Prefix
	if len(e.NextHops) > 0 {
		s += " [" + strings.Join(e.NextHops, ",") + "]"
	}
	return s
}

func eventLines(events []RouteEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, eventLine(e))
	}
	return out
}

func wantEvents(t *testing.T, b WatchBatch, want ...string) {
	t.Helper()
	got := eventLines(b.Events)
	if len(got) != len(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("events = %q, want %q", got, want)
		}
	}
}

// TestWatchInitSendsTheWholeView is the attach contract: a new subscriber gets
// the table as it stands, as upserts, and nothing else. FRR's shape -- a fresh
// connection re-announces (bgp_zebra_connected -> bgp_zebra_announce_table), it
// does not resume from a remembered position.
func TestWatchInitSendsTheWholeView(t *testing.T) {
	f := newWatchFixture(t)
	rt := testRoute(20, 5, "10.20.0.0/16", "65000:6")
	f.rr.announce(rt, testRoute(21, 5, "10.21.0.0/16", "65000:6"))
	// Both paths are in the Loc-RIB (received=2); only device 20's is
	// installable, because device 21 has no tunnel in testClientConfig.
	waitFor(t, "both routes received, one installable", func() bool {
		st := f.c.Stats()
		return st.Received == 2 && st.RibRoutes == 1
	})

	w := f.subscribe(t)
	b := nextBatch(t, w)
	if !b.Init {
		t.Fatalf("first batch is not an INIT: %+v", b)
	}
	// So the INIT is the installable set, not the Loc-RIB: 10.21.0.0/16 is a
	// path we hold and cannot resolve, and sr-fib has nothing to install for it.
	wantEvents(t, b, "upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2]")
}

// TestWatchAnnounceAndWithdraw covers the steady state: one route in, one
// event; one route out, one withdraw that does not need the old next hops.
func TestWatchAnnounceAndWithdraw(t *testing.T) {
	f := newWatchFixture(t)
	rt := testRoute(20, 5, "10.20.0.0/16", "65000:6")
	f.rr.announce(rt)
	waitFor(t, "route imported", func() bool { return len(f.c.RIB(5)) == 1 })

	w := f.subscribe(t)
	if b := nextBatch(t, w); !b.Init {
		t.Fatalf("first batch is not an INIT: %+v", b)
	}

	neu := testRoute(20, 5, "10.23.0.0/16", "65000:6")
	f.rr.send(Envelope{Type: MsgAnnounce, Routes: []Route{neu}})
	wantEvents(t, nextBatch(t, w), "upsert t5 d20 v5 10.23.0.0/16 [192.168.99.2]")

	f.rr.send(Envelope{Type: MsgWithdraw, Keys: []RouteKey{neu.Key()}})
	// A withdraw carries no next hops: the reader deletes the row, it does not
	// need to know what was there.
	wantEvents(t, nextBatch(t, w), "withdraw t5 d20 v5 10.23.0.0/16")
}

// TestWatchNextHopSetChangeIsAnEvent is the case that makes diffing on
// ribUpdate.changes() wrong. The path stays resolvable -- nothing in the
// Loc-RIB changes, no best path moves -- but the row sr-fib must install now
// has two next hops instead of one.
func TestWatchNextHopSetChangeIsAnEvent(t *testing.T) {
	f := newWatchFixture(t)
	f.rr.announce(testRoute(20, 5, "10.20.0.0/16", "65000:6"))
	waitFor(t, "route imported", func() bool { return len(f.c.RIB(5)) == 1 })

	w := f.subscribe(t)
	wantEvents(t, nextBatch(t, w), "upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2]")

	before := f.c.Stats()
	if err := f.c.SetTunnels([]Tunnel{
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 20, NextHop: "192.168.99.4"},
	}); err != nil {
		t.Fatalf("SetTunnels: %v", err)
	}
	after := f.c.Stats()
	if after.RibRoutes != before.RibRoutes || after.Imported != before.Imported ||
		after.Withdrawn != before.Withdrawn {
		t.Fatalf("the Loc-RIB moved: %+v -> %+v", before, after)
	}
	wantEvents(t, nextBatch(t, w),
		"upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2,192.168.99.4]")
}

// TestWatchVRFImportChangeIsAnEvent is the second read-time gate. The route has
// been here the whole time and its path never changed; what changed is that a
// local table now imports its RTs, so it becomes installable.
func TestWatchVRFImportChangeIsAnEvent(t *testing.T) {
	f := newWatchFixture(t)
	// 65000:9 is imported by nobody in testClientConfig, so this is pending.
	f.rr.announce(testRoute(20, 5, "10.29.0.0/16", "65000:9"))
	waitFor(t, "route held as pending", func() bool {
		return len(f.c.Pending()) == 1 && len(f.c.RIB(5)) == 0
	})

	w := f.subscribe(t)
	b := nextBatch(t, w)
	if !b.Init || len(b.Events) != 0 {
		t.Fatalf("INIT = %+v, want an empty snapshot", b)
	}

	if err := f.c.SetVRFs([]VRFConfig{{
		ID:        5,
		ExportRTs: []string{"65000:5"},
		ImportRTs: []string{"65000:6", "65000:9"},
	}}); err != nil {
		t.Fatalf("SetVRFs: %v", err)
	}
	if n := len(f.c.RIB(5)); n != 1 {
		t.Fatalf("route did not import: %d rows", n)
	}
	wantEvents(t, nextBatch(t, w), "upsert t5 d20 v5 10.29.0.0/16 [192.168.99.2]")
}

// TestWatchNoBatchForInvisibleChange is the other half of the same property:
// an edit that leaves the view byte-for-byte identical must not wake the
// reader. Without this, every 10 s tunnel-state sample from the agent would
// buy a VPP reconcile pass for nothing.
func TestWatchNoBatchForInvisibleChange(t *testing.T) {
	f := newWatchFixture(t)
	f.rr.announce(testRoute(20, 5, "10.20.0.0/16", "65000:6"))
	waitFor(t, "route imported", func() bool { return len(f.c.RIB(5)) == 1 })

	w := f.subscribe(t)
	nextBatch(t, w) // INIT

	// Same tunnel table: SetTunnels returns early on equality, so not even a
	// dirty flag is set.
	if err := f.c.SetTunnels(testClientConfig().Tunnels); err != nil {
		t.Fatalf("SetTunnels: %v", err)
	}
	// Same reachability verdict: the paths are re-processed with the flag they
	// already had, which is a Loc-RIB write and an unchanged view.
	if err := f.c.SetTunnelState([]TunnelState{{PeerNodeID: 20, BFDState: "up"}}); err != nil {
		t.Fatalf("SetTunnelState: %v", err)
	}
	if err := f.c.SetTunnelState([]TunnelState{{PeerNodeID: 20, BFDState: "up"}}); err != nil {
		t.Fatalf("SetTunnelState (again): %v", err)
	}
	wantQuiet(t, w, 200*time.Millisecond, "an unchanged view woke the reader")
}

// TestWatchTunnelDownWithdrawsAndUpReinstalls is the #262 window in one test:
// the agent saying "BFD down" must produce a withdraw immediately, and the
// same tunnel coming back must produce the upsert without another announce.
func TestWatchTunnelDownWithdrawsAndUpReinstalls(t *testing.T) {
	f := newWatchFixture(t)
	f.rr.announce(testRoute(20, 5, "10.20.0.0/16", "65000:6"))
	waitFor(t, "route imported", func() bool { return len(f.c.RIB(5)) == 1 })

	w := f.subscribe(t)
	nextBatch(t, w) // INIT

	if err := f.c.SetTunnelState([]TunnelState{{PeerNodeID: 20, BFDState: "down"}}); err != nil {
		t.Fatalf("SetTunnelState(down): %v", err)
	}
	wantEvents(t, nextBatch(t, w), "withdraw t5 d20 v5 10.20.0.0/16")

	if err := f.c.SetTunnelState([]TunnelState{{PeerNodeID: 20, BFDState: "up"}}); err != nil {
		t.Fatalf("SetTunnelState(up): %v", err)
	}
	wantEvents(t, nextBatch(t, w), "upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2]")
}

// TestWatchLosingOneRRIsSilent is the dual-RR case as a watch property: the
// same path exists in both sessions' copies, so one session dying does not
// change what sr-fib installs by one byte. The FIB must not twitch when a
// reflector fails -- that is the whole reason for two of them.
func TestWatchLosingOneRRIsSilent(t *testing.T) {
	rrA := newTestReflector(t, ReflectorConfig{RRID: "rr-a"})
	rrB := newTestReflector(t, ReflectorConfig{RRID: "rr-b"})
	srvA := newTestRRSrv(t, rrA, 10)
	srvB := newTestRRSrv(t, rrB, 10)
	for _, r := range []*Reflector{rrA, rrB} {
		peer, _ := dial(t, r, 20)
		peer.join()
		peer.send(Envelope{Type: MsgAnnounce, Routes: []Route{
			testRoute(20, 5, "10.20.0.0/16", "65000:6"),
		}})
	}

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr-a", Addr: "rr-a"}, {ID: "rr-b", Addr: "rr-b"}}
	dialFor := map[string]func(context.Context, string) (net.Conn, error){
		"rr-a": srvA.dial, "rr-b": srvB.dial,
	}
	cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		return dialFor[addr](ctx, addr)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)

	waitFor(t, "the shared prefix in the table", func() bool { return len(c.RIB(5)) == 1 })
	w, err := c.WatchSubscribe()
	if err != nil {
		t.Fatalf("WatchSubscribe: %v", err)
	}
	defer w.Close()
	if b := nextBatch(t, w); !b.Init {
		t.Fatalf("first batch is not an INIT: %+v", b)
	}

	srvA.kill()
	waitFor(t, "rr-a's session is down", func() bool { return !c.Stats().Sessions[0].Connected })
	wantQuiet(t, w, 200*time.Millisecond, "losing one of two RR copies reached the reader")

	// Losing the second one, though, empties the table, and that is an event.
	srvB.kill()
	wantEvents(t, nextBatch(t, w), "withdraw t5 d20 v5 10.20.0.0/16")
}

// TestWatchCoalescesABurst is the reason the window exists: one RR snapshot of
// hundreds of prefixes must become one write, the same way zebra's 10 ms RIB
// hold time collapses a redistribution burst.
func TestWatchCoalescesABurst(t *testing.T) {
	f := newWatchFixture(t)
	var routes []Route
	for i := 1; i <= 50; i++ {
		routes = append(routes, testRoute(20, 5, "10.50."+strconv.Itoa(i)+".0/24", "65000:6"))
	}
	f.rr.announce(routes...)
	waitFor(t, "burst imported", func() bool { return len(f.c.RIB(5)) == 50 })

	w := f.subscribe(t)
	nextBatch(t, w) // INIT carries all 50

	// A second frame pushed as one announce is one flush, whatever its size.
	var more []Route
	for i := 1; i <= 20; i++ {
		more = append(more, testRoute(20, 5, "10.60."+strconv.Itoa(i)+".0/24", "65000:6"))
	}
	f.rr.send(Envelope{Type: MsgAnnounce, Routes: more})
	b := nextBatch(t, w)
	if len(b.Events) != 20 {
		t.Fatalf("burst came out as %d events, want one batch of 20: %q", len(b.Events), eventLines(b.Events))
	}
}

// TestWatchBackpressureDropsTheReader states the policy out loud: we would
// rather lose the subscription and be handed a fresh INIT than let a slow
// reader accumulate a state gap.
func TestWatchBackpressureDropsTheReader(t *testing.T) {
	f := newWatchFixture(t)
	f.rr.announce(testRoute(20, 5, "10.20.0.0/16", "65000:6"))
	waitFor(t, "route imported", func() bool { return len(f.c.RIB(5)) == 1 })

	w, err := f.c.WatchSubscribe()
	if err != nil {
		t.Fatalf("WatchSubscribe: %v", err)
	}
	defer w.Close()

	// Park the INIT in the one buffered slot and never read it: the next flush
	// then has nowhere to go, and the policy is to lose the reader.
	waitFor(t, "the hub is holding one unread batch", func() bool {
		return unreadDepth(f.c) == 1
	})
	f.rr.send(Envelope{Type: MsgAnnounce, Routes: []Route{testRoute(20, 5, "10.70.0.0/16", "65000:6")}})
	waitFor(t, "the stalled reader was dropped", func() bool {
		return unreadDepth(f.c) == -1
	})

	select {
	case b, ok := <-w.Events:
		if !ok {
			t.Fatal("the unread INIT batch was dropped along with the reader")
		}
		// Buffered deliveries come out before the closure: the first read is the
		// batch the hub parked, the second one is the drop.
		if !b.Init {
			t.Fatalf("the parked batch was not the INIT: %+v", b.Events)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled reader was never handed the batch it had room for")
	}
	select {
	case extra, ok := <-w.Events:
		if ok {
			t.Fatalf("the stalled reader was not dropped, it got %+v", extra.Events)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a reader that never drains was not dropped")
	}

	// The slot is free again, and the next subscriber gets the whole view --
	// including the prefix the dropped reader never saw.
	w2, err := f.c.WatchSubscribe()
	if err != nil {
		t.Fatalf("re-subscribe after a drop: %v", err)
	}
	defer w2.Close()
	b := nextBatch(t, w2)
	if !b.Init {
		t.Fatalf("re-subscribe did not get an INIT: %+v", b)
	}
	if len(b.Events) != 2 {
		t.Fatalf("INIT after a drop = %q, want both prefixes including the one missed", eventLines(b.Events))
	}
}

func TestWatchOneSubscriberAtATime(t *testing.T) {
	f := newWatchFixture(t)
	w := f.subscribe(t)
	nextBatch(t, w) // INIT occupies the slot only until read; subscribe first.

	if _, err := f.c.WatchSubscribe(); err == nil {
		t.Fatal("a second subscriber was accepted")
	}

	// Closing frees it, and the replacement gets a fresh INIT rather than the
	// tail of the previous reader's position.
	w.Close()
	w2, err := f.c.WatchSubscribe()
	if err != nil {
		t.Fatalf("subscribe after Close: %v", err)
	}
	defer w2.Close()
	if b := nextBatch(t, w2); !b.Init {
		t.Fatalf("replacement subscriber did not get an INIT: %+v", b)
	}
}

// TestWatchRunShutdownClosesTheStream is what keeps sr-fib's read from hanging:
// Client.Run returning must unblock whoever is sitting on Events, or the HTTP
// handler outlives the process's shutdown.
func TestWatchRunShutdownClosesTheStream(t *testing.T) {
	f := newWatchFixture(t)
	w := f.subscribe(t)
	nextBatch(t, w) // INIT

	f.cancel()
	select {
	case _, ok := <-w.Events:
		if ok {
			// A late flush is legal if it raced the cancel; what must not
			// happen is the channel staying open.
			if _, ok := <-w.Events; ok {
				t.Fatal("stream still open after shutdown")
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not close the watch channel")
	}

	if _, err := f.c.WatchSubscribe(); err == nil {
		t.Fatal("subscribe accepted after shutdown")
	}
}

// TestWatchViewMatchesAllRIB is the equivalence the reader relies on: the INIT
// snapshot and the polled /status body must be the same rows. If they drift,
// "fall back to a full fetch" silently changes the FIB.
func TestWatchViewMatchesAllRIB(t *testing.T) {
	f := newWatchFixture(t)
	f.rr.announce(
		testRoute(20, 5, "10.20.0.0/16", "65000:6"),
		testRoute(21, 5, "10.21.0.0/16", "65000:6"),
		testRoute(20, 7, "10.20.1.0/24", "65000:6"),
	)
	waitFor(t, "the resolvable routes imported", func() bool { return len(f.c.RIB(5)) == 2 })

	w := f.subscribe(t)
	b := nextBatch(t, w)

	// viewOf renders a row identity the two sources can be compared on.
	viewOf := func(op string, table, dev, vrf uint8, prefix string, nh []string) string {
		return op + "|" + strconv.Itoa(int(table)) + "|" + strconv.Itoa(int(dev)) +
			"|" + strconv.Itoa(int(vrf)) + "|" + prefix + "|" + strings.Join(nh, ",")
	}
	fromInit := map[string]bool{}
	for _, e := range b.Events {
		fromInit[viewOf(e.Op, e.Table, uint8(e.DeviceID), e.VRFID, e.Prefix, e.NextHops)] = true
	}
	fromStatus := map[string]bool{}
	for table, entries := range f.c.AllRIB() {
		for _, e := range entries {
			// RIBEntry embeds Route: the device/VRF/prefix on the row *is* the
			// source route's, which is what viewKey keys on too.
			fromStatus[viewOf(OpUpsert, table, uint8(e.DeviceID), e.VRFID,
				e.Prefix, e.NextHops)] = true
		}
	}
	for k := range fromStatus {
		if !fromInit[k] {
			t.Fatalf("/status row %q missing from the INIT", k)
		}
	}
	for k := range fromInit {
		if !fromStatus[k] {
			t.Fatalf("INIT row %q not in /status", k)
		}
	}
}
