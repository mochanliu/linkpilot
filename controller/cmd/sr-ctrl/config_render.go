package main

import (
	"fmt"
	"sort"
	"strings"
)

func vrfName(vpn uint32) string { return fmt.Sprintf("vpn%d", vpn) }

// renderFRR emits the customer-side FRR config. It deliberately does not emit
// the overlay pieces (tag 1001 route-map, redistribute table-direct 1250);
// those belong to the overlay layer and are marked as comments so a reader of
// the artifact can see where they would go.
func renderFRR(c *BasicConfig) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}

	line("! 由控制器生成：基础配置（客户侧）")
	line("! 接口地址与静态路由由 zebra/staticd 下发，linux-cp 会同步进 VPP")
	line("frr defaults traditional")
	if c.Hostname != "" {
		line("hostname %s", c.Hostname)
	}
	line("log file /var/log/frr/frr.log")
	line("service integrated-vtysh-config")
	line("!")

	if len(c.Ifaces) > 0 {
		line("! 客户口地址：写在内核 TAP 上，lcp-sync 复制进 VPP 同一张表")
		for _, it := range c.Ifaces {
			if it.LCP == "" {
				continue
			}
			if it.VPN == 0 {
				line("interface %s", it.LCP)
			} else {
				line("interface %s vrf %s", it.LCP, vrfName(it.VPN))
			}
			if it.IP != "" && it.PrefixLen > 0 {
				line(" ip address %s/%d", it.IP, it.PrefixLen)
			}
			line("!")
		}
	}

	// Static routes: VPN 0 at top level, every other VPN in its vrf block.
	if len(c.Statics) > 0 {
		byVPN := map[uint32][]ConfigStatic{}
		var order []uint32
		for _, r := range c.Statics {
			if _, ok := byVPN[r.VPN]; !ok {
				order = append(order, r.VPN)
			}
			byVPN[r.VPN] = append(byVPN[r.VPN], r)
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		line("! 静态路由：VPN 0 写主表，其余写各自 vrf 段")
		for _, vpn := range order {
			if vpn != 0 {
				line("vrf %s", vrfName(vpn))
			}
			for _, r := range byVPN[vpn] {
				if vpn == 0 {
					line("ip route %s %s", r.Prefix, r.NextHop)
				} else {
					line(" ip route %s %s", r.Prefix, r.NextHop)
				}
			}
			if vpn != 0 {
				line("exit-vrf")
			}
			line("!")
		}
	}

	for _, v := range c.VPNs {
		if !v.BGP.On {
			continue
		}
		if v.ID == 0 {
			line("router bgp %d", v.BGP.ASN)
		} else {
			line("router bgp %d vrf %s", v.BGP.ASN, vrfName(v.ID))
		}
		if v.BGP.RouterID != "" {
			line(" bgp router-id %s", v.BGP.RouterID)
		}
		line(" no bgp ebgp-requires-policy")
		for _, n := range v.BGP.Neighbors {
			line(" neighbor %s remote-as %d", n.IP, n.RemoteAS)
			if n.Description != "" {
				line(" neighbor %s description %s", n.IP, n.Description)
			}
		}
		line(" address-family ipv4 unicast")
		line("  ! overlay 层生成，本页不编辑：redistribute table-direct 1250 / CUST-IN tag 1001")
		for _, n := range v.BGP.Neighbors {
			if n.RemoteAS == v.BGP.ASN && n.NextHopSelf {
				line("  neighbor %s next-hop-self", n.IP)
			}
		}
		line(" exit-address-family")
		line("!")
	}

	for _, v := range c.VPNs {
		if !v.OSPF.On {
			continue
		}
		if v.ID == 0 {
			line("router ospf")
		} else {
			line("router ospf vrf %s", vrfName(v.ID))
		}
		if v.OSPF.RouterID != "" {
			line(" ospf router-id %s", v.OSPF.RouterID)
		}
		for _, p := range vpnPrefixes(c, v.ID) {
			line(" network %s area %d", p, v.OSPF.Area)
		}
		line("!")
	}

	return b.String()
}

