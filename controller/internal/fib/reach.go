package fib

import (
	"net/netip"

	"sdwan/controller/internal/vppapi"
)

// UnreachablePeers 从 VPP 的 BFD 会话表里挑出"明确不可达"的对端。
//
// 规则，以及每条规则为什么这么定：
//
//   - 只看多跳会话（隧道内探测一律 multihop，绑接口的会话不是隧道的）。
//   - 只有 Up 算可达。Init 还没验证通过，把流量送进去和送进黑洞没区别；
//     Down/AdminDown 同理。
//   - 同一个对端有多条会话（本机多个 overlay 源）时，任意一条 Up 就算可达：
//     路由只带对端地址，没有本地源，挑不了更细。
//   - **没有会话的对端不出现在结果里**。这不是"可达"，而是"没有信息"：
//     没跑 BFD 的 CPE 不该因为本包而黑洞所有路由。
func UnreachablePeers(sessions []vppapi.BFDSession) map[netip.Addr]bool {
	known := map[netip.Addr]bool{}
	up := map[netip.Addr]bool{}
	for _, s := range sessions {
		if s.SwIfIndex != vppapi.BFDMultihopSwIfIndex {
			continue
		}
		peer, ok := netip.AddrFromSlice(s.PeerIP)
		if !ok {
			continue
		}
		peer = peer.Unmap()
		if !peer.Is4() {
			continue
		}
		known[peer] = true
		if s.State == vppapi.BFDStateUp {
			up[peer] = true
		}
	}
	down := make(map[netip.Addr]bool, len(known)-len(up))
	for peer := range known {
		if !up[peer] {
			down[peer] = true
		}
	}
	return down
}
