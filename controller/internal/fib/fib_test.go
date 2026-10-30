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

// planFrom 是"没有可达性信息"的 PlanFrom：这些用例只关心 RIB 的翻译。
func planFrom(vrfs []uint8, routes []RIBRoute, tunnels []Tunnel) (Plan, error) {
	return PlanFrom(vrfs, routes, tunnels, nil)
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
	// 本机两张 VPN 主表 + 真被装进路由的那一张颜色表（§4.2：不是 8×256 全开）。
	wantTables := []uint32{100, 101, colorTable(t, 0, 100)}
	if len(plan.Tables) != len(wantTables) {
		t.Fatalf("tables = %v, want %v", plan.Tables, wantTables)
	}
	for i, w := range wantTables {
		if plan.Tables[i] != w {
			t.Fatalf("tables = %v, want %v", plan.Tables, wantTables)
		}
	}
	want := Path{NextHop: netip.MustParseAddr("10.255.0.2"), Interface: "wg0", Label: 1100, Preference: 230}
	for _, table := range []uint32{100, colorTable(t, 0, 100)} {
		r := mustRoute(t, plan, table, "10.1.0.0/24")
		if len(r.Paths) != 1 || r.Paths[0] != want {
			t.Errorf("表 %d paths = %+v, want [%+v]", table, r.Paths, want)
		}
	}
	if len(plan.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2 条（主表 + 颜色 0 的表，同前缀已合并）", plan.Routes)
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
	// 表 2 张（VPN 主表 + 颜色 0 的表）、路由 2 条：§4.3 第 5 步两张表各装一份。
	if res.TablesAdded != 2 || res.LabelsSet != 1 || res.RoutesSet != 2 || !res.MPLSChanged {
		t.Errorf("first apply = %+v, want {2 1 0 2 0 true}", res)
	}
	for _, id := range []uint32{100, colorTable(t, 0, 100)} {
		if !dp.tables[id] {
			t.Errorf("表 %d 没建", id)
		}
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
	want := Result{TablesAdded: 2, LabelsSet: 1, LabelsRemoved: 1, RoutesSet: 2, RoutesRemoved: 2, MPLSChanged: true}
	if res != want {
		t.Errorf("res = %+v, want %+v", res, want)
	}
	for _, k := range []string{"100|10.1.0.0/24", fmt.Sprintf("%d|10.1.0.0/24", colorTable(t, 0, 100))} {
		if _, ok := dp.routes[k]; ok {
			t.Errorf("%s 没撤", k)
		}
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
	if res.TablesAdded != 0 || res.LabelsSet != 0 || res.RoutesSet != 2 || res.RoutesRemoved != 0 {
		t.Errorf("res = %+v, want 只重下发两张表里这条路由", res)
	}
	for _, table := range []uint32{100, colorTable(t, 0, 100)} {
		paths := dp.routes[fmt.Sprintf("%d|10.1.0.0/24", table)]
		if len(paths) != 1 || paths[0].SWIfIndex != 11 || paths[0].Labels[0] != 1100 {
			t.Errorf("表 %d path = %+v, want wg1 压 1100（整条替换，不是追加）", table, paths)
		}
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
	if res.TablesAdded != 2 || res.LabelsSet != 1 || res.RoutesSet != 2 {
		t.Errorf("重试 res = %+v, want 从空基线重写全部", res)
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
		tun, down,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 2 { // 10.1.0.0/24 的主表 + 颜色表
		t.Fatalf("routes = %+v, want 只有还有活路径的 10.1.0.0/24（两张表）", plan.Routes)
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
	up, err := PlanFrom([]uint8{100}, routes, tun, nil)
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
		map[netip.Addr]bool{netip.MustParseAddr("10.255.0.3"): true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, down)
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesSet != 2 || res.RoutesRemoved != 0 || res.LabelsSet != 0 {
		t.Errorf("res = %+v, want 只重下发这条路由（两张表；标签/表不动）", res)
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

// VPN 0 的主表就是 global 表：登记标签、不建表，但它的颜色表照建（§4.2 公式对
// 主表号 0 同样成立 —— 10000 这张表只归 sr-fib 管，不碰 underlay）。
func TestApplyVPN0UsesGlobalTable(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	plan, err := PlanFrom([]uint8{0},
		[]RIBRoute{ribOne(0, "10.68.2.0/24", "192.168.99.2")},
		[]Tunnel{{NextHop: "192.168.99.2", Interface: "wg0"}},
		nil)
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
	if res.TablesAdded != 1 || !dp.tables[colorTable(t, 0, 0)] || dp.tables[0] {
		t.Errorf("res = %+v tables = %v, want 只建颜色表、global 表建不得", res, dp.tables)
	}
	remote := dp.routes["0|10.68.2.0/24"]
	if len(remote) != 1 || remote[0].SWIfIndex != 10 ||
		len(remote[0].Labels) != 1 || remote[0].Labels[0] != 1000 {
		t.Errorf("remote = %+v, want 压 1000 走 wg0(10)", remote)
	}
}

// 逃生关（默认）的口径：主表只收颜色 0，非 0 颜色只活在它自己的颜色表里。
// 打开开关（主表收全部颜色、靠 preference 排序）是 O4，不在这里测。
func TestPlanFromColorPreference(t *testing.T) {
	plan, err := PlanFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2", "10.255.0.3")},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0", Color: 0},
			{NextHop: "10.255.0.3", Interface: "wg1", Color: 7},
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	main := mustRoute(t, plan, 100, "10.1.0.0/24")
	if len(main.Paths) != 1 || main.Paths[0].Interface != "wg0" || main.Paths[0].Preference != 230 {
		t.Errorf("主表 paths = %+v, want 只有 wg0（pref 230）", main.Paths)
	}
	c7 := mustRoute(t, plan, colorTable(t, 7, 100), "10.1.0.0/24")
	if len(c7.Paths) != 1 || c7.Paths[0].Interface != "wg1" || c7.Paths[0].Preference != 237 {
		t.Errorf("颜色 7 表 paths = %+v, want 只有 wg1（pref 237）", c7.Paths)
	}
	c0 := mustRoute(t, plan, colorTable(t, 0, 100), "10.1.0.0/24")
	if len(c0.Paths) != 1 || c0.Paths[0].Interface != "wg0" {
		t.Errorf("颜色 0 表 paths = %+v, want 只有 wg0", c0.Paths)
	}
	if len(plan.Routes) != 3 {
		t.Errorf("routes = %+v, want 3（主表 + 颜色 0 + 颜色 7）", plan.Routes)
	}
}

func TestPlanFromRejectsBadColor(t *testing.T) {
	_, err := PlanFrom(
		[]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0", Color: 230}}, nil)
	if err == nil {
		t.Fatal("Color 230 (230+230 > 255) must fail")
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
	if _, err := a.Apply(ctx, p(230)); err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, p(235))
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesRemoved != 1 || res.RoutesSet != 1 {
		t.Errorf("res = %+v, want RoutesRemoved=1 + RoutesSet=1 (delete-then-add)", res)
	}
	paths := dp.routes["100|10.1.0.0/24"]
	if len(paths) != 1 || paths[0].Preference != 235 {
		t.Errorf("path pref = %+v, want single path with Preference=235", paths)
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
	if res.RoutesSet != 2 {
		t.Errorf("RoutesSet = %d, want 2（主表 + 颜色 0 表各替换一次）", res.RoutesSet)
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
	// §4.2：颜色 0 → 10000..10255，颜色 7 → 17000..17255，色段之间不重叠。
	for c := uint8(0); c <= 7; c++ {
		want := uint32(c)*1000 + 10000 + 100
		if got := colorTable(t, c, 100); got != want {
			t.Errorf("ColorTable(%d,100) = %d, want %d", c, got, want)
		}
	}
	if got := colorTable(t, 0, 0); got != 10000 {
		t.Errorf("ColorTable(0,0) = %d, want 10000", got)
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
	for _, table := range []uint32{20, colorTable(t, 0, 20)} {
		r := mustRoute(t, plan, table, "10.10.1.0/24")
		if len(r.Paths) != 1 || r.Paths[0].Label != 1010 {
			t.Errorf("表 %d paths = %+v, want 一条压 1010（来源 VPN 的标签）", table, r.Paths)
		}
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
	for _, table := range []uint32{20, colorTable(t, 0, 20)} {
		r := mustRoute(t, plan, table, "10.10.1.0/24")
		if len(r.Paths) != 1 || r.Paths[0].Label != 1011 || r.Paths[0].Interface != "wg1" {
			t.Errorf("表 %d paths = %+v, want 只剩 wg1 压 1011（RD 1/11 胜）", table, r.Paths)
		}
	}
	if len(plan.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2（每个桶一条）", plan.Routes)
	}
	if len(plan.Warnings) != 2 {
		t.Fatalf("warnings = %v, want 每个桶一条择一警告", plan.Warnings)
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
	if len(plan.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2（主表 + 颜色 0 表）", plan.Routes)
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
	// 颜色 3 的路径不进主表 20（逃生关）：主表和它的颜色 0 表都没有这条前缀。
	if _, ok := routeAt(t, plan, 20, "10.10.1.0/24"); ok {
		t.Error("主表 20 收了颜色 3 的路径，逃生开关是关的")
	}
	if _, ok := routeAt(t, plan, colorTable(t, 0, 20), "10.10.1.0/24"); ok {
		t.Error("颜色 0 表收了颜色 3 的路径")
	}
	if len(plan.Routes) != 1 {
		t.Errorf("routes = %+v, want 只有颜色 3 表那一条", plan.Routes)
	}
}
