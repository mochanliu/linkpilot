package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// ---------------- 构造 ----------------

// testIdentity 造一台已经入网的 CPE 身份：证书与本包验对端时用的是同一份 CA，
// 所以测试里的"验过了"是真的过了证书链与 URI 校验。
func testIdentity(t *testing.T, pki *testPKI, tenant string, node uint16) *Identity {
	t.Helper()
	id, err := LoadOrCreate(t.TempDir(), "serial-test")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	certPEM := pki.issueDevice(t, id.IdentityPublicKey(), tenant, node)
	if err := id.SetEnrolled(tenant, node, certPEM, pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	return id
}

func testPKeys() []wire.PKey {
	return []wire.PKey{{HmacID: 9001, Key: "00112233445566778899aabbccddeeff"}}
}

// tunnelSpec 造一条"能装上"的隧道规格：出口是真机名字（fake 里那张 host-eth0）、
// 端口/实例/overlay 都按控制器的派生规则来。默认用密钥槽 0（与 peerMaterial 的担保
// 同号），要测别的槽就在返回的规格上改 Slot 并换一份同号的对端担保。
func tunnelSpec(t *testing.T, pki *testPKI, id string, peerNode uint16, instance uint32,
	port uint16, localOverlay, peerOverlay, peerEndpoint string) wire.TunnelSpec {

	t.Helper()
	certPEM, binding, _ := peerMaterial(t, pki, "t1", peerNode)
	return wire.TunnelSpec{
		TunnelID: id, PeerNodeID: peerNode, Slot: 0,
		Instance: instance, PathIDBase: wire.UnderlayPathIDBase,
		Iface: "host-eth0", SrcIP: "10.10.1.2",
		ListenPort: port, PeerEndpoint: peerEndpoint,
		LocalOverlay: localOverlay, PeerOverlay: peerOverlay,
		PeerCertPEM: certPEM, PeerBinding: binding,
		MTU: wire.DefaultTunnelMTU, UnderlayMTU: 1500,
	}
}

func testBundle(tunnels ...wire.TunnelSpec) wire.UnderlayBundle {
	return wire.UnderlayBundle{
		TenantID: "t1", NodeID: 1, Version: 3,
		OverlayTable: wire.OverlayRouteTable,
		PKeys:        testPKeys(),
		Tunnels:      tunnels,
	}
}

// newTunnelSync 造一个"总是能拉到 b"的 reconciler；fetch 可以被调用方换掉。
// pki 必须与签对端证书的那份是同一份 —— 本机身份 pin 的就是它。
func newTunnelSync(t *testing.T, f *fakeVPP, pki *testPKI,
	b wire.UnderlayBundle) (*TunnelSync, *Identity) {
	t.Helper()
	id := testIdentity(t, pki, "t1", 1)
	ts := NewTunnelSync(f, id, func(context.Context) (wire.UnderlayBundle, error) {
		return b, nil
	}, func(string, ...any) {})
	return ts, id
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// ---------------- 测试 ----------------

func TestTunnelSyncInstallsAndIsIdempotent(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := ts.State()
	if !st.OK || st.Version != 3 || st.Tunnels != 1 || st.Skipped != 0 || st.Error != "" {
		t.Fatalf("装上之后的状态不对: %+v", st)
	}
	if len(f.ifaces) != 1 || f.ifaces[0].Port != 51820 || f.ifaces[0].UserInstance != 1001 {
		t.Fatalf("隧道接口不对: %+v", f.ifaces)
	}
	if len(f.peers) != 1 {
		t.Fatalf("应当正好装一个 peer: %+v", f.peers)
	}
	// overlay 三件套：地址、表、路由。
	swIf := f.ifaces[0].SWIfIndex
	want := netip.MustParsePrefix("10.250.0.1/32")
	if got := f.addrs[swIf]; len(got) != 1 || got[0] != want {
		t.Errorf("隧道接口上的地址不对: %v", got)
	}
	if f.ifTable[swIf] != wire.OverlayRouteTable {
		t.Errorf("隧道接口不在 overlay 表里: %v", f.ifTable)
	}
	if _, ok := f.routes[wire.OverlayRouteTable][netip.MustParsePrefix("10.250.0.3/32")]; !ok {
		t.Errorf("对端 overlay 的路由没装上: %v", f.routes)
	}
	// 多跳 BFD 的发包查表跟着 local0 走：不绑它，会话查的是 table 0（客户 VPN 0，
	// 那份对端 /32 是 dpo-drop），隧道会一直被 sr-fib 判成不可达。
	if f.ifTable[localSwIfIndex] != wire.OverlayRouteTable {
		t.Errorf("local0 没绑到 overlay 表（多跳 BFD 的查表路径）: %v", f.ifTable)
	}
	// 会话按隧道 overlay /32 建，而且必须是多跳：绑接口会把 overlay 路由打成
	// dpo-drop（见 internal/vppapi/telemetry.go 文件头）。
	if len(f.bfds) != 1 {
		t.Fatalf("BFD 会话 %d 条，期望 1: %+v", len(f.bfds), f.bfds)
	}
	for _, s := range f.bfds {
		if s.SwIfIndex != vppapi.BFDMultihopSwIfIndex {
			t.Errorf("BFD 会话不是多跳: sw_if_index=%d", s.SwIfIndex)
		}
		if !s.LocalIP.Equal(net.ParseIP("10.250.0.1")) || !s.PeerIP.Equal(net.ParseIP("10.250.0.3")) {
			t.Errorf("BFD 会话地址 = %v -> %v，期望隧道两端的 overlay /32", s.LocalIP, s.PeerIP)
		}
	}
	if p, ok := f.probes[swIf]; !ok || !p.PeerIP.Equal(net.ParseIP("10.250.0.3")) {
		t.Errorf("隧道口 %d 上没有探针: %+v", swIf, f.probes)
	}

	// 再跑一遍：接口、地址、peer 都不能重做（重复加地址在真机上没定义，fake 直接 panic）。
	before := len(f.calls)
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatalf("第二次 Sync: %v", err)
	}
	if n := len(f.callWith("CreateInterface")); n != 1 {
		t.Errorf("第二次 Sync 又建接口了（共 %d 次）", n)
	}
	second := f.calls[before:]
	for _, c := range second {
		if strings.HasPrefix(c, "AddPeer") || strings.HasPrefix(c, "CreateInterface") ||
			strings.HasPrefix(c, "AddDelInterfaceAddress") || strings.HasPrefix(c, "SetInterfaceIPTable") ||
			strings.HasPrefix(c, "AddBFD") {
			t.Errorf("幂等重放里出现了写操作: %s（这一段调用 %v）", c, second)
		}
	}
	if st := ts.State(); !st.OK || st.Tunnels != 1 {
		t.Errorf("第二次之后状态不对: %+v", st)
	}
}

func TestTunnelSyncKeepsSkippedTunnelRunning(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}
	if len(f.ifaces) != 1 {
		t.Fatalf("前置条件不成立: %+v", f.ifaces)
	}

	// 下一条快照里这条隧道变成 skipped（比如对端观测过期）。已经装上的那条不许
	// 被拆：一个暂时的观测缺失不是拆一条在跑隧道的理由。
	b2 := b
	b2.Version = 4
	b2.Tunnels = nil
	b2.Skipped = []wire.TunnelSkip{{TunnelID: "t1", PeerNodeID: 3, Reason: "对端外层地址过期"}}
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) { return b2, nil }
	if err := ts.Sync(ctx, b2.Version); err != nil {
		t.Fatalf("Sync(skipped): %v", err)
	}
	if got := f.callWith("DeleteInterface"); len(got) != 0 {
		t.Errorf("skipped 的隧道被拆了: %v", got)
	}
	if len(f.ifaces) != 1 || len(f.peers) != 1 {
		t.Errorf("skipped 之后接口/peer 不该动: ifaces=%+v peers=%+v", f.ifaces, f.peers)
	}
	st := ts.State()
	if !st.OK || st.Skipped != 1 || st.Tunnels != 0 {
		t.Errorf("skipped 的回报不对: %+v", st)
	}
}

