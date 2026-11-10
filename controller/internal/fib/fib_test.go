package fib

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// fakeDP 是内存版数据面，记录每一次写动作。
type fakeDP struct {
	ifs    []vppapi.SwInterface
	tables map[uint32]bool
	labels map[uint32]uint32
	routes map[string][]vppapi.FIBPath
	cli    []string
	writes int
	failAt string // 一次性注入失败：table / label / route / cli
}

func newFakeDP() *fakeDP {
	return &fakeDP{
		ifs: []vppapi.SwInterface{
			{Name: "wg0", SWIfIndex: 10},
			{Name: "wg1", SWIfIndex: 11},
		},
		tables: map[uint32]bool{},
		labels: map[uint32]uint32{},
		routes: map[string][]vppapi.FIBPath{},
	}
}

func (f *fakeDP) fail(op string) error {
	if f.failAt != op {
		return nil
	}
	f.failAt = ""
	return errors.New("injected: " + op)
}

func (f *fakeDP) IPTableAddDel(_ context.Context, id uint32, add bool) error {
	f.writes++
	if err := f.fail("table"); err != nil {
		return err
	}
	if add {
		f.tables[id] = true
	} else {
		delete(f.tables, id)
	}
	return nil
}

func (f *fakeDP) IPRouteAddDel(_ context.Context, table uint32, prefix netip.Prefix, paths []vppapi.FIBPath, add bool) error {
	f.writes++
	if err := f.fail("route"); err != nil {
		return err
	}
	k := fmt.Sprintf("%d|%s", table, prefix)
	if add {
		f.routes[k] = paths
	} else {
		delete(f.routes, k)
	}
	return nil
}

func (f *fakeDP) MPLSLabelAddDel(_ context.Context, label uint32, table uint32, add bool) error {
	f.writes++
	if err := f.fail("label"); err != nil {
		return err
	}
	if add {
		f.labels[label] = table
	} else {
		delete(f.labels, label)
	}
	return nil
}

func (f *fakeDP) MPLSLabelDel(_ context.Context, label uint32) error {
	return f.MPLSLabelAddDel(context.Background(), label, 0, false)
}

func (f *fakeDP) DumpInterfaces(context.Context) ([]vppapi.SwInterface, error) {
	return f.ifs, nil
}

func (f *fakeDP) FIBRoutes(_ context.Context, table uint32) ([]vppapi.IPRoute, error) {
	var out []vppapi.IPRoute
	for k, paths := range f.routes {
		var t uint32
		var p string
		if _, err := fmt.Sscanf(k, "%d|%s", &t, &p); err != nil || t != table {
			continue
		}
		out = append(out, vppapi.IPRoute{
			TableID: table,
			Prefix:  netip.MustParsePrefix(p),
			Paths:   paths,
		})
	}
	return out, nil
}

func (f *fakeDP) CLI(_ context.Context, cmd string) (string, error) {
	if err := f.fail("cli"); err != nil {
		return "", err
	}
	f.cli = append(f.cli, cmd)
	return "", nil
}

// planFrom 是"没有可达性信息、逃生开关关着"的 PlanFrom：这些用例只关心 RIB 的翻译。
func planFrom(vrfs []uint8, routes []RIBRoute, tunnels []Tunnel) (Plan, error) {
	return PlanFrom(vrfs, routes, tunnels, nil, false, nil)
}

// planEscape 只把逃生开关翻成"开"（§5）。开关影响的是**主表收哪些颜色**，
// 颜色表两种情况下都照写，所以它必须和关着的那份逐条对比才有意义。
func planEscape(vrfs []uint8, routes []RIBRoute, tunnels []Tunnel) (Plan, error) {
	return PlanFrom(vrfs, routes, tunnels, nil, true, nil)
}

// ribOne 是单 VPN 的常见形状：来源 VPN 就是目标表。Origin 固定成一个正常设备号，
// 这些用例不比 RD。跨 VPN 导通用 ribCross。
func ribOne(table uint8, prefix string, hops ...string) RIBRoute {
	return RIBRoute{Table: table, SourceVRF: table, Origin: 2, Prefix: prefix, NextHops: hops}
}

// ribCross 是导通的形状：路由来自 origin 的来源 VPN source，要装进本机表 target。
func ribCross(target, source uint8, origin uint16, prefix string, hops ...string) RIBRoute {
	return RIBRoute{Table: target, SourceVRF: source, Origin: origin, Prefix: prefix, NextHops: hops}
}

// routeAt 取计划里 (表, 前缀) 那条路由。不按下标取：O3 之后一个前缀同时出现在主表
// 和各颜色表里，下标会随颜色数量变，测试要钉的是"这张表里这条前缀装了哪些路径"。
func routeAt(t *testing.T, p Plan, table uint32, prefix string) (Route, bool) {
	t.Helper()
	for _, r := range p.Routes {
		if r.Table == table && r.Prefix.String() == prefix {
			return r, true
		}
	}
	return Route{}, false
}

// mustRoute 断言路由存在并返回它。
func mustRoute(t *testing.T, p Plan, table uint32, prefix string) Route {
	t.Helper()
	r, ok := routeAt(t, p, table, prefix)
	if !ok {
		t.Fatalf("计划里没有表 %d 的 %s（全部路由: %+v）", table, prefix, p.Routes)
	}
	return r
}

// mustAbsent 断言计划里**任何表**都没有这条前缀 —— 择一与"逃生关主表只收颜色 0"
// 的负向都靠它，光看主表会漏掉"被塞进别的颜色表"这种错法。
func mustAbsent(t *testing.T, p Plan, prefix string) {
	t.Helper()
	for _, r := range p.Routes {
		if r.Prefix.String() == prefix {
			t.Fatalf("计划里有表 %d 的 %s，want 完全没有这条前缀", r.Table, prefix)
		}
	}
}

func plan1() Plan {
	p, err := planFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}},
	)
	if err != nil {
		panic(err)
	}
	return p
}

func TestLabelFor(t *testing.T) {
	for vrf, want := range map[uint8]uint32{0: 1000, 1: 1001, 100: 1100, 255: 1255} {
		if got := LabelFor(vrf); got != want {
			t.Errorf("LabelFor(%d) = %d, want %d", vrf, got, want)
		}
	}
}

