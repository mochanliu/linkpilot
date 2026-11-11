package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
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

	// IfStats 读全部接口的 rx/tx 累计计数（VPP stats 共享内存段）。读不到
	// 报错即可——计数是监控的加分项，绝不能因为它拖垮质量上报。
	IfStats(ctx context.Context) ([]vppapi.IfCounter, error)
}

var _ TelemetryPlane = (*vppapi.Client)(nil)

// tunnelWindow 是 agent 侧的采样窗口。只记"上次读到的累计值"，不缓存任何
// 数据面状态：真值永远来自 Probes/BFDSessions 的 dump。
type tunnelWindow struct {
	cfg wire.TunnelTelemetry // 规范化后的配置（间隔已填默认）

	tx, rx, lost, late uint64
	at                 time.Time
	sampled            bool

	// 接口计数器有自己的窗口状态：一条隧道可能先有 BFD、探针一直没配起来，
	// 计数器照样要算增量。counted/cAt 与 sampled/at 分开，避免"计数窗口把
	// 探针窗口带跑了"和反过来的串味。
	rxPk, rxBy, txPk, txBy uint64
	cAt                    time.Time
	counted                bool
}

// Telemetry 是每台 CPE 一份的遥测采样器。Ensure/Remove 来自隧道对账，
// Sample 来自上报循环，两者在不同 goroutine 里跑，所以这里自带一把锁。
type Telemetry struct {
	mu      sync.Mutex
	src     func() (TelemetryPlane, error)
	now     func() time.Time
	tunnels map[uint32]*tunnelWindow

	// logf 是可选的（TunnelSync 接线时给）。接口计数读不到时喊一次——
	// "流量"列显示未上报而没有一行日志，运维只会以为 agent 坏了。
	logf        func(string, ...any)
	statsWarned bool
}

// SetLogf 给采样器接日志（在 NewTunnelSync 里接线；不接就是哑的）。
func (t *Telemetry) SetLogf(logf func(string, ...any)) { t.logf = logf }

// NewTelemetry 建一个采样器，固定用 dp 这条连接。dp 是 *vppapi.Client。
func NewTelemetry(dp TelemetryPlane) *Telemetry {
	return newTelemetry(func() (TelemetryPlane, error) {
		if dp == nil {
			return nil, errors.New("agent: 遥测：没有数据面连接")
		}
		return dp, nil
	})
}

// newTelemetry 每次调用都现取连接。VPP 重启后 agent 会换一条 api 连接，
// 遥测抱着旧句柄只会让每次采样都报同一个错，所以由调用方给一个取当前连接的函数。
func newTelemetry(src func() (TelemetryPlane, error)) *Telemetry {
	return &Telemetry{src: src, now: time.Now, tunnels: map[uint32]*tunnelWindow{}}
}

func (t *Telemetry) plane() (TelemetryPlane, error) { return t.src() }

