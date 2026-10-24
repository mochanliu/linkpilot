package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// 隧道内遥测（定案：可达性用 VPP core BFD，质量用插件探针，两张表合成一行）。
// 这一层负责三件事，都在 agent（管理面）里做：
//
//  1. 把控制器下发的 per-tunnel 参数落进数据面（探针间隔/BFD 定时器）；
//  2. 周期采样并把**累计计数器换算成窗口增量** —— 控制器不能从两次独立上报里
//     算增量，因为会话重建会让计数器归零，而"归零"和"真没流量"长得一样；
//  3. 把 BFD 状态与探针质量合成一条隧道的视图。
//
// 两张表按 **(local, peer) 地址**对齐，不是 sw_if_index：探针按接口建，而 BFD
// 一律建**多跳**会话（sw_if_index = ~0），因为绑 wg 接口的 BFD 在对端没有会话时
// 会把 overlay 路由的转发桶换成 dpo-drop，隧道直接 100% 丢包（实测，见
// vppapi.BFDMultihopSwIfIndex）。所以只认多跳会话，绑接口的一律不算本组件的。
//
// 它不推导隧道、更不下发路由：telemetry 只是观测，FIB 归控制面（用户定案）。

// TelemetryPlane 是遥测这一层要的数据面动作。故意比 DataPlane 窄：这里一个
// 转发配置都没有，把整条 DataPlane 塞进来只会让测试替身凭空多实现 20 个方法。
type TelemetryPlane interface {
	SetProbe(ctx context.Context, p vppapi.ProbeParams) error
	DelProbe(ctx context.Context, swIfIndex uint32) error
	Probes(ctx context.Context) ([]vppapi.ProbeStats, error)

	AddBFDSession(ctx context.Context, p vppapi.BFDSessionParams) error
	DelBFDSession(ctx context.Context, p vppapi.BFDSessionParams) error
	BFDSessions(ctx context.Context) ([]vppapi.BFDSession, error)
}

var _ TelemetryPlane = (*vppapi.Client)(nil)

// tunnelWindow 是 agent 侧的采样窗口。只记"上次读到的累计值"，不缓存任何
// 数据面状态：真值永远来自 Probes/BFDSessions 的 dump。
type tunnelWindow struct {
	cfg wire.TunnelTelemetry // 规范化后的配置（间隔已填默认）

	tx, rx, lost, late uint64
	at                 time.Time
	sampled            bool
}

// Telemetry 是每台 CPE 一份的遥测采样器。并发安全由调用方保证（agent 的
// 控制循环是单线程的）；要并发调用，把它放在一个 goroutine 里。
type Telemetry struct {
	dp      TelemetryPlane
	now     func() time.Time
	tunnels map[uint32]*tunnelWindow
}

// NewTelemetry 建一个采样器。dp 是 *vppapi.Client。
func NewTelemetry(dp TelemetryPlane) *Telemetry {
	return &Telemetry{dp: dp, now: time.Now, tunnels: map[uint32]*tunnelWindow{}}
}

