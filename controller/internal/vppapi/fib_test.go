package vppapi

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
)

// 这些测试锁的是**字节布局**，不是"调用没报错"。布局错了 VPP 的行为统一是
// "超时且没有错误"或"收下但对不上"，两条都比崩溃贵得多。

func TestIPTableAddDelEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.IPTableAddDel(ctx, 100, true); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "ip_table_add_del" {
		t.Fatalf("请求名 = %s", r.name)
	}
	// is_add(1) + ip_table{table_id(4) is_ip6(1) name[64]}
	if len(r.body) != 70 {
		t.Fatalf("体长 = %d，期望 70", len(r.body))
	}
	if r.body[0] != 1 {
		t.Errorf("is_add = %d，期望 1", r.body[0])
	}
	if got := binary.BigEndian.Uint32(r.body[1:5]); got != 100 {
		t.Errorf("table_id = %d，期望 100", got)
	}
	if r.body[5] != 0 {
		t.Errorf("is_ip6 = %d，期望 0", r.body[5])
	}
}

func TestIPRouteAddDelEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	prefix := netip.MustParsePrefix("10.20.0.1/32")
	nh := netip.MustParseAddr("192.168.99.2")
	if err := c.IPRouteAddDel(ctx, 100, prefix, []FIBPath{{SWIfIndex: 2, NextHop: nh, Labels: []uint32{777}, Weight: 3, Preference: 235}}, true); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "ip_route_add_del" {
		t.Fatalf("请求名 = %s", r.name)
	}
	if len(r.body) != ipRouteFixedLen+fibPathLen {
		t.Fatalf("体长 = %d，期望 %d", len(r.body), ipRouteFixedLen+fibPathLen)
	}
	if r.body[0] != 1 || r.body[1] != 0 {
		t.Errorf("is_add/is_multipath = %d/%d，期望 1/0", r.body[0], r.body[1])
	}
	if got := binary.BigEndian.Uint32(r.body[2:6]); got != 100 {
		t.Errorf("table_id = %d，期望 100", got)
	}
	// prefix 从 body[10] 开始：af(u8) + union[16] + len(u8) = 18
	if r.body[10] != apiAddressFamilyIP4 {
		t.Errorf("prefix af = %d，期望 %d", r.body[10], apiAddressFamilyIP4)
	}
	if got := r.body[11:15]; got[0] != 10 || got[1] != 20 || got[2] != 0 || got[3] != 1 {
		t.Errorf("prefix 地址 = %v，期望 10.20.0.1", got)
	}
	if r.body[27] != 32 {
		t.Errorf("prefix 长度 = %d，期望 32", r.body[27])
	}
	if r.body[28] != 1 {
		t.Errorf("n_paths = %d，期望 1", r.body[28])
	}
	p := r.body[29:]
	if got := binary.BigEndian.Uint32(p[0:4]); got != 2 {
		t.Errorf("sw_if_index = %d，期望 2", got)
	}
	if got := binary.BigEndian.Uint32(p[4:8]); got != 0 {
		t.Errorf("path table_id = %d，期望 0", got)
	}
	if p[12] != 3 {
		t.Errorf("weight = %d，期望 3", p[12])
	}
	if p[13] != 235 {
		t.Errorf("preference = %d，期望 235", p[13])
	}
	// type/flags/proto 都是普通枚举、各 4 字节（与 af 不同）
	if got := binary.BigEndian.Uint32(p[14:18]); got != 0 {
		t.Errorf("type = %d，期望 NORMAL(0)", got)
	}
	if got := binary.BigEndian.Uint32(p[22:26]); got != 0 {
		t.Errorf("proto = %d，期望 IP4(0)", got)
	}
	if got := p[26:30]; got[0] != 192 || got[1] != 168 || got[2] != 99 || got[3] != 2 {
		t.Errorf("下一跳 = %v，期望 192.168.99.2", got)
	}
	if p[54] != 1 {
		t.Errorf("n_labels = %d，期望 1", p[54])
	}
	if got := binary.BigEndian.Uint32(p[56:60]); got != 777 {
		t.Errorf("label = %d，期望 777", got)
	}
}

func TestMPLSLabelAddDelEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.MPLSLabelAddDel(ctx, 116, 100, true); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "mpls_route_add_del" {
		t.Fatalf("请求名 = %s", r.name)
	}
	if len(r.body) != mplsRouteFixedLen+fibPathLen {
		t.Fatalf("体长 = %d，期望 %d", len(r.body), mplsRouteFixedLen+fibPathLen)
	}
	if r.body[0] != 1 || r.body[1] != 0 {
		t.Errorf("is_add/is_multipath = %d/%d", r.body[0], r.body[1])
	}
	if got := binary.BigEndian.Uint32(r.body[2:6]); got != 0 {
		t.Errorf("mr_table_id = %d，期望 0", got)
	}
	if got := binary.BigEndian.Uint32(r.body[6:10]); got != 116 {
		t.Errorf("label = %d，期望 116", got)
	}
	if r.body[10] != 1 {
		t.Errorf("eos = %d，期望 1", r.body[10])
	}
	if r.body[11] != 0 {
		t.Errorf("eos_proto = %d，期望 IP4(0)", r.body[11])
	}
	if r.body[13] != 1 {
		t.Errorf("n_paths = %d，期望 1", r.body[13])
	}
	p := r.body[mplsRouteFixedLen:]
	if got := binary.BigEndian.Uint32(p[0:4]); got != ^uint32(0) {
		t.Errorf("sw_if_index = %#x，期望 ~0（lookup-in-table 伪路径）", got)
	}
	if got := binary.BigEndian.Uint32(p[4:8]); got != 100 {
		t.Errorf("lookup table = %d，期望 100", got)
	}
	if p[54] != 0 {
		t.Errorf("n_labels = %d，期望 0", p[54])
	}
}

func TestCLIEncoding(t *testing.T) {
	f := newFake(t)
	f.cliReply = "sr-wg4-mpls-out: 1 MPLS adjacency(ies) repointed\n"
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	out, err := c.CLI(ctx, "sr-wg mpls-egress")
	if err != nil {
		t.Fatal(err)
	}
	if out != f.cliReply {
		t.Errorf("应答 = %q，期望 %q", out, f.cliReply)
	}
	r := f.lastRequest(t)
	if r.name != "cli_inband" {
		t.Fatalf("请求名 = %s", r.name)
	}
	// vl_api_string_t = u32 length + bytes，**不含 NUL**
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != 17 {
		t.Errorf("字符串长度 = %d，期望 17（不含 NUL）", got)
	}
	if string(r.body[4:]) != "sr-wg mpls-egress" {
		t.Errorf("命令 = %q", r.body[4:])
	}
}

// ip_route_details 的净体：table_id(4) stats_index(4) prefix(18) n_paths(1) paths(167*n)
func encodeIPRouteRaw(table uint32, addr [4]byte, plen byte, paths [][]byte) []byte {
	b := make([]byte, 0, 27+len(paths)*fibPathLen)
	b = appendU32(b, table)
	b = appendU32(b, 0)
	b = append(b, apiAddressFamilyIP4)
	b = append(b, addr[:]...)
	b = append(b, make([]byte, 12)...)
	b = append(b, plen)
	b = append(b, byte(len(paths)))
	for _, p := range paths {
		b = append(b, p...)
	}
	return b
}

func encodeFIBPathRaw(swIf uint32, nh [4]byte, labels ...uint32) []byte {
	return encodeFIBPathRawWP(swIf, 1, 0, nh, labels...)
}

func encodeFIBPathRawWP(swIf uint32, weight, pref uint8, nh [4]byte, labels ...uint32) []byte {
	b := make([]byte, fibPathLen)
	binary.BigEndian.PutUint32(b[0:4], swIf)
	b[12] = weight
	b[13] = pref
	copy(b[26:30], nh[:])
	b[54] = byte(len(labels))
	for i, l := range labels {
		binary.BigEndian.PutUint32(b[55+i*fibLabelLen+1:], l)
	}
	return b
}

func TestFIBRoutesParsing(t *testing.T) {
	f := newFake(t)
	f.ipRouteRaw = [][]byte{
		encodeIPRouteRaw(100, [4]byte{10, 20, 0, 1}, 32,
			[][]byte{encodeFIBPathRaw(2, [4]byte{192, 168, 99, 2}, 777)}),
	}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	routes, err := c.FIBRoutes(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("路由数 = %d，期望 1", len(routes))
	}
	r := routes[0]
	if r.TableID != 100 {
		t.Errorf("table = %d", r.TableID)
	}
	if r.Prefix != netip.MustParsePrefix("10.20.0.1/32") {
		t.Errorf("prefix = %s", r.Prefix)
	}
	if len(r.Paths) != 1 || r.Paths[0].SWIfIndex != 2 ||
		r.Paths[0].NextHop != netip.MustParseAddr("192.168.99.2") ||
		len(r.Paths[0].Labels) != 1 || r.Paths[0].Labels[0] != 777 {
		t.Errorf("路径读错: %+v", r.Paths)
	}
}

