package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// 这一组守的是 2026-10-30 实测出来的那个坑（docs/overlay-design-draft.md §2.8）：
// VPP 的 MPLS 开关按 sw_if_index 记引用计数、只在 0↔1 翻转时才真的动 feature 弧，
// 而**计数不随接口删除清零**。一张开过 MPLS 的口被直接删掉后，这个号带着残计数交给
// 下一条重建到同一号上的隧道：enable 回成功、`show mpls interface` 照样把它列成
// "MPLS enabled"（那张表读的是计数），mpls-input 弧上却还挂着 mpls-not-enabled，
// 于是带标签的 VPN 流量在收端全丢，而 overlay 的纯 IPv4 照 ping —— 现场看起来像
// "ECMP 坏了"或"配置没下发"。
//
// 对策两条，缺一不可，假数据面把 VPP 的计数字性照抄了（见 fakeVPP.mplsCount），
// 所以这两条都有真回归测试，不是把结果摆进 map 里假装对：
//
//  (b) 撤隧道时**先 disable 再删口** —— 防住，让计数回到 0 才交出这个号；
//  (a) 开完 MPLS 后向**弧本身**核验（vppapi.MPLSEnabled：CLI 读
//      `show interface features` 的 mpls-input 段）—— 兜住外部残留
//      （操作员手敲 vppctl、别的进程删的口），实测没有任何事后补救能救回一个
//      已中毒的 index，只有重启 VPP，所以这里只能拒绝把路由装上去并报错。

// syncOneTunnel 装一条隧道并把 Sync 跑通，返回它的 sw_if_index。
func syncOneTunnel(t *testing.T, f *fakeVPP, pki *testPKI, port uint16) (uint32, *TunnelSync, wire.UnderlayBundle) {
	t.Helper()
	spec := tunnelSpec(t, pki, "t1", 3, 1001, port,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)
	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(f.ifaces) != 1 {
		t.Fatalf("应当正好一条隧道接口: %+v", f.ifaces)
	}
	return f.ifaces[0].SWIfIndex, ts, b
}

// swapBundle 把 reconciler 的 fetch 换成"只有这些隧道、版本是 ver"的快照。
func swapBundle(ts *TunnelSync, ver int, tunnels ...wire.TunnelSpec) wire.UnderlayBundle {
	b := testBundle(tunnels...)
	b.Version = ver
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) { return b, nil }
	return b
}

// TestTeardownDisablesMPLSBeforeDeletingInterface 查 (b)：撤隧道路径里那句
// disable 必须**排在删口之前**，而且撤完那份计数得是 0（0 才算把这个号干净交出去）。
func TestTeardownDisablesMPLSBeforeDeletingInterface(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	swIf, ts, b := syncOneTunnel(t, f, pki, 51820)

	if n := f.mplsCount[swIf]; n != 1 {
		t.Fatalf("装上之后 MPLS 计数应当是 1，实际 %d", n)
	}

	b2 := swapBundle(ts, b.Version+1)
	if err := ts.Sync(context.Background(), b2.Version); err != nil {
		t.Fatalf("Sync(空快照): %v", err)
	}

	disable := callIndex(f.calls, "SetInterfaceMPLS "+strconv.FormatUint(uint64(swIf), 10)+"=false")
	del := callIndex(f.calls, "DeleteInterface "+strconv.FormatUint(uint64(swIf), 10))
	switch {
	case disable < 0:
		t.Fatalf("撤隧道 %d 时没有先关 MPLS（调用记录 %v）", swIf, f.calls)
	case del < 0:
		t.Fatalf("没找到 DeleteInterface %d 这条调用", swIf)
	case disable > del:
		t.Errorf("disable 排在了删口之后（第 %d 条 vs 第 %d 条）：这个号会带着残计数交出去", disable, del)
	}
	if n := f.mplsCount[swIf]; n != 0 {
		t.Errorf("撤完之后 sw_if_index %d 的 MPLS 计数应当归 0，实际 %d —— 非 0 就是给下一条隧道下毒", swIf, n)
	}
}

