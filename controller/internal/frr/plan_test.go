package frr

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
)

type fakeDP struct {
	installed map[string]Route
	foreign   map[string]bool // 同前缀有别人的路由：加会回 ErrRouteExists、归属不是我们
	local     map[uint32]map[netip.Prefix]bool
	writes    int
	replaces  int
	failAt    string // 一次性注入失败：add / del
}

func newFakeDP() *fakeDP {
	return &fakeDP{installed: map[string]Route{}, foreign: map[string]bool{}}
}

func (f *fakeDP) fail(op string) error {
	if f.failAt != op {
		return nil
	}
	f.failAt = ""
	return errors.New("injected: " + op)
}

func (f *fakeDP) RouteAdd(_ context.Context, r Route) error {
	f.writes++
	if err := f.fail("add"); err != nil {
		return err
	}
	if _, ok := f.installed[r.key()]; ok || f.foreign[r.key()] {
		return fmt.Errorf("%w: %s", ErrRouteExists, r.key())
	}
	f.installed[r.key()] = r
	return nil
}

func (f *fakeDP) RouteReplace(_ context.Context, r Route) error {
	f.writes++
	f.replaces++
	if err := f.fail("add"); err != nil {
		return err
	}
	f.installed[r.key()] = r
	return nil
}

func (f *fakeDP) RouteDelete(_ context.Context, r Route) error {
	f.writes++
	if err := f.fail("del"); err != nil {
		return err
	}
	if _, ok := f.installed[r.key()]; !ok {
		return fmt.Errorf("injected: 删除不存在的 %s", r.key())
	}
	delete(f.installed, r.key())
	return nil
}

func (f *fakeDP) Owns(_ context.Context, r Route) (bool, error) {
	got, ok := f.installed[r.key()]
	return ok && got.OIF == r.OIF, nil
}

func (f *fakeDP) LocalSubnets(table uint32) map[netip.Prefix]bool { return f.local[table] }
func oifOf(v uint32) (int, error)                                 { return 100 + int(v), nil }

func TestKernelTable(t *testing.T) {
	if got := KernelTable(0); got != 254 {
		t.Errorf("VPN 0 应落到内核 main(254)，实际 %d", got)
	}
	if got := KernelTable(100); got != 100 {
		t.Errorf("VPN 100 应落到表 100，实际 %d", got)
	}
}