func TestTunnelSyncTearsDownRemovedTunnel(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}
	swIf := f.ifaces[0].SWIfIndex

	b2 := b
	b2.Version = 5
	b2.Tunnels = nil
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) { return b2, nil }
	if err := ts.Sync(ctx, b2.Version); err != nil {
		t.Fatalf("Sync(空快照): %v", err)
	}
	if len(f.ifaces) != 0 || len(f.peers) != 0 || len(f.tunnels) != 0 || len(f.paths) != 0 {
		t.Errorf("撤得不干净: ifaces=%+v peers=%+v tunnels=%+v paths=%+v",
			f.ifaces, f.peers, f.tunnels, f.paths)
	}
	if _, ok := f.addrs[swIf]; ok {
		t.Errorf("接口都删了地址还留着: %v", f.addrs)
	}
	if _, ok := f.routes[wire.OverlayRouteTable][netip.MustParsePrefix("10.250.0.3/32")]; ok {
		t.Errorf("对端 overlay 路由没撤: %v", f.routes)
	}
	// 遥测必须跟着隧道一起撤：BFD 会话不带隧道身份（多跳只有一对地址），留着它
	// 就是一条永远没人再读的孤儿会话，还会让 sr-fib 对着一个不存在的对端报可达。
	if len(f.bfds) != 0 {
		t.Errorf("隧道撤了 BFD 会话还在: %+v", f.bfds)
	}
	if _, ok := f.probes[swIf]; ok {
		t.Errorf("隧道撤了探针还在: %+v", f.probes)
	}
	// 撤回一条隧道要把它自己的监听端口从 vpn 集合里摘掉，否则这个号被 sr-wg 占着。
	for _, p := range f.ports {
		if p.Kind == vppapi.PortKindVPN && p.Port == 51820 {
			t.Errorf("端口 51820 还留在 vpn 集合里: %v", f.ports)
		}
	}
	if len(ts.Links()) != 0 {
		t.Errorf("Link 还记着已删的隧道: %v", ts.Links())
	}
}

