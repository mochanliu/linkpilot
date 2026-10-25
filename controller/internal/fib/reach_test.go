package fib

import (
	"net"
	"net/netip"
	"testing"

	"sdwan/controller/internal/vppapi"
)

func bfdSession(local, peer string, state, swIf uint32) vppapi.BFDSession {
	return vppapi.BFDSession{
		SwIfIndex: swIf,
		LocalIP:   net.ParseIP(local).To4(),
		PeerIP:    net.ParseIP(peer).To4(),
		State:     state,
	}
}

func TestUnreachablePeers(t *testing.T) {
	down := UnreachablePeers([]vppapi.BFDSession{
		bfdSession("10.255.0.1", "10.255.0.2", vppapi.BFDStateUp, vppapi.BFDMultihopSwIfIndex),
		bfdSession("10.255.0.1", "10.255.0.3", vppapi.BFDStateDown, vppapi.BFDMultihopSwIfIndex),
		bfdSession("10.255.0.1", "10.255.0.4", vppapi.BFDStateInit, vppapi.BFDMultihopSwIfIndex),
		bfdSession("10.255.0.1", "10.255.0.5", vppapi.BFDStateAdminDown, vppapi.BFDMultihopSwIfIndex),
		// 绑在具体接口上的会话不是隧道探测，状态再差也不该影响路由。
		bfdSession("10.255.0.1", "10.255.0.6", vppapi.BFDStateDown, 7),
	})
	want := []string{"10.255.0.3", "10.255.0.4", "10.255.0.5"}
	if len(down) != len(want) {
		t.Fatalf("down = %v, want 恰好 %v", down, want)
	}
	for _, p := range want {
		if !down[netip.MustParseAddr(p)] {
			t.Errorf("%s 应判为不可达", p)
		}
	}
	if down[netip.MustParseAddr("10.255.0.2")] {
		t.Error("Up 的会话被判成不可达")
	}
}

func TestUnreachablePeersAnyUpWins(t *testing.T) {
	// 同一个对端、两个本地 overlay 源：一条 Up 就说明对端活着。
	down := UnreachablePeers([]vppapi.BFDSession{
		bfdSession("10.255.0.1", "10.255.0.2", vppapi.BFDStateDown, vppapi.BFDMultihopSwIfIndex),
		bfdSession("10.255.0.9", "10.255.0.2", vppapi.BFDStateUp, vppapi.BFDMultihopSwIfIndex),
	})
	if down[netip.MustParseAddr("10.255.0.2")] {
		t.Error("有一条 Up 仍被判为不可达")
	}
}

func TestUnreachablePeersNoSessionsIsNoInformation(t *testing.T) {
	// 没跑 BFD 的 CPE：结果必须为空，否则它的所有路由都会被黑洞。
	if down := UnreachablePeers(nil); len(down) != 0 {
		t.Errorf("没有会话时应为空，got %v", down)
	}
}

func TestUnreachablePeersIgnoresIPv6(t *testing.T) {
	down := UnreachablePeers([]vppapi.BFDSession{{
		SwIfIndex: vppapi.BFDMultihopSwIfIndex,
		LocalIP:   net.ParseIP("fd00::1"),
		PeerIP:    net.ParseIP("fd00::2"),
		State:     vppapi.BFDStateDown,
	}})
	if len(down) != 0 {
		t.Errorf("IPv6 会话不该出现在 IPv4 路由的判据里: %v", down)
	}
}
