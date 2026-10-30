// Package fib turns the control plane's RIB into VPP FIB state.
//
// 这段代码是路由与转发之间那条**显式**的接缝：sr-route 只维护 RIB，从不碰 FIB；
// 本包把 (VRF, prefix, 下一跳集合) 翻译成 VPP 的 VRF 表 + 带标签路由 + 本地标签，
// 由 cmd/sr-fib 周期性地把差异写下去。
//
// 三个决定，都是数据面实测之后定下来的：
//
//   - **标签按 VRF 派生**：label = LabelBase + VRFID。标签只在收端本地有意义，
//     远端压的值必须等于收端登记的本地标签；两个 CPE 对同一个 VPN 用同一个号，
//     收发就自动对齐，不需要在路由协议里再多一个字段（Cisco 的 VPN-ID 当标签
//     也是这个形状：VPN 标识由控制面分配、在路由里携带、每跳只在本机有意义）。
//   - **下一跳 → 出口**来自控制面给的隧道表，不从 VPP 猜。RIB 里一条路由的每个
//     next_hop 对应一条本地隧道；映射关系（对端 overlay → 本端 wg 接口）
//     是 vBOND 分配隧道时就定下来的，猜它等于把控制面的职责挪进数据面。
//   - **每次重算整份计划再收敛**，不做增量：RIB 是声明式快照，增量下发会留下
//     已经撤掉的路径，而 VPP 不会告诉你它还在。
//
// 表布局：VPN 表号 = VPN ID（0, 1, ...），250 留给 overlay（wg 接口与对外下一跳
// 空间），underlay 在 table 0 —— 所以 VPN 0 与 underlay 共用 table 0。MPLS 标签
// 不分表：VPP 收包后的标签查表恒为全局 MPLS 表（实测与接口所在 IP 表无关）。
//
// 本包只装 overlay 学到的前缀。客户侧/本机前缀归盒内 FRR：FRR 把路由装进内核
// FIB，VPP 的 linux-nl 插件再按 netlink 同步进同一张 VPP 表。两个写者不碰同一
// 个前缀，也就不需要在 sr-fib 里再造一条"下一跳指向内核"的路径。
package fib

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"sdwan/controller/internal/vppapi"
)

// LabelBase 是 VPN 标签的起点。0..15 是 MPLS 保留标签（0/2 显式空标签、
// 3 隐式空标签……）。不从 16 起：VPN 0 是最常用的默认 VPN，16+0 恰好是
// IPv4 显式空标签，收端会把它当"弹栈后按 IPv4 转发"，而不是 VPN 0。
const LabelBase uint32 = 1000

// MaxVRFID 是 VRFID 的上限（原字段是 uint8），也保证 LabelBase+VRFID < 2^20。
// 表号 = VRFID；只有 250 被 overlay 占掉，其余 0..249、251..255 都能当 VPN。
const MaxVRFID uint8 = 255

// OverlayTable 是隧道与 overlay 下一跳所在的表，不是 VPN。VPN 表号 = VPN ID，
// 所以 VPN ID 取 250 会把客户路由装进 overlay 表，静默破坏隧道。
const OverlayTable uint8 = 250

// PrefBase 是 SD-WAN 路径的 VPP preference 起始值（0 最优，u8 上限 255）。
// preference = PrefBase + 颜色（0..7 ⇒ 230..237），作用仅限 API 源内部排序。
const PrefBase uint8 = 230

// MaxColor 是颜色上限（PrefBase + MaxColor ≤ 255 ⇒ 25）。设计上限是 7，
// 这里取 u8 数学上限做断言；更小的业务上限由调用方/配置层校验。
const MaxColor uint8 = 255 - PrefBase

// LabelFor 是"这个 VRF 的 VPN 标签"的唯一算法。收端登记本地标签、发端压标签
// 都用它 —— 两边各算一次还长得一样，是这套方案不需要信令的原因。
func LabelFor(vrf uint8) uint32 { return LabelBase + uint32(vrf) }

// RIBRoute 是喂给本包的最小路由形状（由 cmd/sr-fib 从 route.Route 转换）。
type RIBRoute struct {
	VRF      uint8
	Prefix   string
	NextHops []string
}

