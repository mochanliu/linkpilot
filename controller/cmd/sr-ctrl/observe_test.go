// Unit tests for the observation path: attributing an outer source address to a
// node, and only when the evidence names exactly one.
//
// These cases exist because the claims are the kind that fail silently. A wrong
// attribution does not error -- it hands a peer a path to somebody else's box,
// and the symptom is a tunnel that works with the wrong endpoint. So the tests
// are mostly about when the controller must REFUSE to answer.
package main

import (
	"testing"
	"time"

	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

const (
	testVpnPort     = 51820
	testTransitPort = 51821
)

func newTestServer() *server {
	st := &State{
		Nodes:      map[string]*NodeRecord{},
		NextNodeID: map[string]uint16{},
		Claims:     claimMap{},
		Tenants:    map[string]*Tenant{"t": {ID: "t"}, "t2": {ID: "t2"}},
		PKeyRings:  map[string][]wire.PKey{},
		Underlays:  map[string]*UnderlayConfig{},
	}
	return &server{
		state:      st,
		hbSec:      5,
		obs:        map[string]observerView{},
		pendingObs: map[string]map[string]*pendingObs{},
		tel:        map[string]*nodeTelemetry{},
		telTTLSec:  30,
	}
}

// addNode puts a node in the registry with its declared addresses. `declared` is
// the only thing attribution reads besides the reflector mapping.
func addNode(s *server, tenant string, id uint16, declared []string, mapping string) *NodeRecord {
	rec := &NodeRecord{
		TenantID:    tenant,
		NodeID:      id,
		Serial:      "serial",
		VpnPort:     testVpnPort,
		TransitPort: testTransitPort,
		LocalAddrs:  declared,
		UDPMapping:  mapping,
	}
	s.state.Nodes[key(tenant, id)] = rec
	return rec
}

func TestAttributeUniqueByDeclaredAddress(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")

	// node 2 reports having received traffic from 10.10.1.2:41780 on its VPN port.
	id, reason, retry := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: 51820})
	if id != 1 {
		t.Fatalf("attribution gave node %d (%s), want 1", id, reason)
	}
	if reason != "unique" || retry {
		t.Fatalf("reason = %q, retry = %v; want unique, no retry", reason, retry)
	}
}

func TestAttributeByReflectorMapping(t *testing.T) {
	s := newTestServer()
	// A node behind NAT may declare only private addresses; the address a
	// reflector saw is a legitimate second claim to match against.
	addNode(s, "t", 1, nil, "203.0.113.7:40000")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")

	id, reason, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "203.0.113.7", SrcPort: 40000, DstPort: 51820})
	if id != 1 {
		t.Fatalf("reflector-observed address did not attribute: %d (%s)", id, reason)
	}
}

func TestAttributeRefusesAmbiguous(t *testing.T) {
	s := newTestServer()
	// Two nodes behind one NAT, or one lab bridge that rewrote a source twice:
	// both see the same IP. Naming either one would be a guess.
	addNode(s, "t", 1, []string{"10.10.1.1"}, "")
	addNode(s, "t", 2, []string{"10.10.1.1"}, "")
	addNode(s, "t", 3, []string{"10.10.2.2"}, "")

	id, reason, retry := s.attributeLocked("t", 3, wire.Observation{SrcIP: "10.10.1.1", SrcPort: 41780, DstPort: 51820})
	if id != 0 {
		t.Fatalf("ambiguous source attributed to node %d; it should not be attributed at all", id)
	}
	if !retry {
		t.Fatal("an ambiguous row must stay pending: it resolves on its own if one claimant is removed or renumbered")
	}
	if reason == "" {
		t.Fatal("refusal must carry a reason the agent can log")
	}
}