func TestPlanFrom(t *testing.T) {
	tun := Tunnel{NextHop: "10.255.0.2", Interface: "wg0"}
	plan, err := planFrom(
		[]uint8{101, 100},
		[]RIBRoute{
			// 同一条路径被两个 RR 各反射一次：只该出现一次。
			ribOne(100, "10.1.0.0/24", "10.255.0.2"),
			ribOne(100, "10.1.0.0/24", "10.255.0.2"),
			ribOne(100, "10.1.0.9/24", "10.255.0.2"), // 未规范化前缀
		},
		[]Tunnel{tun},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Labels) != 2 || plan.Labels[0] != (Label{1100, 100}) || plan.Labels[1] != (Label{1101, 101}) {
		t.Fatalf("labels = %+v, want [{1100 100} {1101 101}]", plan.Labels)
	}
	// 本机两张 VPN 主表就是全部的表（§4.2 更正）：颜色 0 的落点 = 主表本身，
	// 不再另建一张副本表。这条断言钉的就是"颜色 0 不产生第二张表"。
	wantTables := []uint32{100, 101}
	if len(plan.Tables) != len(wantTables) {
		t.Fatalf("tables = %v, want %v（颜色 0 不许再多出一张表）", plan.Tables, wantTables)
	}
	for i, w := range wantTables {
		if plan.Tables[i] != w {
			t.Fatalf("tables = %v, want %v", plan.Tables, wantTables)
		}
	}
	if got := colorTable(t, 0, 100); got != 100 {
		t.Fatalf("ColorTable(0,100) = %d, want 100 —— 颜色 0 必须折回主表", got)
	}
	want := Path{NextHop: netip.MustParseAddr("10.255.0.2"), Interface: "wg0", Label: 1100, Preference: PrefBase}
	r := mustRoute(t, plan, 100, "10.1.0.0/24")
	if len(r.Paths) != 1 || r.Paths[0] != want {
		t.Errorf("主表 100 paths = %+v, want [%+v]（颜色和主表是同一个桶，两份候选必须并成一条）", r.Paths, want)
	}
	if len(plan.Routes) != 1 {
		t.Fatalf("routes = %+v, want 1 条（颜色 0 就是主表，不重复装一遍）", plan.Routes)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v, want 空", plan.Warnings)
	}
}

// colorTable 是测试里写颜色表号的办法：把公式集中在一处，公式本身另有单测。
func colorTable(t *testing.T, color uint8, main uint32) uint32 {
	t.Helper()
	v, err := ColorTable(color, main)
	if err != nil {
		t.Fatalf("ColorTable(%d, %d): %v", color, main, err)
	}
	return v
}

func TestPlanFromMultipath(t *testing.T) {
	plan, err := planFrom(
		[]uint8{100},
		[]RIBRoute{
			// 一条路由自带两条路径；另一台设备宣告同一前缀、其中一条路径重合。
			ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3"),
			ribOne(100, "10.1.0.0/24", "10.255.0.3"),
		},
		[]Tunnel{
			{NextHop: "10.255.0.3", Interface: "wg1"},
			{NextHop: "10.255.0.2", Interface: "wg0"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	paths := mustRoute(t, plan, 100, "10.1.0.0/24").Paths
	if len(paths) != 2 {
		t.Fatalf("paths = %+v, want 2 条去重后路径", paths)
	}
	if paths[0].Interface != "wg0" || paths[1].Interface != "wg1" {
		t.Errorf("paths 未按接口排序: %+v", paths)
	}
}

func TestPlanFromUnknownNextHopWarnsOnly(t *testing.T) {
	plan, err := planFrom(
		[]uint8{100},
		[]RIBRoute{
			ribOne(100, "10.9.0.0/24", "10.255.0.9"),
			ribOne(100, "10.1.0.0/24", "10.255.0.2"),
		},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}},
	)
	if err != nil {
		t.Fatalf("单条路径没隧道不该让整份计划失败: %v", err)
	}
	mustRoute(t, plan, 100, "10.1.0.0/24")
	mustAbsent(t, plan, "10.9.0.0/24")
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "10.255.0.9") {
		t.Errorf("warnings = %v, want 提到 10.255.0.9", plan.Warnings)
	}
}

func TestPlanFromRejects(t *testing.T) {
	tun := []Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}}
	cases := []struct {
		name   string
		vrfs   []uint8
		routes []RIBRoute
		tuns   []Tunnel
		want   string
	}{
		{"no vrf", nil, nil, tun, "没有配置任何 VRF"},
		{"vrf 250 = overlay", []uint8{OverlayTable}, nil, tun, "overlay 表号冲突"},
		{"duplicate vrf", []uint8{100, 100}, nil, tun, "配了两次"},
		{"target table not local", []uint8{100}, []RIBRoute{ribOne(101, "10.1.0.0/24", "10.255.0.2")}, tun, "没配这个 VPN"},
		{"bad prefix", []uint8{100}, []RIBRoute{ribOne(100, "10.1.0.0/33", "10.255.0.2")}, tun, "不是规范 IPv4 前缀"},
		{"v6 prefix", []uint8{100}, []RIBRoute{ribOne(100, "2001:db8::/64", "10.255.0.2")}, tun, "不是规范 IPv4 前缀"},
		{"bad nexthop", []uint8{100}, []RIBRoute{ribOne(100, "10.1.0.0/24", "wg0")}, tun, "不是规范 IPv4 地址"},
		{"no nexthops", []uint8{100}, []RIBRoute{{Table: 100, SourceVRF: 100, Prefix: "10.1.0.0/24"}}, tun, "没有任何下一跳"},
		{"tunnel bad addr", []uint8{100}, nil, []Tunnel{{NextHop: "10.255.0.2/32", Interface: "wg0"}}, "不是规范的 IPv4 地址"},
		{"tunnel no iface", []uint8{100}, nil, []Tunnel{{NextHop: "10.255.0.2"}}, "没有接口名"},
		{"tunnel duplicate", []uint8{100}, nil, append(tun, tun...), "配了两次"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := planFrom(c.vrfs, c.routes, c.tuns); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want 包含 %q", err, c.want)
			}
		})
	}
}

