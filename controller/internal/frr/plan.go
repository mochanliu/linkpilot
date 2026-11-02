package frr

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
)

// OverlayVRF 是 SD-WAN 传输域（隧道与 overlay /32）的表号，不是 VPN。
// 客户 VPN 的取值是 0..255 排除它：把泄露路由注入这张表只会污染传输域。
const OverlayVRF = 250

// KernelTable 返回 VPN 在盒子内核里的路由表号。VPN 0 是默认 VRF，对应内核
// 的 main（254）；其余 VPN 用 vpn<N> 设备，boot 配置里建的就是 table N。
func KernelTable(vpn uint8) uint32 {
	if vpn == 0 {
		return 254 // RT_TABLE_MAIN
	}
	return uint32(vpn)
}

// Route 是一条要注入内核表的前缀。Table 是**内核表号**（VPN 0 已换算成
// 254），OIF 是出口 ifindex（lo / vpn<N>）。两者一起决定删除时匹配哪一条。
type Route struct {
	Prefix netip.Prefix `json:"prefix"`
	Table  uint32       `json:"table"`
	OIF    int          `json:"oif"`
}

// Key 用于状态文件与测试里的可读比较。表与前缀是身份；OIF 变化只意味着
// 出口设备换了 ifindex，用一次 replace 就能挪过去（见 krt 的 RouteAdd）。
func (r Route) Key() string { return r.key() }

func (r Route) key() string {
	return fmt.Sprintf("%d|%s", r.Table, r.Prefix)
}

// RIBRoute 是 sr-route /status 里喂给本包的最小形状。
// VRF 是**目标主表**（/status 的 rib 键）：导通时要往客户内网通告的就是落进本机
// 这张表的那条前缀，来源 VPN 只决定标签，与本包无关。
type RIBRoute struct {
	VRF      uint8
	Prefix   string
	NextHops []string
}

// Plan 是一轮要保证存在于内核里的完整前缀集合。
type Plan struct {
	Routes []Route
}

// PlanFrom 是纯函数：同样的输入永远得到同样的计划。规则：
//
//   - 只导出 export_vrfs 里的 VPN；其它 VPN 在 RIB 里是正常的，跳过不报错。
//   - 出口按 VPN 解析一次（VPN 0 → lo，其余 vpn<N>），解析失败就是配置错误。
//   - RIB 里没有可解析下一跳的前缀不导出。sr-fib 也不会把它装进 FIB；
//     若仍然发给客户侧，只会把流量吸到这台 CPE 上再黑洞掉。
//   - 前缀必须是已规范的 IPv4（这条接缝只对 IPv4 负责）。
func PlanFrom(exportVRFs []uint32, routes []RIBRoute, oif func(uint32) (int, error)) (Plan, error) {
	idx := make(map[uint32]int, len(exportVRFs))
	for _, v := range exportVRFs {
		if v > 255 {
			return Plan{}, fmt.Errorf("frr: export_vrfs 里的 %d 超出 0..255", v)
		}
		if v == OverlayVRF {
			return Plan{}, fmt.Errorf("frr: export_vrfs 不能包含 %d（overlay 传输域，不是 VPN）", OverlayVRF)
		}
		if _, dup := idx[v]; dup {
			return Plan{}, fmt.Errorf("frr: export_vrfs 里 VPN %d 重复", v)
		}
		i, err := oif(v)
		if err != nil {
			return Plan{}, fmt.Errorf("frr: VPN %d 的出口: %w", v, err)
		}
		if i <= 0 {
			return Plan{}, fmt.Errorf("frr: VPN %d 的出口 ifindex %d 非法", v, i)
		}
		idx[v] = i
	}

	seen := map[string]bool{}
	out := []Route{}
	for _, r := range routes {
		i, ok := idx[uint32(r.VRF)]
		if !ok {
			continue
		}
		if len(r.NextHops) == 0 {
			continue
		}
		p, err := netip.ParsePrefix(r.Prefix)
		if err != nil {
			return Plan{}, fmt.Errorf("frr: 解析前缀 %q: %w", r.Prefix, err)
		}
		if !p.Addr().Is4() || p.Addr().Is4In6() {
			return Plan{}, fmt.Errorf("frr: 只导出 IPv4 前缀，收到 %s", r.Prefix)
		}
		rt := Route{Table: KernelTable(r.VRF), Prefix: p.Masked(), OIF: i}
		if seen[rt.key()] {
			continue
		}
		seen[rt.key()] = true
		out = append(out, rt)
	}
	sortRoutes(out)
	return Plan{Routes: out}, nil
}

