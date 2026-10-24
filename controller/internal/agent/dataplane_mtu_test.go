package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// MTU 这一组测试守的是三件事，都能追溯到本轮测量：
//
//  1. 落地的值**永远是算出来的那个**，不是"控制器给了 0 就什么都不做" —— 手建的
//     WG 接口带着 MTU 0/0/0/0（实测 node1 上 wg0 就是这个样子），而 0 在 VPP 里
//     等于"没有 MTU 记录"。
//  2. 知道 underlay 就必须卡预算：超预算**不会被 VPP 拒绝**。这条口按自己记着的
//     值切内层分片照样发（实测 overlay 1436 过记 1360 的 wg0 → 两片 1356+100），
//     而每个内层分片还要再背一份自己的 WG+SR 头，外层于是又撑破 underlay。真实
//     网络里单片 UDP 常被丢，所以宁可在这里拒绝启动，也不要一条"小包通、大包
//     黑洞"的隧道。
//  3. 两条记录都会被执行，执行动作是**分片**而不是拒发：underlay 记错了（VIF 9000
//     而网卡 1500）会让超长外层帧打到 sendto 上、把 af-packet 的发送侧整张口卡住
//     （2026-10-18 带宽基线卡死、本轮测量又在 node2 复现一次），这是最坏的一种坏；
//     隧道记少了只是多分几片。本轮纠正了一条早先的错判 —— 原先记"隧道那条形同虚设"，
//     那是量了手建接口 MTU 0/0/0/0 的出生状态：没有记录，自然无从执行。

// openTunnelMTU 走完整一遍"解析配置 → Open"，从假数据面读回**真正落到接口记录上**
// 的那个数。测试不直接读 l.cfg：那是发布出去的不可变输入（见 restartLinkWith 的
// 同一条约束），而且读它只能证明"字段被赋过值"，证明不了数据面拿到的是同一个数。
func openTunnelMTU(t *testing.T, mut func(*LinkConfig)) uint16 {
	t.Helper()
	f := newFakeVPP()
	cfg := testConfig(t, f, newTestPKI(t))
	if mut != nil {
		mut(&cfg)
	}
	l, err := NewLink(cfg)
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f.mtus[l.SWIfIndex()]
}

func TestNewLinkResolvesTunnelMTU(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)

	// 控制器没下发（0）：必须落到契约默认值，而不是留 0 给 VPP。
	if got := openTunnelMTU(t, nil); got != wire.DefaultTunnelMTU {
		t.Errorf("没下发时数据面应拿到契约默认 %d，实际 %d", wire.DefaultTunnelMTU, got)
	}

	// 下发了就照它来，包括比默认更小的值（PPPoE 之类的小 underlay）。
	if got := openTunnelMTU(t, func(c *LinkConfig) { c.TunnelMTU = 1200 }); got != 1200 {
		t.Errorf("显式 1200 被改写了: %d", got)
	}

	for _, tc := range []struct {
		name string
		mtu  uint16
	}{
		{"低于下界", wire.MinTunnelMTU - 1},
		{"高于上界", wire.MaxTunnelMTU + 1},
		{"uint16 顶到最大", 65535},
	} {
		cfg := testConfig(t, f, pki)
		cfg.TunnelMTU = tc.mtu
		if _, err := NewLink(cfg); err == nil {
			t.Errorf("%s（MTU=%d）：应当报错", tc.name, tc.mtu)
		}
	}

	// 区间两端本身要收：拒绝的理由是"这个数不合法"，不能顺手把合法值也拦掉。
	for _, mtu := range []uint16{wire.MinTunnelMTU, wire.MaxTunnelMTU} {
		cfg := testConfig(t, f, pki)
		cfg.TunnelMTU = mtu
		if _, err := NewLink(cfg); err != nil {
			t.Errorf("边界值 MTU=%d 不该报错: %v", mtu, err)
		}
	}
}

