package agent

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

type fakeTelemetryPlane struct {
	probeSets []vppapi.ProbeParams
	probeDels []uint32
	probes    []vppapi.ProbeStats

	bfdAdds []vppapi.BFDSessionParams
	bfdDels []vppapi.BFDSessionParams
	bfds    []vppapi.BFDSession
}

func (f *fakeTelemetryPlane) SetProbe(_ context.Context, p vppapi.ProbeParams) error {
	f.probeSets = append(f.probeSets, p)
	return nil
}

func (f *fakeTelemetryPlane) DelProbe(_ context.Context, swIfIndex uint32) error {
	f.probeDels = append(f.probeDels, swIfIndex)
	return nil
}

func (f *fakeTelemetryPlane) Probes(context.Context) ([]vppapi.ProbeStats, error) {
	return f.probes, nil
}

func (f *fakeTelemetryPlane) AddBFDSession(_ context.Context, p vppapi.BFDSessionParams) error {
	f.bfdAdds = append(f.bfdAdds, p)
	f.bfds = append(f.bfds, vppapi.BFDSession{
		// 真客户端一律建多跳会话（sw_if_index=~0），替身照抄这个事实，
		// 否则测试会掩盖"agent 把 BFD 绑到 wg 接口上"这种致命错配。
		SwIfIndex: vppapi.BFDMultihopSwIfIndex, LocalIP: p.LocalIP, PeerIP: p.PeerIP,
		DesiredMinTxUS: p.DesiredMinTxUS, RequiredMinRxUS: p.RequiredMinRxUS,
		DetectMult: p.DetectMult, State: vppapi.BFDStateInit,
	})
	return nil
}

func (f *fakeTelemetryPlane) DelBFDSession(_ context.Context, p vppapi.BFDSessionParams) error {
	f.bfdDels = append(f.bfdDels, p)
	return nil
}

func (f *fakeTelemetryPlane) BFDSessions(context.Context) ([]vppapi.BFDSession, error) {
	return f.bfds, nil
}

func telemetryCfg() wire.TunnelTelemetry {
	return wire.TunnelTelemetry{
		SwIfIndex:  5,
		PeerNodeID: 7,
		PeerIP:     "192.168.99.2",
		LocalIP:    "192.168.99.1",
	}
}

func TestTelemetryEnsureAppliesDefaults(t *testing.T) {
	dp := &fakeTelemetryPlane{}
	tel := NewTelemetry(dp)
	ctx := context.Background()

	if err := tel.Ensure(ctx, telemetryCfg()); err != nil {
		t.Fatal(err)
	}
	if len(dp.probeSets) != 1 {
		t.Fatalf("SetProbe 调用 %d 次，期望 1", len(dp.probeSets))
	}
	p := dp.probeSets[0]
	if p.SwIfIndex != 5 || p.IntervalMS != wire.DefaultProbeIntervalMS || p.TimeoutMS != 0 {
		t.Errorf("探针参数 = %+v，期望 300ms/默认超时", p)
	}
	if !p.PeerIP.Equal(net.IPv4(192, 168, 99, 2)) || !p.LocalIP.Equal(net.IPv4(192, 168, 99, 1)) {
		t.Errorf("探针地址 = %v -> %v", p.LocalIP, p.PeerIP)
	}

	if len(dp.bfdAdds) != 1 {
		t.Fatalf("AddBFDSession 调用 %d 次，期望 1", len(dp.bfdAdds))
	}
	b := dp.bfdAdds[0]
	if b.DesiredMinTxUS != wire.DefaultBFDMinTxUS ||
		b.RequiredMinRxUS != wire.DefaultBFDMinRxUS ||
		b.DetectMult != wire.DefaultBFDDetectMult {
		t.Errorf("BFD 默认定时器 = %+v", b)
	}
}

// 重复 Ensure 不能重建已存在的 BFD 会话：重建会把 Up 打回 Init，健康检查
// 自己制造抖动。探针则每次都要 Set（间隔变化只能靠它落地）。
func TestTelemetryEnsureIsIdempotentForBFD(t *testing.T) {
	dp := &fakeTelemetryPlane{}
	tel := NewTelemetry(dp)
	ctx := context.Background()

	if err := tel.Ensure(ctx, telemetryCfg()); err != nil {
		t.Fatal(err)
	}
	dp.bfds[0].State = vppapi.BFDStateUp
	if err := tel.Ensure(ctx, telemetryCfg()); err != nil {
		t.Fatal(err)
	}
	if len(dp.bfdAdds) != 1 {
		t.Errorf("AddBFDSession 调用了 %d 次，期望 1（第二次应读回后跳过）", len(dp.bfdAdds))
	}
	if len(dp.probeSets) != 2 {
		t.Errorf("SetProbe 调用了 %d 次，期望 2（探针是幂等更新，每次都要下发）", len(dp.probeSets))
	}
}