func TestPlanFrom(t *testing.T) {
	routes := []RIBRoute{
		{VRF: 2, Prefix: "10.2.0.0/16", NextHops: []string{"192.168.99.2"}},
		{VRF: 0, Prefix: "10.0.0.0/24", NextHops: []string{"192.168.99.2"}},
		{VRF: 1, Prefix: "10.1.0.0/24", NextHops: []string{"192.168.99.2", "192.168.99.3"}},
		{VRF: 1, Prefix: "10.1.0.1/24", NextHops: []string{"192.168.99.2"}}, // 未规范，收敛成同一个前缀
		{VRF: 1, Prefix: "10.9.0.0/16", NextHops: nil},                      // 没有路径，不导出
		{VRF: 3, Prefix: "10.3.0.0/16", NextHops: []string{"192.168.99.2"}}, // 不在 export_vrfs，跳过
	}
	plan, err := PlanFrom([]uint32{2, 0, 1}, routes, oifOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 3 {
		t.Fatalf("期望 3 条（去重、无路径与未导出的跳过），实际 %d: %v", len(plan.Routes), plan.Routes)
	}
	if got := plan.Routes[0]; got.Table != 1 || got.OIF != 101 || got.Prefix.String() != "10.1.0.0/24" {
		t.Errorf("排序/表号/出口不对: %+v", got)
	}
	if got := plan.Routes[1]; got.Table != 2 || got.OIF != 102 || got.Prefix.String() != "10.2.0.0/16" {
		t.Errorf("排序/表号/出口不对: %+v", got)
	}
	if got := plan.Routes[2]; got.Table != 254 || got.OIF != 100 || got.Prefix.String() != "10.0.0.0/24" {
		t.Errorf("VPN 0 应进 main(254)、出口用 VPN 0 的 lo: %+v", got)
	}
}

func TestPlanFromEmptyExport(t *testing.T) {
	plan, err := PlanFrom(nil, []RIBRoute{
		{VRF: 0, Prefix: "10.0.0.0/24", NextHops: []string{"1.1.1.1"}},
	}, oifOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 0 {
		t.Fatalf("没有 export_vrfs 时不该导出任何前缀: %v", plan.Routes)
	}
}

func TestPlanFromRejects(t *testing.T) {
	routes := []RIBRoute{{VRF: 5, Prefix: "10.5.0.0/16", NextHops: []string{"1.1.1.1"}}}
	cases := map[string]struct {
		vpns []uint32
		oif  func(uint32) (int, error)
	}{
		"VPN 超过 255": {vpns: []uint32{256}, oif: oifOf},
		"导出 overlay": {vpns: []uint32{OverlayVRF}, oif: oifOf},
		"导出内核保留表":    {vpns: []uint32{253}, oif: oifOf},
		"VPN 重复":     {vpns: []uint32{1, 1}, oif: oifOf},
		"出口解析失败":     {vpns: []uint32{5}, oif: func(uint32) (int, error) { return 0, errors.New("没有 vpn5") }},
		"出口 ifindex 非法": {
			vpns: []uint32{5}, oif: func(uint32) (int, error) { return 0, nil },
		},
	}
	for name, tc := range cases {
		if _, err := PlanFrom(tc.vpns, routes, tc.oif, nil); err == nil {
			t.Errorf("%s: 期望报错", name)
		}
	}

	badPrefix := map[string]string{
		"没有斜杠": "10.1.0.0",
		"IPv6": "2001:db8::/32",
		"空串":   "",
		"4in6": "::ffff:10.0.0.0/120",
	}
	for name, pfx := range badPrefix {
		if _, err := PlanFrom([]uint32{5}, []RIBRoute{
			{VRF: 5, Prefix: pfx, NextHops: []string{"1.1.1.1"}},
		}, oifOf, nil); err == nil {
			t.Errorf("%s: 期望报错", name)
		}
	}
}

// ownedOf 把 "VPN -> 前缀列表" 摊成 PlanFrom 的 owned 输入。
func ownedOf(t *testing.T, byVPN map[uint8][]string) map[uint8]map[netip.Prefix]struct{} {
	t.Helper()
	out := map[uint8]map[netip.Prefix]struct{}{}
	for vpn, ps := range byVPN {
		set := map[netip.Prefix]struct{}{}
		for _, s := range ps {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				t.Fatalf("解析 %s: %v", s, err)
			}
			set[p.Masked()] = struct{}{}
		}
		out[vpn] = set
	}
	return out
}

// TestPlanWithholdsOwnedPrefix 钉住导出侧的本地闸门：客户侧已经有路由的前缀
// 一条都不注入，只进 Withheld。注入它会让 zebra 用 distance 0 的 K 压过客户的
// 真路径，客户协议再 redistribute kernel 自发一条把它顶掉，通知从此停发 ——
// 本盒再也看不到"这条前缀是我的"（实测 2026-11-06，lab-cpe3）。
func TestPlanWithholdsOwnedPrefix(t *testing.T) {
	routes := []RIBRoute{
		{VRF: 0, Prefix: "172.16.3.1/32", NextHops: []string{"10.250.0.0"}}, // 本盒客户侧已有
		{VRF: 0, Prefix: "10.0.9.0/24", NextHops: []string{"10.250.0.0"}},   // 纯远端，正常导出
		{VRF: 1, Prefix: "172.16.3.1/32", NextHops: []string{"10.250.0.0"}}, // 别的 VPN 不算拥有
	}
	plan, err := PlanFrom([]uint32{0, 1}, routes, oifOf, ownedOf(t, map[uint8][]string{0: {"172.16.3.1/32"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 2 {
		t.Fatalf("只该挡住 VPN 0 那一条，实际计划 %v", plan.Routes)
	}
	for _, r := range plan.Routes {
		if r.Table == 254 && r.Prefix.String() == "172.16.3.1/32" {
			t.Errorf("客户侧已有的前缀被注入了: %+v", r)
		}
	}
	if len(plan.Withheld) != 1 || plan.Withheld[0].Table != 254 ||
		plan.Withheld[0].Prefix.String() != "172.16.3.1/32" {
		t.Fatalf("Withheld 该只有 VPN 0 的 172.16.3.1/32，实际 %v", plan.Withheld)
	}
}

// TestPlanOwnedMatchesMasked 钉住前缀身份的口径：客户侧那条是 /32，通知里
// 带来的可能是未规范地址，两边都按 Masked 比。
func TestPlanOwnedMatchesMasked(t *testing.T) {
	plan, err := PlanFrom([]uint32{0}, []RIBRoute{
		{VRF: 0, Prefix: "10.7.0.0/24", NextHops: []string{"10.250.0.0"}},
	}, oifOf, ownedOf(t, map[uint8][]string{0: {"10.7.0.1/24"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 0 || len(plan.Withheld) != 1 {
		t.Fatalf("未规范的同前缀该被挡住，实际 routes=%v withheld=%v", plan.Routes, plan.Withheld)
	}
}

func TestExporterApply(t *testing.T) {
	dp := newFakeDP()
	ex := NewExporter(dp)
	ctx := context.Background()

	plan := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 100, OIF: 5},
		{Prefix: netip.MustParsePrefix("10.2.0.0/24"), Table: 100, OIF: 5},
	}}
	res, err := ex.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed() || res.Added != 2 || res.Removed != 0 {
		t.Fatalf("首次收敛结果不对: %+v", res)
	}
	if len(dp.installed) != 2 {
		t.Fatalf("数据面应有 2 条，实际 %d", len(dp.installed))
	}

	// 同样的计划再来一次：不该产生任何写。
	before := dp.writes
	res, err = ex.Apply(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() || dp.writes != before {
		t.Fatalf("无变化时不该写: %+v writes %d→%d", res, before, dp.writes)
	}

	// 去掉一条、挪一条的出口 → 一删两加（出口变化用 replace 完成，不先删）。
	plan2 := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 100, OIF: 7},
		{Prefix: netip.MustParsePrefix("10.3.0.0/24"), Table: 100, OIF: 5},
	}}
	res, err = ex.Apply(ctx, plan2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Removed != 1 {
		t.Fatalf("差异结果不对: %+v", res)
	}
	if len(dp.installed) != 2 {
		t.Fatalf("收敛后应有 2 条，实际 %v", dp.installed)
	}
	if got := dp.installed["100|10.1.0.0/24"]; got.OIF != 7 {
		t.Fatalf("出口变化没有生效: %+v", got)
	}

	want := []string{"100|10.1.0.0/24", "100|10.3.0.0/24"}
	got := ex.Routes()
	if len(got) != 2 || got[0].key() != want[0] || got[1].key() != want[1] {
		t.Fatalf("Routes 快照不对: %v", got)
	}
}

func TestExporterKeepsPrevOnFailure(t *testing.T) {
	dp := newFakeDP()
	ex := NewExporter(dp)
	ctx := context.Background()
	first := Plan{Routes: []Route{{Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 100, OIF: 5}}}
	if _, err := ex.Apply(ctx, first); err != nil {
		t.Fatal(err)
	}

	dp.failAt = "add"
	second := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 100, OIF: 5},
		{Prefix: netip.MustParsePrefix("10.2.0.0/24"), Table: 100, OIF: 5},
	}}
	if _, err := ex.Apply(ctx, second); err == nil {
		t.Fatal("注入的失败没有传出来")
	}
	if len(ex.Routes()) != 1 {
		t.Fatalf("失败的一轮不该改动基线: %v", ex.Routes())
	}
	// 下一轮（不注入失败）应补上差的那条。
	if _, err := ex.Apply(ctx, second); err != nil {
		t.Fatal(err)
	}
	if len(ex.Routes()) != 2 {
		t.Fatalf("重试后应有 2 条: %v", ex.Routes())
	}
}

func TestExporterFlush(t *testing.T) {
	dp := newFakeDP()
	ex := NewExporter(dp)
	ctx := context.Background()
	plan := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 100, OIF: 5},
		{Prefix: netip.MustParsePrefix("10.2.0.0/24"), Table: 101, OIF: 6},
	}}
	if _, err := ex.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := ex.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(dp.installed) != 0 || len(ex.Routes()) != 0 {
		t.Fatalf("Flush 之后不该留东西: dp=%v ex=%v", dp.installed, ex.Routes())
	}
	if err := ex.Flush(ctx); err != nil {
		t.Fatalf("空 Flush 应成功: %v", err)
	}
}

