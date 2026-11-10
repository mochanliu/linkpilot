package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func vrfName(vpn uint32) string { return fmt.Sprintf("vpn%d", vpn) }

// sdwan2cpePolicyName is derived from the VPN id because FRR keeps route-map
// and prefix-list names global to the daemon, not per VRF (docs/o6e-leak-plan.md
// §2.2). Reusing one name on the second VPN would overwrite the first.
func sdwan2cpePolicyName(vpn uint32) string { return fmt.Sprintf("SDWAN2CPE-vpn%d", vpn) }

// leakRenderRule is one SDWAN-CPE row with its prefix-list already expanded.
// The entries are frozen at publish time: editing the library afterwards does
// not change what a box is running until it is published again.
type leakRenderRule struct {
	VPN       uint32
	Protocols []string
	Entries   []PrefixEntry
	HasList   bool
}

func (r leakRenderRule) hasProtocol(p string) bool {
	for _, got := range r.Protocols {
		if got == p {
			return true
		}
	}
	return false
}

// renderFRR emits the customer-side FRR config. Overlay pieces (tag 1001
// route-map, redistribute table-direct 1250) are gone: SDWAN-CPE now injects K
// routes into the VPN's own kernel table and redistributes `kernel` here.
// toCPE carries the expanded SDWAN-CPE rows; passing nil emits none of their
// lines.
func renderFRR(c *BasicConfig, toCPE []leakRenderRule) string {
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

	// SDWAN-CPE: the prefix-lists and route-maps come first, then each selected
	// protocol instance redistributes `kernel`. An empty list means "全放过"
	// (docs/o6e-leak-plan.md §3.1), so it must NOT get a match line -- an empty
	// FRR prefix-list matches nothing and would silently deny every prefix.
	toCPEByVPN := map[uint32]leakRenderRule{}
	for _, r := range toCPE {
		toCPEByVPN[r.VPN] = r
	}
	if len(toCPE) > 0 {
		line("! SDWAN-CPE 泄露：列表与 route-map 名字按 VPN 派生（FRR 的这两个名字是 daemon 全局的）")
		for _, r := range toCPE {
			if !r.HasList || len(r.Entries) == 0 {
				continue
			}
			name := sdwan2cpePolicyName(r.VPN)
			for i, e := range r.Entries {
				mask := ""
				if e.GE > 0 {
					mask += fmt.Sprintf(" ge %d", e.GE)
				}
				if e.LE > 0 {
					mask += fmt.Sprintf(" le %d", e.LE)
				}
				line("ip prefix-list %s seq %d permit %s%s", name, (i+1)*5, e.Prefix, mask)
			}
		}
		for _, r := range toCPE {
			name := sdwan2cpePolicyName(r.VPN)
			line("route-map %s permit 10", name)
			if r.HasList && len(r.Entries) > 0 {
				line(" match ip address prefix-list %s", name)
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
		if r, ok := toCPEByVPN[v.ID]; ok && r.hasProtocol(leakProtoBGP) {
			line("  redistribute kernel route-map %s", sdwan2cpePolicyName(v.ID))
		}
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
		if r, ok := toCPEByVPN[v.ID]; ok && r.hasProtocol(leakProtoOSPF) {
			line(" redistribute kernel route-map %s", sdwan2cpePolicyName(v.ID))
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
	// Route is bundle 第 4 段: the keys of sr-route's route.json the controller
	// owns -- tunnels, vrfs, and rrs once the tenant has picked a reflector. It is
	// an overlay, not a whole file: tenant/identity_dir/serial/status_addr stay the
	// box's own (docs/overlay-console-plan.md §1.2, 更正见 docs/o6c-overlay-page-plan.md
	// §1.2). omitempty because bundles published before O6-b have no such key and
	// sr-apply must keep reading them.
	Route string `json:"route,omitempty"`
	// Srfrr is bundle 第 5 段: the two keys of /etc/sr-frr.json the controller
	// owns -- export_vrfs and import_rules. Same overlay rule as Route: the box
	// keeps status_url, zserv_socket, state_file and poll_sec. It is always
	// present on a new publish (empty arrays included) so that removing the last
	// leak row turns the leak off instead of leaving the old document in place.
	// omitempty only spares pre-O6-e bundles.
	Srfrr string `json:"srfrr,omitempty"`
	// Vrrp is bundle 第 6 段: the virtual routers this box must run on its
	// customer ports. Unlike the other five it is not a file the box keeps --
	// VRRP lives in VPP's memory and is configured through the binary API, so
	// this segment is a reconciliation instruction, and sr-apply must send the
	// empty list when the tenant removes the last row. omitempty only spares
	// bundles published before VRRP existed.
	Vrrp string `json:"vrrp,omitempty"`
	// Policy is bundle 第 7 段: 静态策略（ACL + ABF）与每 VRF 每颜色的兜底开关。
	// Same shape as Vrrp -- VPP memory, reconciled through the binary API -- plus
	// one difference: the fallback half is executed by sr-fib (it owns the colour
	// tables), so sr-agent derives it out of this same segment. Empty rows are
	// emitted as [] because "no policy" is an instruction: remove what is there.
	// omitempty only spares bundles published before policy existed.
	Policy string `json:"policy,omitempty"`
}

// vrrpArtifact is the decoded form of the 6th segment. The rows are keyed by
// the VPP interface name, never by sw_if_index: an index is box-local and is
// reassigned when VPP restarts, which is precisely the reboot this segment has
// to survive.
type vrrpArtifact struct {
	VRRP []vrrpEntry `json:"vrrp"`
}

type vrrpEntry struct {
	VPP        string   `json:"vpp"`
	VRID       uint8    `json:"vr_id"`
	Priority   uint8    `json:"priority"`
	IntervalMS uint16   `json:"interval_ms,omitempty"`
	Preempt    bool     `json:"preempt,omitempty"`
	Accept     bool     `json:"accept,omitempty"`
	Addrs      []string `json:"addrs"`
}

func renderArtifacts(c *BasicConfig) configArtifacts {
	return configArtifacts{
		FRR:    renderFRR(c, nil),
		VPP:    renderVPP(c),
		Boot:   renderBoot(c),
		Vrrp:   renderVRRPArtifact(c),
		Policy: renderPolicyArtifact(c, PolicySet{}),
	}
}

// renderVRRPArtifact emits the 6th segment. An empty list is still emitted:
// "no VRs" has to be distinguishable from "this bundle predates VRRP" (empty
// string), and the box reconciles against exactly that list.
func renderVRRPArtifact(c *BasicConfig) string {
	a := vrrpArtifact{VRRP: make([]vrrpEntry, 0, len(c.VRRP))}
	for _, v := range c.VRRP {
		a.VRRP = append(a.VRRP, vrrpEntry{
			VPP:        v.VPP,
			VRID:       v.VRID,
			Priority:   v.Priority,
			IntervalMS: v.IntervalMS,
			Preempt:    v.Preempt,
			Accept:     v.Accept,
			Addrs:      []string{v.VirtualAddr},
		})
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		// Only plain scalars and a slice of one string; unreachable.
		return ""
	}
	return string(b) + "\n"
}

func renderSrfrrArtifact(a srfrrArtifact) string {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		// Only plain scalars and slices of them; unreachable.
		return ""
	}
	return string(b) + "\n"
}

// artifactsLocked adds the per-device overlay, leak and policy artifacts to the
// three text fragments. Route depends on the tenant's groups *and* this device's
// tunnels, the leak half depends on the tenant's prefix-list library, and a
// policy row's attach list depends on the draft's customer ports -- so none of
// them can be rendered from the draft alone.
func (s *server) artifactsLocked(tenantID string, nodeID uint16, c *BasicConfig, set LeakSet, pol PolicySet) configArtifacts {
	rules, srfrr := s.leakRenderLocked(tenantID, set)
	return configArtifacts{
		FRR:    renderFRR(c, rules),
		VPP:    renderVPP(c),
		Boot:   renderBoot(c),
		Route:  s.overlayRouteArtifactLocked(tenantID, nodeID),
		Srfrr:  renderSrfrrArtifact(srfrr),
		Vrrp:   renderVRRPArtifact(c),
		Policy: renderPolicyArtifact(c, pol),
	}
}
