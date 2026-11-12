// 服务面的配置模型：NAT 出口、DHCP server、DNS forwarder。
//
// 这三张表存在 DeviceConfig.Services（不在 BasicConfig 里）， wiped-proof 的理由
// 与 Leak/Policy 相同：基础配置页面 PUT 的是整个 BasicConfig，任何存在里面的行
// 都会被该页面的每次保存抹掉。行的"挂点"是草稿里的客户口（VPP 名），所以渲染
// 发生在发布时，把地址、掩码、内核 TAP 名一起解出来冻进 bundle。
//
// NAT 池是配置出来的，不是写死的（owner 2026-11-11 定）：一行里 Pool 是显式的
// 地址列表，盒子把它配成 VPP NAT 池；不在模型里发明"复用 WAN 口地址"的隐式
// 规则 —— 要用 WAN 口地址就在 Pool 里写它，页面上看得见。
package main

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const (
	cfgMaxNATRows  = 16
	cfgMaxDHCPRows = 32
	cfgMaxDNSRows  = 32
	cfgMaxPool     = 64

	dhcpMinLeaseSec = 300
	dhcpMaxLeaseSec = 30 * 24 * 3600
	dhcpLeaseDef    = 24 * 3600

	dnsMaxUpstream = 8
)

// ConfigNAT is one NAT44 出口行: 客户口(VPN 内)的流量经 Outside 出去，源地址翻
// 译成 Pool 里的地址。Pool 必须是 Outside 所在网络里、客户可达回程的地址 ——
// 页面只查合法性（IPv4、不重叠），可达性由租户自己保证。
type ConfigNAT struct {
	On bool `json:"on"`
	// VPN 是 Inside 口所在的 VPN（即 inside VRF 表号）。
	VPN uint32 `json:"vpn"`
	// Inside / Outside 是 VPP 接口名：客户口 / 出口（WAN 侧）。
	Inside  string `json:"inside"`
	Outside string `json:"outside"`
	// Pool 是本行的 NAT 地址池，显式配置。空列表 = 行不生效（保存时不报错，
	// 发布校验给 warning，避免半行配置被当成错误挡住编辑）。
	Pool []string `json:"pool"`
	// MSSClamp 出口方向的 TCP MSS 钳制值；0 = 不钳制。
	MSSClamp uint16 `json:"mss_clamp,omitempty"`
}

// ConfigDHCP is one DHCPv4 server row on one customer port. The address pool is
// explicit (Start..End within the port's subnet); gateway/DNS handed to clients
// default to the port's own address and can be overridden per row.
type ConfigDHCP struct {
	VPN uint32 `json:"vpn"`
	// VPP 是客户口（该 VPN 内）；服务器绑定它镜像到内核的 TAP 地址。
	VPP string `json:"vpp"`
	// Start..End 是可分配地址池，必须落在客户口的子网内。
	Start string `json:"start"`
	End   string `json:"end"`
	// LeaseSec 租约时长；0 = 默认 24h。
	LeaseSec int `json:"lease_sec,omitempty"`
	// Router 下发给客户端的网关；空 = 客户口自己的地址。
	Router string `json:"router,omitempty"`
	// DNS 下发给客户端的 DNS 服务器；空 = 本机 DNS 转发器（客户口地址）。
	DNS string `json:"dns,omitempty"`
}

// ConfigDNS is one DNS forwarder row: listens on one customer port's address in
// one VPN and forwards upstream. Upstream may be empty, meaning "还没配"（保存
// 不报错，发布校验给 warning；转发器在空上游时只回 SERVFAIL，不拦客户的 53）。
type ConfigDNS struct {
	VPN uint32 `json:"vpn"`
	// VPP 是客户口；监听它镜像到内核的 TAP 地址上的 53/udp。
	VPP string `json:"vpp"`
	// Upstream 上游解析器（IPv4），按顺序使用。
	Upstream []string `json:"upstream"`
}

// ServicesSet is the whole 服务面 of one device. 每张表都规范成非 nil，渲染出的
// 段永远带空数组 —— "删掉最后一行"必须能到达盒子，而不是留在原地。
type ServicesSet struct {
	NAT  []ConfigNAT  `json:"nat"`
	DHCP []ConfigDHCP `json:"dhcp"`
	DNS  []ConfigDNS  `json:"dns"`
}