func TestAttributeNeverSaysSelf(t *testing.T) {
	s := newTestServer()
	// The docker bridge in the lab really does this: a frame crossing it arrives
	// with the bridge gateway's address, which is the observer's own address. If
	// the controller answered "that was you", the node would be told to open a
	// tunnel to itself.
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.1.2", "10.10.2.2"}, "")

	id, _, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: 51820})
	if id == 2 {
		t.Fatal("observer was attributed its own source address")
	}
	// node 1 also claims it, so node 1 is the only legal answer.
	if id != 1 {
		t.Fatalf("got %d, want 1 (the other claimant)", id)
	}
}

func TestAttributeIgnoresOtherTenants(t *testing.T) {
	s := newTestServer()
	// Both tenants have a node 2 as the observer, and only tenant a owns the
	// address being reported. Node ids are unique per tenant, so "node 2" alone
	// cannot say which tenant is asking -- and an address from another tenant's
	// node must never answer for a tenant that does not claim it.
	addNode(s, "a", 1, []string{"10.10.1.2"}, "")
	addNode(s, "a", 2, []string{"10.10.2.2"}, "")
	addNode(s, "b", 2, []string{"10.10.2.2"}, "")

	id, reason, retry := s.attributeLocked("b", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: 51820})
	if id != 0 {
		t.Fatalf("tenant b was attributed tenant a's node %d: cross-tenant leak", id)
	}
	if reason == "" || !retry {
		t.Fatalf("a stranger's address must be parked with a reason, got %q retry=%v", reason, retry)
	}
	// The same row in the owning tenant does resolve, so the refusal above came
	// from scoping and not from the row being rejected as malformed.
	if id, _, _ := s.attributeLocked("a", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: 51820}); id != 1 {
		t.Fatalf("same-tenant attribution broke: got node %d", id)
	}
}

func TestAttributeUsesDstPortAsRoleKey(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	n2 := addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	// The destination port is the dataplane's function key: a row that landed on
	// some other port did not come from a tunnel this controller runs.
	for _, p := range []uint16{51820, 51821} {
		if id, _, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 9999, DstPort: p}); id != 1 {
			t.Fatalf("role port %d should be accepted, got node %d", p, id)
		}
	}
	id, reason, retry := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 9999, DstPort: 443})
	if id != 0 || retry {
		t.Fatalf("foreign dst port must be refused outright (id=%d retry=%v)", id, retry)
	}
	if reason == "" {
		t.Fatal("role rejection must explain itself")
	}
	// A node that rebound its port is judged by ITS ports, not the tenant's.
	n2.VpnPort = 51999
	if id, _, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 9999, DstPort: 51999}); id != 1 {
		t.Fatal("a rebound port should still be a valid role")
	}
}

func TestAttributeRejectsMalformed(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	for _, ob := range []wire.Observation{
		{SrcIP: "", SrcPort: 1, DstPort: 51820},
		{SrcIP: "10.10.1.2", SrcPort: 0, DstPort: 51820},
	} {
		if id, _, retry := s.attributeLocked("t", 2, ob); id != 0 || retry {
			t.Fatalf("malformed row %+v must be dropped, not parked: id=%d retry=%v", ob, id, retry)
		}
	}
}

func TestLateAttributionResolvesWhenAddressesArrive(t *testing.T) {
	s := newTestServer()
	// node 1 has enrolled but not yet declared its addresses (the mapping report
	// is the next thing the agent does, and order is not guaranteed).
	addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")

	ob := wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: 51820, Verified: true, Hits: 3}
	id, reason, retry := s.attributeLocked("t", 2, ob)
	if id != 0 || !retry {
		t.Fatalf("unknown address should park for later: id=%d retry=%v", id, retry)
	}
	s.pendingObs[key("t", 2)] = map[string]*pendingObs{
		"10.10.1.2:41780": {ob: ob, reason: reason, seenAt: time.Now().Unix()},
	}

	// Now node 1 reports its own addresses -- exactly what handleMapping does.
	s.state.Nodes[key("t", 1)].LocalAddrs = normaliseAddrs([]string{"10.10.1.2/24", "10.10.1.2"})
	if n := s.reattributeLocked("t"); n != 1 {
		t.Fatalf("reattributeLocked resolved %d rows, want 1", n)
	}
	o := s.obs[key("t", 2)][1]
	if o == nil || o.mapping != "10.10.1.2:41780" || !o.verified || o.hits != 3 {
		t.Fatalf("late attribution lost the row: %+v", o)
	}
	if len(s.pendingObs[key("t", 2)]) != 0 {
		t.Fatal("resolved row must leave the pending set")
	}
}