// Tunnel 是控制面分配的隧道：对端 overlay 地址 → 本端 wg 接口名。
//
// 对端 overlay 地址必须与 RIB 里 next_hops 的某个元素逐字相同（规范 IPv4 点分）。
// 这里不做"可达就猜"：同一台 CPE 到同一个人可以有多条隧道，猜错一条的后果是流量
// 静默走错端口，而端口是这套数据面区分隧道的唯一键。
type Tunnel struct {
	NextHop   string
	Interface string
	Color     uint8
}

// Path 是一条可下发的路径。Label 是压入的 VPN 标签。
type Path struct {
	NextHop    netip.Addr
	Interface  string
	Label      uint32
	Preference uint8
}

// Route 是一条要落进 VPP 的前缀。
type Route struct {
	Table  uint32
	Prefix netip.Prefix
	Paths  []Path
}

// Label 是一条要在收端登记的本地标签：弹栈后查 Table。
type Label struct {
	Label uint32
	Table uint32
}

// Plan 是一份完整的期望状态。
type Plan struct {
	Labels []Label
	Routes []Route
	// Warnings 记录"RIB 里有、但这台 CPE 装不了"的路径（下一跳不在隧道表里）。
	// 它们不阻断整份计划：一条隧道没配好，不该让同一个 VRF 的其它前缀也停摆。
	Warnings []string
}