func sortRoutes(routes []Route) {
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Table != routes[j].Table {
			return routes[i].Table < routes[j].Table
		}
		if c := routes[i].Prefix.Addr().Compare(routes[j].Prefix.Addr()); c != 0 {
			return c < 0
		}
		return routes[i].Prefix.Bits() < routes[j].Prefix.Bits()
	})
}

// DP 是写路的一端；*krt.Client 是生产实现（直写内核表），测试用内存版。
type DP interface {
	RouteAdd(ctx context.Context, r Route) error
	RouteDelete(ctx context.Context, r Route) error
}

// Result 是一轮收敛实际发出的写操作数，用于日志。
type Result struct {
	Added   int
	Removed int
}

func (r Result) Changed() bool { return r.Added > 0 || r.Removed > 0 }

// Exporter 把 Plan 收敛到内核，并记住"我已经写下去了什么"。
//
// prev 是刻意保留的：内核在进程退出（或断开 netlink）时**不会**回收这些
// 路由，所以这一份既是差异比较的基线，也是进程正常退出时要逐条撤掉的
// 东西。zebra 重启不影响它 —— 前缀停在内核里，zebra 只是重新从内核学到
// 一次 K。
type Exporter struct {
	dp   DP
	prev map[string]Route
}

func NewExporter(dp DP) *Exporter {
	return NewExporterFrom(dp, nil)
}

// NewExporterFrom 用上一轮落盘的集合当基线。进程被强杀后这些前缀还在内核
// 里，先把它们当成"已下发"，第一次 Apply 就只补差量：没变的前缀一次都不
// 碰，不会因为重启让客户侧 BGP 抖一下。
func NewExporterFrom(dp DP, prev []Route) *Exporter {
	e := &Exporter{dp: dp, prev: make(map[string]Route, len(prev))}
	for _, r := range prev {
		e.prev[r.key()] = r
	}
	return e
}

func (e *Exporter) Apply(ctx context.Context, plan Plan) (Result, error) {
	var res Result
	desired := make(map[string]Route, len(plan.Routes))
	for _, r := range plan.Routes {
		desired[r.key()] = r
	}

	// 先加后删：前缀挪表或出口变化时宁可短暂重复，也不要出现空窗。
	next := make(map[string]Route, len(desired))
	for k, r := range desired {
		if old, ok := e.prev[k]; ok && old == r {
			next[k] = old
			continue
		}
		if err := e.dp.RouteAdd(ctx, r); err != nil {
			return res, fmt.Errorf("frr: 加 %s 到表 %d: %w", r.Prefix, r.Table, err)
		}
		next[k] = r
		res.Added++
	}
	for k, r := range e.prev {
		if _, ok := desired[k]; ok {
			continue
		}
		if err := e.dp.RouteDelete(ctx, r); err != nil {
			return res, fmt.Errorf("frr: 从表 %d 删 %s: %w", r.Table, r.Prefix, err)
		}
		res.Removed++
	}
	e.prev = next
	return res, nil
}

// Routes 返回当前认为已下发的集合（按表号、前缀排序）。状态文件与退出
// 清理都用它，保证"文件里写的"与"我们打算撤的"是同一份东西。
func (e *Exporter) Routes() []Route {
	out := make([]Route, 0, len(e.prev))
	for _, r := range e.prev {
		out = append(out, r)
	}
	sortRoutes(out)
	return out
}

// Flush 撤掉自己写过的所有前缀。退出时调用：内核不会替我们做这件事。
// 单条失败不阻断其余条目，最后把错误一起返回 —— 能撤多少撤多少。
func (e *Exporter) Flush(ctx context.Context) error {
	var errs []error
	for k, r := range e.prev {
		if err := e.dp.RouteDelete(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("frr: 撤表 %d 的 %s: %w", r.Table, r.Prefix, err))
			continue
		}
		delete(e.prev, k)
	}
	return errors.Join(errs...)
}