func TestTunnelSyncSweepsOnlyAgentOwnedOrphans(t *testing.T) {
	f := newFakeVPP()
	b := testBundle() // 快照里一条隧道都没有：上次在这里建过的都成了孤儿
	ts, _ := newTunnelSync(t, f, newTestPKI(t), b)
	ctx := context.Background()

	// 两把身份：一把是 agent 自己上次建的（实例号在 agent 的段位里），一把是
	// 操作员手配的（实例号 7 —— 实验室手敲 bringup.sh 就是这么写的）。
	if _, err := f.CreateInterface(ctx, vppapi.InterfaceParams{
		UserInstance: wire.UnderlayInstanceBase + 200, ListenPort: 51890,
		PrivateKeys: mustWGKey(t), SrcIP: []byte{10, 10, 1, 2},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateInterface(ctx, vppapi.InterfaceParams{
		UserInstance: 7, ListenPort: 51820,
		PrivateKeys: mustWGKey(t), SrcIP: []byte{10, 10, 1, 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var kept []uint32
	for _, i := range f.ifaces {
		kept = append(kept, i.UserInstance)
	}
	if len(kept) != 1 || kept[0] != 7 {
		t.Fatalf("孤儿清理应只删 agent 自己那一段（≥%d），实际留下 %v",
			wire.UnderlayInstanceBase, kept)
	}
	if st := ts.State(); !st.OK {
		t.Errorf("清理孤儿不该报成失败: %+v", st)
	}
}

func TestTunnelSyncRebuildsWhenFixedInputChanges(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}
	oldSWIf := f.ifaces[0].SWIfIndex

	// 端口是接口身份的一部分（VPP 改不了，只能删了重来）。
	b2 := b
	b2.Version = 9
	b2.Tunnels = []wire.TunnelSpec{spec}
	b2.Tunnels[0].ListenPort = 51830
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) { return b2, nil }
	if err := ts.Sync(ctx, b2.Version); err != nil {
		t.Fatalf("Sync(换端口): %v", err)
	}
	if len(f.ifaces) != 1 || f.ifaces[0].Port != 51830 {
		t.Fatalf("没按新端口重建: %+v", f.ifaces)
	}
	if f.ifaces[0].SWIfIndex == oldSWIf {
		t.Errorf("新接口复用了旧接口号 %d —— 旧的那张没删掉？", oldSWIf)
	}
	if got := f.callWith("DeleteInterface"); len(got) != 1 {
		t.Errorf("重建应当只删一次旧接口: %v", got)
	}
	// 旧端口要摘掉，新端口要在集合里。
	var vpn []uint16
	for _, p := range f.ports {
		if p.Kind == vppapi.PortKindVPN {
			vpn = append(vpn, p.Port)
		}
	}
	if len(vpn) != 1 || vpn[0] != 51830 {
		t.Errorf("vpn 端口集合不对（旧端口该摘掉、新端口该登记）: %v", vpn)
	}
}

func TestTunnelSyncReportsPerTunnelErrorWithoutStoppingOthers(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	good := tunnelSpec(t, pki, "t-good", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	bad := tunnelSpec(t, pki, "t-bad", 4, 1002, 51840,
		"10.250.0.2/32", "10.250.0.4/32", "10.10.1.4:51840")
	bad.Iface = "host-eth9" // 这台设备上没有这张口
	b := testBundle(good, bad)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	// 单条隧道的错误不该让整次 reconcile 失败：好的那条确实收敛了，那是对
	// 这台设备的事实，不能被另一条的失败吞掉。
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatalf("单隧道错误被当成整次失败: %v", err)
	}
	st := ts.State()
	if st.OK || st.Tunnels != 1 {
		t.Fatalf("状态应报不完整但记下装上的一条: %+v", st)
	}
	if !strings.Contains(st.Error, "t-bad") || !strings.Contains(st.Error, "host-eth9") {
		t.Errorf("错误里应点名是哪条隧道、哪个接口: %q", st.Error)
	}
	if len(f.peers) != 1 {
		t.Errorf("好的那条应当装上: %+v", f.peers)
	}
}

func TestTunnelSyncRejectsForeignBundle(t *testing.T) {
	f := newFakeVPP()
	b := testBundle()
	b.NodeID = 42
	ts, _ := newTunnelSync(t, f, newTestPKI(t), b)
	err := ts.Sync(context.Background(), b.Version)
	if err == nil || !strings.Contains(err.Error(), "node 42") {
		t.Fatalf("发给别的节点的包必须被拒: %v", err)
	}
	if st := ts.State(); st.OK || !strings.Contains(st.Error, "node 42") {
		t.Errorf("拒收要如实回报: %+v", st)
	}
	if f.wroteAnything() {
		t.Errorf("拒收之前不许动数据面: %v", f.calls)
	}
}

func TestTunnelSyncNoUnderlayKeepsLinksRunning(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)
	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}

	// 心跳说还有版本，但控制器回 409（发布被撤了 / 状态没加载）：这不是"把隧道
	// 全拆了"的理由 —— 快照内容未知，已知的那条还在跑。
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) {
		return wire.UnderlayBundle{}, ErrNoUnderlay
	}
	if err := ts.Sync(ctx, 6); err == nil {
		t.Fatal("409 应当报成错误")
	}
	if len(f.ifaces) != 1 || len(f.peers) != 1 {
		t.Errorf("409 不该拆任何东西: ifaces=%+v peers=%+v", f.ifaces, f.peers)
	}
	st := ts.State()
	if st.OK || !strings.Contains(st.Error, "409") {
		t.Errorf("409 的回报不对: %+v", st)
	}
}

