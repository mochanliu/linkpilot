// NAT 行的保存期校验。这两条不是风格问题，是这台盒子上量出来的事故：
// pool 里写出口口自己的地址 = NAT 一开管理面必失联；pool 越出外口网段 =
// 盒子挂不上这个 secondary，地址池永远不通。
package main

import (
	"strings"
	"testing"
)

func natFixture() *BasicConfig {
	return &BasicConfig{Ifaces: []ConfigIface{
		{VPP: "host-enp0s8", LCP: "lcp-x0", IP: "172.31.4.1", PrefixLen: 24, VPN: 0},
		{VPP: "host-enp0s9", LCP: "lcp-x1", IP: "192.168.1.104", PrefixLen: 24, VPN: 0},
	}}
}

func checksText(cs []ConfigCheck) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.Severity + ":" + c.Text + "\n")
	}
	return b.String()
}

func TestServicesChecksRejectsPoolEqualToOutsideAddr(t *testing.T) {
	s := &ServicesSet{NAT: []ConfigNAT{{
		On: true, VPN: 0, Inside: "host-enp0s8", Outside: "host-enp0s9",
		Pool: []string{"192.168.1.104"},
	}}}
	got := checksText(servicesChecks(natFixture(), s))
	if !strings.Contains(got, "不能包含出口口自己的地址") {
		t.Fatalf("没有拦住 pool=外口地址：\n%s", got)
	}
}

func TestServicesChecksRejectsPoolOutsideSubnet(t *testing.T) {
	s := &ServicesSet{NAT: []ConfigNAT{{
		On: true, VPN: 0, Inside: "host-enp0s8", Outside: "host-enp0s9",
		Pool: []string{"10.9.9.10"},
	}}}
	got := checksText(servicesChecks(natFixture(), s))
	if !strings.Contains(got, "不在出口口 host-enp0s9 的网段") {
		t.Fatalf("没有拦住越界池地址：\n%s", got)
	}
}

func TestServicesChecksRejectsDuplicatePoolAcrossRows(t *testing.T) {
	s := &ServicesSet{NAT: []ConfigNAT{
		{On: true, VPN: 0, Inside: "host-enp0s8", Outside: "host-enp0s9", Pool: []string{"192.168.1.200"}},
		{On: true, VPN: 0, Inside: "host-enp0s8", Outside: "host-enp0s9", Pool: []string{"192.168.1.200"}},
	}}
	got := checksText(servicesChecks(natFixture(), s))
	if !strings.Contains(got, "已被第 1 行使用") {
		t.Fatalf("没有拦住两行抢同一个池地址：\n%s", got)
	}
}

// 正例：专用池地址 + 合法内外口，一条 error 都不能有（外观上这就是产品形状 G）。
func TestServicesChecksAcceptsDedicatedPool(t *testing.T) {
	s := &ServicesSet{NAT: []ConfigNAT{{
		On: true, VPN: 0, Inside: "host-enp0s8", Outside: "host-enp0s9",
		Pool: []string{"192.168.1.200"},
	}}}
	for _, c := range servicesChecks(natFixture(), s) {
		if c.Severity == "error" {
			t.Fatalf("合法配置被判成错误：%s", c.Text)
		}
	}
}