func TestNewLinkChecksMTUBudgetAgainstUnderlay(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)

	// 1500 的 underlay、最多 8 段：预算 1368，所以默认 1360 过。
	cfg := testConfig(t, f, pki)
	cfg.UnderlayMTU = 1500
	if _, err := NewLink(cfg); err != nil {
		t.Fatalf("1500 underlay 用默认 MTU 应当可以: %v", err)
	}

	// 同一套参数换成 1450 的 underlay，**默认值**就超预算了（1318）。这里必须拦住：
	// 出界的后果不是报错，是大包变分片、分片被中间设备丢掉，然后没人怀疑一个配置
	// 成功的接口。报错文案要点名"分片"，否则运维会以为是 VPP 拒绝了。
	cfg = testConfig(t, f, pki)
	cfg.UnderlayMTU = 1450
	_, err := NewLink(cfg)
	if err == nil {
		t.Fatalf("1450 underlay 上默认 %d 超预算，应当报错", wire.DefaultTunnelMTU)
	}
	if !strings.Contains(err.Error(), "分片") {
		t.Errorf("报错要说清后果是静默分片，实际: %v", err)
	}

	// 预算内（正好等于 8 段预算）要过 —— 判据是 <=，不是 <。
	cfg = testConfig(t, f, pki)
	cfg.UnderlayMTU = 1450
	cfg.TunnelMTU = wire.TunnelMTUBudget(1450, wire.MaxSRSegments)
	if _, err := NewLink(cfg); err != nil {
		t.Fatalf("恰好等于预算（%d）不该报错: %v", cfg.TunnelMTU, err)
	}

	// 预算本身要小到会连一个合规包都装不下的程度时，报的是"预算=0"这一类：
	// underlay 100、8 段 → 100-28-32-72 < 0。这时任何合法 MTU 都被拒。
	cfg = testConfig(t, f, pki)
	cfg.UnderlayMTU = 100
	if _, err := NewLink(cfg); err == nil {
		t.Fatal("underlay 100 装不下任何合法隧道 MTU，应当报错")
	}

	// 不知道 underlay（0）时**不做**预算判断：这个号目前是操作员给的，
	// 没有号码就别假装算得出预算 —— 拦住只会让人以为 agent 坏了。
	cfg = testConfig(t, f, pki)
	cfg.UnderlayMTU = 0
	cfg.TunnelMTU = 9000
	if _, err := NewLink(cfg); err != nil {
		t.Fatalf("underlay 未知时不该做预算判断: %v", err)
	}
}