func normalizeServices(s *ServicesSet) {
	if s.NAT == nil {
		s.NAT = []ConfigNAT{}
	}
	if s.DHCP == nil {
		s.DHCP = []ConfigDHCP{}
	}
	if s.DNS == nil {
		s.DNS = []ConfigDNS{}
	}
	for i := range s.NAT {
		s.NAT[i].Inside = strings.TrimSpace(s.NAT[i].Inside)
		s.NAT[i].Outside = strings.TrimSpace(s.NAT[i].Outside)
		pool := make([]string, 0, len(s.NAT[i].Pool))
		for _, a := range s.NAT[i].Pool {
			a = strings.TrimSpace(a)
			if a != "" {
				pool = append(pool, a)
			}
		}
		s.NAT[i].Pool = pool
	}
	for i := range s.DHCP {
		s.DHCP[i].VPP = strings.TrimSpace(s.DHCP[i].VPP)
		s.DHCP[i].Start = strings.TrimSpace(s.DHCP[i].Start)
		s.DHCP[i].End = strings.TrimSpace(s.DHCP[i].End)
		s.DHCP[i].Router = strings.TrimSpace(s.DHCP[i].Router)
		s.DHCP[i].DNS = strings.TrimSpace(s.DHCP[i].DNS)
		if s.DHCP[i].LeaseSec == 0 {
			s.DHCP[i].LeaseSec = dhcpLeaseDef
		}
	}
	for i := range s.DNS {
		s.DNS[i].VPP = strings.TrimSpace(s.DNS[i].VPP)
		up := make([]string, 0, len(s.DNS[i].Upstream))
		for _, a := range s.DNS[i].Upstream {
			a = strings.TrimSpace(a)
			if a != "" {
				up = append(up, a)
			}
		}
		s.DNS[i].Upstream = up
	}
	// 行没有顺序语义，规范化成稳定顺序：两次只差行序的草稿必须渲染出同一份
	// artifact，否则"发布没变化"不再可证。
	sort.SliceStable(s.NAT, func(i, j int) bool {
		if s.NAT[i].Inside != s.NAT[j].Inside {
			return s.NAT[i].Inside < s.NAT[j].Inside
		}
		return s.NAT[i].Outside < s.NAT[j].Outside
	})
	sort.SliceStable(s.DHCP, func(i, j int) bool {
		if s.DHCP[i].VPP != s.DHCP[j].VPP {
			return s.DHCP[i].VPP < s.DHCP[j].VPP
		}
		return s.DHCP[i].Start < s.DHCP[j].Start
	})
	sort.SliceStable(s.DNS, func(i, j int) bool { return s.DNS[i].VPP < s.DNS[j].VPP })
}

