// Package sla 是动态策略的选路组件（"CPE 上专门盯 SLA 的那一个"）。
//
// 它不做测量：隧道内的延迟/抖动/丢包由插件探针（sr_wg_probe，每条 wg 接口一个
// 会话）量好，这里只读。一个 wg 接口就是一条颜色隧道，所以"按隧道读指标"天然
// 就是"按颜色读指标"。
//
// 它也不碰 ACL/ABF：动态行的 ABF 指向这一行自己的**选路表**（第 7 段渲染的
// selector_table，见 internal/apply/policy.go），选路表里只有一条 0.0.0.0/0 的
// deag 路由指向当前选中的颜色表。改选路 = 重写那一条路由，一次 API 调用，ACL
// 与 ABF 原封不动 —— 这正是把"现场挑颜色"从第 7 段里拆出来的原因：改 ABF 要走
// 摘挂点→删策略→建策略→挂回，秒级重选来一遍既慢又危险（删挂着的策略会崩 VPP，
// 实测 2026-11-10）。
//
// 语义（owner 2026-11-10 拍）：
//
//   - 达标 = 探针有样本，且每项设置的门槛（延迟/抖动/丢包，0 = 不约束）都满足；
//   - 达标的里面挑**延迟最低**的颜色（并列看抖动、丢包、颜色号）；
//   - 全部不达标 = 走**最接近**的一条（丢包最少 → 延迟最低），SLA 是偏好不是
//     准入，不因为一时抖动断流；
//   - Preempt（owner 2026-11-11 补）：默认**粘住** —— 当前隧道还达标就一直用，
//     更优的候选不抢；勾了抢占才永远追最优。切换本身有迟滞：新颜色要连续赢
//     holdRounds 拍才切；但当前颜色 100% 丢包（探针全判丢）时立即切，不等迟滞；
//   - 选路表空（VPP 重启过、路由没了）时立即恢复写入，不经过迟滞；
//   - 每次（重）连上 VPP 后强制重写一遍（连接代数，2026-11-11 实机补）：VPP 重启
//     会换掉整套表，apply 管道会把选路表的 0/0 重新种成 drop。此时"表里有 0/0"
//     为真、内存里的 written 也为真、决策也没变 —— 只看存在性会把种子 drop 路由
//     当成自己写的，永远不再写，实机 CPE1 就这样黑了。所以落表记录"写在第几代
//     连接上"，只有同代才允许跳过；重拨必然换代，换代必然重写一次（幂等替换）。
//
// 宿主是 sr-agent：它已经拥有探针会话的生命周期、隧道→对端→颜色的映射和到控制
// 器的上报通道，单独起进程得把这三样再实现一遍，还会出现两个客户端抢同一批会话。
// 本包对 sr-agent 没有任何依赖，哪天要独立成进程，搬走的是一个目录不是一个功能。
package sla

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

// holdRounds 是切换迟滞：新颜色要连续赢这么多拍（每拍一个 Tick）才真正切过去。
// 3 拍 × 1 秒 = 3 秒，比探针的超时窗口（3 个间隔 ≈ 0.9s）长，一次孤立的抖动
// 不会切，而真实的链路劣化在 3 秒内会持续体现。
const holdRounds = 3

// Row 是一条动态策略行，从第 7 段解码（字段与 apply.policyRowDoc 对齐）。
type Row struct {
	ID              int
	VPN             uint32
	Peer            uint16
	Colors          []uint8 // 候选颜色；空 = 该对端全部颜色
	MaxLatencyMS    float64 // 0 = 不约束
	MaxJitterMS     float64 // 0 = 不约束
	MaxLossPermille uint32  // 0 = 不约束
	// Preempt（owner 2026-11-11 补）：默认 false = 粘住 —— 当前隧道只要还达标
	// 就一直用，别的隧道更优也不切；它不达标且有达标候选可用才切。true = 抢占，
	// 永远追最优（下方迟滞语义）。两种模式的其余路径（100% 丢包立即切、颜色被
	// 挤掉立即重选、全不达标走最接近）完全一致。
	Preempt       bool
	SelectorTable uint32
}

// Tunnel 是这台盒子的一条隧道（agent.TunnelSync.FibRows 的形状 + 对端节点号）。
type Tunnel struct {
	SWIfIndex uint32
	Peer      uint16
	Color     uint8
	NextHop   netip.Addr
}