func TestTelemetryEnsureReappliesChangedBFDTimers(t *testing.T) {
	dp := &fakeTelemetryPlane{}
	tel := NewTelemetry(dp)
	ctx := context.Background()

	if err := tel.Ensure(ctx, telemetryCfg()); err != nil {
		t.Fatal(err)
	}
	cfg := telemetryCfg()
	cfg.BFDMinTxUS, cfg.BFDMinRxUS, cfg.BFDDetectMult = 100000, 100000, 5
	if err := tel.Ensure(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(dp.bfdAdds) != 2 {
		t.Fatalf("定时器变化必须重新 Add，调用了 %d 次", len(dp.bfdAdds))
	}
	if last := dp.bfdAdds[1]; last.DesiredMinTxUS != 100000 || last.DetectMult != 5 {
		t.Errorf("重下发的定时器 = %+v", last)
	}
}

func TestTelemetryEnsureRejectsBadConfigBeforeSend(t *testing.T) {
	cases := []struct {
		name string
		cfg  wire.TunnelTelemetry
		want string
	}{
		{"缺 sw_if_index", wire.TunnelTelemetry{PeerIP: "192.168.99.2", LocalIP: "192.168.99.1"}, "sw_if_index"},
		{"对端不是 IPv4", wire.TunnelTelemetry{SwIfIndex: 5, PeerIP: "2001:db8::2", LocalIP: "192.168.99.1"}, "对端"},
		{"本地不是 IPv4", wire.TunnelTelemetry{SwIfIndex: 5, PeerIP: "192.168.99.2", LocalIP: "2001:db8::1"}, "本地"},
		{"超时短于间隔", wire.TunnelTelemetry{SwIfIndex: 5, PeerIP: "192.168.99.2", LocalIP: "192.168.99.1", IntervalMS: 300, TimeoutMS: 100}, "超时"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dp := &fakeTelemetryPlane{}
			tel := NewTelemetry(dp)
			err := tel.Ensure(context.Background(), c.cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望包含 %q 的错误，got: %v", c.want, err)
			}
			if len(dp.probeSets) != 0 || len(dp.bfdAdds) != 0 {
				t.Errorf("非法配置不该发出任何请求: probe=%d bfd=%d", len(dp.probeSets), len(dp.bfdAdds))
			}
		})
	}
}

func TestTelemetrySampleWindowDeltas(t *testing.T) {
	dp := &fakeTelemetryPlane{}
	tel := NewTelemetry(dp)
	ctx := context.Background()

	base := time.Unix(1_700_000_000, 0)
	tel.now = func() time.Time { return base }

	if err := tel.Ensure(ctx, telemetryCfg()); err != nil {
		t.Fatal(err)
	}
	dp.bfds[0].State = vppapi.BFDStateUp

	// 第一个窗口：会话建立以来的全部计数。
	dp.probes = []vppapi.ProbeStats{{
		SwIfIndex: 5, PeerIP: net.IPv4(192, 168, 99, 2), LocalIP: net.IPv4(192, 168, 99, 1),
		IntervalMS: 300, TimeoutMS: 900, Enabled: true,
		Tx: 10, Rx: 9, Lost: 1, LateReplies: 0,
		LastRTTus: 2500, MinRTTus: 1200, AvgRTTus: 3100, MaxRTTus: 7000,
		JitterUs: 180, LossPermille: 100,
	}}
	first, err := tel.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("行数 = %d，期望 1", len(first))
	}
	m := first[0]
	if !m.Up || m.BFDState != "up" || m.PeerNodeID != 7 || m.PeerIP != "192.168.99.2" {
		t.Errorf("合并字段错: %+v", m)
	}
	if m.ProbeTx != 10 || m.ProbeRx != 9 || m.ProbeLost != 1 || m.WindowSec != 0 {
		t.Errorf("首窗口 = %+v", m)
	}
	if m.RTTms != 2.5 || m.MinRTTms != 1.2 || m.AvgRTTms != 3.1 || m.MaxRTTms != 7 || m.JitterMs != 0.18 {
		t.Errorf("RTT 换算错: %+v", m)
	}
	if m.LossPermille != 100 {
		t.Errorf("首窗口丢包率 = %d，期望 100", m.LossPermille)
	}

	// 第二个窗口：2 秒后，新增 8 发 7 收 1 丢。丢包率不跟着这几个增量重算 ——
	// 它是插件滑窗里的当前值，这里换成了 90‰ 来证明取的是插件那份。
	tel.now = func() time.Time { return base.Add(2 * time.Second) }
	dp.probes[0].Tx, dp.probes[0].Rx, dp.probes[0].Lost = 18, 16, 2
	dp.probes[0].LastRTTus = 4000
	dp.probes[0].LossPermille = 90
	second, err := tel.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m = second[0]
	if m.ProbeTx != 8 || m.ProbeRx != 7 || m.ProbeLost != 1 || m.WindowSec != 2 {
		t.Errorf("窗口增量错: %+v", m)
	}
	if m.LossPermille != 90 { // 插件滑窗的当前值，不是 lost_delta/tx_delta
		t.Errorf("窗口丢包率 = %d，期望插件的 90", m.LossPermille)
	}
	if m.RTTms != 4 {
		t.Errorf("last RTT = %v，期望 4", m.RTTms)
	}
}

