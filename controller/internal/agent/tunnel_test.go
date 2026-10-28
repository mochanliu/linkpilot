package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
// 端口/实例/overlay 都按控制器的派生规则来。
func tunnelSpec(t *testing.T, pki *testPKI, id string, peerNode uint16, instance uint32,
	port uint16, localOverlay, peerOverlay, peerEndpoint string) wire.TunnelSpec {

	t.Helper()
	certPEM, binding, _ := peerMaterial(t, pki, "t1", peerNode)
	return wire.TunnelSpec{
		TunnelID: id, PeerNodeID: peerNode,
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
			strings.HasPrefix(c, "AddDelInterfaceAddress") || strings.HasPrefix(c, "SetInterfaceIPTable") {
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
