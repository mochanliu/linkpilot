package frr

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
)

// Table 是"控制面的一个 VRF 停到哪张内核表"。ID 必须 > 255：sr-fib 用
// 0..255 当 VPN VRF 表号（250 留给 overlay），而 linux-cp 会把内核表 N
// 同步到 VPP table N —— 两张表撞号时两边都以为路由是自己的。
type Table struct {
	VRF uint8
	ID  uint32
}

// MaxTableID 取 netlink/FRR 都当有符号 32 位处理的边界。
const MaxTableID = 1<<31 - 1

// ValidateTables 拒绝那些"配下去也不会报错、只是静默走错表"的组合。
func ValidateTables(tables []Table) error {
	if len(tables) == 0 {
		return errors.New("frr: 至少要配一个 VRF→表 的映射")
	}
	seenVRF := map[uint8]bool{}
	seenID := map[uint32]bool{}
	for _, t := range tables {
		if t.ID <= 255 {
			return fmt.Errorf("frr: 表号 %d 落在 sr-fib 的 VPN 表号（0..255）里，必须 > 255", t.ID)
		}
		if t.ID > MaxTableID {
			return fmt.Errorf("frr: 表号 %d 超出 %d", t.ID, MaxTableID)
		}
		if seenVRF[t.VRF] {
			return fmt.Errorf("frr: VRF %d 配了两次", t.VRF)
		}
		if seenID[t.ID] {
			return fmt.Errorf("frr: 表号 %d 配了两次", t.ID)
		}
		seenVRF[t.VRF] = true
		seenID[t.ID] = true
	}
	return nil
}

// RIBRoute 是 sr-route /status 里喂给本包的最小形状。
type RIBRoute struct {
	VRF      uint8
	Prefix   string
	NextHops []string
}

// Plan 是一轮要保证存在于 FRR 里的完整前缀集合。
type Plan struct {
	Routes []Route
}

// Key 用于状态文件与测试里的可读比较。
func (r Route) Key() string { return r.key() }

// PlanFrom 是纯函数：同样的输入永远得到同样的计划。规则只有三条：
//
//   - RIB 里没有可解析下一跳的前缀不导出。sr-fib 也不会把它装进 FIB；
//     若仍然发给客户侧，只会把流量吸到这台 CPE 上再黑洞掉。
//   - 前缀必须是已规范的 IPv4（这条接缝只对 IPv4 负责）。
//   - 同一个 VRF 的 RIB 必须有对应的表；没配就是配置错误，不猜。
func PlanFrom(tables []Table, routes []RIBRoute, distance uint8, metric uint32) (Plan, error) {
	if err := ValidateTables(tables); err != nil {
		return Plan{}, err
	}
	if distance == 0 {
		return Plan{}, errors.New("frr: distance 必须在 1..255")
	}
	tableOf := make(map[uint8]uint32, len(tables))
	for _, t := range tables {
		tableOf[t.VRF] = t.ID
	}

	seen := map[string]bool{}
	var out []Route
	for _, r := range routes {
		tableID, ok := tableOf[r.VRF]
		if !ok {
			return Plan{}, fmt.Errorf("frr: RIB 里有 VRF %d 的路由（%s），但本机没配这个 VRF", r.VRF, r.Prefix)
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
		p = p.Masked()
		rt := Route{Prefix: p, TableID: tableID, Distance: distance, Metric: metric}
		if seen[rt.key()] {
			continue
		}
		seen[rt.key()] = true
		out = append(out, rt)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TableID != out[j].TableID {
			return out[i].TableID < out[j].TableID
		}
		if c := out[i].Prefix.Addr().Compare(out[j].Prefix.Addr()); c != 0 {
			return c < 0
		}
		return out[i].Prefix.Bits() < out[j].Prefix.Bits()
	})
	return Plan{Routes: out}, nil
}

// DP 是写路的一端；*Client 是唯一的生产实现，测试用内存版。
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

// Exporter 把 Plan 收敛到 zebra，并记住"我已经写下去了什么"。
//
// prev 是刻意保留的：zebra 在客户端断开时**不会**回收路由（实测），
// 所以这一份既是差异比较的基线，也是进程正常退出时要逐条撤掉的东西。
// 远端 zebra 重启后 prev 就不代表现实了，调用方必须换一个新 Exporter。
type Exporter struct {
	dp   DP
	prev map[string]Route
}

func NewExporter(dp DP) *Exporter {
	return &Exporter{dp: dp, prev: map[string]Route{}}
}

func (e *Exporter) Apply(ctx context.Context, plan Plan) (Result, error) {
	var res Result
	desired := make(map[string]Route, len(plan.Routes))
	for _, r := range plan.Routes {
		desired[r.key()] = r
	}

	// 先加后删：前缀挪表或属性变化时宁可短暂重复，也不要出现空窗。
	next := make(map[string]Route, len(desired))
	for k, r := range desired {
		if old, ok := e.prev[k]; ok && old == r {
			next[k] = old
			continue
		}
		if err := e.dp.RouteAdd(ctx, r); err != nil {
			return res, fmt.Errorf("frr: 加 %s 到表 %d: %w", r.Prefix, r.TableID, err)
		}
		next[k] = r
		res.Added++
	}
	for k, r := range e.prev {
		if _, ok := desired[k]; ok {
			continue
		}
		if err := e.dp.RouteDelete(ctx, r); err != nil {
			return res, fmt.Errorf("frr: 从表 %d 删 %s: %w", r.TableID, r.Prefix, err)
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].TableID != out[j].TableID {
			return out[i].TableID < out[j].TableID
		}
		if c := out[i].Prefix.Addr().Compare(out[j].Prefix.Addr()); c != 0 {
			return c < 0
		}
		return out[i].Prefix.Bits() < out[j].Prefix.Bits()
	})
	return out
}

// Flush 撤掉自己写过的所有前缀。退出时调用：zebra 不会替我们做这件事。
// 单条失败不阻断其余条目，最后把错误一起返回 —— 能撤多少撤多少。
func (e *Exporter) Flush(ctx context.Context) error {
	var errs []error
	for k, r := range e.prev {
		if err := e.dp.RouteDelete(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("frr: 撤表 %d 的 %s: %w", r.TableID, r.Prefix, err))
			continue
		}
		delete(e.prev, k)
	}
	return errors.Join(errs...)
}