func TestPerReaderViewInRegistry(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	// Node 2 saw node 1 at 41780; node 1 saw node 2 at 42780. There is no single
	// "address of node 1" once a symmetric NAT is in the picture, so the registry
	// row must carry the READER's answer and nothing else.
	s.obs[key("t", 2)] = observerView{1: &attributedObs{mapping: "10.10.1.2:41780", dstPort: 51820, verified: true, seenAt: time.Now().Unix()}}
	s.obs[key("t", 1)] = observerView{2: &attributedObs{mapping: "10.10.2.2:42780", dstPort: 51820, seenAt: time.Now().Unix()}}

	rec1 := s.state.Nodes[key("t", 1)]
	as2 := s.toPeerInfoLocked(rec1, 2)
	if as2.ObservedMapping != "10.10.1.2:41780" || !as2.ObservedVerified {
		t.Fatalf("reader 2 must see its own observation, got %+v", as2)
	}
	as1 := s.toPeerInfoLocked(rec1, 1)
	if as1.ObservedMapping != "" {
		t.Fatalf("reader 1 must not see an observation of itself, got %q", as1.ObservedMapping)
	}
	if !as1.IsSelf || as2.IsSelf {
		t.Fatal("IsSelf is reader-dependent and got it wrong")
	}
	if as2.VpnPort != testVpnPort || as2.TransitPort != testTransitPort {
		t.Fatalf("peer rows must carry the ports to send to: %+v", as2)
	}
	// peersLocked is what /v1/registry and the watch snapshot use; every row in it
	// must be rendered for the one reader that asked. Node 2 has no observation of
	// itself -- the row it is reading is about node 1, from node 2's vantage.
	for2 := s.peersLocked("t", 2)
	if len(for2) != 2 || !for2[1].IsSelf || for2[1].ObservedMapping != "" {
		t.Fatalf("reader 2 must not be shown an observation of itself: %+v", for2)
	}
	if for2[0].NodeID != 1 || for2[0].ObservedMapping != "10.10.1.2:41780" {
		t.Fatalf("registry did not render peer rows for the reader: %+v", for2)
	}
	// The other reader gets the mirror image: its own answer about node 2, and
	// nothing about node 1, which it never saw.
	for1 := s.peersLocked("t", 1)
	if for1[0].ObservedMapping != "" || !for1[0].IsSelf || for1[1].ObservedMapping != "10.10.2.2:42780" {
		t.Fatalf("reader 1 got reader 2's vantage: %+v", for1)
	}
}

func TestObservationsExpire(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	old := time.Now().Add(-observationTTL - time.Second).Unix()
	s.obs[key("t", 2)] = observerView{
		1: &attributedObs{mapping: "10.10.1.2:41780", seenAt: old},
	}
	s.pendingObs[key("t", 2)] = map[string]*pendingObs{
		"198.51.100.9:5000": {seenAt: old},
	}
	s.expireObservationsLocked()
	if len(s.obs) != 0 {
		t.Fatalf("stale attributed row survived expiry: %+v", s.obs)
	}
	if len(s.pendingObs) != 0 {
		t.Fatal("stale pending row survived expiry")
	}
}