// A VPP restart kills the api socket and takes its whole config with it. The
// reconciler must drop the Link objects (their sw_if_index belongs to a dead
// connection), reconnect, and rebuild from the snapshot -- not keep retrying
// against a socket that will never answer again.
func TestTunnelSyncRedialsAfterDeadConnection(t *testing.T) {
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)
	first, second := newFakeVPP(), newFakeVPP()

	id := testIdentity(t, pki, "t1", 1)
	ts := NewTunnelSync(first, id, func(context.Context) (wire.UnderlayBundle, error) {
		return b, nil
	}, func(string, ...any) {})
	var dials atomic.Int32
	ts.SetRedial(func(context.Context) (DataPlane, error) {
		dials.Add(1)
		return second, nil
	})
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}
	if ts.DataPlane() != DataPlane(first) || len(ts.Links()) != 1 {
		t.Fatalf("第一次收敛后应当是连着第一份 fake：dp=%v links=%d", ts.DataPlane(), len(ts.Links()))
	}

	// VPP 重启：下一次拉取拿到的是"连接已断"。
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) {
		return wire.UnderlayBundle{}, vppapi.ErrClosed
	}
	if err := ts.Sync(ctx, b.Version); err == nil {
		t.Fatal("连接断了应当报错")
	}
	if ts.DataPlane() != nil {
		t.Errorf("连接断了还留着旧句柄: %v", ts.DataPlane())
	}
	if n := len(ts.Links()); n != 0 {
		t.Errorf("死连接上的 Link 必须丢掉（它们的 sw_if_index 属于旧连接），还剩 %d", n)
	}

	// 下一轮重连并按快照从头收敛。
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) { return b, nil }
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Errorf("应当只重连一次，实际 %d", dials.Load())
	}
	if ts.DataPlane() != DataPlane(second) || len(ts.Links()) != 1 {
		t.Fatalf("重连后没收敛到新连接：dp=%v links=%d", ts.DataPlane(), len(ts.Links()))
	}
	if len(second.ifaces) != 1 || len(second.peers) != 1 {
		t.Errorf("新连接上应当重新建好了接口与 peer: ifaces=%+v peers=%+v", second.ifaces, second.peers)
	}
}