// Sample 是一条隧道的探针读数（vppapi.ProbeStats 里选路要用的那几个）。
type Sample struct {
	SwIfIndex    uint32
	HasProbe     bool
	Tx           uint64
	LossPermille uint32
	RTTms        float64
	JitterMs     float64
}

// Plane 是选路组件要的 VPP 动作。探针 dump 是只读的：会话的建立与删除归
// agent.Telemetry，这里绝不能写（两个写者对同一张会话表是竞态）。
type Plane interface {
	Probes(context.Context) ([]vppapi.ProbeStats, error)
	IPRoutes(context.Context, uint32) ([]netip.Prefix, error)
	IPRouteAddDel(context.Context, uint32, netip.Prefix, []vppapi.FIBPath, bool) error
}

// rowState 是一行在两次 Tick 之间的记忆。
type rowState struct {
	current uint8 // 当前生效的颜色（与选路表里的路由一致）
	written bool  // 这个值真的写下去过（false = 还没写过任何路由）
	// writtenGen 是 written 那次写入所在的连接代数。跳过条件要求它与 Selector.gen
	// 同代：重拨（VPP 重启、连接出错后恢复）换代，换代后哪怕决策没变、表里也有
	// 0/0（可能是 apply 重种的 drop），也要幂等重写一次，把自己的颜色路由盖回去。
	writtenGen uint64
	pending    uint8  // 连续赢着的新候选（迟滞计数的对象）
	pendingN   int    // 已连续赢的拍数
	reason     string // 最近一次决策的人话解释
	updatedAt  time.Time
}

// Selector 是一台盒子上的选路决策器。Tick 必须串行（内部锁保证）；
// Reconcile 随时可以调（agent 每次拿到新 bundle / 新隧道表就喂一次）。
type Selector struct {
	mu      sync.Mutex
	dial    func(context.Context) (Plane, error)
	client  Plane  // 上一次拨成功的连接；任何一次 API 失败都把它置回 nil，下一拍重拨
	gen     uint64 // 连接代数：每成功拨一次 +1。换代后所有行强制幂等重写
	logf    func(string, ...any)
	now     func() time.Time
	rows    []Row
	tunnels []Tunnel
	states  map[int]*rowState // 按行号
}

// NewSelector 建一个选路器。dial 每次失败后由组件自己重拨（VPP 重启会换掉
// 所有连接，抱着旧句柄只会让每一拍都报同一个错 —— 与 TunnelSync 同一条纪律）。
// logf 为空时不打日志。
func NewSelector(dial func(context.Context) (Plane, error), logf func(string, ...any)) *Selector {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Selector{
		dial:   dial,
		logf:   logf,
		now:    time.Now,
		states: map[int]*rowState{},
	}
}

// plane 取当前连接；坏了就重拨。每次成功拨号新开一代（gen++）—— 上一代写下去
// 的路由属于旧连接背后的那个 VPP 世界，换代后必须重写一遍才算数。
func (s *Selector) planeConn(ctx context.Context) (Plane, error) {
	if s.client != nil {
		return s.client, nil
	}
	p, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.client = p
	s.gen++
	return p, nil
}

// Reconcile 换上这一轮的行与隧道清单。整份替换：行随 bundle 变，隧道随
// VPP 重启变（sw_if_index 会变，探针会话也跟着重建）。行的增删会重置那一行的
// 决策状态 —— 行号被复用（删行再加）时，旧状态指向的是别人。
func (s *Selector) Reconcile(rows []Row, tunnels []Tunnel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append([]Row(nil), rows...)
	s.tunnels = append([]Tunnel(nil), tunnels...)
	live := map[int]bool{}
	for _, r := range rows {
		live[r.ID] = true
		if _, ok := s.states[r.ID]; !ok {
			s.states[r.ID] = &rowState{}
		}
	}
	for id := range s.states {
		if !live[id] {
			delete(s.states, id)
		}
	}
}

// SetTunnels 只换隧道清单（行不动）：每拍都重读，因为 VPP 重启会换
// sw_if_index，隧道表是运行时读数，不是配置。
func (s *Selector) SetTunnels(tunnels []Tunnel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tunnels = append([]Tunnel(nil), tunnels...)
}