// PlanFrom 把 RIB 快照翻译成期望状态。纯函数，不碰 VPP。
//
// vrfs 是这台 CPE 服务的 VPN VRF（与 sr-route 的 vrfs 配置同源）；每一条都要
// 登记本地标签 —— 即使某个 VRF 眼下没有任何路由，也可能有对端压着标签进来。
//
// down 是"明确不可达"的下一跳集合（来自 UnreachablePeers）；nil 表示没有
// 可达性信息，所有路径保留。显式不可达的路径与"下一跳不在隧道表里"同样处理：
// 跳过它、留一条 warning，而不是把整条前缀撤掉 —— 同前缀可能还有别的隧道。
func PlanFrom(vrfs []uint8, routes []RIBRoute, tunnels []Tunnel, down map[netip.Addr]bool) (Plan, error) {
	var p Plan
	if len(vrfs) == 0 {
		return p, fmt.Errorf("fib: 没有配置任何 VRF，无事可做")
	}
	seenVRF := map[uint8]bool{}
	for _, v := range vrfs {
		if v == OverlayTable {
			return p, fmt.Errorf("fib: VPN %d 与 overlay 表号冲突（250 留给隧道）", v)
		}
		if seenVRF[v] {
			return p, fmt.Errorf("fib: VRF %d 配了两次", v)
		}
		seenVRF[v] = true
		p.Labels = append(p.Labels, Label{Label: LabelFor(v), Table: uint32(v)})
	}
	sort.Slice(p.Labels, func(i, j int) bool { return p.Labels[i].Label < p.Labels[j].Label })

	tunnelOf := map[string]Tunnel{}
	for _, t := range tunnels {
		addr, err := netip.ParseAddr(t.NextHop)
		if err != nil || !addr.Is4() {
			return p, fmt.Errorf("fib: 隧道下一跳 %q 不是规范的 IPv4 地址", t.NextHop)
		}
		if t.Interface == "" {
			return p, fmt.Errorf("fib: 隧道 %s 没有接口名", t.NextHop)
		}
		if t.Color > MaxColor {
			return p, fmt.Errorf("fib: 隧道 %s 颜色 %d 超出上限（PrefBase %d + 颜色 ≤ 255 ⇒ 最大 %d）", t.NextHop, t.Color, PrefBase, MaxColor)
		}
		if _, dup := tunnelOf[addr.String()]; dup {
			return p, fmt.Errorf("fib: 隧道下一跳 %s 配了两次（同一对端只能有一条隧道映射到同一个 overlay 地址）", addr)
		}
		tunnelOf[addr.String()] = t
	}

	type key struct {
		table  uint32
		prefix netip.Prefix
	}
	grouped := map[key][]Path{}
	warned := map[string]bool{}
	for _, r := range routes {
		if !seenVRF[r.VRF] {
			return p, fmt.Errorf("fib: RIB 里有 VRF %d 的路由（%s），但本机没配这个 VRF", r.VRF, r.Prefix)
		}
		prefix, err := netip.ParsePrefix(r.Prefix)
		if err != nil || !prefix.Addr().Is4() {
			return p, fmt.Errorf("fib: 路由前缀 %q 不是规范 IPv4 前缀", r.Prefix)
		}
		prefix = prefix.Masked()
		if len(r.NextHops) == 0 {
			return p, fmt.Errorf("fib: %s 没有任何下一跳", prefix)
		}
		for _, raw := range r.NextHops {
			nh, err := netip.ParseAddr(raw)
			if err != nil || !nh.Is4() {
				return p, fmt.Errorf("fib: %s 的下一跳 %q 不是规范 IPv4 地址", prefix, raw)
			}
			t, ok := tunnelOf[nh.String()]
			if !ok {
				// 这台 CPE 上没有到该下一跳的隧道：这条路径不可用。留一条 warning，
				// 不去猜别的隧道 —— 猜错比不通更难查。
				w := fmt.Sprintf("%s: 下一跳 %s 不在隧道表里，该路径跳过", prefix, nh)
				if !warned[w] {
					warned[w] = true
					p.Warnings = append(p.Warnings, w)
				}
				continue
			}
			if down[nh] {
				// BFD 明确说这条隧道不通。跳过而不是整条撤掉：同前缀的多路径里
				// 可能还有活着的。
				w := fmt.Sprintf("%s: 下一跳 %s 的 BFD 会话不是 Up，该路径跳过", prefix, nh)
				if !warned[w] {
					warned[w] = true
					p.Warnings = append(p.Warnings, w)
				}
				continue
			}
			k := key{table: uint32(r.VRF), prefix: prefix}
			grouped[k] = append(grouped[k], Path{NextHop: nh, Interface: t.Interface, Label: LabelFor(r.VRF), Preference: PrefBase + t.Color})
		}
	}

	for k, paths := range grouped {
		// 去重 + 稳定排序：同一条路径被两个 RR 各反射一次是常态（双 RR 主备），
		// 路径集合相同就不该产生一次下发。
		sort.Slice(paths, func(i, j int) bool {
			if paths[i].Interface != paths[j].Interface {
				return paths[i].Interface < paths[j].Interface
			}
			return paths[i].NextHop.Less(paths[j].NextHop)
		})
		uniq := paths[:0]
		for i, pa := range paths {
			if i > 0 && pa == paths[i-1] {
				continue
			}
			uniq = append(uniq, pa)
		}
		p.Routes = append(p.Routes, Route{Table: k.table, Prefix: k.prefix, Paths: uniq})
	}
	sort.Slice(p.Routes, func(i, j int) bool {
		if p.Routes[i].Table != p.Routes[j].Table {
			return p.Routes[i].Table < p.Routes[j].Table
		}
		return p.Routes[i].Prefix.String() < p.Routes[j].Prefix.String()
	})
	sort.Strings(p.Warnings)
	return p, nil
}

// DP 是下发面。*vppapi.Client 满足它；测试用内存假端。
type DP interface {
	IPTableAddDel(ctx context.Context, tableID uint32, add bool) error
	IPRouteAddDel(ctx context.Context, tableID uint32, prefix netip.Prefix, paths []vppapi.FIBPath, add bool) error
	MPLSLabelAddDel(ctx context.Context, label uint32, lookupTable uint32, add bool) error
	MPLSLabelDel(ctx context.Context, label uint32) error
	DumpInterfaces(ctx context.Context) ([]vppapi.SwInterface, error)
	FIBRoutes(ctx context.Context, tableID uint32) ([]vppapi.IPRoute, error)
	CLI(ctx context.Context, cmd string) (string, error)
}

var _ DP = (*vppapi.Client)(nil)

// MPLSEgressCmd 是让插件立刻把 wg 接口上的 MPLS 邻接改到 sr-wg4-mpls-out 的
// CLI。插件自己也有 100 ms 看门狗，这里显式调一次是为了不留那 100 ms 的明文窗口
// （见 vpp-plugin/sr_wg4_mpls_out.c 的头注释）。
const MPLSEgressCmd = "sr-wg mpls-egress"

