package frr

import (
	"encoding/hex"
	"net/netip"
	"testing"
	"time"
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

	// 重连不再清空：旧集合继续对外可见，直到窗口关闭。
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
	im.BeginResync()
	if im.Len() != 1 {
		t.Fatalf("重连窗口里 Len = %d, want 1（旧集合要留着）", im.Len())
	}
}

// 重连窗口：旧前缀一直可见，直到回放安静下来；回放里没有的才在关门那一刻撤掉。
func TestImporterResyncKeepsOldSetUntilReplayQuiet(t *testing.T) {
	im, clock := newClockImporter()
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	p2 := netip.MustParsePrefix("172.16.1.0/24")
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
	im.Handle(RouteEvent{Prefix: p2, Tag: 1001, Add: true})

	im.BeginResync()
	if got := im.Prefixes(); len(got) != 2 {
		t.Fatalf("窗口刚打开 prefixes = %v, want 2 条旧集合", got)
	}

	// 回放只带回 p1，p2 客户真的撤了。
	clock.advance(100 * time.Millisecond)
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})

	// 回放之后 500ms 内不算完：p2 还在（宁可多留）。
	clock.advance(300 * time.Millisecond)
	im.Tick(clock.now())
	if got := im.Prefixes(); len(got) != 2 {
		t.Fatalf("回放未安静 prefixes = %v, want 2 条", got)
	}

	clock.advance(300 * time.Millisecond) // 距最后一条事件 600ms
	im.Tick(clock.now())
	got := im.Prefixes()
	if len(got) != 1 || got[0] != p1 {
		t.Fatalf("关窗后 prefixes = %v, want 只剩 %s", got, p1)
	}
	if len(im.stale) != 0 || im.resync {
		t.Fatalf("窗口没关干净：resync=%v stale=%d", im.resync, len(im.stale))
	}
}

// 回放一直不停（大表）时由 10s 上限收尾，而不是无限挂着旧集合。
func TestImporterResyncDeadline(t *testing.T) {
	im, clock := newClockImporter()
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	p2 := netip.MustParsePrefix("172.16.1.0/24")
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
	im.Handle(RouteEvent{Prefix: p2, Tag: 1001, Add: true})

	im.BeginResync()
	for i := 0; i < 20; i++ {
		clock.advance(600 * time.Millisecond)
		im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
		im.Tick(clock.now())
	}
	if im.resync {
		t.Fatal("超过 10s 上限后窗口还开着")
	}
	if got := im.Prefixes(); len(got) != 1 || got[0] != p1 {
		t.Fatalf("上限收尾 prefixes = %v, want 只剩 %s", got, p1)
	}
}

// 一条回放都没收到时不能当成"客户没有前缀"：dump 可能还在路上，早关窗等于把
// 整份前缀撤掉。要等到 10s 上限。
func TestImporterResyncWaitsForFirstEvent(t *testing.T) {
	im, clock := newClockImporter()
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})

	im.BeginResync()
	clock.advance(time.Second)
	im.Tick(clock.now())
	if got := im.Prefixes(); len(got) != 1 {
		t.Fatalf("还没有回放就把前缀撤了：%v", got)
	}

	clock.advance(10 * time.Second)
	im.Tick(clock.now())
	if im.Len() != 0 {
		t.Fatalf("10s 后仍没有回放，Len = %d, want 0", im.Len())
	}
}

// 第二次重连发生在第一段回放中间时，stale 必须还是最初那份好集合。
func TestImporterNestedResyncKeepsOriginalStale(t *testing.T) {
	im, clock := newClockImporter()
	p1 := netip.MustParsePrefix("10.68.1.0/24")
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})

	im.BeginResync()
	clock.advance(100 * time.Millisecond)
	im.Handle(RouteEvent{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Tag: 1001, Add: true})
	im.BeginResync() // 又断了一次，pending 的半份回放丢掉

	clock.advance(100 * time.Millisecond)
	im.Handle(RouteEvent{Prefix: p1, Tag: 1001, Add: true})
	clock.advance(600 * time.Millisecond)
	im.Tick(clock.now())

	got := im.Prefixes()
	if len(got) != 1 || got[0] != p1 {
		t.Fatalf("二次重连 prefixes = %v, want 只剩 %s", got, p1)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClockImporter() (*Importer, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	im := NewImporter(1001)
	im.Now = c.now
	im.Logf = func(string, ...any) {}
	return im, c
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
