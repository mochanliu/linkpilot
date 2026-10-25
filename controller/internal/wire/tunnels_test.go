// Tests for the declared tunnel table: the contract that lets a peer resolve a
// next hop for the route protocol without the origin ever carrying one.
//
// The failure this file guards against is quiet: a non-canonical or self entry
// stored as-is would be relayed to peers as a next hop, and the symptom would be
// routes that install and then forward into nowhere.
package wire

import (
	"strings"
	"testing"
)

func TestNormaliseTunnelEndpoints(t *testing.T) {
	in := []TunnelEndpoint{
		{PeerNodeID: 3, LocalIP: "192.168.99.13"},
		{PeerNodeID: 2, LocalIP: "192.168.99.12"},
		{PeerNodeID: 2, LocalIP: "192.168.88.12"},
		{PeerNodeID: 2, LocalIP: "192.168.88.12"}, // exact duplicate: collapsed
	}
	got, err := NormaliseTunnelEndpoints(1, in)
	if err != nil {
		t.Fatalf("NormaliseTunnelEndpoints: %v", err)
	}
	want := []TunnelEndpoint{
		{PeerNodeID: 2, LocalIP: "192.168.88.12"},
		{PeerNodeID: 2, LocalIP: "192.168.99.12"},
		{PeerNodeID: 3, LocalIP: "192.168.99.13"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// Two entries for the same peer are legal (parallel tunnels); they are kept.
	if len(got) != 3 {
		t.Fatalf("parallel tunnels collapsed: %v", got)
	}
	// The caller's slice is not reordered in place.
	if in[0].PeerNodeID != 3 || in[1].LocalIP != "192.168.99.12" {
		t.Fatalf("input was mutated: %v", in)
	}
}

func TestNormaliseTunnelEndpointsRejects(t *testing.T) {
	cases := []struct {
		name string
		self uint16
		in   []TunnelEndpoint
		want string
	}{
		{"self", 1, []TunnelEndpoint{{PeerNodeID: 1, LocalIP: "192.168.99.1"}}, "itself"},
		{"peer zero", 1, []TunnelEndpoint{{PeerNodeID: 0, LocalIP: "192.168.99.2"}}, "peer_node_id"},
		{"ipv6", 1, []TunnelEndpoint{{PeerNodeID: 2, LocalIP: "2001:db8::2"}}, "not IPv4"},
		{"garbage", 1, []TunnelEndpoint{{PeerNodeID: 2, LocalIP: "192.168.99"}}, "bad local_ip"},
		{"empty", 1, []TunnelEndpoint{{PeerNodeID: 2, LocalIP: ""}}, "bad local_ip"},
		{"unspecified", 1, []TunnelEndpoint{{PeerNodeID: 2, LocalIP: "0.0.0.0"}}, "unicast"},
		{"multicast", 1, []TunnelEndpoint{{PeerNodeID: 2, LocalIP: "224.0.0.1"}}, "unicast"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormaliseTunnelEndpoints(tc.self, tc.in)
			if err == nil {
				t.Fatalf("%v accepted", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNormaliseTunnelEndpointsTooMany(t *testing.T) {
	in := make([]TunnelEndpoint, MaxTunnelEndpoints+1)
	for i := range in {
		in[i] = TunnelEndpoint{PeerNodeID: uint16(i + 2), LocalIP: "192.168.99.2"}
	}
	if _, err := NormaliseTunnelEndpoints(1, in); err == nil {
		t.Fatal("table over the cap accepted")
	}
}