// Tick 跑一轮评估：读探针 → 每行动态行做一个决策 → 必要时写选路表。
// 返回的错误只是"这一轮没跑成"（VPP 没连上等），状态保留，下一轮继续 ——
// 选路器不许因为一轮失败就停摆。
func (s *Selector) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) == 0 {
		return nil
	}
	p, err := s.planeConn(ctx)
	if err != nil {
		return fmt.Errorf("sla: 连 VPP: %w", err)
	}
	stats, err := p.Probes(ctx)
	if err != nil {
		s.client = nil // 下一拍重拨
		return fmt.Errorf("sla: 读探针指标: %w", err)
	}
	byIf := make(map[uint32]Sample, len(stats))
	for _, p := range stats {
		byIf[p.SwIfIndex] = Sample{
			SwIfIndex:    p.SwIfIndex,
			HasProbe:     true,
			Tx:           p.Tx,
			LossPermille: p.LossPermille,
			RTTms:        float64(p.LastRTTus) / 1000,
			JitterMs:     float64(p.JitterUs) / 1000,
		}
	}

	for _, r := range s.rows {
		st := s.states[r.ID]
		if st == nil {
			st = &rowState{}
			s.states[r.ID] = st
		}
		color, reason := s.decide(r, byIf, st)
		st.reason = reason
		st.updatedAt = s.now()
		// 落表：决策变了、表空了（VPP 重启/路由被冲掉）、或者换代了（重拨后
		// 第一次落表机会）才写。表空不能只信内存里的 written —— 重启之后
		// written 还是 true，但 VPP 里什么都没有，所以每一拍都查一次存在性。
		// 存在性也只说明"有个 0/0"：VPP 重启后 apply 会把 0/0 重种成 drop，
		// 所以重拨之后（writtenGen != gen）哪怕决策没变也要重写一次盖掉它。
		exists, err := s.routeExists(ctx, r.SelectorTable)
		if err != nil {
			return fmt.Errorf("sla: 行 %d 读选路表 %d: %w", r.ID, r.SelectorTable, err)
		}
		if exists && st.written && st.writtenGen == s.gen && color == st.current {
			continue
		}
		if err := s.writeRoute(ctx, r, color); err != nil {
			return fmt.Errorf("sla: 行 %d 写选路表 %d: %w", r.ID, r.SelectorTable, err)
		}
		s.logf("sla: 行 %d → 颜色 %d（%s）", r.ID, color, reason)
	}
	return nil
}

