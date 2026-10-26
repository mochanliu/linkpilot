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
	return PlanFrom(vrfs, routes, tunnels, nil, nil, LocalPath{})
}

func plan1() Plan {
	p, err := planFrom(
		[]uint8{100},
		[]RIBRoute{{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2"}}},
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
			{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2"}},
			{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2"}},
			{VRF: 100, Prefix: "10.1.0.9/24", NextHops: []string{"10.255.0.2"}}, // 未规范化前缀
		},
		[]Tunnel{tun},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Labels) != 2 || plan.Labels[0] != (Label{1100, 100}) || plan.Labels[1] != (Label{1101, 101}) {
		t.Fatalf("labels = %+v, want [{1100 100} {1101 101}]", plan.Labels)
	}
	if len(plan.Routes) != 1 {
		t.Fatalf("routes = %+v, want 1 条（同前缀合并）", plan.Routes)
	}
	r := plan.Routes[0]
	if r.Table != 100 || r.Prefix != netip.MustParsePrefix("10.1.0.0/24") {
		t.Errorf("route = %d %s, want 100 10.1.0.0/24", r.Table, r.Prefix)
	}
	want := Path{NextHop: netip.MustParseAddr("10.255.0.2"), Interface: "wg0", Label: 1100}
	if len(r.Paths) != 1 || r.Paths[0] != want {
		t.Errorf("paths = %+v, want [%+v]", r.Paths, want)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v, want 空", plan.Warnings)
	}
}

func TestPlanFromMultipath(t *testing.T) {
	plan, err := planFrom(
		[]uint8{100},
		[]RIBRoute{
			// 一条路由自带两条路径；另一台设备宣告同一前缀、其中一条路径重合。
			{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2", "10.255.0.3"}},
			{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.3"}},
		},
		[]Tunnel{
			{NextHop: "10.255.0.3", Interface: "wg1"},
			{NextHop: "10.255.0.2", Interface: "wg0"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	paths := plan.Routes[0].Paths
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
			{VRF: 100, Prefix: "10.9.0.0/24", NextHops: []string{"10.255.0.9"}},
			{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2"}},
		},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}},
	)
	if err != nil {
		t.Fatalf("单条路径没隧道不该让整份计划失败: %v", err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Prefix.String() != "10.1.0.0/24" {
		t.Errorf("routes = %+v, want 只有 10.1.0.0/24", plan.Routes)
	}
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
		{"vrf not local", []uint8{100}, []RIBRoute{{VRF: 101, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2"}}}, tun, "没配这个 VRF"},
		{"bad prefix", []uint8{100}, []RIBRoute{{VRF: 100, Prefix: "10.1.0.0/33", NextHops: []string{"10.255.0.2"}}}, tun, "不是规范 IPv4 前缀"},
		{"v6 prefix", []uint8{100}, []RIBRoute{{VRF: 100, Prefix: "2001:db8::/64", NextHops: []string{"10.255.0.2"}}}, tun, "不是规范 IPv4 前缀"},
		{"bad nexthop", []uint8{100}, []RIBRoute{{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"wg0"}}}, tun, "不是规范 IPv4 地址"},
		{"no nexthops", []uint8{100}, []RIBRoute{{VRF: 100, Prefix: "10.1.0.0/24"}}, tun, "没有任何下一跳"},
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
	if res.TablesAdded != 1 || res.LabelsSet != 1 || res.RoutesSet != 1 || !res.MPLSChanged {
		t.Errorf("first apply = %+v, want {1 1 0 1 0 true}", res)
	}
	if !dp.tables[100] {
		t.Error("表 100 没建")
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
		[]RIBRoute{{VRF: 101, Prefix: "172.16.0.0/16", NextHops: []string{"10.255.0.3"}}},
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
	if _, ok := dp.routes["100|10.1.0.0/24"]; ok {
		t.Error("旧 VRF 的路由没撤")
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
		[]RIBRoute{{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.3"}}},
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
		t.Errorf("res = %+v, want 只重下发这条路由", res)
	}
	paths := dp.routes["100|10.1.0.0/24"]
	if len(paths) != 1 || paths[0].SWIfIndex != 11 || paths[0].Labels[0] != 1100 {
		t.Errorf("path = %+v, want wg1 压 1100（整条替换，不是追加）", paths)
	}
}

func TestApplyMissingInterfaceWritesNothing(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	plan, err := planFrom(
		[]uint8{100},
		[]RIBRoute{{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2"}}},
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
			{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2", "10.255.0.3"}},
			{VRF: 100, Prefix: "10.2.0.0/24", NextHops: []string{"10.255.0.3"}},
		},
		tun, down, nil, LocalPath{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Prefix.String() != "10.1.0.0/24" {
		t.Fatalf("routes = %+v, want 只有还有活路径的 10.1.0.0/24", plan.Routes)
	}
	if paths := plan.Routes[0].Paths; len(paths) != 1 || paths[0].Interface != "wg0" {
		t.Errorf("paths = %+v, want 只剩 wg0", paths)
	}
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
		{VRF: 100, Prefix: "10.1.0.0/24", NextHops: []string{"10.255.0.2", "10.255.0.3"}},
	}
	up, err := PlanFrom([]uint8{100}, routes, tun, nil, nil, LocalPath{})
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
		map[netip.Addr]bool{netip.MustParseAddr("10.255.0.3"): true}, nil, LocalPath{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(ctx, down)
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesSet != 1 || res.RoutesRemoved != 0 || res.LabelsSet != 0 {
		t.Errorf("res = %+v, want 只重下发这条路由（标签/表不动）", res)
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

func TestPlanFromLocalRoutesUnlabeled(t *testing.T) {
	lp := LocalPath{NextHop: netip.MustParseAddr("10.78.1.20"), Interface: "host-inside"}
	plan, err := PlanFrom(
		[]uint8{100},
		[]RIBRoute{{VRF: 100, Prefix: "10.68.2.0/24", NextHops: []string{"192.168.99.2"}}},
		[]Tunnel{{NextHop: "192.168.99.2", Interface: "wg0"}},
		nil,
		[]LocalRoute{
			{VRF: 100, Prefix: "10.68.1.9/24"}, // 未规范化
			{VRF: 100, Prefix: "10.68.1.0/24"}, // 与上一条同一前缀
		},
		lp,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 2 {
		t.Fatalf("routes = %+v, want overlay 一条 + 本地一条", plan.Routes)
	}
	var local *Route
	for i := range plan.Routes {
		if plan.Routes[i].Prefix.String() == "10.68.1.0/24" {
			local = &plan.Routes[i]
		}
	}
	if local == nil {
		t.Fatal("本地前缀没进计划")
	}
	if local.Table != 100 || len(local.Paths) != 1 {
		t.Fatalf("local = %+v, want 表 100 单路径", local)
	}
	if p := local.Paths[0]; p.NextHop != lp.NextHop || p.Interface != "host-inside" || p.Label != 0 {
		t.Errorf("local path = %+v, want 10.78.1.20 via host-inside 无标签", p)
	}
}

func TestApplyLocalRouteWritesNoLabel(t *testing.T) {
	dp := newFakeDP()
	dp.ifs = append(dp.ifs, vppapi.SwInterface{Name: "host-inside", SWIfIndex: 12})
	a := NewApplier(dp)
	plan, err := PlanFrom([]uint8{100}, nil, nil, nil,
		[]LocalRoute{{VRF: 100, Prefix: "10.68.1.0/24"}},
		LocalPath{NextHop: netip.MustParseAddr("10.78.1.20"), Interface: "host-inside"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.TablesAdded != 1 || res.LabelsSet != 1 || res.RoutesSet != 1 {
		t.Errorf("res = %+v, want 建表 + 登记 1100 收端标签 + 一条路由", res)
	}
	paths := dp.routes["100|10.68.1.0/24"]
	if len(paths) != 1 || paths[0].SWIfIndex != 12 || len(paths[0].Labels) != 0 {
		t.Errorf("paths = %+v, want host-inside(12) 无标签", paths)
	}
}

// VPN 0 落在 global 表：登记标签，但不建表（table 0 本来就存在，underlay 在里面）。
func TestApplyVPN0UsesGlobalTable(t *testing.T) {
	dp := newFakeDP()
	dp.ifs = append(dp.ifs, vppapi.SwInterface{Name: "host-inside", SWIfIndex: 12})
	a := NewApplier(dp)
	plan, err := PlanFrom([]uint8{0},
		[]RIBRoute{{VRF: 0, Prefix: "10.68.2.0/24", NextHops: []string{"192.168.99.2"}}},
		[]Tunnel{{NextHop: "192.168.99.2", Interface: "wg0"}},
		nil,
		[]LocalRoute{{VRF: 0, Prefix: "10.68.1.0/24"}},
		LocalPath{NextHop: netip.MustParseAddr("10.78.1.20"), Interface: "host-inside"})
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
		t.Errorf("res = %+v tables = %v, want 不建表（VPN 0 就是 global 表）", res, dp.tables)
	}
	remote := dp.routes["0|10.68.2.0/24"]
	if len(remote) != 1 || remote[0].SWIfIndex != 10 ||
		len(remote[0].Labels) != 1 || remote[0].Labels[0] != 1000 {
		t.Errorf("remote = %+v, want 压 1000 走 wg0(10)", remote)
	}
}

func TestPlanFromLocalConflictsAndMisconfig(t *testing.T) {
	lp := LocalPath{NextHop: netip.MustParseAddr("10.78.1.20"), Interface: "host-inside"}
	tun := []Tunnel{{NextHop: "192.168.99.2", Interface: "wg0"}}
	overlay := []RIBRoute{{VRF: 100, Prefix: "10.68.1.0/24", NextHops: []string{"192.168.99.2"}}}
	cases := []struct {
		name  string
		local []LocalRoute
		lp    LocalPath
		want  string
	}{
		{"缺出口", []LocalRoute{{VRF: 100, Prefix: "10.68.1.0/24"}}, LocalPath{}, "本地出口"},
		{"VRF 未配", []LocalRoute{{VRF: 251, Prefix: "10.68.1.0/24"}}, lp, "没配的 VRF"},
		{"坏前缀", []LocalRoute{{VRF: 100, Prefix: "10.68.1.0/33"}}, lp, "不是规范 IPv4 前缀"},
		{"与 overlay 冲突", []LocalRoute{{VRF: 100, Prefix: "10.68.1.0/24"}}, lp, "混合路径"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := PlanFrom([]uint8{100}, overlay, tun, nil, c.local, c.lp)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want 包含 %q", err, c.want)
			}
		})
	}
}