// Result 描述一次收敛动了什么（幂等时全为 0/false）。
type Result struct {
	TablesAdded   int
	LabelsSet     int
	LabelsRemoved int
	RoutesSet     int
	RoutesRemoved int
	MPLSChanged   bool
}

// Changed 报告这次是否真的写了数据面。
func (r Result) Changed() bool {
	return r.TablesAdded+r.LabelsSet+r.LabelsRemoved+r.RoutesSet+r.RoutesRemoved > 0
}

// Applier 持有"上一次下发成功的那份计划"，用来算差异。
//
// 只允许单线程调用（sr-fib 的轮询循环就是单线程）。不要用两份 Applier 打同一
// 张表：它们各自以为自己是真相，A 刚撤掉的路由会被 B 的旧计划又加回来。
type Applier struct {
	dp    DP
	prev  Plan
	ifMap map[string]uint32 // 每次 Apply 现取：接口重建后 sw_if_index 会变
}

func NewApplier(dp DP) *Applier { return &Applier{dp: dp} }

// Apply 把 next 收敛到数据面。失败时如果已经写了部分内容，prev 不前进 ——
// 下一轮会重新按同一份目标再试，而不是把半成品当成基线。
func (a *Applier) Apply(ctx context.Context, next Plan) (Result, error) {
	var res Result
	if err := a.refreshInterfaces(ctx); err != nil {
		return res, err
	}

	// 0. 先把"接口不存在"挡在写动作之前。
	for _, r := range next.Routes {
		for _, p := range r.Paths {
			if _, ok := a.ifMap[p.Interface]; !ok {
				return res, fmt.Errorf("fib: 隧道接口 %s 不存在（路由 %s 的下一跳 %s）",
					p.Interface, r.Prefix, p.NextHop)
			}
		}
	}

	// 1. 表：新增的表先建。table 0 是 global 表（underlay 也在里面），本来就
	//    存在，建不得也删不得。不删已经不在计划里的表 —— 表里可能还有本包没管
	//    的路由（例如操作员手敲的）；删表会把它们一起带走。
	prevTables := map[uint32]bool{}
	for _, l := range a.prev.Labels {
		prevTables[l.Table] = true
	}
	for _, l := range next.Labels {
		if l.Table == 0 || prevTables[l.Table] {
			continue
		}
		if err := a.dp.IPTableAddDel(ctx, l.Table, true); err != nil {
			return res, fmt.Errorf("fib: 建 VRF 表 %d: %w", l.Table, err)
		}
		res.TablesAdded++
	}

	// 2. 本地标签：新增/变更先加；删除放最后（先让新路径可用，再撤旧的）。
	prevLabels := map[uint32]Label{}
	for _, l := range a.prev.Labels {
		prevLabels[l.Label] = l
	}
	nextLabels := map[uint32]Label{}
	for _, l := range next.Labels {
		nextLabels[l.Label] = l
		if old, ok := prevLabels[l.Label]; ok && old == l {
			continue
		}
		if err := a.dp.MPLSLabelAddDel(ctx, l.Label, l.Table, true); err != nil {
			return res, fmt.Errorf("fib: 登记本地标签 %d → 表 %d: %w", l.Label, l.Table, err)
		}
		res.LabelsSet++
	}

	// 3. 路由：按 (表, 前缀) 整条替换。
	//    身份集合没变、只有 preference 变了时先删后加。
	//    这条规则**不是必需的**：docs/overlay-design-draft.md §2.2（2026-10-30 重测）表明
	//    我们发的 is_multipath=0 整条替换自己会重算选路，删加只是让改色瞬间多一个路由整条
	//    消失再回来的窗口。用户 2026-10-30 拍了：改颜色丢包属于正常、应该接受，规则保留。
	//    所以这里保证的是"改色一定重算"，代价是窗口；窗口时长由 lab 判据记录，不做优化。
	//    覆盖它的单测：TestApplyDeleteThenAddOnPrefChange（改色 ⇒ RoutesRemoved=1 + RoutesSet=1）
	//    与 TestApplyAddRemovePathNoExtraDelete（路径集合本身变化 ⇒ 不触发删加）。
	prevRoutes := map[string]Route{}
	for _, r := range a.prev.Routes {
		prevRoutes[routeKey(r)] = r
	}
	nextRoutes := map[string]Route{}
	for _, r := range next.Routes {
		nextRoutes[routeKey(r)] = r
		old, exists := prevRoutes[routeKey(r)]
		if exists && equalPaths(old.Paths, r.Paths) {
			continue
		}
		if exists && pathIdentityEqual(old.Paths, r.Paths) {
			oldPaths := fibPaths(old, a.ifMap)
			if err := a.dp.IPRouteAddDel(ctx, r.Table, r.Prefix, oldPaths, false); err != nil {
				return res, fmt.Errorf("fib: 删(改色) %s 到表 %d: %w", r.Prefix, r.Table, err)
			}
			res.RoutesRemoved++
		}
		paths := fibPaths(r, a.ifMap)
		if err := a.dp.IPRouteAddDel(ctx, r.Table, r.Prefix, paths, true); err != nil {
			return res, fmt.Errorf("fib: 下发 %s 到表 %d: %w", r.Prefix, r.Table, err)
		}
		res.RoutesSet++
	}

	// 4. 撤掉已经不在计划里的路由（以及标签）。
	for k, r := range prevRoutes {
		if _, ok := nextRoutes[k]; ok {
			continue
		}
		paths := fibPaths(r, a.ifMap)
		if err := a.dp.IPRouteAddDel(ctx, r.Table, r.Prefix, paths, false); err != nil {
			return res, fmt.Errorf("fib: 撤 %s（表 %d）: %w", r.Prefix, r.Table, err)
		}
		res.RoutesRemoved++
	}
	for label := range prevLabels {
		if _, ok := nextLabels[label]; ok {
			continue
		}
		if err := a.dp.MPLSLabelDel(ctx, label); err != nil {
			return res, fmt.Errorf("fib: 撤本地标签 %d: %w", label, err)
		}
		res.LabelsRemoved++
	}

	res.MPLSChanged = res.LabelsSet+res.LabelsRemoved+res.RoutesSet+res.RoutesRemoved > 0
	if res.MPLSChanged {
		// 装了带标签的路由之后立刻改向，关掉插件 100 ms 看门狗的明文窗口。
		// 第一次收敛同样要调：新邻接就是在这一刻才出现的。
		// 失败不回滚 FIB（路由是对的，只是加密要晚 100 ms），但必须报出来。
		if _, err := a.dp.CLI(ctx, MPLSEgressCmd); err != nil {
			return res, fmt.Errorf("fib: 触发 %s: %w", MPLSEgressCmd, err)
		}
	}

	a.prev = next
	return res, nil
}