// decide 给一行做一次决策，并推进迟滞状态。返回（生效颜色，解释）。
// 它不改选路表，也不写 st.current（那是 Tick 在真正落表之后的事）—— 副作用
// 只有 st 上的迟滞计数。
//
// 三个分支的次序就是语义：
//  1. 当前已经是最优 → 保持（最常见路径，零动作）；
//  2. 有比当前更优的候选，且**两边都达标** → 看 Preempt：不勾（默认，粘住）就
//     按兵不动；勾了（抢占）走迟滞，新候选连续赢 holdRounds 拍才切；
//  3. 其余（当前不达标 / 当前颜色已不在候选里）→ 立即切到最优。当前 100% 丢包、
//     首次选择、当前颜色被配置挤掉、SLA 不达标四种来路各有一句原因。
func (s *Selector) decide(r Row, byIf map[uint32]Sample, st *rowState) (uint8, string) {
	// 候选 = 到这个对端的、颜色在候选集里的隧道，按颜色聚出"每颜色最好的一条"。
	byColor := map[uint8]candidate{}
	for _, t := range s.tunnels {
		if t.Peer != r.Peer {
			continue
		}
		if len(r.Colors) > 0 && !containsColor(r.Colors, t.Color) {
			continue
		}
		smp := byIf[t.SWIfIndex] // 没有探针会话的隧道拿到零值：HasProbe=false
		if cur, ok := byColor[t.Color]; !ok || better(smp, cur.s) {
			byColor[t.Color] = candidate{color: t.Color, s: smp}
		}
	}
	if len(byColor) == 0 {
		// 对端不存在/没有候选隧道：保持现状。隧道表空通常是隧道正在收敛
		// （bundle 刚落地、VPP 刚重启），几拍之后就有了。
		return st.current, fmt.Sprintf("暂无到对端 %d 的候选隧道，保持颜色 %d", r.Peer, st.current)
	}

	list := make([]candidate, 0, len(byColor))
	for _, c := range byColor {
		list = append(list, c)
	}
	// 排序即选择：达标的在前；同组里丢包少 → 延迟低 → 抖动低 → 颜色号小。
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ea, eb := eligible(r, a.s), eligible(r, b.s); ea != eb {
			return ea
		}
		if a.color != b.color && better(a.s, b.s) == better(b.s, a.s) && a.s == b.s {
			return a.color < b.color // 读数完全相同：颜色号定序，结果确定
		}
		return better(a.s, b.s)
	})
	best := list[0]

	if best.color == st.current {
		st.pendingN = 0
		return st.current, keepReason(best, r)
	}
	cur, hasCur := byColor[st.current]
	if hasCur && eligible(r, cur.s) && eligible(r, best.s) {
		if !r.Preempt {
			// 粘住模式（默认）：当前隧道还达标就按兵不动，即使别的更优。
			st.pendingN = 0
			return st.current, fmt.Sprintf("保持颜色 %d（未勾抢占：仍达标，延迟 %.1fms，抖动 %.1fms，丢包 %d‰；颜色 %d 更优（延迟 %.1fms）但不抢占）",
				st.current, cur.s.RTTms, cur.s.JitterMs, cur.s.LossPermille, best.color, best.s.RTTms)
		}
		// 抢占模式：双方都达标，只是当前不是最优：迟滞。
		if st.pending == best.color {
			st.pendingN++
		} else {
			st.pending = best.color
			st.pendingN = 1
		}
		if st.pendingN < holdRounds {
			return st.current, fmt.Sprintf("保持颜色 %d（颜色 %d 连续 %d/%d 拍更优，未到切换线）",
				st.current, best.color, st.pendingN, holdRounds)
		}
		st.pendingN = 0
		return best.color, switchReason(best, r, "当前颜色仍达标，但颜色 "+fmt.Sprint(best.color)+" 连续 "+fmt.Sprint(holdRounds)+" 拍更优")
	}
	st.pendingN = 0
	if hasCur && cur.s.HasProbe && cur.s.Tx > 0 && cur.s.LossPermille >= 1000 {
		// 当前颜色 100% 丢包：立即切，不等迟滞 —— 等三秒等于断三秒。
		return best.color, switchReason(best, r, "当前颜色 100% 丢包，立即切")
	}
	if !hasCur {
		// 当前颜色不在候选里。两种来路要说人话：第一次决策（什么都没选过），
		// 和配置变了把当前颜色挤掉（改颜色、删隧道）。
		if !st.written {
			return best.color, switchReason(best, r, "首次选择")
		}
		colors := make([]string, 0, len(list))
		for _, c := range list {
			colors = append(colors, fmt.Sprint(c.color))
		}
		return best.color, switchReason(best, r,
			fmt.Sprintf("当前颜色 %d 已不在候选隧道里（候选：%s），按 SLA 重选", st.current, strings.Join(colors, ", ")))
	}
	return best.color, switchReason(best, r, "当前颜色不再达标（SLA 是偏好不是准入，走最接近的一条）")
}

// eligible 判一条读数过不过这一行的门槛。
func eligible(r Row, smp Sample) bool {
	if !smp.HasProbe || smp.Tx == 0 {
		return false // 没有样本不算达标："没量过"和"量出来很好"不是一回事
	}
	if r.MaxLossPermille > 0 && smp.LossPermille > r.MaxLossPermille {
		return false
	}
	if r.MaxLatencyMS > 0 && smp.RTTms > r.MaxLatencyMS {
		return false
	}
	if r.MaxJitterMS > 0 && smp.JitterMs > r.MaxJitterMS {
		return false
	}
	return true
}

// candidate 是聚合到"每颜色最好一条"之后的候选：颜色 + 它的探针读数。
type candidate struct {
	color uint8
	s     Sample
}

// better 是候选间的比较序：丢包少者优先，再比延迟、抖动。颜色号在调用方处理。
func better(a, b Sample) bool {
	if a.LossPermille != b.LossPermille {
		return a.LossPermille < b.LossPermille
	}
	if a.RTTms != b.RTTms {
		return a.RTTms < b.RTTms
	}
	return a.JitterMs < b.JitterMs
}

