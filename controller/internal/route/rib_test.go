package route

import "testing"

// Every test here judges the Loc-RIB against GoBGP's table semantics
// (internal/pkg/table: destination.Calculate / insertSort / Update.GetChanges),
// not against an opinion of ours. The two judgements that need the session
// layer -- "a session before EOR contributes nothing" and "RT decides only what
// a VRF can see" -- land with R2/R3, when the read path moves onto this table.

const (
	ribSrcA = "10.99.0.1:18446"
	ribSrcB = "10.99.0.2:18446"
)

func ribRoute(prefix string) Route {
	return Route{DeviceID: 20, VRFID: 5, Prefix: prefix, RTs: []string{"65000:5"}}
}

// Two sessions carrying the same route are two copies of one path, not one
// synthesized route: one destination, two paths, one winner.
func TestLocRibReflectedCopyIsNotASecondRoute(t *testing.T) {
	rt := ribRoute("10.10.0.0/16")
	r := newLocRib()

	if _, adv := r.process(rt, ribSrcA, true).changes(); adv == nil {
		t.Fatal("first path must be advertised")
	}
	if w, a := r.process(rt, ribSrcB, true).changes(); w != nil || a != nil {
		t.Fatalf("a copy from the second session is not a best-path change: withdrew %v, advertised %v", w, a)
	}

	if n := r.len(); n != 1 {
		t.Fatalf("destinations = %d, want 1", n)
	}
	d := r.dsts[rt.Key()]
	if len(d.paths) != 2 {
		t.Fatalf("paths = %d, want one copy per session", len(d.paths))
	}
	if got := r.bestOf(rt.Key()); got == nil || got.source != ribSrcA {
		t.Fatalf("best = %v, want the copy from %s", got, ribSrcA)
	}

	n := 0
	r.eachBest(func(RouteKey, *path) { n++ })
	if n != 1 {
		t.Fatalf("eachBest yielded %d entries, want 1", n)
	}

	// Re-advertising the same route from the same session replaces the copy in
	// place (implicitWithdraw) and changes nothing.
	r.process(rt, ribSrcA, true)
	if w, a := r.process(rt, ribSrcA, true).changes(); w != nil || a != nil {
		t.Fatalf("soft re-advertisement must be a no-op: withdrew %v, advertised %v", w, a)
	}
	if n := len(r.dsts[rt.Key()].paths); n != 2 {
		t.Fatalf("paths = %d after re-advertisement, want 2", n)
	}
}

// The point of the Loc-RIB: losing one session does not shrink the table. The
// other session's copy takes over. GoBGP reports that as a best-path change
// (Path.Equal compares the source peer, so the winner moving to another session
// is not "equal"), and so do we -- what matters here is that the prefix stays.
func TestLocRibSurvivesOneSessionGoingAway(t *testing.T) {
	rt := ribRoute("10.10.0.0/16")
	k := rt.Key()
	r := newLocRib()
	r.process(rt, ribSrcA, true)
	r.process(rt, ribSrcB, true)

	was, now := r.withdraw(k, ribSrcA).changes()
	if was == nil || now == nil || was.source != ribSrcA || now.source != ribSrcB {
		t.Fatalf("failover = (withdrew %v, advertised %v), want %s replaced by %s", was, now, ribSrcA, ribSrcB)
	}
	if got := r.bestOf(k); got == nil || got.source != ribSrcB {
		t.Fatalf("best = %v, want the surviving copy from %s", got, ribSrcB)
	}
	if n := r.len(); n != 1 {
		t.Fatalf("destinations = %d after one session left, want 1", n)
	}

	was, now = r.withdraw(k, ribSrcB).changes()
	if was == nil || now != nil {
		t.Fatalf("last copy leaving must withdraw: withdrew %v, advertised %v", was, now)
	}
	if n := r.len(); n != 0 {
		t.Fatalf("destinations = %d after every session left, want 0", n)
	}
	if u := r.withdraw(k, ribSrcB); u != nil {
		t.Fatal("withdrawing a route that is not there must be a no-op")
	}
}