// Ensure 把一条隧道的遥测配置落进数据面，幂等。
//
// BFD 先读现状再决定要不要 Add：重复 Add 会把一个已 Up 的会话重建回 Init，
// 健康检查自己制造抖动。探针则每次都 Set —— 插件对同地址是原地更新、不重置
// 统计，而间隔变化只有通过 Set 才能落地。
func (t *Telemetry) Ensure(ctx context.Context, cfg wire.TunnelTelemetry) error {
	if cfg.SwIfIndex == 0 {
		return errors.New("agent: 遥测配置缺 sw_if_index")
	}
	peer, err := parseOverlayIP("对端", cfg.PeerIP)
	if err != nil {
		return err
	}
	local, err := parseOverlayIP("本地", cfg.LocalIP)
	if err != nil {
		return err
	}

	interval := cfg.IntervalMS
	if interval == 0 {
		interval = wire.DefaultProbeIntervalMS
	}
	// 超时短于间隔时窗口的槽会在回包之前就判丢，纯属自欺。插件也拦这一条，
	// 在这里拦是为了让错误出现在 agent 的日志里，而不是变成一次 API retval。
	if cfg.TimeoutMS != 0 && cfg.TimeoutMS < interval {
		return fmt.Errorf("agent: 探针超时 %dms 短于间隔 %dms", cfg.TimeoutMS, interval)
	}
	effective := cfg
	effective.IntervalMS = interval

	if err := t.dp.SetProbe(ctx, vppapi.ProbeParams{
		SwIfIndex:  cfg.SwIfIndex,
		PeerIP:     peer,
		LocalIP:    local,
		IntervalMS: interval,
		TimeoutMS:  cfg.TimeoutMS,
	}); err != nil {
		return fmt.Errorf("agent: 配探针 sw_if=%d: %w", cfg.SwIfIndex, err)
	}

	bfd := vppapi.BFDSessionParams{
		LocalIP:         local,
		PeerIP:          peer,
		DesiredMinTxUS:  cfg.BFDMinTxUS,
		RequiredMinRxUS: cfg.BFDMinRxUS,
		DetectMult:      cfg.BFDDetectMult,
	}
	if bfd.DesiredMinTxUS == 0 {
		bfd.DesiredMinTxUS = wire.DefaultBFDMinTxUS
	}
	if bfd.RequiredMinRxUS == 0 {
		bfd.RequiredMinRxUS = wire.DefaultBFDMinRxUS
	}
	if bfd.DetectMult == 0 {
		bfd.DetectMult = wire.DefaultBFDDetectMult
	}
	cur, err := t.dp.BFDSessions(ctx)
	if err != nil {
		return fmt.Errorf("agent: 读 BFD 会话: %w", err)
	}
	if !hasBFDSession(cur, bfd) {
		if err := t.dp.AddBFDSession(ctx, bfd); err != nil {
			return fmt.Errorf("agent: 建 BFD 会话 sw_if=%d: %w", cfg.SwIfIndex, err)
		}
	}

	w := t.tunnels[cfg.SwIfIndex]
	if w == nil {
		w = &tunnelWindow{}
		t.tunnels[cfg.SwIfIndex] = w
	}
	w.cfg = effective
	return nil
}

// Remove 撤掉一条隧道的遥测，幂等。
//
// BFD 删除对不存在的会话会回错误，所以先 dump 再删 —— Remove 的语义必须是
// "删完以后没有它"，而不是"只有当它恰好存在时才能调"。多跳会话不带隧道身份，
// 所以要先把这条隧道的 (local, peer) 找出来：先看自己的配置，agent 重启过就
// 从探针会话里读（探针是按 sw_if_index 建的，这一半不依赖内存）。
func (t *Telemetry) Remove(ctx context.Context, swIfIndex uint32) error {
	var errs []error
	peer, local := t.endpointsOf(ctx, swIfIndex, &errs)
	if peer != nil {
		sessions, err := t.dp.BFDSessions(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: 读 BFD 会话: %w", err))
		}
		for _, s := range sessions {
			if s.SwIfIndex != vppapi.BFDMultihopSwIfIndex || !s.PeerIP.Equal(peer) {
				continue
			}
			// 同一对端可能有多条并行隧道（local /32 不同）：只删自己那条，
			// 否则会把别人的可达性一起删掉。
			if local != nil && !s.LocalIP.Equal(local) {
				continue
			}
			if err := t.dp.DelBFDSession(ctx, vppapi.BFDSessionParams{
				LocalIP: s.LocalIP, PeerIP: s.PeerIP,
			}); err != nil {
				errs = append(errs, fmt.Errorf("agent: 删 BFD 会话 sw_if=%d: %w", swIfIndex, err))
			}
		}
	}
	if err := t.dp.DelProbe(ctx, swIfIndex); err != nil {
		errs = append(errs, fmt.Errorf("agent: 删探针 sw_if=%d: %w", swIfIndex, err))
	}
	delete(t.tunnels, swIfIndex)
	return errors.Join(errs...)
}