func containsColor(cs []uint8, c uint8) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

// switchReason / keepReason 生成上报给控制台的一句话。它们只说量出来的事实，
// 不说"应该" —— 读的人要能从这句话反推出当时的探针读数。
func switchReason(c candidate, r Row, why string) string {
	return fmt.Sprintf("%s：颜色 %d（延迟 %.1fms，抖动 %.1fms，丢包 %d‰，%s）",
		why, c.color, c.s.RTTms, c.s.JitterMs, c.s.LossPermille, slaText(r))
}

func keepReason(c candidate, r Row) string {
	if eligible(r, c.s) {
		return fmt.Sprintf("颜色 %d 当前最优且达标（延迟 %.1fms，抖动 %.1fms，丢包 %d‰）",
			c.color, c.s.RTTms, c.s.JitterMs, c.s.LossPermille)
	}
	return fmt.Sprintf("无颜色达标，颜色 %d 最接近（延迟 %.1fms，抖动 %.1fms，丢包 %d‰，SLA 是偏好不是准入）",
		c.color, c.s.RTTms, c.s.JitterMs, c.s.LossPermille)
}

func slaText(r Row) string {
	var parts []string
	if r.MaxLatencyMS > 0 {
		parts = append(parts, fmt.Sprintf("延迟≤%gms", r.MaxLatencyMS))
	}
	if r.MaxJitterMS > 0 {
		parts = append(parts, fmt.Sprintf("抖动≤%gms", r.MaxJitterMS))
	}
	if r.MaxLossPermille > 0 {
		parts = append(parts, fmt.Sprintf("丢包≤%d‰", r.MaxLossPermille))
	}
	if len(parts) == 0 {
		return "不设门槛"
	}
	return strings.Join(parts, "，")
}

// routeExists 查选路表里还有没有那条默认路由。VPP 重启会把所有表清掉，
// 内存里的 written 挡不住这个，所以每一拍都查一次 —— 一个 dump 换"重启后
// 秒级恢复"，是便宜的。
func (s *Selector) routeExists(ctx context.Context, table uint32) (bool, error) {
	p, err := s.planeConn(ctx)
	if err != nil {
		return false, err
	}
	pfx, err := p.IPRoutes(ctx, table)
	if err != nil {
		s.client = nil
		return false, err
	}
	for _, p := range pfx {
		if p == netip.MustParsePrefix("0.0.0.0/0") {
			return true, nil
		}
	}
	return false, nil
}

// writeRoute 把选路表的默认路由指到 color 的颜色表。ip_route_add_del 的
// is_add=1 对已存在前缀是**替换路径**（VPP 语义），所以不用先删。
func (s *Selector) writeRoute(ctx context.Context, r Row, color uint8) error {
	p, err := s.planeConn(ctx)
	if err != nil {
		return err
	}
	table, err := fib.ColorTable(color, r.VPN)
	if err != nil {
		return err
	}
	path := vppapi.FIBPath{SWIfIndex: ^uint32(0), TableID: table}
	if err := p.IPRouteAddDel(ctx, r.SelectorTable, netip.MustParsePrefix("0.0.0.0/0"), []vppapi.FIBPath{path}, true); err != nil {
		s.client = nil
		return err
	}
	st := s.states[r.ID]
	if st != nil {
		st.current = color
		st.written = true
		st.writtenGen = s.gen
	}
	return nil
}

// Status 是给上报用的快照：每行动态行的当前颜色与解释。行号与第 7 段一致。
func (s *Selector) Status() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]State, 0, len(s.rows))
	for _, r := range s.rows {
		st := s.states[r.ID]
		if st == nil {
			continue
		}
		out = append(out, State{
			RowID:     r.ID,
			VPN:       r.VPN,
			Color:     st.current,
			Reason:    st.reason,
			UpdatedAt: st.updatedAt,
		})
	}
	return out
}

// State 是一行选路状态的外部形状（wire.PolicySelection 的源）。
type State struct {
	RowID     int
	VPN       uint32
	Color     uint8
	Reason    string
	UpdatedAt time.Time
}