func TestForgetTargetOnStale(t *testing.T) {
	s := newTestServer()
	n1 := addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	s.obs[key("t", 2)] = observerView{1: &attributedObs{mapping: "10.10.1.2:41780", seenAt: time.Now().Unix()}}
	s.pendingObs[key("t", 2)] = map[string]*pendingObs{
		"10.10.1.2:41781": {seenAt: time.Now().Unix()},
		"198.51.100.9:1":  {seenAt: time.Now().Unix()},
	}
	n1.Stale = true
	s.forgetTargetLocked("t", 1)
	if len(s.obs[key("t", 2)]) != 0 {
		t.Fatal("a stale node's address is still being published to peers")
	}
	// The row that looks like it came from the lapsed node's own declared address
	// goes too; an unrelated stranger stays pending for someone else.
	if _, ok := s.pendingObs[key("t", 2)]["10.10.1.2:41781"]; ok {
		t.Fatal("pending row of the lapsed node survived")
	}
	if _, ok := s.pendingObs[key("t", 2)]["198.51.100.9:1"]; !ok {
		t.Fatal("unrelated pending row was dropped")
	}
}

func TestNormaliseAddrs(t *testing.T) {
	got := normaliseAddrs([]string{"10.10.1.2/24", " 10.10.1.2 ", "", "10.10.2.2/24"})
	want := []string{"10.10.1.2", "10.10.2.2"}
	if !equalStrings(got, want) {
		t.Fatalf("normaliseAddrs = %v, want %v", got, want)
	}
	// Reordering must not read as a change, or every report would churn the
	// registry and broadcast an "update" that says nothing.
	if equalStrings(normaliseAddrs([]string{"b", "a"}), normaliseAddrs([]string{"a", "b"})) != true {
		t.Fatal("normaliseAddrs must sort so a reordered list compares equal")
	}
}

// A controller-managed tunnel listens on a port from the pool, not on the VPN
// port a device once declared. Attribution has to know the ports it allocated,
// or every observation that arrives over such a tunnel is called "unattributed"
// and the controller keeps claiming it never saw the peer -- the log line the
// lab run of 2026-10-28 was full of.
func TestAttributeAcceptsAllocatedTunnelPorts(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, []string{"10.10.1.2"}, "")
	peer := addNode(s, "t", 2, []string{"10.10.2.2"}, "")
	peer.VpnPort = 0 // never declared one: the pool gave it 20000

	st := underlay.New(underlay.DefaultSettings())
	st.TransitPorts[2] = 15000
	st.Tunnels["t1"] = &underlay.Tunnel{
		ID: "t1",
		A:  underlay.Endpoint{NodeID: 2, Port: 20000},
		B:  underlay.Endpoint{NodeID: 1, Port: 20000},
	}
	s.state.Underlays["t"] = &UnderlayConfig{TenantID: "t", Draft: st.Settings, State: st}

	for _, port := range []uint16{20000, 15000} {
		id, reason, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: port})
		if id != 1 {
			t.Fatalf("端口 %d 上的观测没归到设备 1: %d (%s)", port, id, reason)
		}
	}
	// A port this node does not own is still refused: that is how a packet that
	// reached the wrong service stays out of the topology.
	if id, _, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780, DstPort: 20007}); id != 0 {
		t.Fatalf("端口 20007 不属于设备 2，却归给了设备 %d", id)
	}
	if id, _, _ := s.attributeLocked("t", 2, wire.Observation{SrcIP: "10.10.1.2", SrcPort: 41780}); id != 0 {
		t.Fatalf("目标端口 0（没有端口可言）竟被归给设备 %d", id)
	}
}

func TestTenantKeyRingIsStable(t *testing.T) {
	s := newTestServer()
	ring := s.tenantKeyRing("t")
	if len(ring) != 1 || ring[0].HmacID != firstHmacID || len(ring[0].Key) != pKeyLen*2 {
		t.Fatalf("ring = %+v; want one hmac_id=%d key of %d hex chars", ring, firstHmacID, pKeyLen*2)
	}
	again := s.tenantKeyRing("t")
	if again[0].Key != ring[0].Key {
		t.Fatal("key ring changed between enrollments: peers would stop verifying each other")
	}
	other := s.tenantKeyRing("t2")
	if other[0].Key == ring[0].Key {
		t.Fatal("two tenants were issued the same domain key")
	}
}