// TestRebuiltTunnelOnReusedIndexHasCleanMPLSArc 是 (b) 的正面证据，也就是这次要的
// "workaround 真的workaround"：撤掉一条隧道、把 sw_if_index 复用给下一条（真 VPP
// 就是这样分配号），新隧道装完之后弧必须是干净的。
//
// 撤隧道路径里那句"删口前先 disable"如果被删掉，这里的 Sync 会立刻失败在
// finishOverlay 的弧核验上（下一条测试就是那个状态），所以这条测试守的是防住本身。
func TestRebuiltTunnelOnReusedIndexHasCleanMPLSArc(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	swIf, ts, b := syncOneTunnel(t, f, pki, 51820)

	b2 := swapBundle(ts, b.Version+1) // 撤掉
	if err := ts.Sync(context.Background(), b2.Version); err != nil {
		t.Fatalf("Sync(空快照): %v", err)
	}
	// 真 VPP 会把这个空出来的号复用给下一个新建接口；假数据面按 nextIdx 单调走，
	// 这里手动把号退回去，模拟的正是"同一条隧道换了端口重建、拿到了同一个号"。
	f.nextIdx = swIf

	b3 := swapBundle(ts, b2.Version+1,
		tunnelSpec(t, pki, "t1", 3, 1001, 51830,
			"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51830"))
	if err := ts.Sync(context.Background(), b3.Version); err != nil {
		t.Fatalf("复用同一个 sw_if_index 重建隧道应当成功（删口前已经 disable，计数是 0）: %v", err)
	}
	if st := ts.State(); !st.OK {
		t.Fatalf("复用同一个号重建隧道没装上（这就是那个坑：enable 静默空操作）: %s", st.Error)
	}
	if len(f.ifaces) != 1 || f.ifaces[0].SWIfIndex != swIf {
		t.Fatalf("重建的隧道没拿到复用的号 %d: %+v", swIf, f.ifaces)
	}
	if !f.mpls[swIf] {
		t.Errorf("重建的隧道上 mpls-input 弧还挂着 mpls-not-enabled：%d 开不了 MPLS，带标签的流量会全丢", swIf)
	}
	if n := f.mplsCount[swIf]; n != 1 {
		t.Errorf("重建之后 MPLS 计数应当正好 1（enable 一次），实际 %d", n)
	}
	// 核验这一步必须真的发生：没有它，上面两个断言就只是"我们以为开了"。
	if callIndex(f.calls, "MPLSEnabled "+strconv.FormatUint(uint64(swIf), 10)) < 0 {
		t.Errorf("装完隧道后没有向弧本身核验（调用记录 %v）", f.calls)
	}
}