func TestApplyFirstThenIdempotent(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	ctx := context.Background()

	res, err := a.Apply(ctx, plan1())
	if err != nil {
		t.Fatal(err)
	}
	// 表 1 张、路由 1 条：这条隧道的颜色是 0，而 §4.2 更正后颜色 0 的落点就是
	// 主表本身 —— 不再像过去那样在主表之外另建一张 10000+100 装同一份路径。
	if res.TablesAdded != 1 || res.LabelsSet != 1 || res.RoutesSet != 1 || !res.MPLSChanged {
		t.Errorf("first apply = %+v, want {1 1 0 1 0 true}", res)
	}
	if !dp.tables[100] {
		t.Errorf("表 100 没建")
	}
	if len(dp.tables) != 1 {
		t.Errorf("建出来的表 = %v, want 只有 100（颜色 0 不该再多一张）", dp.tables)
	}
	if dp.labels[1100] != 100 {
		t.Errorf("label 1100 → %d, want 表 100", dp.labels[1100])
	}
	paths := dp.routes["100|10.1.0.0/24"]
	if len(paths) != 1 || paths[0].SWIfIndex != 10 ||
		paths[0].NextHop != netip.MustParseAddr("10.255.0.2") ||
		len(paths[0].Labels) != 1 || paths[0].Labels[0] != 1100 {
		t.Fatalf("fIB path = %+v, want wg0(10) via 10.255.0.2 push 1100", paths)
	}
	if len(dp.cli) != 1 || dp.cli[0] != MPLSEgressCmd {
		t.Errorf("第一次装路由后应立刻触发 %q，got %v", MPLSEgressCmd, dp.cli)
	}

	writes := dp.writes
	res, err = a.Apply(ctx, plan1())
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() || res.MPLSChanged {
		t.Errorf("second apply = %+v, want 全零", res)
	}
	if dp.writes != writes {
		t.Errorf("幂等轮写了 %d 次，want 0", dp.writes-writes)
	}
	if len(dp.cli) != 1 {
		t.Errorf("幂等轮不该再触发 CLI，got %v", dp.cli)
	}
}