func TestTunnelSyncKickIgnoresUnpublishedVersion(t *testing.T) {
	f := newFakeVPP()
	var calls atomic.Int32
	id := testIdentity(t, newTestPKI(t), "t1", 1)
	ts := NewTunnelSync(f, id, func(context.Context) (wire.UnderlayBundle, error) {
		calls.Add(1)
		return testBundle(), nil
	}, nil)
	ts.Kick(context.Background(), 0)
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("desired=0 不该去拉配置（拉了 %d 次）", n)
	}
}

func TestTunnelSyncKickBacksOffAfterFailure(t *testing.T) {
	f := newFakeVPP()
	var calls atomic.Int32
	id := testIdentity(t, newTestPKI(t), "t1", 1)
	ts := NewTunnelSync(f, id, func(context.Context) (wire.UnderlayBundle, error) {
		calls.Add(1)
		return wire.UnderlayBundle{}, errors.New("控制器连不上")
	}, nil)

	ctx := context.Background()
	ts.Kick(ctx, 3)
	waitFor(t, "第一次失败被记下", func() bool {
		st := ts.State()
		return st.Error != "" && !st.Applying
	})
	if n := calls.Load(); n != 1 {
		t.Fatalf("第一次 Kick 应拉一次，实际 %d", n)
	}
	// 退避窗口内再 Kick 不许再拉：控制器躺在那里时，每 15 秒一次心跳都去敲它
	// 没有意义，只会把日志刷满。
	ts.Kick(ctx, 3)
	time.Sleep(30 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("退避窗口内又拉了 %d 次", n-1)
	}
	// 窗口过去之后（把时钟往前拨）才重试。
	ts.mu.Lock()
	ts.nextTry = time.Now().Add(-time.Second)
	ts.mu.Unlock()
	ts.Kick(ctx, 3)
	waitFor(t, "退避结束后重试", func() bool { return calls.Load() == 2 })
}

// A bundle whose spec the dataplane refuses is a partial delivery: Sync returns
// nil (the other tunnels did land) and only state.OK says no. Saying "已收敛" to
// that is what let the first lab run of a controller-managed tunnel hide -- the
// agent log claimed convergence every heartbeat while VPP had no interface at all.
func TestTunnelSyncKickReportsPartialAsNotConverged(t *testing.T) {
	pki := newTestPKI(t)
	f := newFakeVPP()
	bad := tunnelSpec(t, pki, "t1", 3, 1001, 20001, "10.250.0.1", "10.250.0.3/32", "10.10.1.3:20002")
	good := tunnelSpec(t, pki, "t2", 4, 1002, 20003, "10.250.0.2/32", "10.250.0.4/32", "10.10.1.3:20004")
	b := testBundle(bad, good)

	var calls atomic.Int32
	var lg sync.Mutex
	var logged []string
	id := testIdentity(t, pki, "t1", 1)
	ts := NewTunnelSync(f, id, func(context.Context) (wire.UnderlayBundle, error) {
		calls.Add(1)
		return b, nil
	}, func(format string, args ...any) {
		lg.Lock()
		defer lg.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})

	ctx := context.Background()
	ts.Kick(ctx, b.Version)
	waitFor(t, "部分失败被记下", func() bool {
		st := ts.State()
		return !st.Applying && !st.OK && st.Error != ""
	})
	if n := len(f.ifaces); n != 1 {
		t.Errorf("只应装上那条好的隧道，接口表有 %d 个: %+v", n, f.ifaces)
	}
	waitFor(t, "日志说没全装上", func() bool {
		lg.Lock()
		defer lg.Unlock()
		for _, s := range logged {
			if strings.Contains(s, "只装上一部分") {
				return true
			}
		}
		return false
	})
	// The backoff applies to a partial failure too: without it every heartbeat
	// re-ran the same refusing spec.
	callsBefore := calls.Load()
	ts.Kick(ctx, b.Version)
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != callsBefore {
		t.Fatalf("退避窗口内又去拉了配置（%d -> %d）", callsBefore, calls.Load())
	}
}

