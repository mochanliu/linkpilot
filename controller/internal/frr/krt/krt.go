// Package krt 把前缀直接写进 Linux 内核路由表（rtnetlink），不经过 zebra。
//
// 这是 "SDWAN → 客户" 的落地点。为什么不是 ZAPI 的非主表：那条路要走
// zebra → 内核表 N → linux-nl 镜像成 VPP table N，而 VPP 里那张表全是
// dpo-drop，对转发毫无用处。直接写内核以后，这些前缀只停在纯内核，VPP 完全
// 看不到它们，客户侧的 BGP/OSPF 再把它们当事实发给客户路由器。
//
// 三个实测结论（2026-11-02，lab-cpe1，见 docs/o6e-leak-plan.md §1）决定了
// 这里每一个参数：
//
//   - 出口是 lo（根命名空间）或 vpn<N>（VRF 设备本身）时，linux-nl **不镜像**
//     进 VPP；出口是 lcp 配对口或 blackhole 时才会镜像。所以注入只用这两种
//     出口，绝不带黑洞下一跳。
//   - 内核协议号用 RTPROT_BOOT（`ip route add` 的默认值），zebra 把它认成 K，
//     distance 0，同前缀会压过 BGP(20)/OSPF(110) 的本地路由。这是"K 覆盖"
//     这条拍板的实现。
//   - 删一条内核里已经不存在路由返回 ESRCH（容器换了 netns 时整张表本来就
//     没了），按成功算，不让一次重启卡住对账。
//
// 已知代价：内核认为这些前缀挂在 lo / vpn<N> 上。盒子 ip_forward=0、没有
// 默认路由，也不主动访问客户前缀，所以不影响转发；只有"盒子自己发起去这些
// 前缀的流量"会落到 lo —— 这个机制的边界。
package krt

import (
	"fmt"
	"net"
)

// Index 返回 VPN 的注入出口 ifindex：VPN 0 用 lo，其余用 boot 配置里建的
// vpn<N> 设备。设备不存在就是配置错误（不猜、不退回主表）。
func Index(vpn uint32) (int, error) {
	if vpn > 255 {
		return 0, fmt.Errorf("krt: VPN %d 超出 0..255", vpn)
	}
	name := "lo"
	if vpn != 0 {
		name = fmt.Sprintf("vpn%d", vpn)
	}
	return LinkIndex(name)
}

// LinkIndex 返回内核接口的 ifindex。导入侧要的是 VRF 设备本身的 ifindex
// （FRR 的 vrf_id 就是它），与导出侧用什么出口无关。
func LinkIndex(name string) (int, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0, fmt.Errorf("krt: 找接口 %s: %w", name, err)
	}
	return ifi.Index, nil
}

// ImportIndex 返回 VPN 在 ZAPI 里用的 vrf_id：VPN 0 是默认 VRF（0），其余是
// vpn<N> 设备的 ifindex。设备不存在就是配置错误 —— 订阅一个不存在的 VRF
// 只会让 zebra 回 ZEBRA_NO_VRF，或者干脆一条通知都不来。
func ImportIndex(vpn uint8) (uint32, error) {
	if vpn == 0 {
		return 0, nil
	}
	i, err := LinkIndex(fmt.Sprintf("vpn%d", vpn))
	if err != nil {
		return 0, err
	}
	return uint32(i), nil
}

// LinkEvent 是内核里会让 zebra 清掉自己 K 路由视图的那几类事件（实测见
// netlink_linux.go 的 WatchLinks）。Kind 只用于日志。
type LinkEvent struct {
	Name string
	Kind string
}

const (
	LinkDeleted = "del"
	LinkDown    = "down"
	AddrDeleted = "addr-del"
)
