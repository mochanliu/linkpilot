package vppapi

import (
	"encoding/binary"
	"testing"
)

// 策略里的"查表"路径必须是 sw_if_index=~0 + table_id=<目标表>。
// 这是 VPP 表达 deag（换表重查）的唯一形状；写成普通下一跳就变成"钉死一条隧道"，
// 颜色表的收敛（BFD 摘路/加路）就与策略无关了。
func TestDeagPathShape(t *testing.T) {
	p := deagPath(ABFPath{Table: 7001, Preference: 3})
	if p.SWIfIndex != ^uint32(0) {
		t.Errorf("sw_if_index 应为 ~0，实际 %d", p.SWIfIndex)
	}
	if p.TableID != 7001 {
		t.Errorf("table_id 应为 7001，实际 %d", p.TableID)
	}
	if p.Preference != 3 {
		t.Errorf("preference 应为 3，实际 %d", p.Preference)
	}
	if p.NextHop.IsValid() {
		t.Errorf("查表路径不该带下一跳，实际 %v", p.NextHop)
	}
	if err := validatePath(p); err != nil {
		t.Errorf("查表路径必须能通过 validatePath: %v", err)
	}
}

// 读回时表号在 fib_path 的第 4..8 字节、preference 在第 13 字节。
func TestParseABFPolicyDetails(t *testing.T) {
	body := make([]byte, 15+fibPathLen)
	binary.BigEndian.PutUint32(body[6:10], 42)    // policy_id
	binary.BigEndian.PutUint32(body[10:14], 4095) // acl_index
	body[14] = 1
	off := 15
	binary.BigEndian.PutUint32(body[off:off+4], ^uint32(0))
	binary.BigEndian.PutUint32(body[off+4:off+8], 7001)
	body[off+13] = 7 // preference

	p, err := parseABFPolicyDetails(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.ID != 42 || p.ACLIndex != 4095 {
		t.Errorf("id/acl 解错: %+v", p)
	}
	if len(p.Paths) != 1 || p.Paths[0].Table != 7001 || p.Paths[0].Preference != 7 {
		t.Errorf("路径解错: %+v", p.Paths)
	}
}

func TestParseABFPolicyDetailsTruncated(t *testing.T) {
	body := make([]byte, 15+fibPathLen-1)
	body[14] = 1
	if _, err := parseABFPolicyDetails(body); err == nil {
		t.Fatal("路径区不足时应报错，而不是把半条路径当成一条")
	}
	if _, err := parseABFPolicyDetails(make([]byte, 9)); err == nil {
		t.Fatal("比定长还短的帧应报错")
	}
}

func TestParseABFAttachDetails(t *testing.T) {
	body := make([]byte, 19)
	binary.BigEndian.PutUint32(body[6:10], 42)
	binary.BigEndian.PutUint32(body[10:14], 7)
	binary.BigEndian.PutUint32(body[14:18], 100)
	a, err := parseABFAttachDetails(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.PolicyID != 42 || a.SWIfIndex != 7 || a.Priority != 100 {
		t.Errorf("挂点解错: %+v", a)
	}
}

func TestABFPolicyAddDelValidates(t *testing.T) {
	var c Client
	if err := c.ABFPolicyAddDel(nil, ABFPolicy{ID: 1, ACLIndex: 2}, true); err == nil {
		t.Error("没有路径的策略应被本地挡住")
	}
	if err := c.ABFPolicyAddDel(nil, ABFPolicy{ID: 0, ACLIndex: 2, Paths: []ABFPath{{Table: 1}}}, true); err == nil {
		t.Error("id=0 应被挡住（0 在 attach 里表示没有策略）")
	}
	if err := c.ABFAttachAddDel(nil, ABFAttach{PolicyID: 1, SWIfIndex: 0}, true); err == nil {
		t.Error("没有接口的挂点应被挡住")
	}
}
