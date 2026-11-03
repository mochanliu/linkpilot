package main

import (
	"context"
	"net"
	"testing"

	"sdwan/controller/internal/agent"
	"sdwan/controller/internal/stun"
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

// 状态页上的三种状态必须分得开：控制器没给反射器、给了但这一台没探到、探到了。
// 这正是控制台"未上报"的出处 —— AGENTS.md 第 2 条不许显示 0 或占位值，靠的就是
// 这里三个字段各自为空/非空的组合。
func TestStatusSeparatesUnadvertisedFromUnprobed(t *testing.T) {
	st := &agentStatus{}
	none := st.snapshot().Stun
	if len(none.Reflectors) != 0 || none.Mapping != "" || none.Err != "" {
		t.Errorf("没开探测时状态页却有内容：%+v", none)
	}

	ref := startTestReflector(t)
	okSync := agent.NewStunSync("127.0.0.1:0", 2, nil)
	defer okSync.Close()
	st.setStun(okSync)
	okSync.SetReflectors([]string{ref})
	okSync.Sync(context.Background())
	got := st.snapshot().Stun
	if got.Mapping == "" || got.ProbeLocal == "" || len(got.Reflectors) != 1 {
		t.Errorf("探到了却没写进状态页：%+v", got)
	}
	if got.Err != "" || got.AgeSec < 0 {
		t.Errorf("成功的一轮里带了错误：%+v", got)
	}

	// 探不到的一台：只能有原因，不许有地址。页面据此写"未上报（附原因）"。
	bad := agent.NewStunSync("256.256.256.256:0", 1, nil)
	defer bad.Close()
	st.setStun(bad)
	bad.SetReflectors([]string{ref})
	bad.Sync(context.Background())
	b := st.snapshot().Stun
	if b.Mapping != "" || b.ProbeLocal != "" {
		t.Errorf("探测失败却还留着地址：%+v", b)
	}
	if b.Err == "" {
		t.Errorf("探测失败却没留下原因：%+v（页面会显示成一切正常）", b)
	}
}

// startTestReflector 起一个只用来回答的内核 UDP 反射器，返回它的地址。
// 测试要的是"有人应答"这件事，所以不复用产品的 Reflector：那样一旦反射器自己
// 坏了，这条测试就分不清是设备的错还是应答器的错。
func startTestReflector(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起反射器：%v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if !stun.IsBindingRequest(buf[:n]) {
				continue
			}
			udp, ok := from.(*net.UDPAddr)
			if !ok || udp.IP.To4() == nil {
				continue
			}
			tx := make([]byte, 12)
			copy(tx, buf[8:20])
			resp, err := stun.Response(tx, udp.IP.To4().String(), udp.Port)
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(resp, from)
		}
	}()
	return conn.LocalAddr().String()
}