// 丢包率的口径必须是「率」：没丢 = 0，最多 100%。
//
// 这条测的是曾经的错误：agent 拿两次采样的差值再算一遍 lost/tx。一条探针在这
// 个窗口发出、要 3×间隔（900ms）后才被判丢，于是它只进下一窗口的分子；那个
// 窗口里 lost_delta 能大于 tx_delta，页面按百分比读出来就是 103%、下一帧又
// 变成 97%。现在分子分母都取自插件同一个 128 槽窗口：会话刚建立时是 0，全丢
// 时是 1000‰，落在什么区间由插件的窗口决定，不会因为采样边界而越界。
func TestTelemetryLossIsABoundedRate(t *testing.T) {
	dp := &fakeTelemetryPlane{probes: []vppapi.ProbeStats{{
		SwIfIndex: 5, PeerIP: net.IPv4(192, 168, 99, 2), LocalIP: net.IPv4(192, 168, 99, 1),
		IntervalMS: 300, Enabled: true,
		Tx: 33, Rx: 0, Lost: 0, LossPermille: 0,
	}}}
	tel := NewTelemetry(dp)
	base := time.Unix(1_700_000_000, 0)
	tel.now = func() time.Time { return base }

	rows, err := tel.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].LossPermille != 0 {
		t.Errorf("刚建会话就该是 0‰，实到 %d", rows[0].LossPermille)
	}
	if !rows[0].HasProbe {
		t.Error("有探针会话的行必须标 has_probe，页面据此区分「0 丢包」和「未上报」")
	}

	// 下一窗口：这 33 发全部超时判丢、窗口内只新发了一包。旧的差值口径在这里
	// 算出 33/1，页面显示 3300%；插件的滑窗给的是 1000‰（全丢），这才是「率」。
	tel.now = func() time.Time { return base.Add(time.Second) }
	dp.probes[0].Tx, dp.probes[0].Rx, dp.probes[0].Lost = 34, 0, 33
	dp.probes[0].LossPermille = 1000
	rows, err = tel.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := rows[0]
	if m.LossPermille != 1000 {
		t.Errorf("全丢应当是 1000‰，实到 %d", m.LossPermille)
	}
	if m.ProbeTx != 1 || m.ProbeLost != 33 {
		t.Errorf("计数增量照旧要报出来（ %+v ）", m)
	}
}

// 会话重建后累计值变小：这个窗口的内容是新会话至今的全部计数，不能算成
// uint64 下溢、也不能报成丢包。
func TestTelemetrySampleHandlesCounterReset(t *testing.T) {
	dp := &fakeTelemetryPlane{probes: []vppapi.ProbeStats{{
		SwIfIndex: 5, PeerIP: net.IPv4(192, 168, 99, 2), LocalIP: net.IPv4(192, 168, 99, 1),
		Tx: 100, Rx: 100,
	}}}
	tel := NewTelemetry(dp)
	base := time.Unix(1_700_000_000, 0)
	tel.now = func() time.Time { return base }
	if _, err := tel.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}

	tel.now = func() time.Time { return base.Add(time.Second) }
	dp.probes[0].Tx, dp.probes[0].Rx = 3, 2
	rows, err := tel.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ProbeTx != 3 || rows[0].ProbeRx != 2 {
		t.Errorf("重建后的窗口 = %+v，期望 tx=3 rx=2", rows[0])
	}
}