// refreshInterfaces 每次收敛都重新取接口表：WG 接口被重建后 sw_if_index 会变，
// 缓存下来的号会指向别人。
func (a *Applier) refreshInterfaces(ctx context.Context) error {
	ifs, err := a.dp.DumpInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("fib: 读接口表: %w", err)
	}
	a.ifMap = make(map[string]uint32, len(ifs))
	for _, i := range ifs {
		a.ifMap[i.Name] = i.SWIfIndex
	}
	return nil
}

func fibPaths(r Route, ifMap map[string]uint32) []vppapi.FIBPath {
	paths := make([]vppapi.FIBPath, 0, len(r.Paths))
	for _, p := range r.Paths {
		fp := vppapi.FIBPath{SWIfIndex: ifMap[p.Interface], NextHop: p.NextHop, Preference: p.Preference}
		if p.Label != 0 {
			fp.Labels = []uint32{p.Label}
		}
		paths = append(paths, fp)
	}
	return paths
}

func routeKey(r Route) string {
	return fmt.Sprintf("%d|%s", r.Table, r.Prefix)
}

func equalPaths(a, b []Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pathIdentityEqual(a, b []Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Interface != b[i].Interface || a[i].NextHop != b[i].NextHop || a[i].Label != b[i].Label {
			return false
		}
	}
	return true
}

// WarningsString 把 warning 拼成一行，给日志用。
func WarningsString(ws []string) string { return strings.Join(ws, "; ") }
