package main

import (
	"encoding/json"
	"testing"

	"sdwan/controller/internal/wire"
)

func TestRRFlagParsing(t *testing.T) {
	var l rrAddresses
	if err := l.Set("rr-a=10.0.0.1:7946"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set("rr-b=[fd00::2]:7946"); err != nil {
		t.Fatal(err)
	}
	if got := l.String(); got != "rr-a=10.0.0.1:7946,rr-b=[fd00::2]:7946" {
		t.Fatalf("String() = %q", got)
	}
	if l[1].ID != "rr-b" || l[1].Addr != "[fd00::2]:7946" {
		t.Fatalf("second entry = %+v", l[1])
	}

	for _, bad := range []string{
		"rr-a",           // no '='
		"=10.0.0.1:7946", // empty id
		"rr-a=",          // empty address
		"rr-a=10.0.0.1",  // no port
		"rr-a=nonsense",  // not host:port
	} {
		var x rrAddresses
		if err := x.Set(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}

	// Duplicates are ambiguous: the order IS the failover preference.
	var dup rrAddresses
	if err := dup.Set("rr-a=10.0.0.1:7946"); err != nil {
		t.Fatal(err)
	}
	if err := dup.Set("rr-a=10.0.0.2:7946"); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if err := dup.Set("rr-b=10.0.0.1:7946"); err == nil {
		t.Fatal("duplicate address accepted")
	}
}

func TestEnrollResponseCarriesRRsInOrder(t *testing.T) {
	s := newTestServer()
	s.rrs = []wire.RRAddress{
		{ID: "rr-a", Addr: "10.0.0.1:7946"},
		{ID: "rr-b", Addr: "10.0.0.2:7946"},
	}
	rec := &NodeRecord{TenantID: "t", NodeID: 7}
	resp := s.enrollResponse("t", rec, "cert-pem", "ca-pem")

	if len(resp.RRs) != 2 || resp.RRs[0].ID != "rr-a" || resp.RRs[1].Addr != "10.0.0.2:7946" {
		t.Fatalf("rrs = %+v, want the configured order", resp.RRs)
	}
	// The wire contract names them `rrs`: the agent must not have to guess a
	// spelling, and a renamed field silently drops the whole list.
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var onWire map[string]any
	if err := json.Unmarshal(b, &onWire); err != nil {
		t.Fatal(err)
	}
	list, ok := onWire["rrs"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("json rrs = %v", onWire["rrs"])
	}
	if resp.NodeID != 7 || resp.TenantID != "t" || resp.VpnPort != 51820 {
		t.Fatalf("response lost its other fields: %+v", resp)
	}
}