// endpointsOf 找出某条隧道的 overlay 两端地址。配置在就用配置（不需要一次
// dump），否则从探针会话里读 —— 这条路径也是 agent 重启后清理的唯一依据。
func (t *Telemetry) endpointsOf(ctx context.Context, swIfIndex uint32, errs *[]error) (peer, local net.IP) {
	if w := t.tunnels[swIfIndex]; w != nil {
		peer, _ = parseOverlayIP("对端", w.cfg.PeerIP)
		local, _ = parseOverlayIP("本地", w.cfg.LocalIP)
		if peer != nil {
			return peer, local
		}
	}
	probes, err := t.dp.Probes(ctx)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("agent: 读探针会话: %w", err))
		return nil, nil
	}
	for _, p := range probes {
		if p.SwIfIndex == swIfIndex {
			return p.PeerIP, p.LocalIP
		}
	}
	return nil, nil
}

// Sample 读一次数据面，返回每个窗口的增量与当前状态。
//
// 行是"探针 ∪ BFD"：只有 BFD、没有探针的隧道照样要报 —— 可达性是遥测的一半，
// 探针没配起来不该让这条隧道从视图里消失（那正好是最需要被看见的故障）。
// 但 BFD 行必须能映射回一条隧道（cfg 里有这个 peer 的 sw_if_index），否则它
// 不是本组件管的隧道，报出去也没有隧道键可用。
func (t *Telemetry) Sample(ctx context.Context) ([]wire.TunnelMetrics, error) {
	probes, err := t.dp.Probes(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: 读探针会话: %w", err)
	}
	bfds, err := t.dp.BFDSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: 读 BFD 会话: %w", err)
	}

	byPeer := make(map[string]vppapi.BFDSession, len(bfds))
	for _, s := range bfds {
		// 只认多跳会话。绑接口的会话会让隧道全丢（见文件头），它不是我们建的，
		// 也不能替一条隧道报"可达"。
		if s.SwIfIndex == vppapi.BFDMultihopSwIfIndex {
			byPeer[s.PeerIP.String()] = s
		}
	}
	swIfOfPeer := make(map[string]uint32, len(t.tunnels))
	for swIf, w := range t.tunnels {
		if ip, err := parseOverlayIP("对端", w.cfg.PeerIP); err == nil {
			swIfOfPeer[ip.String()] = swIf
		}
	}

	now := t.now()
	out := make([]wire.TunnelMetrics, 0, len(probes)+len(bfds))
	seenPeer := make(map[string]bool, len(probes))
	for _, p := range probes {
		seenPeer[p.PeerIP.String()] = true
		bfd, ok := byPeer[p.PeerIP.String()]
		out = append(out, t.row(p, bfd, ok, now, true))
	}
	for _, s := range bfds {
		if s.SwIfIndex != vppapi.BFDMultihopSwIfIndex {
			continue
		}
		k := s.PeerIP.String()
		if seenPeer[k] {
			continue
		}
		seenPeer[k] = true
		swIf, ok := swIfOfPeer[k]
		if !ok {
			continue
		}
		out = append(out, t.row(vppapi.ProbeStats{SwIfIndex: swIf}, s, true, now, false))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SwIfIndex < out[j].SwIfIndex })
	return out, nil
}

