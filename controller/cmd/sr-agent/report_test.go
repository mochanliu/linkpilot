package main

import (
	"context"
	"testing"

	"sdwan/controller/internal/agent"
	"sdwan/controller/internal/wire"
)

// The controller keys its source-attribution table by bare address, so a prefix
// length or a link-local entry would simply never match an observation.
func TestLocalAddrsDropsWhatCannotBeAnOuterSource(t *testing.T) {
	ifaces := []wire.IfaceInfo{
		{Name: "eth0", Addrs: []string{"192.0.2.5/24", "fe80::1/64", "198.51.100.7/32"}},
		{Name: "eth1", Addrs: []string{"2001:db8::1/64"}},
		{Name: "eth2"},
	}
	got := localAddrs(&ifaces)
	want := []string{"192.0.2.5", "198.51.100.7"}
	if len(got) != len(want) {
		t.Fatalf("localAddrs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("localAddrs = %v, want %v", got, want)
		}
	}

	// nil is "not reported", and an agent that could not read the inventory must
	// not send an empty list -- that would mean "this box owns no addresses".
	if got := localAddrs(nil); got != nil {
		t.Fatalf("没有清单时应报 nil，得到 %v", got)
	}
	empty := []wire.IfaceInfo{}
	if got := localAddrs(&empty); len(got) != 0 {
		t.Fatalf("空清单里没有可用地址，得到 %v", got)
	}
}

func TestIfaceNamesMapsSWIfIndex(t *testing.T) {
	ifaces := []wire.IfaceInfo{{Name: "host-eth0", SWIfIndex: 3}, {Name: "wg1001", SWIfIndex: 9}}
	names := ifaceNames(&ifaces)
	if names[3] != "host-eth0" || names[9] != "wg1001" {
		t.Fatalf("ifaceNames = %v", names)
	}
	if len(ifaceNames(nil)) != 0 {
		t.Fatalf("没有清单时应当是空表")
	}
}

// Three states must stay distinguishable on the wire: no dataplane at all,
// channel present but switched off, and channel running.
func TestTunnelReportDistinguishesAbsentFromSwitchedOff(t *testing.T) {
	st := &agentStatus{}
	if got := st.tunnelReport(); got != nil {
		t.Fatalf("没有 -vpp-api 时应报 nil（旧 agent 语义），得到 %+v", got)
	}

	st.setChannels(nil, true)
	if got := st.tunnelReport(); got == nil || got.Capable {
		t.Fatalf("有 socket 但没开 -tunnels 时报 capable:false，得到 %+v", got)
	}

	st.setChannels(agent.NewTunnelSync(nil, nil, nil, nil), true)
	if got := st.tunnelReport(); got == nil || !got.Capable {
		t.Fatalf("-tunnels 打开后应当报 capable:true，得到 %+v", got)
	}
}

func TestDataPlaneDescNeverClaimsMoreThanItHas(t *testing.T) {
	st := &agentStatus{}
	none := st.dataPlaneDesc()
	st.setChannels(nil, true)
	inv := st.dataPlaneDesc()
	st.setChannels(agent.NewTunnelSync(nil, nil, nil, nil), true)
	tun := st.dataPlaneDesc()
	if none == inv || inv == tun || none == tun {
		t.Fatalf("三种数据面能力应当各不相同: %q / %q / %q", none, inv, tun)
	}
}

// Without a tunnel applier there is no connection to read the observation table
// over, and reporting "no observations" would be a claim this box cannot make.
func TestReportSenderWithoutDataPlaneDoesNotPanic(t *testing.T) {
	r := &reportSender{}
	r.send(context.Background(), nil)
	r.reportObservations(context.Background(), nil)
}
