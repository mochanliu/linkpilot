package main

import (
	"fmt"
	"net/netip"
)

// validateBasicConfig is the same rule set the page previews. It is applied
// server-side again on save; the page is a convenience, not the gate.
func validateBasicConfig(c *BasicConfig) []ConfigCheck {
	var out []ConfigCheck
	add := func(severity, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: severity, Text: fmt.Sprintf(format, args...)})
	}

	if c.Hostname == "" {
		add("warning", "未设置主机名：FRR 会把这台设备显示为默认主机名。")
	} else if !validHostname(c.Hostname) {
		add("error", "主机名只能用字母、数字和连字符，长度 1–63。")
	}

	if len(c.VPNs) > cfgMaxVPNs {
		add("error", "VPN 数量超过上限 %d。", cfgMaxVPNs)
	}
	seenVPN := map[uint32]bool{}
	for _, v := range c.VPNs {
		if seenVPN[v.ID] {
			add("error", "VPN %d 重复定义。", v.ID)
			continue
		}
		seenVPN[v.ID] = true
		if v.ID > vpnMaxID {
			add("error", "VPN %d 超出范围（0–%d）。", v.ID, vpnMaxID)
		}
		if v.ID == vpnReservedOverlay {
			add("error", "VPN 250 是 overlay 保留表，不能给客户口使用。")
		}
	}
	if !seenVPN[0] {
		add("error", "缺少 VPN 0：默认 VPN 不能被删除。")
	}
	if len(c.Ifaces) == 0 {
		add("warning", "还没有客户口：客户侧流量没有入口，BGP/OSPF 也没有可以建会话的地址。")
	}
	if len(c.Ifaces) > cfgMaxIfaces {
		add("error", "客户口数量超过上限 %d。", cfgMaxIfaces)
	}

	seenVPP := map[string]bool{}
	seenLCP := map[string]bool{}
	seenIP := map[string]bool{} // key: vpn|address
	seenSubnet := map[string]bool{}
	for _, it := range c.Ifaces {
		label := "客户口 " + it.VPP
		if it.VPP == "" {
			add("error", "存在没有 VPP 接口名的客户口。")
			label = "客户口"
		} else if !validIfaceName(it.VPP) {
			add("error", "客户口 %s 的 VPP 接口名不合法。", it.VPP)
		} else if seenVPP[it.VPP] {
			add("error", "VPP 接口 %s 重复配置。", it.VPP)
		}
		seenVPP[it.VPP] = true

		if it.LCP == "" {
			add("error", "%s 没有内核 TAP 名。", label)
		} else if !validTAPName(it.LCP) {
			add("error", "%s 的内核 TAP 名不合法（最多 15 个字符，字母数字和 -_.）。", label)
		} else if seenLCP[it.LCP] {
			add("error", "内核 TAP %s 重复配置。", it.LCP)
		}
		seenLCP[it.LCP] = true

		addr, ok := parseIPv4(it.IP)
		if !ok {
			add("error", "%s 的 IP 地址不合法。", label)
		}
		if it.PrefixLen < 8 || it.PrefixLen > 30 {
			add("error", "%s 的前缀长度需在 8–30 之间。", label)
		}
		if !seenVPN[it.VPN] {
			add("error", "%s 指向不存在的 VPN %d。", label, it.VPN)
		}
		if it.MTU < 576 || it.MTU > 9216 {
			add("error", "%s 的 MTU 需在 576–9216 之间。", label)
		}
		if ok && it.PrefixLen >= 8 && it.PrefixLen <= 30 {
			ipKey := fmt.Sprintf("%d|%s", it.VPN, addr)
			if seenIP[ipKey] {
				add("error", "VPN %d 内有两个客户口使用同一地址 %s。", it.VPN, addr)
			}
			seenIP[ipKey] = true
			p := netip.PrefixFrom(addr, int(it.PrefixLen)).Masked()
			subKey := fmt.Sprintf("%d|%s", it.VPN, p)
			if seenSubnet[subKey] {
				add("error", "VPN %d 内有两个客户口处于同一网段 %s。", it.VPN, p)
			}
			seenSubnet[subKey] = true
		}
	}

	// Overlapping (not merely equal) subnets in one VPN are also a conflict: the
	// kernel would install two connected routes with different prefix lengths
	// and FRR would have to pick one.
	for i := 0; i < len(c.Ifaces); i++ {
		a, aok := ifacePrefix(c.Ifaces[i])
		if !aok {
			continue
		}
		for j := i + 1; j < len(c.Ifaces); j++ {
			b, bok := ifacePrefix(c.Ifaces[j])
			if !bok || c.Ifaces[i].VPN != c.Ifaces[j].VPN {
				continue
			}
			if a.Overlaps(b) {
				add("error", "VPN %d 内 %s 与 %s 的网段重叠。", c.Ifaces[i].VPN, a, b)
			}
		}
	}

	if len(c.Statics) > cfgMaxStatics {
		add("error", "静态路由数量超过上限 %d。", cfgMaxStatics)
	}
	seenStatic := map[string]bool{}
	for _, r := range c.Statics {
		p, err := netip.ParsePrefix(r.Prefix)
		if err != nil || !p.Addr().Is4() || p.Bits() > 32 {
			add("error", "静态路由目的前缀「%s」不合法（应为 a.b.c.d/len）。", r.Prefix)
			continue
		}
		if p.Masked() != p {
			add("error", "静态路由 %s 不是规范网段，应写成 %s。", r.Prefix, p.Masked())
		}
		nh, ok := parseIPv4(r.NextHop)
		if !ok {
			add("error", "静态路由 %s 的下一跳不是合法 IPv4。", r.Prefix)
		}
		if !seenVPN[r.VPN] {
			add("error", "静态路由 %s 指向不存在的 VPN %d。", r.Prefix, r.VPN)
		}
		dupKey := fmt.Sprintf("%d|%s|%s", r.VPN, p, r.NextHop)
		if seenStatic[dupKey] {
			add("warning", "静态路由 %s via %s 在 VPN %d 内重复。", r.Prefix, r.NextHop, r.VPN)
		}
		seenStatic[dupKey] = true

		direct := false
		for _, cp := range vpnPrefixes(c, r.VPN) {
			if ok && cp.Contains(nh) {
				direct = true
				break
			}
		}
		if ok && !direct {
			add("warning", "静态路由 %s 的下一跳 %s 不在 VPN %d 的任何客户口直连网段内，这条路由需要别的路由先解析它。", r.Prefix, r.NextHop, r.VPN)
		}
		for _, cp := range vpnPrefixes(c, r.VPN) {
			if cp.Overlaps(p) {
				add("warning", "静态路由 %s 与 VPN %d 的直连网段 %s 重叠，直连路由优先。", r.Prefix, r.VPN, cp)
				break
			}
		}
	}

	// VRRP: one virtual router per row, keyed by (VPP port, vr_id). The rules
	// here are the ones a box cannot notice for itself -- a VR hung on a port it
	// does not own, or a gateway address outside the subnet it is supposed to be
	// the gateway of, is configured happily and then never answers an ARP.
	if len(c.VRRP) > cfgMaxVRRP {
		add("error", "VRRP 条目数量超过上限 %d。", cfgMaxVRRP)
	}
	seenVR := map[string]bool{}
	vipOwner := map[string]string{}
	for _, v := range c.VRRP {
		label := fmt.Sprintf("VRRP %s vr_id=%d", v.VPP, v.VRID)
		if v.VPP == "" {
			add("error", "存在没有 VPP 接口名的 VRRP 条目。")
			label = "VRRP"
		}
		if v.VRID < vrrpMinVRID || v.VRID > vrrpMaxVRID {
			add("error", "%s 的 vr_id 需在 %d–%d 之间。", label, vrrpMinVRID, vrrpMaxVRID)
		}
		if v.Priority < vrrpMinPriority || v.Priority > vrrpMaxPriority {
			add("error", "%s 的 priority 需在 %d–%d 之间：0 是备机停发通告时用的，255 留给真正持有这个地址的路由器。",
				label, vrrpMinPriority, vrrpMaxPriority)
		}
		if v.IntervalMS != 0 {
			// A row that reaches here with interval 0 was never normalized; an
			// empty field on the page is filled with vrrpDefaultIntervalMS before
			// this function runs, because VPP refuses 0 outright (retval=-7).
			switch {
			case v.IntervalMS < vrrpMinInterval || v.IntervalMS > vrrpMaxInterval:
				add("error", "%s 的通告间隔需在 %d–%d ms 之间（留空按 %d ms 处理）。",
					label, vrrpMinInterval, vrrpMaxInterval, vrrpDefaultIntervalMS)
			case v.IntervalMS%10 != 0:
				// VPP stores this as a u16 count of centiseconds, so a value that
				// is not a whole centisecond would be silently rounded.
				add("error", "%s 的通告间隔只能到 10 ms 一档，%d 会被 VPP 取整。", label, v.IntervalMS)
			}
		}

		vip, vipOK := parseIPv4(v.VirtualAddr)
		if !vipOK {
			add("error", "%s 的虚拟地址「%s」不是合法 IPv4 地址。", label, v.VirtualAddr)
		} else if owner, dup := vipOwner[v.VirtualAddr]; dup {
			add("error", "虚拟地址 %s 同时属于 %s 和 %s。", v.VirtualAddr, owner, label)
		} else {
			vipOwner[v.VirtualAddr] = label
		}

		it, ok := ifaceByVPP(c, v.VPP)
		if v.VPP != "" && !ok {
			add("error", "%s 挂的客户口 %s 不在这台设备的接口列表里：VRRP 只能跑在基础配置已经配好的客户口上。", label, v.VPP)
			continue
		}
		vrKey := fmt.Sprintf("%s|%d", it.VPP, v.VRID)
		if seenVR[vrKey] {
			add("error", "%s 重复定义。", label)
		}
		seenVR[vrKey] = true
		if !it.Up {
			add("warning", "%s 挂的客户口 %s 没有置为 up，VPP 不会在 down 的口上发 VRRP 通告。", label, it.VPP)
		}

		p, pOK := ifacePrefix(it)
		if !pOK {
			add("warning", "%s 挂的客户口 %s 还没有合法的地址与掩码，无法判断虚拟地址落在不在它的网段里。", label, it.VPP)
			continue
		}
		if !vipOK {
			continue
		}
		if vip.String() == it.IP {
			add("error", "%s 的虚拟地址等于客户口 %s 自己的地址 %s：主备两台会把网关和自己的地址搞混。", label, it.VPP, it.IP)
			continue
		}
		if !p.Contains(vip) {
			add("error", "%s 的虚拟地址 %s 不在客户口 %s 的直连网段 %s 内，客户侧主机不会把它当网关。", label, vip, it.VPP, p)
			continue
		}
		network, broadcast := subnetBounds(p)
		if vip == network || vip == broadcast {
			which := "广播"
			if vip == network {
				which = "网络"
			}
			add("error", "%s 的虚拟地址 %s 是网段 %s 的%s地址，不能当网关用。", label, vip, p, which)
		}
		// A second port in the same VPN already owns this address: two gateways
		// for one broadcast domain, and the ARP answer becomes a race.
		for _, other := range c.Ifaces {
			if other.VPP == it.VPP || other.VPN != it.VPN {
				continue
			}
			if o, ok := parseIPv4(other.IP); ok && o == vip {
				add("error", "%s 的虚拟地址 %s 已被同一 VPN %d 的客户口 %s 占用。", label, vip, it.VPN, other.VPP)
			}
		}
	}

	type ridUse struct {
		vpn   uint32
		owner string
	}
	ridOwner := map[string]ridUse{}
	checkRID := func(rid, owner string, vpn uint32) {
		if rid == "" {
			return
		}
		if prev, ok := ridOwner[rid]; ok {
			// Sharing a router-id between BGP and OSPF on the SAME device is
			// normal (both are usually derived from the same loopback). Sharing
			// it across VRFs is the mistake worth flagging.
			if prev.vpn != vpn {
				add("warning", "router-id %s 同时用于 %s 和 %s。", rid, prev.owner, owner)
			}
			return
		}
		ridOwner[rid] = ridUse{vpn: vpn, owner: owner}
	}
	for _, v := range c.VPNs {
		vpnLabel := fmt.Sprintf("VPN %d", v.ID)
		if v.BGP.On {
			if v.BGP.ASN < 1 || v.BGP.ASN > 65535 {
				add("error", "%s 的 BGP 本地 AS 需在 1–65535 之间。", vpnLabel)
			}
			if _, ok := parseIPv4(v.BGP.RouterID); !ok {
				add("error", "%s 的 BGP router-id 需要是合法 IPv4。", vpnLabel)
			}
			if len(v.BGP.Neighbors) == 0 {
				add("warning", "%s 已启用 BGP，但还没有邻居。", vpnLabel)
			}
			if len(v.BGP.Neighbors) > cfgMaxNeighbors {
				add("error", "%s 的 BGP 邻居数量超过上限 %d。", vpnLabel, cfgMaxNeighbors)
			}
			if len(vpnPrefixes(c, v.ID)) == 0 {
				add("warning", "%s 启用了 BGP，但这个 VPN 没有客户口。", vpnLabel)
			}
			seenPeer := map[string]bool{}
			for _, n := range v.BGP.Neighbors {
				addr, ok := parseIPv4(n.IP)
				if !ok {
					add("error", "%s 的 BGP 邻居地址「%s」不合法。", vpnLabel, n.IP)
					continue
				}
				if seenPeer[n.IP] {
					add("error", "%s 的 BGP 邻居 %s 重复。", vpnLabel, n.IP)
				}
				seenPeer[n.IP] = true
				if n.RemoteAS < 1 || n.RemoteAS > 65535 {
					add("error", "邻居 %s 的对端 AS 需在 1–65535 之间。", n.IP)
				}
				if !validDescription(n.Description) {
					add("error", "邻居 %s 的描述不能包含控制字符，最多 64 字符。", n.IP)
				}

				direct := false
				for _, cp := range vpnPrefixes(c, v.ID) {
					if cp.Contains(addr) {
						direct = true
						break
					}
				}
				if n.RemoteAS != v.BGP.ASN {
					// eBGP: the first packet goes to the neighbor with TTL 1, so
					// a neighbor that is not on a directly connected subnet can
					// never establish.
					if !direct {
						add("error", "eBGP 邻居 %s 不在 VPN %d 的任何客户口直连网段内，会话建立不起来。", n.IP, v.ID)
					}
				} else {
					// iBGP is multi-hop by design. It only needs SOME route to
					// the neighbor; a static route in this VPN is the usual way
					// on a CPE, but OSPF can supply one too, so this is a
					// warning rather than an error.
					if !direct && !staticCovers(c, v.ID, addr) {
						add("warning", "iBGP 邻居 %s 不直连，且 VPN %d 内没有覆盖它的静态路由；请确认设备上有到达它的路由。", n.IP, v.ID)
					}
					if !n.NextHopSelf {
						add("warning", "iBGP 邻居 %s 未启用 next-hop-self：客户路由器从这台盒子学到的前缀，下一跳可能不可达。", n.IP)
					}
				}
			}
			checkRID(v.BGP.RouterID, fmt.Sprintf("VPN %d BGP", v.ID), v.ID)
		}

		if v.OSPF.On {
			if _, ok := parseIPv4(v.OSPF.RouterID); !ok {
				add("error", "%s 的 OSPF router-id 需要是合法 IPv4。", vpnLabel)
			}
			if len(vpnPrefixes(c, v.ID)) == 0 {
				add("warning", "%s 启用了 OSPF，但这个 VPN 没有客户口可以宣告。", vpnLabel)
			}
			checkRID(v.OSPF.RouterID, fmt.Sprintf("VPN %d OSPF", v.ID), v.ID)
		}
	}

	if len(out) == 0 {
		add("info", "没有发现冲突：接口、静态路由与动态路由会话一致。")
	}
	// QoS（§13）：出方向物理口一条规则一个 shaper，重复配置不是"覆盖"而是
	// 渲染出两条 rate 命令，最后一次生效——这是配置漂移的来源，直接拒绝。
	seenQosEgress := map[string]bool{}
	for i, q := range c.Qos {
		n := i + 1
		if !validIfaceName(q.Ingress) {
			add("error", "QoS 第 %d 条：入接口名 %q 非法。", n, q.Ingress)
		} else {
			found := false
			for _, it := range c.Ifaces {
				if it.VPP == q.Ingress {
					found = true
					break
				}
			}
			if !found {
				add("warning", "QoS 第 %d 条：入接口 %s 不在本节点客户口里，标记器会空挂。", n, q.Ingress)
			}
		}
		if !validIfaceName(q.Egress) {
			add("error", "QoS 第 %d 条：出方向物理口名 %q 非法。", n, q.Egress)
		} else if seenQosEgress[q.Egress] {
			add("error", "QoS 出方向物理口 %s 重复配置。", q.Egress)
		} else {
			seenQosEgress[q.Egress] = true
		}
		if q.RateKbps == 0 {
			add("error", "QoS 第 %d 条：速率 0 等于直通，删掉这条规则即可，不要配 0。", n)
		} else if q.RateKbps > qosMaxRateKbps {
			add("error", "QoS 第 %d 条：速率 %d 超过插件上限 %d kbps。", n, q.RateKbps, qosMaxRateKbps)
		}
		if q.LLQPct > 100 {
			add("error", "QoS 第 %d 条：LLQ %% 不能超过 100。", n)
		}
	}
	return out
}
