package frr

import (
	"encoding/hex"
	"net/netip"
	"testing"
)

// TestEncodeRedistributeAddGolden 固定订阅报文的字节序列：
// afi=AFI_IP(1)、type=ZEBRA_ROUTE_BGP(10)、instance=0。
func TestEncodeRedistributeAddGolden(t *testing.T) {
	got := encodeRedistributeAdd(afiIPv4, routeTypeBGP, 0)
	want := "000efe0600000000000c" + "010a0000"
	if enc := hex.EncodeToString(got); enc != want {
		t.Fatalf("REDISTRIBUTE_ADD 编码不对:\n got %s\nwant %s", enc, want)
	}
}

// TestDecodeRedistributeRouteGolden 用按 FRR 10.7.1 zapi_route_encode 布局
// 手写的字节验证解码：前缀 10.68.1.0/24、一条 IPV4_IFINDEX 下一跳、
// distance/metric/tag/mtu 齐全。
func TestDecodeRedistributeRouteGolden(t *testing.T) {
	body, err := hex.DecodeString(
		"0a" + "0000" + "00000000" + "0000001f" + // type/instance/flags/message
			"01" + "02" + "18" + "0a4401" + // safi/family/plen/prefix
			"0001" + // nexthop count
			"00000000" + "03" + "00" + "0a4f0103" + "00000003" + // vrf/type/flags/ip/ifindex
			"14" + // distance
			"00000000" + // metric
			"000003e9" + // tag 1001
			"000005dc") // mtu 1500
	if err != nil {
		t.Fatal(err)
	}
	ev, err := decodeRedistributeRoute(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Prefix != netip.MustParsePrefix("10.68.1.0/24") {
		t.Errorf("prefix = %s, want 10.68.1.0/24", ev.Prefix)
	}
	if ev.Tag != 1001 || !ev.Add {
		t.Errorf("ev = %+v, want tag 1001 add", ev)
	}
}

// TestDecodeRedistributeRouteNoTag 覆盖最常见的形态：只有下一跳，没有 tag。
// 带权重的 IPV4 下一跳也在这里顺带验证跳过逻辑。
func TestDecodeRedistributeRouteNoTag(t *testing.T) {
	body, err := hex.DecodeString(
		"0a" + "0000" + "00000000" + "00000001" + // NEXTHOP only
			"01" + "02" + "14" + "c0a800" +
			"0001" +
			"00000000" + "02" + "04" + "c0a80101" + "00000000" +
			"0000000000000064") // weight（flags 0x04）
	if err != nil {
		t.Fatal(err)
	}
	ev, err := decodeRedistributeRoute(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Prefix != netip.MustParsePrefix("192.168.0.0/20") || ev.Tag != 0 || ev.Add {
		t.Errorf("ev = %+v, want 192.168.0.0/20 tag 0 del", ev)
	}
}

func TestDecodeRedistributeRouteRejects(t *testing.T) {
	base := "0a" + "0000" + "00000000" + "00000000" + "01" + "02" + "18" + "0a4401"
	cases := map[string]string{
		"截断":          base[:len(base)-4],
		"非 BGP":       "10" + "0000" + "00000000" + "00000000" + "01" + "02" + "18" + "0a4401",
		"IPv6 family": "0a" + "0000" + "00000000" + "00000000" + "01" + "0a" + "18" + "0a4401",
		"未知 message":  "0a" + "0000" + "00000000" + "00000800" + "01" + "02" + "18" + "0a4401",
		"前缀长度非法":      "0a" + "0000" + "00000000" + "00000000" + "01" + "02" + "21" + "0a440100",
		"尾部多余字节":      base + "00",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			body, err := hex.DecodeString(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeRedistributeRoute(body, true); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestImporterSetSemantics(t *testing.T) {
	im := NewImporter(1001)
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	p2 := netip.MustParsePrefix("172.16.1.0/24")

	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
	im.Handle(RouteEvent{Prefix: p2, Tag: 1001, Add: true})
	if got := im.Prefixes(); len(got) != 2 {
		t.Fatalf("prefixes = %v, want 2 条", got)
	}

	// tag 变了：这个前缀的最新选中路由不再归我们导入，必须从集合里删掉。
	im.Handle(RouteEvent{Prefix: p2, Tag: 2002, Add: true})
	if got := im.Prefixes(); len(got) != 1 || got[0] != p1 {
		t.Fatalf("prefixes = %v, want 只剩 %s", got, p1)
	}

	im.Handle(RouteEvent{Prefix: p1, Add: false})
	if im.Len() != 0 {
		t.Fatalf("Len = %d, want 0", im.Len())
	}

	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
	im.Reset()
	if im.Len() != 0 {
		t.Fatalf("Reset 后 Len = %d, want 0", im.Len())
	}
}

func TestImporterSorts(t *testing.T) {
	im := NewImporter(7)
	for _, p := range []string{"172.16.0.0/12", "10.68.1.0/24", "10.0.0.0/8"} {
		im.Handle(RouteEvent{Prefix: netip.MustParsePrefix(p), Tag: 7, Add: true})
	}
	got := im.Prefixes()
	want := []string{"10.0.0.0/8", "10.68.1.0/24", "172.16.0.0/12"}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("prefixes = %v, want %v", got, want)
		}
	}
}