// servicesChecks is the server-side validation for one ServicesSet against the
// draft it will be rendered against. Same contract as validateBasicConfig:
// errors block a publish, warnings do not.
func servicesChecks(c *BasicConfig, s *ServicesSet) []ConfigCheck {
	var out []ConfigCheck
	add := func(severity, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: severity, Text: fmt.Sprintf(format, args...)})
	}

	if len(s.NAT) > cfgMaxNATRows {
		add("error", "NAT 行数超过上限 %d。", cfgMaxNATRows)
	}
	seenNAT := map[string]bool{}
	for i, r := range s.NAT {
		if r.Inside == "" || r.Outside == "" {
			add("error", "NAT 第 %d 行：客户口与出口口都必须填。", i+1)
			continue
		}
		key := r.Inside + ">" + r.Outside
		if seenNAT[key] {
			add("error", "NAT 第 %d 行：%s → %s 重复。", i+1, r.Inside, r.Outside)
		}
		seenNAT[key] = true
		it, ok := ifaceByVPP(c, r.Inside)
		if !ok {
			add("error", "NAT 第 %d 行：客户口 %s 不在基础配置里。", i+1, r.Inside)
		} else if it.VPN != r.VPN {
			add("error", "NAT 第 %d 行：VPN %d 与客户口 %s 所在的 VPN %d 不一致。", i+1, r.VPN, r.Inside, it.VPN)
		}
		if r.On && len(r.Pool) == 0 {
			add("warning", "NAT 第 %d 行（%s）：已启用但没有地址池，不会翻译任何流量。", i+1, r.Inside)
		}
		if len(r.Pool) > cfgMaxPool {
			add("error", "NAT 第 %d 行：地址池超过上限 %d 个。", i+1, cfgMaxPool)
		}
		seenPool := map[netip.Addr]bool{}
		for _, a := range r.Pool {
			ip, ok := parseIPv4(a)
			if !ok {
				add("error", "NAT 第 %d 行：地址池里的 %q 不是合法的 IPv4。", i+1, a)
				continue
			}
			if seenPool[ip] {
				add("warning", "NAT 第 %d 行：地址池里 %s 重复。", i+1, a)
			}
			seenPool[ip] = true
		}
		if r.MSSClamp != 0 && r.MSSClamp < 536 {
			add("error", "NAT 第 %d 行：MSS 钳制值 %d 低于 TCP 最小值 536。", i+1, r.MSSClamp)
		}
	}

	if len(s.DHCP) > cfgMaxDHCPRows {
		add("error", "DHCP 行数超过上限 %d。", cfgMaxDHCPRows)
	}
	for i, r := range s.DHCP {
		if r.VPP == "" {
			add("error", "DHCP 第 %d 行：客户口必填。", i+1)
			continue
		}
		it, ok := ifaceByVPP(c, r.VPP)
		if !ok {
			add("error", "DHCP 第 %d 行：客户口 %s 不在基础配置里。", i+1, r.VPP)
			continue
		}
		if it.VPN != r.VPN {
			add("error", "DHCP 第 %d 行：VPN %d 与客户口 %s 所在的 VPN %d 不一致。", i+1, r.VPN, r.VPP, it.VPN)
		}
		start, okS := parseIPv4(r.Start)
		end, okE := parseIPv4(r.End)
		if !okS || !okE {
			add("error", "DHCP 第 %d 行：地址池起止 %q/%q 必须是合法的 IPv4。", i+1, r.Start, r.End)
			continue
		}
		if bytesOf(start) > bytesOf(end) {
			add("error", "DHCP 第 %d 行：地址池起点 %s 大于终点 %s。", i+1, r.Start, r.End)
		}
		if p, ok := ifacePrefix(it); ok {
			if !p.Contains(start) || !p.Contains(end) {
				add("error", "DHCP 第 %d 行：地址池 %s-%s 不在客户口 %s 的子网 %s 内。", i+1, r.Start, r.End, r.VPP, p)
			}
		}
		if ip, ok := parseIPv4(it.IP); ok {
			if p, ok := ifacePrefix(it); ok && p.Contains(ip) && (p.Contains(start) || p.Contains(end)) {
				if bytesOf(start) <= bytesOf(ip) && bytesOf(ip) <= bytesOf(end) {
					add("error", "DHCP 第 %d 行：地址池包含客户口自己的地址 %s。", i+1, it.IP)
				}
			}
		}
		if r.LeaseSec < dhcpMinLeaseSec || r.LeaseSec > dhcpMaxLeaseSec {
			add("error", "DHCP 第 %d 行：租约时长 %d 秒超出 %d–%d。", i+1, r.LeaseSec, dhcpMinLeaseSec, dhcpMaxLeaseSec)
		}
		if r.Router != "" {
			if ip, ok := parseIPv4(r.Router); !ok {
				add("error", "DHCP 第 %d 行：网关 %q 不是合法的 IPv4。", i+1, r.Router)
			} else if p, ok := ifacePrefix(it); ok && !p.Contains(ip) {
				add("warning", "DHCP 第 %d 行：网关 %s 不在客户口子网内，客户端通常到不了它。", i+1, ip)
			}
		}
		if r.DNS != "" {
			if _, ok := parseIPv4(r.DNS); !ok {
				add("error", "DHCP 第 %d 行：DNS %q 不是合法的 IPv4。", i+1, r.DNS)
			}
		}
	}

	if len(s.DNS) > cfgMaxDNSRows {
		add("error", "DNS 行数超过上限 %d。", cfgMaxDNSRows)
	}
	for i, r := range s.DNS {
		if r.VPP == "" {
			add("error", "DNS 第 %d 行：客户口必填。", i+1)
			continue
		}
		it, ok := ifaceByVPP(c, r.VPP)
		if !ok {
			add("error", "DNS 第 %d 行：客户口 %s 不在基础配置里。", i+1, r.VPP)
			continue
		}
		if it.VPN != r.VPN {
			add("error", "DNS 第 %d 行：VPN %d 与客户口 %s 所在的 VPN %d 不一致。", i+1, r.VPN, r.VPP, it.VPN)
		}
		if len(r.Upstream) > dnsMaxUpstream {
			add("error", "DNS 第 %d 行：上游解析器超过上限 %d 个。", i+1, dnsMaxUpstream)
		}
		for _, a := range r.Upstream {
			if _, ok := parseIPv4(a); !ok {
				add("error", "DNS 第 %d 行：上游 %q 不是合法的 IPv4。", i+1, a)
			}
		}
	}

	// 同一个客户口上 DHCP 与 DNS 各只允许一行：两台 DHCP server 在一个子网里
	// 是事故，不是配置。
	seenDHCP := map[string]bool{}
	for i, r := range s.DHCP {
		if seenDHCP[r.VPP] {
			add("error", "DHCP 第 %d 行：客户口 %s 上已有 DHCP 行。", i+1, r.VPP)
		}
		seenDHCP[r.VPP] = true
	}
	seenDNS := map[string]bool{}
	for i, r := range s.DNS {
		if seenDNS[r.VPP] {
			add("error", "DNS 第 %d 行：客户口 %s 上已有 DNS 行。", i+1, r.VPP)
		}
		seenDNS[r.VPP] = true
	}
	return out
}

func bytesOf(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