// row 把一条探针会话（可能有）与 BFD 会话（可能有）合成一行，并推进窗口。
// hasProbe=false 时 p 只带 sw_if_index；hasBFD 区分"状态恰好是 admin-down"
// 与"根本没有会话"。
func (t *Telemetry) row(p vppapi.ProbeStats, bfd vppapi.BFDSession, hasBFD bool, now time.Time, hasProbe bool) wire.TunnelMetrics {
	m := wire.TunnelMetrics{
		SwIfIndex:  p.SwIfIndex,
		BFDState:   "absent",
		IntervalMS: p.IntervalMS,
	}

	w := t.tunnels[p.SwIfIndex]
	if w != nil {
		m.PeerNodeID = w.cfg.PeerNodeID
	}
	if hasBFD {
		m.BFDState = vppapi.BFDStateName(bfd.State)
		m.Up = bfd.State == vppapi.BFDStateUp
		m.PeerIP = bfd.PeerIP.String()
	}

	if !hasProbe {
		// 没有探针就没有质量数据，但可达性照报。窗口不推进：这条隧道没在我们
		// 管理的探针集合里，拿不到计数器。
		return m
	}

	m.PeerIP = p.PeerIP.String()
	if m.IntervalMS == 0 && w != nil {
		m.IntervalMS = w.cfg.IntervalMS
	}
	m.LossPermille = p.LossPermille
	m.RTTms = usToMs(p.LastRTTus)
	m.MinRTTms = usToMs(p.MinRTTus)
	m.AvgRTTms = usToMs(p.AvgRTTus)
	m.MaxRTTms = usToMs(p.MaxRTTus)
	m.JitterMs = usToMs(p.JitterUs)

	if w == nil {
		// 会话不是本组件建的（agent 重启、或操作员手敲了 CLI）：把当前累计值
		// 整个当成第一个窗口，而不是从 0 开始假装之前没流量。
		w = &tunnelWindow{cfg: wire.TunnelTelemetry{SwIfIndex: p.SwIfIndex}}
		t.tunnels[p.SwIfIndex] = w
	}
	if w.sampled {
		m.ProbeTx = counterDelta(p.Tx, w.tx)
		m.ProbeRx = counterDelta(p.Rx, w.rx)
		m.ProbeLost = counterDelta(p.Lost, w.lost)
		m.ProbeLate = counterDelta(p.LateReplies, w.late)
		m.WindowSec = uint32(now.Sub(w.at) / time.Second)
		if m.ProbeTx > 0 {
			m.LossPermille = uint32((m.ProbeLost*1000 + m.ProbeTx/2) / m.ProbeTx)
		}
	} else {
		m.ProbeTx, m.ProbeRx = p.Tx, p.Rx
		m.ProbeLost, m.ProbeLate = p.Lost, p.LateReplies
		if m.ProbeTx > 0 {
			m.LossPermille = uint32((m.ProbeLost*1000 + m.ProbeTx/2) / m.ProbeTx)
		}
	}
	w.tx, w.rx, w.lost, w.late = p.Tx, p.Rx, p.Lost, p.LateReplies
	w.at = now
	w.sampled = true
	return m
}

// counterDelta 处理会话重建：累计值变小只能是归零，此时这个窗口的内容就是
// 新会话到目前为止的全部计数（旧会话那段已经不可恢复，不该报成"丢包"）。
func counterDelta(cur, prev uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

func usToMs(us uint32) float64 { return float64(us) / 1000 }

func parseOverlayIP(what, s string) (net.IP, error) {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("agent: 遥测%s地址 %q 不是 IPv4（隧道内是手写的 IPv4 头）", what, s)
	}
	return ip.To4(), nil
}

// hasBFDSession 判断现有会话里是否已经有一条参数完全相同的**多跳**会话。
// 绑接口的会话不算：即使参数一样，它也不是本组件要的那一条。
func hasBFDSession(sessions []vppapi.BFDSession, p vppapi.BFDSessionParams) bool {
	for _, s := range sessions {
		if s.SwIfIndex != vppapi.BFDMultihopSwIfIndex ||
			!s.LocalIP.Equal(p.LocalIP) || !s.PeerIP.Equal(p.PeerIP) {
			continue
		}
		if s.DesiredMinTxUS == p.DesiredMinTxUS &&
			s.RequiredMinRxUS == p.RequiredMinRxUS &&
			s.DetectMult == p.DetectMult {
			return true
		}
	}
	return false
}