func TestApplyConvergesToNewPlan(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	ctx := context.Background()
	if _, err := a.Apply(ctx, plan1()); err != nil {
		t.Fatal(err)
	}

	next, err := planFrom(
		[]uint8{101},
		[]RIBRoute{ribOne(101, "172.16.0.0/16", "10.255.0.3")},
		[]Tunnel{{NextHop: "10.255.0.3", Interface: "wg1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{TablesAdded: 1, LabelsSet: 1, LabelsRemoved: 1, RoutesSet: 1, RoutesRemoved: 1, MPLSChanged: true}
	if res != want {
		t.Errorf("res = %+v, want %+v", res, want)
	}
	// 旧计划只有一条路由（颜色 0 就在主表里），撤它就是撤这一条。
	if _, ok := dp.routes["100|10.1.0.0/24"]; ok {
		t.Errorf("100|10.1.0.0/24 没撤")
	}
	if dp.routes["101|172.16.0.0/16"][0].SWIfIndex != 11 {
		t.Error("新路由没指到 wg1")
	}
	if _, ok := dp.labels[1100]; ok {
		t.Error("旧本地标签 1100 没撤")
	}
	if dp.labels[1101] != 101 {
		t.Errorf("label 1101 → %d, want 表 101", dp.labels[1101])
	}
	if len(dp.cli) != 2 {
		t.Errorf("CLI 调了 %d 次，want 2（首轮 + 收敛轮）", len(dp.cli))
	}
}

func TestApplyReplacesChangedPaths(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	ctx := context.Background()
	if _, err := a.Apply(ctx, plan1()); err != nil {
		t.Fatal(err)
	}

	next, err := planFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.3")},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0"},
			{NextHop: "10.255.0.3", Interface: "wg1"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if res.TablesAdded != 0 || res.LabelsSet != 0 || res.RoutesSet != 1 || res.RoutesRemoved != 0 {
		t.Errorf("res = %+v, want 只重下发这一张表里这条路由（颜色 0 与主表同一张）", res)
	}
	paths := dp.routes["100|10.1.0.0/24"]
	if len(paths) != 1 || paths[0].SWIfIndex != 11 || paths[0].Labels[0] != 1100 {
		t.Errorf("表 100 path = %+v, want wg1 压 1100（整条替换，不是追加）", paths)
	}
}

func TestApplyMissingInterfaceWritesNothing(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	plan, err := planFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg9"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "wg9") {
		t.Fatalf("err = %v, want 提到 wg9 不存在", err)
	}
	if dp.writes != 0 {
		t.Errorf("接口不存在时写了 %d 次，want 0", dp.writes)
	}
}

func TestApplyFailureDoesNotAdvancePrev(t *testing.T) {
	dp := newFakeDP()
	dp.failAt = "route"
	a := NewApplier(dp)
	ctx := context.Background()

	if _, err := a.Apply(ctx, plan1()); err == nil {
		t.Fatal("注入的下发失败没有报出来")
	}
	// prev 未前进：下一轮会从空基线重来，把表/标签再写一遍（都是幂等的）。
	res, err := a.Apply(ctx, plan1())
	if err != nil {
		t.Fatal(err)
	}
	if res.TablesAdded != 1 || res.LabelsSet != 1 || res.RoutesSet != 1 {
		t.Errorf("重试 res = %+v, want 从空基线重写全部（一张表、一条路由）", res)
	}
	if len(dp.routes["100|10.1.0.0/24"]) != 1 {
		t.Error("重试后路由缺失")
	}
}

func TestPlanFromSkipsBFDDownPaths(t *testing.T) {
	tun := []Tunnel{
		{NextHop: "10.255.0.2", Interface: "wg0"},
		{NextHop: "10.255.0.3", Interface: "wg1"},
	}
	down := map[netip.Addr]bool{netip.MustParseAddr("10.255.0.3"): true}
	plan, err := PlanFrom(
		[]uint8{100},
		[]RIBRoute{
			ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3"),
			ribOne(100, "10.2.0.0/24", "10.255.0.3"),
		},
		tun, down, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 { // 只有 10.1.0.0/24，而且颜色 0 就落在主表里
		t.Fatalf("routes = %+v, want 只有还有活路径的 10.1.0.0/24（一条）", plan.Routes)
	}
	main := mustRoute(t, plan, 100, "10.1.0.0/24")
	if paths := main.Paths; len(paths) != 1 || paths[0].Interface != "wg0" {
		t.Errorf("paths = %+v, want 只剩 wg0", paths)
	}
	mustAbsent(t, plan, "10.2.0.0/24")
	if len(plan.Warnings) != 2 { // 两条前缀各一条
		t.Errorf("warnings = %v, want 每条被跳过路径一条", plan.Warnings)
	}
	for _, w := range plan.Warnings {
		if !strings.Contains(w, "BFD") {
			t.Errorf("warning 没说明原因: %s", w)
		}
	}
}

func TestApplyBFDDownRemovesPath(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	ctx := context.Background()
	tun := []Tunnel{
		{NextHop: "10.255.0.2", Interface: "wg0"},
		{NextHop: "10.255.0.3", Interface: "wg1"},
	}
	routes := []RIBRoute{
		ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3"),
	}
	up, err := PlanFrom([]uint8{100}, routes, tun, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, up); err != nil {
		t.Fatal(err)
	}
	if paths := dp.routes["100|10.1.0.0/24"]; len(paths) != 2 {
		t.Fatalf("初始应有 2 条路径，got %+v", paths)
	}

	down, err := PlanFrom([]uint8{100}, routes, tun,
		map[netip.Addr]bool{netip.MustParseAddr("10.255.0.3"): true}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, down)
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesSet != 1 || res.RoutesRemoved != 0 || res.LabelsSet != 0 {
		t.Errorf("res = %+v, want 只重下发这张表里这条路由（颜色 0 与主表同一张；标签/表不动）", res)
	}
	paths := dp.routes["100|10.1.0.0/24"]
	if len(paths) != 1 || paths[0].SWIfIndex != 10 {
		t.Errorf("paths = %+v, want 只剩 wg0 那条", paths)
	}
}

func TestApplyEmptyPlanOnFreshApplier(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	res, err := a.Apply(context.Background(), Plan{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() || dp.writes != 0 || len(dp.cli) != 0 {
		t.Errorf("空计划不该动数据面: res=%+v writes=%d cli=%v", res, dp.writes, dp.cli)
	}
}

// VPN 0 的主表就是 global 表：登记标签、往它里面装路由，但**不建表**。
// §4.2 更正后颜色 0 的落点就是主表，所以"VPN 0 + 颜色 0"一张表都不该建 ——
// global 表本来就存在，建不得也删不得（删了会把 underlay 一起带走）。
// 非 0 颜色照建自己的表：那是 7000，与 0 号表无关。
func TestApplyVPN0UsesGlobalTable(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	plan, err := PlanFrom([]uint8{0},
		[]RIBRoute{ribOne(0, "10.68.2.0/24", "192.168.99.2")},
		[]Tunnel{{NextHop: "192.168.99.2", Interface: "wg0"}},
		nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Labels) != 1 || plan.Labels[0] != (Label{Label: 1000, Table: 0}) {
		t.Fatalf("labels = %+v, want 1000 → 表 0", plan.Labels)
	}
	res, err := a.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.TablesAdded != 0 || len(dp.tables) != 0 {
		t.Errorf("res = %+v tables = %v, want 一张表都不建（VPN 0 的颜色 0 就是 global 主表）", res, dp.tables)
	}
	remote := dp.routes["0|10.68.2.0/24"]
	if len(remote) != 1 || remote[0].SWIfIndex != 10 ||
		len(remote[0].Labels) != 1 || remote[0].Labels[0] != 1000 {
		t.Errorf("remote = %+v, want 压 1000 走 wg0(10)", remote)
	}

	dp7 := newFakeDP()
	a7 := NewApplier(dp7)
	plan7, err := PlanFrom([]uint8{0},
		[]RIBRoute{ribOne(0, "10.68.2.0/24", "192.168.99.2")},
		[]Tunnel{{NextHop: "192.168.99.2", Interface: "wg0", Color: 7}},
		nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	res7, err := a7.Apply(context.Background(), plan7)
	if err != nil {
		t.Fatal(err)
	}
	if res7.TablesAdded != 1 || !dp7.tables[7000] {
		t.Errorf("res = %+v tables = %v, want 只建 7000（VPN 0 的颜色 7 表）", res7, dp7.tables)
	}
	if _, bad := dp7.tables[0]; bad {
		t.Errorf("给 global 主表 0 建了表，VPP 会把 underlay 一起算进来")
	}
}

// 逃生关（默认）的口径：主表只收颜色 0，非 0 颜色只活在它自己的颜色表里。
func TestPlanFromColorPreference(t *testing.T) {
	plan, err := PlanFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3")},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0", Color: 0},
			{NextHop: "10.255.0.3", Interface: "wg1", Color: 7},
		}, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	main := mustRoute(t, plan, 100, "10.1.0.0/24")
	if len(main.Paths) != 1 || main.Paths[0].Interface != "wg0" || main.Paths[0].Preference != PrefBase {
		t.Errorf("主表 paths = %+v, want 只有 wg0（pref PrefBase）", main.Paths)
	}
	c7 := mustRoute(t, plan, colorTable(t, 7, 100), "10.1.0.0/24")
	if len(c7.Paths) != 1 || c7.Paths[0].Interface != "wg1" || c7.Paths[0].Preference != PrefBase+7 {
		t.Errorf("颜色 7 表 paths = %+v, want 只有 wg1（pref PrefBase+7）", c7.Paths)
	}
	// 颜色 0 没有自己的一张表了：它的落点就是上面那张主表（§4.2 更正），
	// 所以这里钉的是"主表里 wg0 只有一条、没有第二条副本"。
	if len(plan.Routes) != 2 {
		t.Errorf("routes = %+v, want 2（主表=颜色 0 的落点 + 颜色 7 表）", plan.Routes)
	}
}

// 逃生开关打开（§5）：主表收全部颜色，谁先用交给 VPP 按 preference 排（§2.1）。
// 颜色表**两种情况下都照写** —— 它是 policy 阶段引流的落点，不是开关的替代品，
// 所以这里断言的是"开关只多出一份主表路径，颜色表一条不多一条不少"。
func TestPlanFromEscapePutsAllColorsInMainTable(t *testing.T) {
	vrfs := []uint8{100}
	routes := []RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3")}
	tunnels := []Tunnel{
		{NextHop: "10.255.0.2", Interface: "wg0", Color: 0},
		{NextHop: "10.255.0.3", Interface: "wg1", Color: 7},
	}
	off, err := PlanFrom(vrfs, routes, tunnels, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	on, err := planEscape(vrfs, routes, tunnels)
	if err != nil {
		t.Fatal(err)
	}

	main := mustRoute(t, on, 100, "10.1.0.0/24")
	if len(main.Paths) != 2 {
		t.Fatalf("逃生开：主表 paths = %+v, want 两条都在主表里", main.Paths)
	}
	for _, want := range []struct {
		iface string
		pref  uint8
	}{{"wg0", PrefBase}, {"wg1", PrefBase + 7}} {
		found := false
		for _, p := range main.Paths {
			if p.Interface == want.iface && p.Preference == want.pref {
				found = true
			}
		}
		if !found {
			t.Errorf("主表缺 %s（pref %d）: %+v", want.iface, want.pref, main.Paths)
		}
	}
	// 非 0 颜色的表与开关无关：把关掉时那张表原样比一遍。
	// 颜色 0 不在这里比 —— 它的落点就是主表，而主表正是这个开关唯一改动的地方。
	for _, ct := range []uint32{colorTable(t, 7, 100)} {
		offR, onR := mustRoute(t, off, ct, "10.1.0.0/24"), mustRoute(t, on, ct, "10.1.0.0/24")
		if len(offR.Paths) != len(onR.Paths) || offR.Paths[0] != onR.Paths[0] {
			t.Errorf("颜色表 %d 被开关改动了: 关 %+v 开 %+v", ct, offR.Paths, onR.Paths)
		}
	}
	// 开关不新增路由对象、也不多建一张表：主表 100 本来就在（Tables 由 vrfs 打底），
	// 两个颜色同前缀又落进同一个桶。所以差异只体现在**桶里的路径数**。
	if len(on.Routes) != len(off.Routes) || len(on.Tables) != len(off.Tables) {
		t.Errorf("开/关的 routes=%d tables=%d，关 routes=%d tables=%d，want 数量都不变",
			len(on.Routes), len(on.Tables), len(off.Routes), len(off.Tables))
	}
	if offMain := mustRoute(t, off, 100, "10.1.0.0/24"); len(offMain.Paths) != 1 ||
		offMain.Paths[0].Interface != "wg0" {
		t.Errorf("逃生关：主表 paths = %+v, want 只有颜色 0 的 wg0", offMain.Paths)
	}
}

// 开关最尖锐的差别在"只剩昂贵颜色"时（§5 口径）：关 ⇒ 这条前缀在主表里**不存在**，
// 打流直接丢，不回落给昂贵颜色买单；开 ⇒ 主表用那条颜色 7 兜住，pref 仍是 PrefBase+色。
// 两种情况下颜色表 7100 都照写 —— 它是 policy 阶段的落点，不是开关的替身。
func TestPlanFromEscapeDecidesWhetherExpensiveColorSurvives(t *testing.T) {
	vrfs := []uint8{100}
	routes := []RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.3")}
	tunnels := []Tunnel{{NextHop: "10.255.0.3", Interface: "wg1", Color: 7}}

	off, err := planFrom(vrfs, routes, tunnels)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := routeAt(t, off, 100, "10.1.0.0/24"); ok {
		t.Errorf("逃生关：主表 100 里有 %s（paths %+v），want 这条前缀不存在", r.Prefix, r.Paths)
	}
	offColor := mustRoute(t, off, colorTable(t, 7, 100), "10.1.0.0/24")
	if len(offColor.Paths) != 1 || offColor.Paths[0].Preference != PrefBase+7 {
		t.Errorf("逃生关：颜色表 paths = %+v, want wg1 pref %d", offColor.Paths, PrefBase+7)
	}

	on, err := planEscape(vrfs, routes, tunnels)
	if err != nil {
		t.Fatal(err)
	}
	onMain := mustRoute(t, on, 100, "10.1.0.0/24")
	if len(onMain.Paths) != 1 || onMain.Paths[0].Interface != "wg1" ||
		onMain.Paths[0].Preference != PrefBase+7 {
		t.Errorf("逃生开：主表 paths = %+v, want wg1 pref %d", onMain.Paths, PrefBase+7)
	}
	onColor := mustRoute(t, on, colorTable(t, 7, 100), "10.1.0.0/24")
	if len(onColor.Paths) != len(offColor.Paths) || onColor.Paths[0] != offColor.Paths[0] {
		t.Errorf("逃生开改动颜色表: 关 %+v 开 %+v", offColor.Paths, onColor.Paths)
	}
	// 主表本来就是 vrfs 打底的表，开关不该多建表 —— 只多出一条路由对象。
	if len(on.Tables) != len(off.Tables) {
		t.Errorf("建表数 = %+v / %+v, want 相同", on.Tables, off.Tables)
	}
	if len(on.Routes) != len(off.Routes)+1 {
		t.Errorf("routes 数 = %d / %d, want 开的时候多主表那一条", len(on.Routes), len(off.Routes))
	}
}

// 负向：逃生开关**不放松**"同标签才同桶"。开关影响的是主表收哪些颜色（同一来源
// VPN 的不同隧道），不是收哪些来源（不同标签）。两个来源送同一前缀时，主表仍只留
// RD 胜者那个标签 —— 否则 §2.4 的静默投递会换个理由从逃生开关里回来。
func TestPlanFromEscapeDoesNotRelaxLabelBucket(t *testing.T) {
	plan, err := planEscape(
		[]uint8{40},
		[]RIBRoute{
			ribCross(40, 41, 2, "10.44.0.0/24", "10.255.0.2"),
			ribCross(40, 42, 2, "10.44.0.0/24", "10.255.0.2"),
		},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0", Color: 3}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []uint32{40, colorTable(t, 3, 40)} {
		main := mustRoute(t, plan, table, "10.44.0.0/24")
		if len(main.Paths) != 1 || main.Paths[0].Label != LabelFor(41) {
			t.Errorf("表 %d paths = %+v, want 只有 1041 那个标签的一条", table, main.Paths)
		}
	}
	joined := strings.Join(plan.Warnings, "; ")
	if !strings.Contains(joined, "择一") {
		t.Errorf("warnings = %v, want 有择一警告", plan.Warnings)
	}
}

// 逃生开关存在的理由就是这一格：颜色 0 被 BFD 判死之后主表还剩什么。开 ⇒ 主表
// 留下昂贵颜色的路径（pref=PrefBase+色 保证 VPP 先挑颜色 3 再挑颜色 7，§2.1 实测
// down 顶上/up 回落是原生行为）；关 ⇒ 主表这条前缀直接消失，打流宁丢不买单。
// 这是 O4 lab 判据的单元镜像，两边对的是同一件事。
func TestPlanFromColorZeroDownWithEscape(t *testing.T) {
	vrfs := []uint8{100}
	routes := []RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3", "10.255.0.4")}
	tunnels := []Tunnel{
		{NextHop: "10.255.0.2", Interface: "wg0", Color: 0},
		{NextHop: "10.255.0.3", Interface: "wg1", Color: 7},
		{NextHop: "10.255.0.4", Interface: "wg2", Color: 3},
	}
	down := map[netip.Addr]bool{netip.MustParseAddr("10.255.0.2"): true}

	on, err := PlanFrom(vrfs, routes, tunnels, down, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	main := mustRoute(t, on, 100, "10.1.0.0/24")
	if len(main.Paths) != 2 {
		t.Fatalf("逃生开、颜色 0 down：主表 paths = %+v, want wg1+wg2 两条", main.Paths)
	}
	// 计划不按 pref 排序（顺序交给 VPP 判定），钉的是每条路径的 pref 等于 PrefBase+色。
	wantPref := map[string]uint8{"wg1": PrefBase + 7, "wg2": PrefBase + 3}
	for _, p := range main.Paths {
		if p.Interface == "wg0" {
			t.Errorf("逃生开：主表里还有 down 掉的 wg0（%+v）", main.Paths)
			continue
		}
		if p.Preference != wantPref[p.Interface] {
			t.Errorf("%s 的 pref = %d, want %d（PrefBase+颜色）", p.Interface, p.Preference, wantPref[p.Interface])
		}
		delete(wantPref, p.Interface)
	}
	if len(wantPref) != 0 {
		t.Errorf("主表缺路径 %v（现有 %+v）", wantPref, main.Paths)
	}
	// down 掉的那条路径（wg0，颜色 0）在整份计划里一次都不该出现。这里不能问
	// "颜色 0 那张表空不空"：§4.2 更正后颜色 0 的落点**就是主表**，主表里本来
	// 就带着别的颜色的路径（逃生开着）；能问的是这条路径本身。
	for _, r := range on.Routes {
		for _, p := range r.Paths {
			if p.Interface == "wg0" {
				t.Errorf("逃生开：表 %d 里仍给 down 掉的 wg0 下发路径 %+v", r.Table, p)
			}
		}
	}

	off, err := PlanFrom(vrfs, routes, tunnels, down, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := routeAt(t, off, 100, "10.1.0.0/24"); ok {
		t.Errorf("逃生关、颜色 0 down：主表 100 里还有这条前缀，want 不存在（§5 不回落）")
	}
	// 颜色表两侧都照写：policy 引流不靠开关，别把它当逃生开关的替代品删了。
	for _, c := range []uint8{3, 7} {
		ct, err := ColorTable(c, 100)
		if err != nil {
			t.Fatal(err)
		}
		mustRoute(t, off, ct, "10.1.0.0/24")
		mustRoute(t, on, ct, "10.1.0.0/24")
	}
}

func TestPlanFromRejectsBadColor(t *testing.T) {
	_, err := PlanFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0", Color: 8}}, nil, false, nil)
	if err == nil {
		t.Fatal("Color 8 (PrefBase+8 > PrefBase+MaxColor) must fail")
	}
	if !strings.Contains(err.Error(), "超出上限") {
		t.Errorf("err = %v, want mention color overflow", err)
	}
}

// Applier 的"身份没变、只有 pref 变了 ⇒ 先删后加"这条规则单独钉一份计划：
// PlanFrom 层已经造不出这个形状 —— 改颜色会同时把路径搬出主表（逃生关），
// pref 与桶是绑在一起变的。能只变 pref 不动身份的是 O4 的逃生开关翻转，
// 那时 Applier 收到的就是这里这种计划。
func TestApplyDeleteThenAddOnPrefChange(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	ctx := context.Background()

	p := func(pref uint8) Plan {
		return Plan{
			Tables: []uint32{100},
			Labels: []Label{{Label: 1100, Table: 100}},
			Routes: []Route{{Table: 100, Prefix: netip.MustParsePrefix("10.1.0.0/24"),
				Paths: []Path{{NextHop: netip.MustParseAddr("10.255.0.2"), Interface: "wg0", Label: 1100, Preference: pref}}}},
		}
	}
	if _, err := a.Apply(ctx, p(PrefBase)); err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, p(PrefBase+5))
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesRemoved != 1 || res.RoutesSet != 1 {
		t.Errorf("res = %+v, want RoutesRemoved=1 + RoutesSet=1 (delete-then-add)", res)
	}
	paths := dp.routes["100|10.1.0.0/24"]
	if len(paths) != 1 || paths[0].Preference != PrefBase+5 {
		t.Errorf("path pref = %+v, want single path with Preference=%d", paths, PrefBase+5)
	}
}

func TestApplyAddRemovePathNoExtraDelete(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	ctx := context.Background()

	planA, err := planFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0", Color: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, planA); err != nil {
		t.Fatal(err)
	}

	// 同颜色加一条路径：桶的身份集合变了（不是"只改 pref"）⇒ 直接整条替换，不删。
	planB, err := planFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3")},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0", Color: 0},
			{NextHop: "10.255.0.3", Interface: "wg1", Color: 0},
		})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, planB)
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesRemoved != 0 {
		t.Errorf("path-set changed should not trigger delete-then-add, got RoutesRemoved=%d", res.RoutesRemoved)
	}
	if res.RoutesSet != 1 {
		t.Errorf("RoutesSet = %d, want 1（颜色 0 就在主表里，只替换这一条）", res.RoutesSet)
	}
	if res.TablesAdded != 0 || res.LabelsSet != 0 {
		t.Errorf("res = %+v, want 表与标签都不动（颜色和主表都没变）", res)
	}
	if paths := dp.routes["100|10.1.0.0/24"]; len(paths) != 2 {
		t.Errorf("paths = %+v, want 2 条 ECMP", paths)
	}
}

// ---- O3：表号公式、跨 VPN 导通、同标签 ECMP、按 RD 择一 ----

func TestColorTableFormula(t *testing.T) {
	// §4.2（2026-11-03 更正）：表号 = 颜色*1000 + 主表号，段落在 0..7255。
	// 颜色 0 折出来**就是主表**，所以 c=0 那一步不是"另一张表"而是同一张。
	for c := uint8(0); c <= 7; c++ {
		want := uint32(c)*1000 + 100
		if got := colorTable(t, c, 100); got != want {
			t.Errorf("ColorTable(%d,100) = %d, want %d", c, got, want)
		}
	}
	if got := colorTable(t, 0, 0); got != 0 {
		t.Errorf("ColorTable(0,0) = %d, want 0 —— VPN 0 的颜色 0 落点就是 global 主表", got)
	}
	if got := colorTable(t, 1, 0); got != 1000 {
		t.Errorf("ColorTable(1,0) = %d, want 1000 —— 段基址去掉后颜色 1 才第一次偏移", got)
	}
	if colorTable(t, 1, 255) <= colorTable(t, 0, 255) {
		t.Error("色段没有按颜色递增")
	}
	// 主表号越界会撞进下一个色段；颜色越界是 preference 溢出（§4.2 两个都要报错，不能绕回）。
	for _, c := range []struct {
		color uint8
		main  uint32
		want  string
	}{
		{MaxColor + 1, 100, "颜色"},
		{0, uint32(MaxVRFID) + 1, "主表号"},
	} {
		if _, err := ColorTable(c.color, c.main); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ColorTable(%d,%d): err = %v, want 包含 %q", c.color, c.main, err, c.want)
		}
	}
}