func TestFIBRoutesWeightPref(t *testing.T) {
	f := newFake(t)
	f.ipRouteRaw = [][]byte{
		encodeIPRouteRaw(10, [4]byte{10, 0, 0, 0}, 24,
			[][]byte{
				encodeFIBPathRawWP(5, 1, 230, [4]byte{192, 168, 1, 1}, 1000),
				encodeFIBPathRawWP(6, 1, 237, [4]byte{192, 168, 2, 1}, 1000),
			}),
	}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	routes, err := c.FIBRoutes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || len(routes[0].Paths) != 2 {
		t.Fatalf("got %d routes with %d paths", len(routes), len(routes[0].Paths))
	}
	p0, p1 := routes[0].Paths[0], routes[0].Paths[1]
	if p0.Weight != 1 || p0.Preference != 230 {
		t.Errorf("path[0] weight/pref = %d/%d, want 1/230", p0.Weight, p0.Preference)
	}
	if p1.Weight != 1 || p1.Preference != 237 {
		t.Errorf("path[1] weight/pref = %d/%d, want 1/237", p1.Weight, p1.Preference)
	}
}

func TestMPLSRoutesParsing(t *testing.T) {
	f := newFake(t)
	path := encodeFIBPathRaw(^uint32(0), [4]byte{})
	binary.BigEndian.PutUint32(path[4:8], 100) // lookup table
	raw := make([]byte, 0, 12+fibPathLen)
	raw = appendU32(raw, 0)   // table
	raw = appendU32(raw, 116) // label
	raw = append(raw, 1, 0, 0, 1)
	raw = append(raw, path...)
	f.mplsRaw = [][]byte{raw}

	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	rs, err := c.MPLSRoutes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("标签数 = %d，期望 1", len(rs))
	}
	got := rs[0]
	if got.Label != 116 || !got.EOS || got.EOSProto != 0 {
		t.Errorf("标签读错: %+v", got)
	}
	if len(got.Paths) != 1 || got.Paths[0].SWIfIndex != ^uint32(0) || got.Paths[0].TableID != 100 {
		t.Errorf("lookup 路径读错: %+v", got.Paths)
	}
}

func TestDefaultWeightPreservesOne(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	nh := netip.MustParseAddr("10.0.0.1")
	if err := c.IPRouteAddDel(ctx, 0, netip.MustParsePrefix("10.99.0.0/24"),
		[]FIBPath{{SWIfIndex: 3, NextHop: nh}}, true); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	p := r.body[ipRouteFixedLen:]
	if p[12] != 1 {
		t.Errorf("weight = %d, want 1 (zero-value fallback)", p[12])
	}
	if p[13] != 0 {
		t.Errorf("preference = %d, want 0", p[13])
	}
}

func TestFIBValidation(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
	}{
		{"IPv6 前缀", c.IPRouteAddDel(ctx, 100, netip.MustParsePrefix("2001:db8::/64"),
			[]FIBPath{{SWIfIndex: 2, NextHop: netip.MustParseAddr("192.168.99.2")}}, true)},
		{"没有路径", c.IPRouteAddDel(ctx, 100, netip.MustParsePrefix("10.0.0.0/8"), nil, true)},
		{"路径没有下一跳", c.IPRouteAddDel(ctx, 100, netip.MustParsePrefix("10.0.0.0/8"),
			[]FIBPath{{SWIfIndex: 2}}, true)},
		{"标签超 20 位", c.MPLSLabelAddDel(ctx, 1<<20, 100, true)},
		{"标签过多", c.IPRouteAddDel(ctx, 100, netip.MustParsePrefix("10.0.0.0/8"),
			[]FIBPath{{SWIfIndex: 2, NextHop: netip.MustParseAddr("192.168.99.2"),
				Labels: make([]uint32, fibPathMaxLabels+1)}}, true)},
	}
	for _, tc := range cases {
		if tc.err == nil {
			t.Errorf("%s: 应当在发出前报错", tc.name)
		}
	}
}
