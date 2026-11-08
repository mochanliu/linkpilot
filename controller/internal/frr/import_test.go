package frr

import (
	"encoding/hex"
	"net/netip"
	"testing"
	"time"
)

// TestEncodeRedistributeAddGolden 固定订阅报文的字节序列：头里的 vrf_id 是
// **内核 VRF 设备的 ifindex**（默认 VRF 是 0，vpn100 实测是 10）。
func TestEncodeRedistributeAddGolden(t *testing.T) {
	cases := []struct {
		vrfID uint32
		want  string
	}{
		{0, "000efe0600000000000c" + "010a0000"},
		{10, "000efe060000000a000c" + "010a0000"},
	}
	for _, tc := range cases {
		got := encodeRedistributeAdd(afiIPv4, routeTypeBGP, 0, tc.vrfID)
		if enc := hex.EncodeToString(got); enc != tc.want {
			t.Errorf("vrf_id %d 的 REDISTRIBUTE_ADD 编码不对:\n got %s\nwant %s", tc.vrfID, enc, tc.want)
		}
	}
}

// TestDecodeRedistributeRouteGolden 用按 FRR 10.7.1 zapi_route_encode 布局
// 手写的字节验证解码：前缀 10.68.1.0/24、一条 IPV4_IFINDEX 下一跳、
// distance/metric/tag/mtu 齐全（tag 只做跳过验证，不再参与判定）。
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
	ev, err := decodeRedistributeRoute(body, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Prefix != netip.MustParsePrefix("10.68.1.0/24") {
		t.Errorf("prefix = %s, want 10.68.1.0/24", ev.Prefix)
	}
	if ev.VRFID != 10 || ev.Type != RouteBGP {
		t.Errorf("vrf/type = %d/%d, want 10/%d", ev.VRFID, ev.Type, RouteBGP)
	}
	if !ev.Add {
		t.Errorf("ev = %+v, want add", ev)
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
	ev, err := decodeRedistributeRoute(body, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Prefix != netip.MustParsePrefix("192.168.0.0/20") || ev.Add {
		t.Errorf("ev = %+v, want 192.168.0.0/20 del", ev)
	}
}

// 四种来源都要能解：connected=2 / static=4 / ospf=7 / bgp=10。
func TestDecodeAcceptsTheFourSources(t *testing.T) {
	prefix := "01" + "02" + "18" + "0a4401"
	for _, typ := range []uint8{RouteConnected, RouteStatic, RouteOSPF, RouteBGP} {
		body, err := hex.DecodeString(hexByte(typ) + "0000" + "00000000" + "00000000" + prefix)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := decodeRedistributeRoute(body, true, 0)
		if err != nil {
			t.Fatalf("type %d: %v", typ, err)
		}
		if ev.Type != typ {
			t.Fatalf("type = %d, want %d", ev.Type, typ)
		}
	}
}

func hexByte(b uint8) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0xf]})
}

func TestDecodeRedistributeRouteRejects(t *testing.T) {
	base := "0a" + "0000" + "00000000" + "00000000" + "01" + "02" + "18" + "0a4401"
	cases := map[string]string{
		"截断":          base[:len(base)-4],
		"未知类型":        "10" + "0000" + "00000000" + "00000000" + "01" + "02" + "18" + "0a4401",
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
			if _, err := decodeRedistributeRoute(body, true, 0); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

// allowAll 是一条"这个 VPN 的这些来源全放行"的规则，测试默认值。
func allowAll(vpn uint8, sources ...uint8) ImportRule {
	if len(sources) == 0 {
		sources = []uint8{RouteBGP}
	}
	return ImportRule{
		VPN:     vpn,
		Sources: sources,
		Allow:   []AllowEntry{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), LE: 32}},
	}
}

func reported(t *testing.T, im *Importer, rules []ImportRule) []string {
	t.Helper()
	ps, _ := im.Report(rules, nil)
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Prefix.String())
	}
	return out
}