// 只有 BFD、没有探针的隧道照样要出现在视图里（可达性是遥测的一半）。
// 多跳 BFD 会话不带隧道身份，所以行里的 sw_if_index 必须来自本组件的配置。
func TestTelemetrySampleIncludesBFDOnlyTunnel(t *testing.T) {
	dp := &fakeTelemetryPlane{
		bfds: []vppapi.BFDSession{{
			SwIfIndex: vppapi.BFDMultihopSwIfIndex,
			LocalIP:   net.IPv4(192, 168, 99, 1), PeerIP: net.IPv4(192, 168, 99, 3),
			State: vppapi.BFDStateUp,
		}},
	}
	tel := NewTelemetry(dp)
	tel.tunnels[9] = &tunnelWindow{cfg: wire.TunnelTelemetry{
		SwIfIndex: 9, PeerNodeID: 11, PeerIP: "192.168.99.3", LocalIP: "192.168.99.1",
	}}
	rows, err := tel.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SwIfIndex != 9 {
		t.Fatalf("行 = %+v", rows)
	}
	if !rows[0].Up || rows[0].BFDState != "up" || rows[0].PeerIP != "192.168.99.3" {
		t.Errorf("BFD-only 行 = %+v", rows[0])
	}
	if rows[0].HasProbe {
		t.Error("没有探针会话的行不能标 has_probe：页面要靠它把时延/抖动/丢包写成「未上报」，而不是 0")
	}
	if rows[0].PeerNodeID != 11 {
		t.Errorf("BFD-only 行的 PeerNodeID = %d，期望从配置里补上 11", rows[0].PeerNodeID)
	}
	if rows[0].ProbeTx != 0 {
		t.Errorf("没有探针却报了 tx=%d", rows[0].ProbeTx)
	}
}

// 绑接口的 BFD 会话不是本组件建的（它会让隧道全丢），不能被当成某条隧道的
// 可达性证据 —— 认错就等于用一条会打死隧道的配置报"up"。
func TestTelemetrySampleIgnoresInterfaceBoundBFD(t *testing.T) {
	dp := &fakeTelemetryPlane{
		probes: []vppapi.ProbeStats{{
			SwIfIndex: 5, PeerIP: net.IPv4(192, 168, 99, 2), LocalIP: net.IPv4(192, 168, 99, 1),
		}},
		bfds: []vppapi.BFDSession{{
			SwIfIndex: 5, LocalIP: net.IPv4(192, 168, 99, 1), PeerIP: net.IPv4(192, 168, 99, 2),
			State: vppapi.BFDStateUp,
		}},
	}
	tel := NewTelemetry(dp)
	tel.tunnels[5] = &tunnelWindow{cfg: telemetryCfg()}
	rows, err := tel.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].BFDState != "absent" || rows[0].Up {
		t.Fatalf("绑接口的会话被当成了隧道可达性: %+v", rows)
	}
}

// 没有 BFD 会话时状态必须是 absent，而不是 admin-down（后者是"我们配了、
// 主动关着"，前者是"根本没会话"，排障方向完全不同）。
func TestTelemetrySampleAbsentBFD(t *testing.T) {
	dp := &fakeTelemetryPlane{probes: []vppapi.ProbeStats{{
		SwIfIndex: 5, PeerIP: net.IPv4(192, 168, 99, 2), LocalIP: net.IPv4(192, 168, 99, 1),
	}}}
	tel := NewTelemetry(dp)
	rows, err := tel.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].BFDState != "absent" || rows[0].Up {
		t.Errorf("无 BFD 会话的行 = %+v", rows[0])
	}
}