func TestOpenWritesBothMTUs(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	cfg := testConfig(t, f, pki)
	cfg.UnderlayMTU = 1500
	l, err := NewLink(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	adopted, err := l.Open(ctx)
	if err != nil || adopted {
		t.Fatalf("第一次 Open 应该新建: adopted=%v err=%v", adopted, err)
	}
	if got := f.mtus[l.SWIfIndex()]; got != wire.DefaultTunnelMTU {
		t.Errorf("隧道接口 %d 的 MTU 应是默认 %d，实际 %d", l.SWIfIndex(), wire.DefaultTunnelMTU, got)
	}
	// underlay 那条是本组里**唯一会被数据面真正执行**的写入：VIF 记录必须等于网卡
	// 真实 MTU，否则 VPP 会按 9000 送出 sendto，把 af-packet 的发送侧卡住。
	if got := f.mtus[encapIfIndex]; got != 1500 {
		t.Errorf("underlay 接口 %d 的 MTU 应是 1500，实际 %d", encapIfIndex, got)
	}
	// 顺序：接口先建出来才有 sw_if_index 可写。
	iCreate := indexOfCallPrefix(f.calls, "CreateInterface")
	iMTU := indexOfCallPrefix(f.calls, fmt.Sprintf("SetInterfaceMTU %d=", encapIfIndex))
	if iCreate < 0 || iMTU < 0 || iMTU < iCreate {
		t.Errorf("MTU 写入应在建接口之后，调用序列: %v", f.calls)
	}

	// 重启认回：MTU 照样要写。VPP 重启或人工清过记录时，"接口在"不等于"MTU 在"。
	l2 := restartLink(t, l)
	if err := l2.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	f.mtus = nil
	f.calls = nil
	adopted, err = l2.Open(ctx)
	if err != nil || !adopted {
		t.Fatalf("第二次 Open 应该认回: adopted=%v err=%v", adopted, err)
	}
	if f.mtus[l2.SWIfIndex()] != wire.DefaultTunnelMTU || f.mtus[encapIfIndex] != 1500 {
		t.Errorf("认回分支没补 MTU: %v", f.mtus)
	}
}

// 操作员没给 underlay MTU（#17 之前是常态）：只写隧道那一条，**绝不**去动出口接口
// —— 我们手里没有那个数，写 0 等于把 VPP 里原有的 MTU 记录清掉，那比不写更糟。
func TestOpenWithoutUnderlayMTUWritesTunnelOnly(t *testing.T) {
	f := newFakeVPP()
	l, err := NewLink(testConfig(t, f, newTestPKI(t)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.mtus[l.SWIfIndex()]; got != wire.DefaultTunnelMTU {
		t.Errorf("隧道 MTU 应是默认 %d，实际 %d", wire.DefaultTunnelMTU, got)
	}
	if _, seen := f.mtus[encapIfIndex]; seen {
		t.Errorf("underlay MTU 未知时不该写出口接口（会把已有记录清掉）: %v", f.mtus)
	}
	if n := len(f.callWith("SetInterfaceMTU")); n != 1 {
		t.Errorf("只应写一次 MTU，实际 %d 次: %v", n, f.calls)
	}
}

// MTU 写失败时**不能**把这条链路标成 ready：否则后面 ApplyPeer 全成功，而实际跑的
// 是一张没有 MTU 记录的接口，症状照旧是"偶发大包黑洞"。
func TestOpenFailsWhenMTUWriteFails(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	f.mtuErr = vppapi.APIError{Op: "sw_interface_set_mtu", Retval: -6}
	if _, err := l.Open(ctx); err == nil {
		t.Fatal("MTU 写失败必须让 Open 失败")
	} else if !strings.Contains(err.Error(), "MTU") {
		t.Errorf("报错要指明是 MTU: %v", err)
	}
	if _, err := l.ApplyPeer(ctx, PeerTarget{NodeID: 2}); err == nil ||
		!strings.Contains(err.Error(), "还没 Open") {
		t.Errorf("MTU 写失败后这条链路不该可用，ApplyPeer 应报没 Open: %v", err)
	}

	// 但接口已经在数据面里了：下一次 Open 必须走认回并把 MTU 补上，而不是撞 -31。
	f.mtuErr = nil
	adopted, err := l.Open(ctx)
	if err != nil || !adopted {
		t.Fatalf("第二次 Open 应认回刚建好但 MTU 没写的接口: adopted=%v err=%v", adopted, err)
	}
	if f.mtus[l.SWIfIndex()] != wire.DefaultTunnelMTU {
		t.Errorf("重试后没补上隧道 MTU: %v", f.mtus)
	}
}

// MSS 钳制是隧道 MTU 的函数（2026-10-24 定案：自动取 MTU - 40），调用方不再给
// 第二个数。它只写在隧道口上 —— underlay 出口的外层是 UDP，钳 TCP MSS 毫无意义。
func TestOpenSetsMSSClampFromTunnelMTU(t *testing.T) {
	for _, tc := range []struct {
		name string
		mtu  uint16
		want uint16
	}{
		{"没下发时用契约默认", 0, wire.DefaultTunnelMTU - wire.TCPOverheadBytes},
		{"显式 1200", 1200, 1160},
		{"下界", wire.MinTunnelMTU, wire.MinTunnelMTU - wire.TCPOverheadBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVPP()
			cfg := testConfig(t, f, newTestPKI(t))
			cfg.TunnelMTU = tc.mtu
			l, err := NewLink(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Open(ctx); err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got := f.mssClamps[l.SWIfIndex()]; got != tc.want {
				t.Errorf("隧道接口 %d 的 MSS 应是 %d（MTU %d - 40），实际 %d",
					l.SWIfIndex(), tc.want, f.mtus[l.SWIfIndex()], got)
			}
			if _, seen := f.mssClamps[encapIfIndex]; seen {
				t.Errorf("不该在 underlay 出口 %d 上钳 MSS：那里只看得见外层 UDP", encapIfIndex)
			}
			// 顺序：先有 MTU 记录，才有"按它算出来"的 MSS。
			iMTU := indexOfCallPrefix(f.calls, fmt.Sprintf("SetInterfaceMTU %d=", l.SWIfIndex()))
			iMSS := indexOfCallPrefix(f.calls, fmt.Sprintf("SetMSSClamp %d=", l.SWIfIndex()))
			if iMTU < 0 || iMSS < 0 || iMSS < iMTU {
				t.Errorf("MSS 应在隧道 MTU 之后写，调用序列: %v", f.calls)
			}
		})
	}
}

// 认回分支同样要补 MSS：mss_clamp 的表不随 WG 接口一起持久，VPP 重启或操作员
// 清过之后，"接口还在"不等于"钳制还在"。
func TestAdoptRewritesMSSClamp(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(ctx); err != nil {
		t.Fatal(err)
	}

	l2 := restartLink(t, l)
	if err := l2.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	f.mssClamps = nil // 模拟"表被清空"
	f.calls = nil
	adopted, err := l2.Open(ctx)
	if err != nil || !adopted {
		t.Fatalf("第二次 Open 应认回: adopted=%v err=%v", adopted, err)
	}
	want := wire.DefaultTunnelMTU - wire.TCPOverheadBytes
	if got := f.mssClamps[l2.SWIfIndex()]; got != want {
		t.Errorf("认回后应补上 MSS=%d，实际 %d", want, got)
	}
}

func TestOpenFailsWhenMSSClampWriteFails(t *testing.T) {
	f := newFakeVPP()
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	f.mssErr = vppapi.APIError{Op: "mss_clamp_enable_disable", Retval: -7}
	if _, err := l.Open(ctx); err == nil {
		t.Fatal("MSS 钳制写失败必须让 Open 失败")
	} else if !strings.Contains(err.Error(), "MSS") {
		t.Errorf("报错要指明是 MSS: %v", err)
	}
	if _, err := l.ApplyPeer(ctx, PeerTarget{NodeID: 2}); err == nil ||
		!strings.Contains(err.Error(), "还没 Open") {
		t.Errorf("MSS 写失败后这条链路不该可用: %v", err)
	}
}

// 读回方向是 TX：真机上不报错、配置显示正常，但包一个字节不改（实测）。所以
// agent 必须自己把"读回来的方向"当判据，而不是信任写入的 retval。
func TestOpenRejectsMSSClampThatLandedAsTX(t *testing.T) {
	f := newFakeVPP()
	f.mssDir = vppapi.MSSClampDirTX
	l, _ := newTestLink(t, f)
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatal(err)
	}
	_, err := l.Open(ctx)
	if err == nil {
		t.Fatal("钳制方向落地为 TX 时必须报错")
	}
	if !strings.Contains(err.Error(), "RX") {
		t.Errorf("报错要点名 RX 方向: %v", err)
	}
}