// 槽号必须真的走到数据面：控制器在规格里点名第 n 把，agent 就把那一把交给 VPP 建口。
// 故意用槽 3 —— 槽 0 恰好等于"这个字段不存在时用的那把"，拿它测不出字段有没有被用上。
func TestTunnelSyncInstallsKeySlotFromSpec(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	const slot = 3
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	spec.Slot = slot
	certPEM, b3, _ := peerMaterialSlot(t, pki, "t1", 3, slot)
	spec.PeerCertPEM, spec.PeerBinding = certPEM, b3
	b := testBundle(spec)

	ts, id := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if st := ts.State(); !st.OK {
		t.Fatalf("槽 %d 的隧道没装上: %s", slot, st.Error)
	}
	if len(f.ifaces) != 1 {
		t.Fatalf("接口表 %+v", f.ifaces)
	}
	want, err := id.WGPublicKey(slot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.ifaces[0].PublicKey, want) {
		t.Errorf("隧道接口上装的不是槽 %d 的那把密钥", slot)
	}
	slot0, err := id.WGPublicKey(0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(f.ifaces[0].PublicKey, slot0) {
		t.Error("规格里写槽 3、接口上却是槽 0 的密钥：spec.Slot 根本没被用上")
	}
}

// 规格里写密钥槽 3、转给本端的对端担保却是槽 2：这两处的来源不一致，装上去
// 等于"这条隧道用的其实是别条隧道的密钥"。整条拒绝，并且在跑的其它隧道不动。
func TestTunnelSyncRefusesSpecWithMismatchedBindingSlot(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	spec.Slot = 3
	certPEM, b2, _ := peerMaterialSlot(t, pki, "t1", 3, 2)
	spec.PeerCertPEM, spec.PeerBinding = certPEM, b2
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := ts.State()
	if st.OK {
		t.Fatal("担保槽号与规格槽号不一致，却仍然报收敛")
	}
	if !strings.Contains(st.Error, "密钥槽") {
		t.Errorf("错误没点出是槽号不一致: %s", st.Error)
	}
	if len(f.ifaces) != 0 {
		t.Errorf("被拒绝的规格不该留下接口: %+v", f.ifaces)
	}
}

// 越界的槽号同样只砸这一条：spec.Slot 是从控制器下发的字符串里来的外部输入，
// agent 是长期进程，一个坏字段不该把已经在转发的隧道一起带走。
func TestTunnelSyncRefusesOutOfRangeKeySlot(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	bad := tunnelSpec(t, pki, "t-bad", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	bad.Slot = keybinding.WGKeySlots
	good := tunnelSpec(t, pki, "t-good", 4, 1002, 51821,
		"10.250.0.1/32", "10.250.0.4/32", "10.10.1.4:51821")
	b := testBundle(bad, good)

	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := ts.State()
	if st.OK {
		t.Fatal("槽号越界的规格却报收敛")
	}
	if !strings.Contains(st.Error, "越界") {
		t.Errorf("错误没点出是槽号越界: %s", st.Error)
	}
	if len(f.ifaces) != 1 || f.ifaces[0].UserInstance != 1002 {
		t.Errorf("越界那条该只砸自己，好的那条要照常装上: %+v", f.ifaces)
	}
	if st.Tunnels != 1 {
		t.Errorf("装上的隧道数 %d，期望 1", st.Tunnels)
	}
}

// 一台只做中转的盒子：一条隧道端点都不是，但每条经过它的路径都要在它身上验签、
// 弹一段。这两件事以前只在建 Link 的路上做，所以这种形态什么都没配 —— 选中它做
// 中转的那些隧道变成静默黑洞，而控制台的回执仍然写着"已收敛"。
func TestTunnelSyncOpensTransitWithoutAnyTunnel(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	b := testBundle()
	b.TransitPort = 15000

	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if f.transitPort != 15000 {
		t.Fatalf("零隧道时中转端口没打开，实际 %d", f.transitPort)
	}
	if len(f.keys) != len(b.PKeys) {
		t.Fatalf("中转要验别人的签名，密钥环必须先装上，实际 %d 条", len(f.keys))
	}
	if st := ts.State(); !st.OK || st.Tunnels != 0 || st.Error != "" {
		t.Fatalf("这台盒子该报告「已收敛、零条隧道」，实际 %+v", st)
	}

	// 没给中转端口就一次都不该碰 transit API：不能把"控制器没说要中转"读成"关掉它"。
	f2 := newFakeVPP()
	ts2, _ := newTunnelSync(t, f2, pki, testBundle())
	if err := ts2.Sync(context.Background(), 3); err != nil {
		t.Fatalf("Sync(无中转): %v", err)
	}
	if n := len(f2.callWith("SetTransit")); n != 0 {
		t.Fatalf("transit_port 为 0 却调了 SetTransit（%d 次）", n)
	}
	if n := len(f2.callWith("SetPKey")); n != 0 {
		t.Fatalf("既没有隧道也没有中转时不该装密钥（%d 次）", n)
	}
}

// 中转没配成不是可以忽略的一行日志：这台设备确实没有收敛，回执必须说没收敛。
func TestTunnelSyncReportsTransitFailureAsNotConverged(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	b := testBundle()
	b.TransitPort = 15000
	b.PKeys = []wire.PKey{{HmacID: 9001, Key: "zz"}}

	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := ts.State()
	if st.OK || !strings.Contains(st.Error, "中转端口 15000") {
		t.Fatalf("中转失败应报成没收敛并带原因，实际 %+v", st)
	}
	if f.transitPort != 0 {
		t.Fatalf("密钥没装成就不该去开中转端口，实际 %d", f.transitPort)
	}
}

func TestLinkKeysRejectsNonHex(t *testing.T) {
	if _, err := linkKeys([]wire.PKey{{HmacID: 1, Key: "zz"}}); err == nil {
		t.Fatal("不是 hex 的密钥必须报错（截断签名会让每个包都失败而没有计数器指向它）")
	}
	keys, err := linkKeys(testPKeys())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].HmacID != 9001 ||
		hex.EncodeToString(keys[0].Key) != "00112233445566778899aabbccddeeff" {
		t.Fatalf("解码结果不对: %+v", keys)
	}
}

func TestSameLinkConfigSeesEveryFixedInput(t *testing.T) {
	base := LinkConfig{
		Instance: 1001, ListenPort: 51820, TransitPort: 5500,
		EncapIfIndex: 5, PathIDBase: 1000, OverlayTable: 250,
		LocalOverlay: netip.MustParsePrefix("10.250.0.1/32"),
		PeerOverlay:  netip.MustParsePrefix("10.250.0.3/32"),
		TunnelMTU:    1360, UnderlayMTU: 1500,
	}
	if !sameLinkConfig(base, base) {
		t.Fatal("同一个配置被判成不同")
	}
	cases := map[string]func(*LinkConfig){
		"实例号":        func(c *LinkConfig) { c.Instance = 1002 },
		"监听端口":       func(c *LinkConfig) { c.ListenPort = 51830 },
		"transit":    func(c *LinkConfig) { c.TransitPort = 5600 },
		"出口":         func(c *LinkConfig) { c.EncapIfIndex = 6 },
		"path base":  func(c *LinkConfig) { c.PathIDBase = 1100 },
		"overlay 表":  func(c *LinkConfig) { c.OverlayTable = 251 },
		"本机 overlay": func(c *LinkConfig) { c.LocalOverlay = netip.MustParsePrefix("10.250.0.9/32") },
		"对端 overlay": func(c *LinkConfig) { c.PeerOverlay = netip.MustParsePrefix("10.250.0.9/32") },
		"隧道 MTU":     func(c *LinkConfig) { c.TunnelMTU = 1300 },
		"underlay":   func(c *LinkConfig) { c.UnderlayMTU = 1400 },
	}
	for name, mut := range cases {
		c := base
		mut(&c)
		if sameLinkConfig(base, c) {
			t.Errorf("改了%s却被判成没变：那条隧道不会重建，数据面会停在旧值上", name)
		}
	}
	// 0 与默认值是同一个意思：控制器没给 MTU 不该每隔一轮就把接口重建一次。
	z := base
	z.TunnelMTU = 0
	d := base
	d.TunnelMTU = wire.DefaultTunnelMTU
	if !sameLinkConfig(z, d) {
		t.Error("TunnelMTU 的 0 与默认值应当等价")
	}
}

// 遥测是 sdwan policy 的依据，建不起来就不能把这条隧道算作装好了：
// 静默跳过会得到一条"转发看着通、可达性没人判断"的隧道。
func TestTunnelSyncFailsTunnelWhenTelemetryCannotBeBuilt(t *testing.T) {
	f := newFakeVPP()
	f.bfdAddErr = errors.New("bfd_udp_add 回 -2")
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatalf("单条隧道的遥测失败不该把整次 reconcile 算成失败: %v", err)
	}
	st := ts.State()
	if st.OK || st.Tunnels != 0 {
		t.Fatalf("BFD 建不起来，这条隧道不该被算作装好: %+v", st)
	}
	if !strings.Contains(st.Error, "t1") || !strings.Contains(st.Error, "BFD") {
		t.Errorf("状态里要说清是这条隧道的 BFD 失败: %+v", st)
	}
	if len(f.bfds) != 0 {
		t.Errorf("BFD 明明失败了却有会话: %+v", f.bfds)
	}
}