func TestImporterSetSemantics(t *testing.T) {
	im := NewImporter(nil)
	rules := []ImportRule{allowAll(0)}
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	p2 := netip.MustParsePrefix("172.16.1.0/24")

	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: true})
	if got := reported(t, im, rules); len(got) != 2 {
		t.Fatalf("prefixes = %v, want 2 条", got)
	}

	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: false})
	if got := reported(t, im, rules); len(got) != 1 || got[0] != p1.String() {
		t.Fatalf("prefixes = %v, want 只剩 %s", got, p1)
	}

	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: false})
	if got := reported(t, im, rules); len(got) != 0 {
		t.Fatalf("prefixes = %v, want 0", got)
	}

	// 重连即重置：旧集合不留，新连接的回说什么就是什么（AGENTS.md 第 3 条，
	// 2026-11-07 定案：不许维持旧集合）。
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.Reset()
	if got := reported(t, im, rules); len(got) != 0 {
		t.Fatalf("重连后 prefixes = %v, want 0（回放到达前不报旧集合）", got)
	}
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	if got := reported(t, im, rules); len(got) != 1 || got[0] != p1.String() {
		t.Fatalf("回放到达后 prefixes = %v, want 只剩 %s", got, p1)
	}
}

// 同一个前缀被两种来源选中时，撤掉其中一种不能带走整条前缀。
func TestImporterSourceUnion(t *testing.T) {
	im := NewImporter(nil)
	rules := []ImportRule{allowAll(0, RouteBGP, RouteConnected)}
	p := netip.MustParsePrefix("10.68.1.0/24")

	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: true})
	im.Handle(RouteEvent{Type: RouteConnected, Prefix: p, Add: true})
	if got := reported(t, im, rules); len(got) != 1 {
		t.Fatalf("prefixes = %v, want 1", got)
	}

	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: false})
	if got := reported(t, im, rules); len(got) != 1 {
		t.Fatalf("撤掉 BGP 后 prefixes = %v, want 仍由 connected 保留", got)
	}

	// 只选 BGP 时，connected 那份不算数。
	if got := reported(t, im, []ImportRule{allowAll(0, RouteBGP)}); len(got) != 0 {
		t.Fatalf("只选 BGP 时 prefixes = %v, want 0", got)
	}
}

// 白名单为空 = 一条都不放行；写错了也不会漏。
func TestImporterWhitelistFailsClosed(t *testing.T) {
	im := NewImporter(nil)
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix("10.68.1.0/24"), Add: true})
	rules := []ImportRule{{VPN: 0, Sources: []uint8{RouteBGP}}}
	if got := reported(t, im, rules); len(got) != 0 {
		t.Fatalf("空前缀表 prefixes = %v, want 0", got)
	}
}

func TestImporterWhitelistGELE(t *testing.T) {
	im := NewImporter(nil)
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix("10.0.0.0/8"), Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix("10.1.0.0/16"), Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix("10.1.2.0/24"), Add: true})
	rules := []ImportRule{{
		VPN: 0, Sources: []uint8{RouteBGP},
		Allow: []AllowEntry{{Prefix: netip.MustParsePrefix("10.0.0.0/8"), GE: 16, LE: 24}},
	}}
	if got := reported(t, im, rules); len(got) != 2 {
		t.Fatalf("prefixes = %v, want 10.1.0.0/16 与 10.1.2.0/24", got)
	}
}