// 同前缀已经有别人的路由：不覆盖、不删、进 Skipped。这是 2026-11-02
// lab-cpe1 那次事故的判据（replace 把 mgmt 的 connected 路由顶掉）。
func TestApplySkipsForeignPrefix(t *testing.T) {
	dp := newFakeDP()
	p := netip.MustParsePrefix("10.99.0.0/24")
	dp.foreign["254|"+p.String()] = true
	ex := NewExporter(dp)
	ctx := context.Background()

	res, err := ex.Apply(ctx, Plan{Routes: []Route{{Prefix: p, Table: 254, OIF: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 || len(res.Skipped) != 1 || res.Skipped[0].Prefix != p {
		t.Fatalf("同前缀有主时应该跳过: %+v", res)
	}
	if len(dp.installed) != 0 || len(ex.Routes()) != 0 {
		t.Fatalf("跳过的前缀不该进账本: dp=%v ex=%v", dp.installed, ex.Routes())
	}
}

// 前缀就是盒子自己某个接口的网段：哪怕表里暂时没有 connected 路由（地址
// 还挂着 noprefixroute、或者上一版把它顶掉过），也永远不注入。
func TestApplySkipsLocalSubnet(t *testing.T) {
	dp := newFakeDP()
	p := netip.MustParsePrefix("10.99.0.0/24")
	dp.local = map[uint32]map[netip.Prefix]bool{254: {p: true}}
	// 账本里有一条上一版留下的（出口 lo），这轮要被撤掉。
	prev := Route{Prefix: p, Table: 254, OIF: 1}
	dp.installed[prev.key()] = prev
	ex := NewExporterFrom(dp, []Route{prev})
	ctx := context.Background()

	res, err := ex.Apply(ctx, Plan{Routes: []Route{{Prefix: p, Table: 254, OIF: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Removed != 1 || res.Added != 0 {
		t.Fatalf("接口网段应跳过并撤掉残留: %+v", res)
	}
	if len(dp.installed) != 0 || len(ex.Routes()) != 0 {
		t.Fatalf("残留应被撤干净: dp=%v ex=%v", dp.installed, ex.Routes())
	}
}

// 内核里已经有我们的路由（上次被杀、状态文件没写全）：收编而不是报错。
func TestApplyAdoptsOwnLeftover(t *testing.T) {
	dp := newFakeDP()
	p := netip.MustParsePrefix("10.1.0.0/24")
	r := Route{Prefix: p, Table: 100, OIF: 5}
	dp.installed[r.key()] = r
	ex := NewExporter(dp)
	ctx := context.Background()

	res, err := ex.Apply(ctx, Plan{Routes: []Route{r}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 || res.Adopted != 1 || len(res.Skipped) != 0 {
		t.Fatalf("应原样收编: %+v", res)
	}
	if got := ex.Routes(); len(got) != 1 || got[0] != r {
		t.Fatalf("收编后账本应有这条: %v", got)
	}
}
