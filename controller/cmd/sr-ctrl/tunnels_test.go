// Tests for the declared tunnel table on the mapping report: what the controller
// stores, what it refuses, and -- the part the route protocol depends on -- what
// each READER is told.
//
// The per-reader rule is the whole point. A node's table names the peer on the
// far end of each tunnel and its own address on that tunnel; relaying the whole
// table would hand a peer next hops for links it is not on. So the assertions
// below read the registry as three different nodes and check that each one sees
// only the address that belongs to ITS link.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sdwan/controller/internal/wire"
)

// nopStore lets a handler test run save() without touching disk.
type nopStore struct{}

func (nopStore) Load() (*State, error) { return nil, nil }
func (nopStore) Save(*State) error     { return nil }

func mappingReq(t *testing.T, s *server, tenant string, node uint16, rep wire.MappingReport) *httptest.ResponseRecorder {
	t.Helper()
	s.store = nopStore{}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleMapping(rec, peerReq(http.MethodPost, "/v1/mapping", string(body), tenant, node))
	return rec
}

func peerRow(t *testing.T, s *server, tenant string, reader, node uint16) wire.PeerInfo {
	t.Helper()
	for _, p := range s.peersLocked(tenant, reader) {
		if p.NodeID == node {
			return p
		}
	}
	t.Fatalf("node %d missing from the registry as seen by %d", node, reader)
	return wire.PeerInfo{}
}

func TestMappingStoresTunnelsAndRelaysPerReader(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, nil, "")
	addNode(s, "t", 3, nil, "")

	// Node 1 has two tunnels to node 2 (parallel) and one to node 3. The list is
	// deliberately unsorted and contains an exact duplicate.
	rec := mappingReq(t, s, "t", 1, wire.MappingReport{Tunnels: &[]wire.TunnelEndpoint{
		{PeerNodeID: 3, LocalIP: "192.168.99.13"},
		{PeerNodeID: 2, LocalIP: "192.168.99.12"},
		{PeerNodeID: 2, LocalIP: "192.168.88.12"},
		{PeerNodeID: 2, LocalIP: "192.168.88.12"},
	}})
	if rec.Code != 200 {
		t.Fatalf("mapping rejected: %d %s", rec.Code, rec.Body.String())
	}
	stored := s.state.Nodes[key("t", 1)].Tunnels
	if len(stored) != 3 || stored[0].LocalIP != "192.168.88.12" || stored[2].LocalIP != "192.168.99.13" {
		t.Fatalf("stored table is not canonical: %+v", stored)
	}

	// Node 2 gets both of node 1's addresses toward it, in canonical order: that
	// set IS the next-hop list for routes learned from node 1.
	if got := peerRow(t, s, "t", 2, 1).OverlayIPs; len(got) != 2 || got[0] != "192.168.88.12" || got[1] != "192.168.99.12" {
		t.Fatalf("node 2 sees %v, want the two addresses node 1 declared toward it", got)
	}
	// Node 3 gets only its own link -- not node 1's addresses toward node 2.
	if got := peerRow(t, s, "t", 3, 1).OverlayIPs; len(got) != 1 || got[0] != "192.168.99.13" {
		t.Fatalf("node 3 sees %v, want only 192.168.99.13", got)
	}
	// A node's own row carries no overlay addresses: it declared none toward
	// itself, and it does not need its peers' opinions of where it lives.
	if got := peerRow(t, s, "t", 1, 1).OverlayIPs; len(got) != 0 {
		t.Fatalf("self row carries overlay addresses: %v", got)
	}
}

func TestMappingRefusesSelfTunnelAndKeepsOldTable(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, nil, "")
	good := []wire.TunnelEndpoint{{PeerNodeID: 2, LocalIP: "192.168.99.12"}}
	if rec := mappingReq(t, s, "t", 1, wire.MappingReport{Tunnels: &good}); rec.Code != 200 {
		t.Fatalf("good table rejected: %d %s", rec.Code, rec.Body.String())
	}

	bad := []wire.TunnelEndpoint{{PeerNodeID: 1, LocalIP: "192.168.99.1"}}
	rec := mappingReq(t, s, "t", 1, wire.MappingReport{Tunnels: &bad})
	if rec.Code != 400 {
		t.Fatalf("self tunnel accepted: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.state.Nodes[key("t", 1)].Tunnels; len(got) != 1 || got[0] != good[0] {
		t.Fatalf("rejected report damaged the stored table: %+v", got)
	}
}

func TestMappingTunnelTablePointerSemantics(t *testing.T) {
	s := newTestServer()
	addNode(s, "t", 1, nil, "")
	addNode(s, "t", 2, nil, "")
	full := []wire.TunnelEndpoint{{PeerNodeID: 2, LocalIP: "192.168.99.12"}}
	if rec := mappingReq(t, s, "t", 1, wire.MappingReport{Tunnels: &full}); rec.Code != 200 {
		t.Fatalf("table rejected: %d %s", rec.Code, rec.Body.String())
	}

	// nil means "not reported": a liveness-only report must not wipe the table.
	if rec := mappingReq(t, s, "t", 1, wire.MappingReport{}); rec.Code != 200 {
		t.Fatalf("nil table rejected: %d", rec.Code)
	}
	if got := s.state.Nodes[key("t", 1)].Tunnels; len(got) != 1 {
		t.Fatalf("nil table cleared the stored one: %+v", got)
	}

	// An explicit empty list is a declaration: "I have no tunnels".
	empty := []wire.TunnelEndpoint{}
	if rec := mappingReq(t, s, "t", 1, wire.MappingReport{Tunnels: &empty}); rec.Code != 200 {
		t.Fatalf("empty table rejected: %d", rec.Code)
	}
	if got := s.state.Nodes[key("t", 1)].Tunnels; len(got) != 0 {
		t.Fatalf("empty table did not clear the stored one: %+v", got)
	}
	if got := peerRow(t, s, "t", 2, 1).OverlayIPs; len(got) != 0 {
		t.Fatalf("peers still see a torn-down tunnel: %v", got)
	}
}