// renderVPP emits only what FRR/lcp cannot own. Note what is absent: no
// `set interface ip address`. That address is FRR's, and lcp-sync mirrors it
// back into VPP (measured on lab-cpe1 in both directions).
func renderVPP(c *BasicConfig) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}

	line("# 由控制器生成：VPP 侧只剩接口/表/MTU/lcp")
	line("# 地址与静态路由由 FRR 下发，lcp-sync 同步进 VPP；这里不能重复配地址")
	line("# 顺序：先建表 -> 移接口进表 -> 设 MTU/状态 -> 建 lcp TAP")
	if len(c.Ifaces) == 0 {
		line("# 还没有客户口")
	}

	var tables []uint32
	seen := map[uint32]bool{}
	for _, it := range c.Ifaces {
		if it.VPN != 0 && !seen[it.VPN] {
			seen[it.VPN] = true
			tables = append(tables, it.VPN)
		}
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i] < tables[j] })
	for _, vpn := range tables {
		line("ip table add %d", vpn)
	}
	if len(tables) > 0 {
		line("")
	}

	for _, it := range c.Ifaces {
		line("# 客户口 %s -> 内核 %s", it.VPP, it.LCP)
		if it.VPN != 0 {
			line("set interface ip table %s %d", it.VPP, it.VPN)
		}
		line("set interface mtu packet %d %s", it.MTU, it.VPP)
		if it.Up {
			line("set interface state %s up", it.VPP)
		} else {
			line("set interface state %s down", it.VPP)
		}
		line("lcp create %s host-if %s", it.VPP, it.LCP)
		line("")
	}
	if len(c.Ifaces) > 0 {
		line("lcp lcp-sync on")
	}
	return b.String()
}

// renderBoot emits the kernel side and the ordering the on-box applier must
// follow. This is the fragment that makes "VPP config survives a restart"
// true: VPP itself does not persist any of it.
func renderBoot(c *BasicConfig) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}

	line("#!/bin/sh")
	line("# 由控制器生成：在 VPP 片段执行完、lcp TAP 出现之后运行")
	line("# 盒子上的 applier 必须持久化本文件并在开机时重放，不能依赖控制器重推")
	line("set -e")
	line("")

	var vpns []uint32
	seen := map[uint32]bool{}
	for _, it := range c.Ifaces {
		if it.VPN != 0 && !seen[it.VPN] {
			seen[it.VPN] = true
			vpns = append(vpns, it.VPN)
		}
	}
	for _, v := range c.VPNs {
		if v.ID != 0 && !seen[v.ID] {
			seen[v.ID] = true
			vpns = append(vpns, v.ID)
		}
	}
	sort.Slice(vpns, func(i, j int) bool { return vpns[i] < vpns[j] })
	if len(vpns) > 0 {
		line("# 内核 VRF：每个非 0 VPN 一个；已存在则复用，内核不支持会立刻失败")
		for _, vpn := range vpns {
			name := vrfName(vpn)
			line("ip link show %s >/dev/null 2>&1 || ip link add %s type vrf table %d", name, name, vpn)
		}
		for _, it := range c.Ifaces {
			if it.VPN != 0 && it.LCP != "" {
				line("ip link set %s master %s", it.LCP, vrfName(it.VPN))
			}
		}
		line("")
	}

	line("# 内核只做控制面，转发全部在 VPP")
	line("sysctl -w net.ipv4.ip_forward=0")
	for _, it := range c.Ifaces {
		if it.LCP == "" {
			continue
		}
		if it.ArpIgnore {
			line("sysctl -w net.ipv4.conf.%s.arp_ignore=1", it.LCP)
		} else {
			line("sysctl -w net.ipv4.conf.%s.arp_ignore=0", it.LCP)
		}
		line("sysctl -w net.ipv4.conf.%s.rp_filter=1", it.LCP)
	}
	line("")
	line("# 最后 reload FRR：地址由 zebra 下发，lcp-sync 再同步回 VPP")
	return b.String()
}

type configArtifacts struct {
	FRR  string `json:"frr"`
	VPP  string `json:"vpp"`
	Boot string `json:"boot"`
}

func renderArtifacts(c *BasicConfig) configArtifacts {
	return configArtifacts{FRR: renderFRR(c), VPP: renderVPP(c), Boot: renderBoot(c)}
}