// A path whose next hop cannot be resolved stays in the table (no_path is a
// flag on the path, not a second table) and drops out of best-path selection.
// Tunnels coming back re-elect it with no help from the RR.
func TestLocRibUnresolvablePathStaysInTheTable(t *testing.T) {
	rt := ribRoute("10.10.0.0/16")
	k := rt.Key()
	r := newLocRib()
	r.process(rt, ribSrcA, false)
	r.process(rt, ribSrcB, false)

	if n := len(r.dsts[k].paths); n != 2 {
		t.Fatalf("paths = %d, want both copies kept while unresolvable", n)
	}
	if got := r.bestOf(k); got != nil {
		t.Fatalf("best = %v, want none while no next hop resolves", got)
	}

	// This is how the flag flips: GoBGP feeds nexthop-state changes back through
	// Table.update (zclient applyToPathList -> updatePath -> propagateUpdate),
	// so the new copy replaces the old one and is re-sorted.
	if _, adv := r.process(rt, ribSrcA, true).changes(); adv == nil || adv.source != ribSrcA {
		t.Fatalf("advertised = %v, want the copy from %s once its tunnel is back", adv, ribSrcA)
	}
	if got := r.bestOf(k); got == nil || got.source != ribSrcA {
		t.Fatalf("best = %v, want %s", got, ribSrcA)
	}

	if w, _ := r.process(rt, ribSrcA, false).changes(); w == nil || w.source != ribSrcA {
		t.Fatalf("withdrew = %v, want the copy from %s when its tunnel went away", w, ribSrcA)
	}
	if got := r.bestOf(k); got != nil {
		t.Fatalf("best = %v, want none", got)
	}
}

// compareByNeighborAddress compares peer addresses, so the order the sessions
// were configured in cannot move the winner.
func TestLocRibBestIgnoresSessionOrder(t *testing.T) {
	rt := ribRoute("10.10.0.0/16")
	k := rt.Key()

	one := newLocRib()
	one.process(rt, ribSrcA, true)
	one.process(rt, ribSrcB, true)

	two := newLocRib()
	two.process(rt, ribSrcB, true)
	two.process(rt, ribSrcA, true)

	if a, b := one.bestOf(k), two.bestOf(k); a == nil || b == nil || a.source != b.source {
		t.Fatalf("best depends on arrival order: %v vs %v", a, b)
	}
	if got := two.bestOf(k); got.source != ribSrcA {
		t.Fatalf("best = %s, want the lexicographically smallest address %s", got.source, ribSrcA)
	}
}

// compareByReachableNexthop decides before the address tie-break, so a
// resolvable path inserted later still sorts ahead of an unresolvable one.
func TestComparePathsReachableNexthopDecides(t *testing.T) {
	rt := ribRoute("10.10.0.0/16")
	valid := &path{route: rt, source: ribSrcB, resolvable: true}
	invalid := &path{route: rt, source: ribSrcA, resolvable: false}

	if !ranksAhead(valid, invalid) || ranksAhead(invalid, valid) {
		t.Fatal("a resolvable next hop must outrank an unresolvable one regardless of address")
	}

	d := &destination{key: rt.Key()}
	d.insert(invalid)
	d.insert(valid)
	if len(d.paths) != 2 || d.paths[0] != valid || d.paths[1] != invalid {
		t.Fatalf("insert order = %v, want valid first", d.paths)
	}
	if got := d.best(); got != valid {
		t.Fatalf("best = %v, want the valid path", got)
	}
}

// byOrigin is the Nexthop Tracking index: it must follow replacements and
// withdrawals, otherwise flipping flags for one origin would touch stale
// pointers or drop live ones.
func TestLocRibOriginIndexFollowsTheTable(t *testing.T) {
	a := ribRoute("10.10.0.0/16")
	b := ribRoute("10.20.0.0/16")
	r := newLocRib()
	r.process(a, ribSrcA, true)
	r.process(b, ribSrcB, true)

	if got := r.originPaths(20); len(got) != 2 {
		t.Fatalf("origin paths = %d, want 2", len(got))
	}

	r.process(a, ribSrcA, false)
	ps := r.originPaths(20)
	for _, p := range ps {
		if p.route.Prefix == a.Prefix && p.resolvable {
			t.Fatal("the replaced copy is still indexed")
		}
	}
	if len(ps) != 2 {
		t.Fatalf("origin paths = %d after a replacement, want 2", len(ps))
	}

	r.withdraw(b.Key(), ribSrcB)
	if got := r.originPaths(20); len(got) != 1 {
		t.Fatalf("origin paths = %d after a withdrawal, want 1", len(got))
	}
}