// 多跳会话只能按 (local, peer) 认：它没有隧道身份，Remove 必须先找到对端。
// 这条路径也是 agent 重启后（内存里没有 cfg）唯一能清干净的路径。
func TestTelemetryRemoveIsIdempotent(t *testing.T) {
	dp := &fakeTelemetryPlane{bfds: []vppapi.BFDSession{{
		SwIfIndex: vppapi.BFDMultihopSwIfIndex,
		LocalIP:   net.IPv4(192, 168, 99, 1), PeerIP: net.IPv4(192, 168, 99, 2),
		DesiredMinTxUS: wire.DefaultBFDMinTxUS, RequiredMinRxUS: wire.DefaultBFDMinRxUS,
		DetectMult: wire.DefaultBFDDetectMult, State: vppapi.BFDStateUp,
	}}}
	tel := NewTelemetry(dp)
	ctx := context.Background()
	if err := tel.Ensure(ctx, telemetryCfg()); err != nil {
		t.Fatal(err)
	}

	// 第二次 Remove 时 BFD 已不在表里：不能报错，探针仍要删。
	if err := tel.Remove(ctx, 5); err != nil {
		t.Fatal(err)
	}
	dp.bfds = nil
	if err := tel.Remove(ctx, 5); err != nil {
		t.Fatalf("重复 Remove 必须幂等: %v", err)
	}
	if len(dp.bfdDels) != 1 {
		t.Errorf("DelBFDSession 调用 %d 次，期望 1", len(dp.bfdDels))
	}
	if len(dp.probeDels) != 2 {
		t.Errorf("DelProbe 调用 %d 次，期望 2", len(dp.probeDels))
	}
	if _, ok := tel.tunnels[5]; ok {
		t.Error("Remove 后窗口记录应被清掉")
	}
}

// agent 重启后内存里没有 cfg：对端地址只能从探针会话读，否则多跳 BFD 会话
// 会永久留在数据面（它不绑接口，不会自己随接口消失）。
func TestTelemetryRemoveFindsPeerFromProbeAfterRestart(t *testing.T) {
	dp := &fakeTelemetryPlane{
		probes: []vppapi.ProbeStats{{
			SwIfIndex: 5, PeerIP: net.IPv4(192, 168, 99, 2), LocalIP: net.IPv4(192, 168, 99, 1),
		}},
		bfds: []vppapi.BFDSession{{
			SwIfIndex: vppapi.BFDMultihopSwIfIndex,
			LocalIP:   net.IPv4(192, 168, 99, 1), PeerIP: net.IPv4(192, 168, 99, 2),
			State: vppapi.BFDStateUp,
		}},
	}
	tel := NewTelemetry(dp) // 注意：没有 Ensure，tunnels 为空
	if err := tel.Remove(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if len(dp.bfdDels) != 1 || !dp.bfdDels[0].PeerIP.Equal(net.IPv4(192, 168, 99, 2)) {
		t.Errorf("重启后的 BFD 清理 = %+v", dp.bfdDels)
	}
}

// 同一个对端上可以有多条并行隧道（local /32 不同）：它们共用同一个 peer，
// 删其中一条时不能把另一条的 BFD 会话一起删掉。
func TestTelemetryRemoveKeepsParallelTunnelSession(t *testing.T) {
	dp := &fakeTelemetryPlane{bfds: []vppapi.BFDSession{
		{SwIfIndex: vppapi.BFDMultihopSwIfIndex,
			LocalIP: net.IPv4(192, 168, 99, 1), PeerIP: net.IPv4(192, 168, 99, 2)},
		{SwIfIndex: vppapi.BFDMultihopSwIfIndex,
			LocalIP: net.IPv4(192, 168, 99, 11), PeerIP: net.IPv4(192, 168, 99, 2)},
	}}
	tel := NewTelemetry(dp)
	ctx := context.Background()
	tel.tunnels[5] = &tunnelWindow{cfg: wire.TunnelTelemetry{
		SwIfIndex: 5, LocalIP: "192.168.99.1", PeerIP: "192.168.99.2",
	}}
	tel.tunnels[6] = &tunnelWindow{cfg: wire.TunnelTelemetry{
		SwIfIndex: 6, LocalIP: "192.168.99.11", PeerIP: "192.168.99.2",
	}}

	if err := tel.Remove(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if len(dp.bfdDels) != 1 || !dp.bfdDels[0].LocalIP.Equal(net.IPv4(192, 168, 99, 1)) {
		t.Fatalf("删掉的是别人的会话: %+v", dp.bfdDels)
	}
	if _, ok := tel.tunnels[6]; !ok {
		t.Error("另一条隧道的窗口记录被一起删掉了")
	}
}
