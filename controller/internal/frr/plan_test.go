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
	writes    int
	failAt    string // 一次性注入失败：add / del
}

func newFakeDP() *fakeDP { return &fakeDP{installed: map[string]Route{}} }

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

func oifOf(v uint32) (int, error) { return 100 + int(v), nil }

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
	plan, err := PlanFrom([]uint32{2, 0, 1}, routes, oifOf)
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
	}, oifOf)
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
		"VPN 重复":     {vpns: []uint32{1, 1}, oif: oifOf},
		"出口解析失败":     {vpns: []uint32{5}, oif: func(uint32) (int, error) { return 0, errors.New("没有 vpn5") }},
		"出口 ifindex 非法": {
			vpns: []uint32{5}, oif: func(uint32) (int, error) { return 0, nil },
		},
	}
	for name, tc := range cases {
		if _, err := PlanFrom(tc.vpns, routes, tc.oif); err == nil {
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
		}, oifOf); err == nil {
			t.Errorf("%s: 期望报错", name)
		}
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