// 防回环：本 VPN 正在导出的前缀不再上报，并计入 LoopDropped。
func TestImporterAntiLoop(t *testing.T) {
	im := NewImporter(nil)
	loop := netip.MustParsePrefix("10.68.1.0/24")
	other := netip.MustParsePrefix("10.68.2.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: loop, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: other, Add: true})
	exported := map[uint8]map[netip.Prefix]struct{}{0: {loop: {}}}
	ps, stats := im.Report([]ImportRule{allowAll(0)}, exported)
	if len(ps) != 1 || ps[0].Prefix != other {
		t.Fatalf("report = %v, want 只剩 %s", ps, other)
	}
	if stats.LoopDropped != 1 {
		t.Fatalf("LoopDropped = %d, want 1", stats.LoopDropped)
	}
}

// 多 VRF：vrf_id 是内核 ifindex，按映射归属到 VPN；没有映射的 id 丢弃并计数，
// 绝不并到 VPN 0。
func TestImporterMultiVRFAndUnknown(t *testing.T) {
	vmap := map[uint32]uint8{0: 0, 10: 100, 20: 200}
	im := NewImporter(func(id uint32) (uint8, bool) {
		v, ok := vmap[id]
		return v, ok
	})
	im.Handle(RouteEvent{VRFID: 0, Type: RouteBGP, Prefix: netip.MustParsePrefix("10.0.1.0/24"), Add: true})
	im.Handle(RouteEvent{VRFID: 10, Type: RouteBGP, Prefix: netip.MustParsePrefix("10.1.1.0/24"), Add: true})
	im.Handle(RouteEvent{VRFID: 20, Type: RouteOSPF, Prefix: netip.MustParsePrefix("10.2.1.0/24"), Add: true})
	im.Handle(RouteEvent{VRFID: 30, Type: RouteBGP, Prefix: netip.MustParsePrefix("10.3.1.0/24"), Add: true})
	im.Handle(RouteEvent{VRFID: 30, Type: RouteBGP, Prefix: netip.MustParsePrefix("10.3.2.0/24"), Add: true})

	rules := []ImportRule{
		allowAll(0, RouteBGP),
		allowAll(100, RouteBGP),
		allowAll(200, RouteOSPF),
	}
	ps, stats := im.Report(rules, nil)
	want := []struct {
		vpn uint8
		p   string
	}{{0, "10.0.1.0/24"}, {100, "10.1.1.0/24"}, {200, "10.2.1.0/24"}}
	if len(ps) != len(want) {
		t.Fatalf("report = %v, want %d 条", ps, len(want))
	}
	for i, w := range want {
		if ps[i].VPN != w.vpn || ps[i].Prefix.String() != w.p {
			t.Fatalf("report[%d] = vpn%d %s, want vpn%d %s", i, ps[i].VPN, ps[i].Prefix, w.vpn, w.p)
		}
	}
	if stats.UnknownVRF != 2 {
		t.Fatalf("UnknownVRF = %d, want 2", stats.UnknownVRF)
	}
}

func TestImporterSorts(t *testing.T) {
	im := NewImporter(nil)
	for _, p := range []string{"172.16.0.0/12", "10.68.1.0/24", "10.0.0.0/8"} {
		im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix(p), Add: true})
	}
	got := reported(t, im, []ImportRule{allowAll(0)})
	want := []string{"10.0.0.0/8", "10.68.1.0/24", "172.16.0.0/12"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prefixes = %v, want %v", got, want)
		}
	}
}

// Slice A #276: Handle and Reset are the only mutations to im.routes, and the
// delta loop is driven by the channel they signal on. A burst within the
// coalesce window must collapse to a single wake-up; otherwise the 10ms hold
// would produce one HTTP round-trip per notification and we are back to the
// per-tick behaviour we came here to remove.
func TestImporterDirtyCoalesces(t *testing.T) {
	im := NewImporter(nil)
	// Drain anything Reset might have primed (nothing on a fresh importer).
	select {
	case <-im.Dirty():
		t.Fatal("fresh importer signalled dirty")
	default:
	}

	p := netip.MustParsePrefix("10.68.1.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: false})
	im.Handle(RouteEvent{Type: RouteConnected, Prefix: p, Add: true})

	// At most one buffered signal survives, no matter how many Handles fired.
	<-im.Dirty()
	select {
	case <-im.Dirty():
		t.Fatal("dirty is not size 1 -- burst would re-wake the coalesce loop")
	default:
	}

	// Reset also fires so the reporter knows to publish the empty set right
	// after the reconnect (rather than wait for the next tick).
	im.Reset()
	select {
	case <-im.Dirty():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Reset did not signal dirty")
	}
}

// A Handle that only touched the source set without adding or removing a key
// still marks dirty (a DEL that leaves the prefix under another source is a
// real change to the visible set only if it hits the selected source, but we
// let the reconcile compute the diff and no-op if there isn't one). A Handle
// rejected at the gate (invalid prefix or unknown VRF) must NOT mark dirty --
// the customer-side set didn't change and there's no reason to wake sr-route.
func TestImporterDirtyOnlyOnTrackedMutation(t *testing.T) {
	im := NewImporter(func(vrfID uint32) (uint8, bool) { return 0, vrfID == 0 })
	// Drain initial state (there isn't one, but keep the test independent).
	select {
	case <-im.Dirty():
	default:
	}

	// Unknown VRF → dropped, no dirty.
	im.Handle(RouteEvent{VRFID: 99, Type: RouteBGP, Prefix: netip.MustParsePrefix("10.0.0.0/8"), Add: true})
	select {
	case <-im.Dirty():
		t.Fatal("dropped notification marked dirty")
	default:
	}

	// Invalid prefix → dropped, no dirty.
	im.Handle(RouteEvent{Type: RouteBGP, Add: true})
	select {
	case <-im.Dirty():
		t.Fatal("invalid prefix marked dirty")
	default:
	}

	// Tracked ADD → dirty.
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix("10.0.0.0/8"), Add: true})
	select {
	case <-im.Dirty():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("tracked ADD did not mark dirty")
	}
}