// 隧道重建（换端口等）会换一个 sw_if_index；旧接口的探针必须跟着旧接口一起
// 撤掉，否则它会一直按一个已经没人读的索引发包。
func TestTunnelSyncRebuildMovesTelemetryToNewInterface(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}
	oldSWIf := f.ifaces[0].SWIfIndex

	b2 := b
	b2.Version = 9
	b2.Tunnels = []wire.TunnelSpec{spec}
	b2.Tunnels[0].ListenPort = 51830
	ts.fetch = func(context.Context) (wire.UnderlayBundle, error) { return b2, nil }
	if err := ts.Sync(ctx, b2.Version); err != nil {
		t.Fatalf("Sync(换端口): %v", err)
	}
	newSWIf := f.ifaces[0].SWIfIndex
	if newSWIf == oldSWIf {
		t.Fatalf("前置条件不成立：接口号没变（%d）", oldSWIf)
	}
	if _, ok := f.probes[oldSWIf]; ok {
		t.Errorf("旧接口 %d 的探针没撤: %+v", oldSWIf, f.probes)
	}
	if _, ok := f.probes[newSWIf]; !ok {
		t.Errorf("新接口 %d 上没建探针: %+v", newSWIf, f.probes)
	}
	// 同一对 overlay 地址，会话只有一条：撤旧的时候删掉、建新的时候再加回来。
	if len(f.bfds) != 1 {
		t.Errorf("重建后 BFD 会话应当仍是 1 条: %+v", f.bfds)
	}
}

// local0 的表是 VPP 的内存配置：VPP 重启后它回到 table 0，agent 认回隧道接口时
// 必须把它重新绑回 overlay 表，否则多跳 BFD 又开始查 table 0（那份是 dpo-drop）。
func TestLinkRebindsLocal0WhenDrifted(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	b := testBundle(spec)

	ts, _ := newTunnelSync(t, f, pki, b)
	ctx := context.Background()
	if err := ts.Sync(ctx, b.Version); err != nil {
		t.Fatal(err)
	}

	f.ifTable[localSwIfIndex] = 0 // VPP 重启 / 操作员手改
	l := ts.Links()["t1"]
	if l == nil {
		t.Fatal("前置条件不成立：没有 Link")
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatalf("Open(认回): %v", err)
	}
	if f.ifTable[localSwIfIndex] != wire.OverlayRouteTable {
		t.Errorf("local0 漂回 table 0 后没重绑: %v", f.ifTable)
	}
}
