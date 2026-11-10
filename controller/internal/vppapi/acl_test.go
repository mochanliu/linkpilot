package vppapi

import (
	"context"
	"net/netip"
	"testing"
)

// 一条规则的字节布局必须是 48 字节，且 action/proto 各占 1 字节。
// 这两个字段写成 4 字节时 VPP 报的是 "Invalid src address"(-57) —— 2026-11-10
// 真机第一次试装就是这个错，所以把它钉在单测里。
func TestACLRuleLayout(t *testing.T) {
	r := ACLRule{
		Action:  ACLPermit,
		Src:     netip.MustParsePrefix("172.16.4.0/24"),
		Dst:     netip.MustParsePrefix("172.16.12.1/32"),
		Proto:   6, // tcp
		SrcPort: PortRange{First: 1024, Last: 65535},
		DstPort: PortRange{First: 443, Last: 443},
	}
	b := appendACLRule(nil, r)
	if len(b) != aclRuleLen {
		t.Fatalf("一条规则编出 %d 字节，期望 %d", len(b), aclRuleLen)
	}
	if b[0] != byte(ACLPermit) {
		t.Errorf("action 落在字节 0，实际 %d", b[0])
	}
	if b[1] != apiAddressFamilyIP4 {
		t.Errorf("src 的 af 落在字节 1，实际 %d", b[1])
	}
	if got := [4]byte{b[2], b[3], b[4], b[5]}; got != [4]byte{172, 16, 4, 0} {
		t.Errorf("src 地址落在字节 2..5，实际 %v", got)
	}
	if b[18] != 24 {
		t.Errorf("src 前缀长度落在字节 18，实际 %d", b[18])
	}
	if b[36] != 32 {
		t.Errorf("dst 前缀长度落在字节 36，实际 %d", b[36])
	}
	if b[37] != 6 {
		t.Errorf("proto 落在字节 37（1 字节），实际 %d", b[37])
	}
	if got := uint16(b[38])<<8 | uint16(b[39]); got != 1024 {
		t.Errorf("sport first 落在字节 38..39，实际 %d", got)
	}
	if got := uint16(b[44])<<8 | uint16(b[45]); got != 443 {
		t.Errorf("dport last 落在字节 44..45，实际 %d", got)
	}
}

func TestACLRuleValidation(t *testing.T) {
	any4 := netip.MustParsePrefix("0.0.0.0/0")
	cases := []struct {
		name string
		rule ACLRule
	}{
		{"动作不认识", ACLRule{Action: 9, Src: any4, Dst: any4, SrcPort: AnyPorts(), DstPort: AnyPorts()}},
		{"前缀没归一", ACLRule{Action: ACLPermit, Src: netip.MustParsePrefix("172.16.4.7/24"), Dst: any4, SrcPort: AnyPorts(), DstPort: AnyPorts()}},
		{"IPv6", ACLRule{Action: ACLPermit, Src: netip.MustParsePrefix("2001:db8::/32"), Dst: any4, SrcPort: AnyPorts(), DstPort: AnyPorts()}},
		{"区间颠倒", ACLRule{Action: ACLPermit, Src: any4, Dst: any4, Proto: 6, SrcPort: PortRange{First: 200, Last: 100}, DstPort: AnyPorts()}},
		{"any 协议带端口", ACLRule{Action: ACLPermit, Src: any4, Dst: any4, Proto: 0, SrcPort: PortRange{First: 1, Last: 2}, DstPort: AnyPorts()}},
	}
	for _, c := range cases {
		if err := c.rule.validate(); err == nil {
			t.Errorf("%s: 应该报错但通过了", c.name)
		}
	}
	ok := ACLRule{Action: ACLPermit, Src: any4, Dst: any4, SrcPort: AnyPorts(), DstPort: AnyPorts()}
	if err := ok.validate(); err != nil {
		t.Errorf("any→any permit 应该合法: %v", err)
	}
}

func TestACLAddReplaceRejectsEmpty(t *testing.T) {
	var c Client
	if _, err := c.ACLAddReplace(context.Background(), 0, "x", nil); err == nil {
		t.Fatal("空规则集应该被挡在客户端，而不是发给 VPP")
	}
}