// 导通的形状：路由来源是别人的 VPN 10，装进本机表 20，压的标签必须是 1010
// （来源 VPN），不是 1020。本机没配 VPN 10 也照样装 —— 登记的本地标签只覆盖
// 本机自己的 VPN，出方向压的是对端的号。这是 §2.4 那个坑的正向半边。
func TestPlanFromCrossVPNInstall(t *testing.T) {
	plan, err := planFrom(
		[]uint8{20},
		[]RIBRoute{ribCross(20, 10, 2, "10.10.1.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Labels) != 1 || plan.Labels[0] != (Label{1020, 20}) {
		t.Errorf("labels = %+v, want 只登记本机 VPN 20 的 1020", plan.Labels)
	}
	// 这条导通路由只有颜色 0，落点就是主表 20 本身（§4.2 更正 ⇒ 没有 10000+20
	// 那份副本了）。标签取**来源** VPN，与装进哪张表无关。
	r := mustRoute(t, plan, 20, "10.10.1.0/24")
	if len(r.Paths) != 1 || r.Paths[0].Label != 1010 {
		t.Errorf("表 20 paths = %+v, want 一条压 1010（来源 VPN 的标签）", r.Paths)
	}
	if len(plan.Routes) != 1 {
		t.Errorf("routes = %+v, want 1（颜色 0 就是主表，不再重复装一张）", plan.Routes)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v, want 空", plan.Warnings)
	}
}

// 同站点两台设备（站点 HA）送同一前缀、来源 VPN 相同 ⇒ 标签相同 ⇒ 同一个桶里
// 两条路径 ECMP，绝不择一。择一只发生在**不同标签**之间（§13.3：主备不黑洞）。
func TestPlanFromSameLabelAcrossOrigins(t *testing.T) {
	plan, err := planFrom(
		[]uint8{10},
		[]RIBRoute{
			ribCross(10, 10, 2, "10.10.1.0/24", "10.255.0.2"),
			ribCross(10, 10, 3, "10.10.1.0/24", "10.255.0.3"),
		},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0"},
			{NextHop: "10.255.0.3", Interface: "wg1"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := mustRoute(t, plan, 10, "10.10.1.0/24")
	if len(r.Paths) != 2 {
		t.Fatalf("paths = %+v, want 2 条（同标签不择一，ECMP）", r.Paths)
	}
	if r.Paths[0].Label != 1010 || r.Paths[1].Label != 1010 {
		t.Errorf("paths = %+v, want 两条都压 1010", r.Paths)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v, want 空（同标签不该报择一）", plan.Warnings)
	}
}

// 负向 = §2.4 静默投递的回归测试：两个**不同来源 VPN**送同一前缀进同一张表，
// 标签不同不能同桶 ⇒ 按 RD = <device, vpn> 小的胜，桶里只剩一个标签，且必须留
// warning。这条要是退化了，一半流量会被静默投进另一个 VPN 的表。
func TestPlanFromDifferentLabelsTieBreakByRD(t *testing.T) {
	// 胜者 RD <1,11> = 267，败者 RD <5,10> = 1290：device 先比，所以 1/11 赢，
	// 即使它的来源 VPN 号更大。
	plan, err := planFrom(
		[]uint8{20},
		[]RIBRoute{
			ribCross(20, 10, 5, "10.10.1.0/24", "10.255.0.2"),
			ribCross(20, 11, 1, "10.10.1.0/24", "10.255.0.3"),
		},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0"},
			{NextHop: "10.255.0.3", Interface: "wg1"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 只有一个桶：颜色 0 的落点就是主表 20，两个桶键相同 ⇒ 合并。
	r := mustRoute(t, plan, 20, "10.10.1.0/24")
	if len(r.Paths) != 1 || r.Paths[0].Label != 1011 || r.Paths[0].Interface != "wg1" {
		t.Errorf("表 20 paths = %+v, want 只剩 wg1 压 1011（RD 1/11 胜）", r.Paths)
	}
	if len(plan.Routes) != 1 {
		t.Fatalf("routes = %+v, want 1（主表就是颜色 0 的落点，只有一个桶）", plan.Routes)
	}
	// 一条而不是两条：pickWinner 的 losers 按 RD 去重，桶合并后同一个败者 RD 只计一次。
	if len(plan.Warnings) != 1 {
		t.Fatalf("warnings = %v, want 1（那个桶一条择一警告）", plan.Warnings)
	}
	for _, w := range plan.Warnings {
		if !strings.Contains(w, "择一") || !strings.Contains(w, "5/10") {
			t.Errorf("warning 没写清胜者/败者: %s", w)
		}
	}
}

// 同一来源、同一前缀被两个 RR 各反射一次（双 RR 主备是常态）：桶键相同 ⇒ 合并，
// 不产生第二次下发。
func TestPlanFromMergesDuplicatedReflection(t *testing.T) {
	plan, err := planFrom(
		[]uint8{10},
		[]RIBRoute{
			ribCross(10, 10, 2, "10.10.1.0/24", "10.255.0.2"),
			ribCross(10, 10, 2, "10.10.1.0/24", "10.255.0.2"),
		},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 {
		t.Fatalf("routes = %+v, want 1（主表就是颜色 0 的落点，只有一个桶）", plan.Routes)
	}
	if paths := mustRoute(t, plan, 10, "10.10.1.0/24").Paths; len(paths) != 1 {
		t.Errorf("paths = %+v, want 1（重复反射已去重）", paths)
	}
}

// 表集合的口径：本机 VPN 主表恒在（哪怕一条路由都没有，标签总要落表），
// 颜色表只在**真装了路由**时出现 —— 不是 8×256 全开（§4.2）。
func TestPlanFromTablesAreSeededPlusUsed(t *testing.T) {
	plan, err := planFrom(
		[]uint8{10, 20},
		[]RIBRoute{ribCross(20, 10, 2, "10.10.1.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0", Color: 3}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{10, 20, colorTable(t, 3, 20)}
	if len(plan.Tables) != len(want) {
		t.Fatalf("tables = %v, want %v", plan.Tables, want)
	}
	for i, w := range want {
		if plan.Tables[i] != w {
			t.Fatalf("tables = %v, want %v", plan.Tables, want)
		}
	}
	// 颜色 3 的路径不进主表 20（逃生关）。这里不用再单独查一次"颜色 0 表"：
	// 颜色 0 的落点就是主表 20，同一个断言已经覆盖了。
	if _, ok := routeAt(t, plan, 20, "10.10.1.0/24"); ok {
		t.Error("主表 20 收了颜色 3 的路径，逃生开关是关的")
	}
	if len(plan.Routes) != 1 {
		t.Errorf("routes = %+v, want 只有颜色 3 表那一条", plan.Routes)
	}
}

// 兜底：颜色表里一条 0.0.0.0/0 → 本 VPN 主表。它必须
//
//	① 让那张颜色表存在（哪怕这张表里一条普通路由都没有），
//	② 走 deag 形状（sw_if_index=~0 + table_id=主表），不是普通下一跳，
//	③ 和其它路由一样参与整份计划的差异计算（幂等）。
func TestPlanFallbackDefaultRoute(t *testing.T) {
	vrfs := []uint8{100}
	routes := []RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")}
	tun := []Tunnel{
		{NextHop: "10.255.0.2", Interface: "wg0", Color: 0},
	}
	fb := []ColorFallback{{VPN: 100, Color: 2}}

	plan, err := PlanFrom(vrfs, routes, tun, nil, false, fb)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := ColorTable(2, 100)
	var got *Route
	for i := range plan.Routes {
		if plan.Routes[i].Table == ct {
			got = &plan.Routes[i]
		}
	}
	if got == nil {
		t.Fatalf("颜色表 %d 里没有兜底路由：%+v", ct, plan.Routes)
	}
	if got.Prefix.String() != "0.0.0.0/0" {
		t.Fatalf("兜底路由的前缀是 %s，应该是 0.0.0.0/0", got.Prefix)
	}
	if len(got.Paths) != 1 || !got.Paths[0].Deag || got.Paths[0].LookupTable != 100 {
		t.Fatalf("兜底路径形状不对（要 deag → 表 100）：%+v", got.Paths)
	}
	if !containsTable(plan.Tables, ct) {
		t.Fatalf("计划里的表没有 %d：%v", ct, plan.Tables)
	}

	// 下发形状：deag 必须是 sw_if_index=~0 + table_id=主表。
	dp := newFakeDP()
	dp.tables[100] = true
	ap := NewApplier(dp)
	if _, err := ap.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	paths := dp.routes[fmt.Sprintf("%d|0.0.0.0/0", ct)]
	if len(paths) != 1 || paths[0].SWIfIndex != ^uint32(0) || paths[0].TableID != 100 {
		t.Fatalf("下发的兜底路径是 %+v，期望 sw_if_index=~0 table_id=100", paths)
	}

	// 幂等：同一个 Applier 再收敛一份同样的计划，什么都不写。
	writes := dp.writes
	res, err := ap.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() || dp.writes != writes {
		t.Fatalf("同一份计划重复收敛不应有写动作：res=%+v writes %d -> %d", res, writes, dp.writes)
	}
}

func TestPlanFallbackRejectsColorZeroAndUnknownVPN(t *testing.T) {
	vrfs := []uint8{100}
	routes := []RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")}
	tun := []Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}}

	if _, err := PlanFrom(vrfs, routes, tun, nil, false, []ColorFallback{{VPN: 100, Color: 0}}); err == nil ||
		!strings.Contains(err.Error(), "颜色 0") {
		t.Fatalf("颜色 0 的兜底必须被拒绝，得到 %v", err)
	}
	if _, err := PlanFrom(vrfs, routes, tun, nil, false, []ColorFallback{{VPN: 77, Color: 2}}); err == nil ||
		!strings.Contains(err.Error(), "没服务这个 VPN") {
		t.Fatalf("没服务的 VPN 上的兜底必须被拒绝，得到 %v", err)
	}
}

// RIB 自己就有这个 VPN 的默认路由时，兜底不能把它顶掉：同一条前缀只能有一条。
func TestPlanFallbackYieldsToRealDefaultRoute(t *testing.T) {
	vrfs := []uint8{100}
	routes := []RIBRoute{ribOne(100, "0.0.0.0/0", "10.255.0.2")}
	tun := []Tunnel{{NextHop: "10.255.0.2", Interface: "wg0", Color: 2}}

	plan, err := PlanFrom(vrfs, routes, tun, nil, false, []ColorFallback{{VPN: 100, Color: 2}})
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := ColorTable(2, 100)
	for _, r := range plan.Routes {
		if r.Table != ct || r.Prefix.String() != "0.0.0.0/0" {
			continue
		}
		if len(r.Paths) != 1 || r.Paths[0].Deag {
			t.Fatalf("真默认路由被兜底顶掉了：%+v", r.Paths)
		}
		return
	}
	t.Fatalf("颜色表 %d 里没有 RIB 的那条默认路由：%+v", ct, plan.Routes)
}

func containsTable(ts []uint32, want uint32) bool {
	for _, t := range ts {
		if t == want {
			return true
		}
	}
	return false
}