// TestOpenRefusesTunnelWithInheritedMPLSCount 是 (a)：号是从外部（手敲 vppctl 删口、
// 别的进程）带着残计数继承来的，enable 是静默空操作，Open 必须**拒绝**这条隧道并把
// 原因说清楚，而不是装好路由等一个只有带标签流量才会暴露的黑洞。
func TestOpenRefusesTunnelWithInheritedMPLSCount(t *testing.T) {
	ctx := context.Background()
	f := newFakeVPP()

	// 先按"操作员手敲"的样子把这个号毒了：建口 → 开 MPLS → 直接删口（没有 disable）。
	poisoned, err := f.CreateInterface(ctx, vppapi.InterfaceParams{
		UserInstance: 7777, ListenPort: 60000, SrcIP: net.IPv4(10, 10, 1, 2).To4(),
		PrivateKeys: randomBytes(t, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetInterfaceMPLS(ctx, poisoned, true); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteInterface(ctx, poisoned); err != nil {
		t.Fatal(err)
	}
	if n := f.mplsCount[poisoned]; n != 1 {
		t.Fatalf("构造失败：直接删口应当留下残计数，实际 %d", n)
	}
	// 复用这个号（真 VPP 的行为）。
	f.nextIdx = poisoned

	l := newOverlayLink(t, f)
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	_, err = l.Open(ctx)
	if err == nil {
		t.Fatalf("在中毒的号 %d 上开隧道应当报错，实际成功（mpls=%v）", poisoned, f.mpls[poisoned])
	}
	if !strings.Contains(err.Error(), "开不了 MPLS") ||
		!strings.Contains(err.Error(), featMPLSNotEnabled) {
		t.Errorf("报错没说清是 MPLS 弧的事: %v", err)
	}
	if l.Ready() {
		t.Errorf("Open 失败了 Link 却自认为 ready")
	}
	// 关键：拒绝发生在"弧还挂着丢弃路径"这一步，所以不能把这条口当成可用的继续用下去。
	if f.mpls[poisoned] {
		t.Errorf("弧上明明还没摘掉 %s，读回来的状态却成了 enabled —— 假数据面不忠了", featMPLSNotEnabled)
	}
}

// TestTeardownKeepsInterfaceWhenMPLSDisableFails 查 (b) 的后半句：disable 失败就
// **不删口**。带着残计数删掉它，等于把这个 sw_if_index 永久毒给下一条重建到同号上
// 的隧道（实测没有任何事后补救能救回），那比"这一条暂时撤不掉"难查得多。
func TestTeardownKeepsInterfaceWhenMPLSDisableFails(t *testing.T) {
	ctx := context.Background()
	f := newFakeVPP()
	pki := newTestPKI(t)
	swIf, ts, b := syncOneTunnel(t, f, pki, 51820)

	b2 := swapBundle(ts, b.Version+1)
	f.mplsErr = errors.New("vppapi: sw_interface_set_mpls_enable 失败")
	if err := ts.Sync(ctx, b2.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := ts.State()
	if st.OK || !strings.Contains(st.Error, "关不掉 MPLS") {
		t.Errorf("disable 失败必须让这一轮不收敛并把原因说出来: %+v", st)
	}
	if len(f.ifaces) != 1 || f.ifaces[0].SWIfIndex != swIf {
		t.Errorf("disable 没成就该留着这张口（不能把残计数交出去）: %+v", f.ifaces)
	}
	if n := f.mplsCount[swIf]; n != 1 {
		t.Errorf("接口还在，MPLS 计数也应当是 1，实际 %d", n)
	}

	// 报错之后重试要能收口：清掉故障再 Sync 一次，这条隧道才真正撤掉、计数才归 0。
	f.mplsErr = nil
	if err := ts.Sync(ctx, b2.Version); err != nil {
		t.Fatalf("重试 Sync: %v", err)
	}
	if st := ts.State(); !st.OK {
		t.Errorf("重试之后应当收敛: %+v", st)
	}
	if len(f.ifaces) != 0 {
		t.Errorf("重试后接口还没撤干净: %+v", f.ifaces)
	}
	if n := f.mplsCount[swIf]; n != 0 {
		t.Errorf("重试后 sw_if_index %d 的计数应当归 0，实际 %d", swIf, n)
	}
}

// newOverlayLink 造一条"走 overlay 模型"的 Link：配置在 NewLink 之前就是最终形状
// （和 TunnelSync 装隧道时一样），不在 Link 建好之后再改 cfg。
func newOverlayLink(t *testing.T, f *fakeVPP) *Link {
	t.Helper()
	cfg := testConfig(t, f, newTestPKI(t))
	cfg.OverlayTable = wire.OverlayRouteTable
	cfg.LocalOverlay = netip.MustParsePrefix("10.250.0.1/32")
	l, err := NewLink(cfg)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	return l
}

// callIndex 返回第一条**整行等于** want 的调用在记录里的位置，没有则 -1。
// 要整行相等而不是前缀匹配：前缀 "DeleteInterface 7" 会把 "DeleteInterface 71" 也算进来。
func callIndex(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}
