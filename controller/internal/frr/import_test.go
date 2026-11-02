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

	// 重连不再清空：旧集合继续对外可见，直到窗口关闭。
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.BeginResync()
	if got := reported(t, im, rules); len(got) != 1 {
		t.Fatalf("重连窗口里 prefixes = %v, want 1（旧集合要留着）", got)
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

// 重连窗口：旧前缀一直可见，直到回放安静下来；回放里没有的才在关门那一刻撤掉。
func TestImporterResyncKeepsOldSetUntilReplayQuiet(t *testing.T) {
	im, clock := newClockImporter()
	rules := []ImportRule{allowAll(0)}
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	p2 := netip.MustParsePrefix("172.16.1.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: true})

	im.BeginResync()
	if got := reported(t, im, rules); len(got) != 2 {
		t.Fatalf("窗口刚打开 prefixes = %v, want 2 条旧集合", got)
	}

	// 回放只带回 p1，p2 客户真的撤了。
	clock.advance(100 * time.Millisecond)
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})

	// 回放之后 500ms 内不算完：p2 还在（宁可多留）。
	clock.advance(300 * time.Millisecond)
	im.Tick(clock.now())
	if got := reported(t, im, rules); len(got) != 2 {
		t.Fatalf("回放未安静 prefixes = %v, want 2 条", got)
	}

	clock.advance(300 * time.Millisecond) // 距最后一条事件 600ms
	im.Tick(clock.now())
	got := reported(t, im, rules)
	if len(got) != 1 || got[0] != p1.String() {
		t.Fatalf("关窗后 prefixes = %v, want 只剩 %s", got, p1)
	}
	if len(im.stale) != 0 || im.resync {
		t.Fatalf("窗口没关干净：resync=%v stale=%d", im.resync, len(im.stale))
	}
}

// 回放一直不停（大表）时由 10s 上限收尾，而不是无限挂着旧集合。
func TestImporterResyncDeadline(t *testing.T) {
	im, clock := newClockImporter()
	rules := []ImportRule{allowAll(0)}
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	p2 := netip.MustParsePrefix("172.16.1.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: true})

	im.BeginResync()
	for i := 0; i < 20; i++ {
		clock.advance(600 * time.Millisecond)
		im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
		im.Tick(clock.now())
	}
	if im.resync {
		t.Fatal("超过 10s 上限后窗口还开着")
	}
	if got := reported(t, im, rules); len(got) != 1 || got[0] != p1.String() {
		t.Fatalf("上限收尾 prefixes = %v, want 只剩 %s", got, p1)
	}
}

// 一条回放都没收到时不能当成"客户没有前缀"：dump 可能还在路上，早关窗等于把
// 整份前缀撤掉。要等到 10s 上限。
func TestImporterResyncWaitsForFirstEvent(t *testing.T) {
	im, clock := newClockImporter()
	rules := []ImportRule{allowAll(0)}
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})

	im.BeginResync()
	clock.advance(time.Second)
	im.Tick(clock.now())
	if got := reported(t, im, rules); len(got) != 1 {
		t.Fatalf("还没有回放就把前缀撤了：%v", got)
	}

	clock.advance(10 * time.Second)
	im.Tick(clock.now())
	if got := reported(t, im, rules); len(got) != 0 {
		t.Fatalf("10s 后仍没有回放，prefixes = %v, want 0", got)
	}
}

// 第二次重连发生在第一段回放中间时，stale 必须还是最初那份好集合。
func TestImporterNestedResyncKeepsOriginalStale(t *testing.T) {
	im, clock := newClockImporter()
	rules := []ImportRule{allowAll(0)}
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})

	im.BeginResync()
	clock.advance(100 * time.Millisecond)
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: netip.MustParsePrefix("10.9.0.0/16"), Add: true})
	im.BeginResync() // 又断了一次，pending 的半份回放丢掉

	clock.advance(100 * time.Millisecond)
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	clock.advance(600 * time.Millisecond)
	im.Tick(clock.now())

	got := reported(t, im, rules)
	if len(got) != 1 || got[0] != p1.String() {
		t.Fatalf("二次重连 prefixes = %v, want 只剩 %s", got, p1)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClockImporter() (*Importer, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	im := NewImporter(nil)
	im.Now = c.now
	im.Logf = func(string, ...any) {}
	return im, c
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