// Ensure 把一条隧道的遥测配置落进数据面，幂等。
//
// BFD 先读现状再决定要不要动：会话在且定时器相同就不碰（重复 Add 会把一个
// 已 Up 的会话重建回 Init，健康检查自己制造抖动）；同 (local, peer) 但定时器
// 不同 = 租户改了剖面，VPP 没有原地改参数的 API、对已存在会话的 Add 一律回
// BFD_EEXIST（-101，#287 实测），只能先删再建 —— 删除会把会话打回 Init、
// 几秒内重新 Up，这是「改剖面」的固有代价。探针则每次都 Set —— 插件对同
// 地址是原地更新、不重置统计，而间隔变化只有通过 Set 才能落地。
func (t *Telemetry) Ensure(ctx context.Context, cfg wire.TunnelTelemetry) error {
	if cfg.SwIfIndex == 0 {
		return errors.New("agent: 遥测配置缺 sw_if_index")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	dp, err := t.plane()
	if err != nil {
		return err
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

	if err := dp.SetProbe(ctx, vppapi.ProbeParams{
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
	cur, err := dp.BFDSessions(ctx)
	if err != nil {
		return fmt.Errorf("agent: 读 BFD 会话: %w", err)
	}
	if !hasBFDSession(cur, bfd) {
		if hasBFDKey(cur, bfd) {
			if err := dp.DelBFDSession(ctx, bfd); err != nil {
				return fmt.Errorf("agent: 换 BFD 定时器先删旧会话 sw_if=%d: %w", cfg.SwIfIndex, err)
			}
		}
		if err := dp.AddBFDSession(ctx, bfd); err != nil {
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
	t.mu.Lock()
	defer t.mu.Unlock()
	dp, err := t.plane()
	if err != nil {
		return err
	}
	var errs []error
	peer, local := t.endpointsOf(ctx, dp, swIfIndex, &errs)
	if peer != nil {
		sessions, err := dp.BFDSessions(ctx)
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
			if err := dp.DelBFDSession(ctx, vppapi.BFDSessionParams{
				LocalIP: s.LocalIP, PeerIP: s.PeerIP,
			}); err != nil {
				errs = append(errs, fmt.Errorf("agent: 删 BFD 会话 sw_if=%d: %w", swIfIndex, err))
			}
		}
	}
	if err := dp.DelProbe(ctx, swIfIndex); err != nil {
		errs = append(errs, fmt.Errorf("agent: 删探针 sw_if=%d: %w", swIfIndex, err))
	}
	delete(t.tunnels, swIfIndex)
	return errors.Join(errs...)
}

// endpointsOf 找出某条隧道的 overlay 两端地址。配置在就用配置（不需要一次
// dump），否则从探针会话里读 —— 这条路径也是 agent 重启后清理的唯一依据。
func (t *Telemetry) endpointsOf(ctx context.Context, dp TelemetryPlane, swIfIndex uint32, errs *[]error) (peer, local net.IP) {
	if w := t.tunnels[swIfIndex]; w != nil {
		peer, _ = parseOverlayIP("对端", w.cfg.PeerIP)
		local, _ = parseOverlayIP("本地", w.cfg.LocalIP)
		if peer != nil {
			return peer, local
		}
	}
	probes, err := dp.Probes(ctx)
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
	t.mu.Lock()
	defer t.mu.Unlock()
	dp, err := t.plane()
	if err != nil {
		return nil, err
	}
	probes, err := dp.Probes(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: 读探针会话: %w", err)
	}
	bfds, err := dp.BFDSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: 读 BFD 会话: %w", err)
	}
	// 接口计数读不到（stats 段没开、目录重建）只影响"流量"这一列，本窗标
	// has_counters=false 即可，绝不能让它把质量上报整个拦下。失败喊一次：
	// 每窗都喊会把日志刷成瀑布，一句"为什么是未上报"的答案已经够了。
	ifStats, err := dp.IfStats(ctx)
	if err != nil {
		ifStats = nil
		if t.logf != nil && !t.statsWarned {
			t.statsWarned = true
			t.logf("telemetry: 接口计数读不到，流量列将显示未上报: %v", err)
		}
	}
	ifMap := make(map[uint32]vppapi.IfCounter, len(ifStats))
	for _, c := range ifStats {
		ifMap[c.SwIfIndex] = c
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
		out = append(out, t.row(p, bfd, ok, now, true, ifMap))
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
		out = append(out, t.row(vppapi.ProbeStats{SwIfIndex: swIf}, s, true, now, false, ifMap))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SwIfIndex < out[j].SwIfIndex })
	return out, nil
}

// row 把一条探针会话（可能有）与 BFD 会话（可能有）合成一行，并推进窗口。
// hasProbe=false 时 p 只带 sw_if_index；hasBFD 区分"状态恰好是 admin-down"
// 与"根本没有会话"。ifStats 是本次采样读到的接口计数（可能为 nil = 没读到，
// 行上 has_counters=false，不是零流量）。
func (t *Telemetry) row(p vppapi.ProbeStats, bfd vppapi.BFDSession, hasBFD bool, now time.Time, hasProbe bool, ifStats map[uint32]vppapi.IfCounter) wire.TunnelMetrics {
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

	// 接口计数：探针行和只有 BFD 的行都带 —— 流量是监控的一半，探针没配
	// 起来不该让"这条隧道每天走多少字节"跟着消失。窗口独立于探针（见
	// tunnelWindow.counted）：计数从哪个窗口开始、增量就从哪个窗口算起。
	if c, ok := ifStats[p.SwIfIndex]; ok {
		m.HasCounters = true
		if w == nil {
			w = &tunnelWindow{}
			t.tunnels[p.SwIfIndex] = w
		}
		if w.counted {
			m.RxPackets = counterDelta(c.RxPackets, w.rxPk)
			m.RxBytes = counterDelta(c.RxBytes, w.rxBy)
			m.TxPackets = counterDelta(c.TxPackets, w.txPk)
			m.TxBytes = counterDelta(c.TxBytes, w.txBy)
			m.CounterSec = uint32(now.Sub(w.cAt) / time.Second)
		} else {
			// 第一个计数窗口：报累计，CounterSec=0，消费方按"累计"读。
			m.RxPackets, m.RxBytes = c.RxPackets, c.RxBytes
			m.TxPackets, m.TxBytes = c.TxPackets, c.TxBytes
		}
		w.rxPk, w.rxBy = c.RxPackets, c.RxBytes
		w.txPk, w.txBy = c.TxPackets, c.TxBytes
		w.cAt, w.counted = now, true
	} else if w != nil {
		// 这窗没读到：窗口作废，下一窗重新从"累计"起步——计数器没读到的
		// 那段流量不可恢复，编一个增量出来比空着更误导。
		w.counted = false
	}

	if !hasProbe {
		// 没有探针就没有质量数据，但可达性照报。窗口不推进：这条隧道没在我们
		// 管理的探针集合里，拿不到计数器。
		return m
	}

	m.HasProbe = true
	m.PeerIP = p.PeerIP.String()
	if m.IntervalMS == 0 && w != nil {
		m.IntervalMS = w.cfg.IntervalMS
	}
	// 丢包率只有一个来源：插件自己那个 128 槽滑动窗口里的 lost/(replied+lost)。
	// 分子分母都在同一张窗口里数，天然落在 0-1000‰，而且"没丢"就是 0。
	//
	// 之前这里在 agent 侧用两次采样的**差值**再算一遍（lost_delta/tx_delta），那
	// 是个没有上界的比值：一条探针在窗口 k 发出去、超时（3×间隔）之后在窗口 k+1
	// 才被判丢，它就只进 k+1 的分子、不进 k+1 的分母；迟到回包又只加 late、不加
	// rx。于是"丢包率"会在 97% 和 103% 之间抖 —— 监控页显示的是百分比，读者按
	// 0-100% 理解它，抖出 103% 就是口径错了，不是抖动。
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
	} else {
		// 第一个采样窗口：没有可比的前一帧，WindowSec 保持 0，报的是设备从会话
		// 建立到现在的累计值。这四个数因此不能当"这一窗口的量"读，页面按
		// window_sec==0 标成累计。丢包率不受影响 —— 它来自插件的窗口。
		m.ProbeTx, m.ProbeRx = p.Tx, p.Rx
		m.ProbeLost, m.ProbeLate = p.Lost, p.LateReplies
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

// hasBFDKey 只按 (local, peer) 认会话、不比定时器：Ensure 用它区分
// "会话不在，直接新建" 和 "会话在但定时器是旧的，先删再建"。VPP 对后者的
// 直接 Add 回 BFD_EEXIST，旧代码在这里无限退避重试、新剖面永远落不了地。
func hasBFDKey(sessions []vppapi.BFDSession, p vppapi.BFDSessionParams) bool {
	for _, s := range sessions {
		if s.SwIfIndex == vppapi.BFDMultihopSwIfIndex &&
			s.LocalIP.Equal(p.LocalIP) && s.PeerIP.Equal(p.PeerIP) {
			return true
		}
	}
	return false
}
