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

func TestValidateTables(t *testing.T) {
	tests := []struct {
		name   string
		tables []Table
		ok     bool
	}{
		{"正常", []Table{{VRF: 1, ID: 1000}}, true},
		{"空", nil, false},
		{"VRF 0", []Table{{VRF: 0, ID: 1000}}, false},
		{"表号 255", []Table{{VRF: 1, ID: 255}}, false},
		{"表号 256", []Table{{VRF: 1, ID: 256}}, true},
		{"表号过大", []Table{{VRF: 1, ID: MaxTableID + 1}}, false},
		{"VRF 重复", []Table{{VRF: 1, ID: 1000}, {VRF: 1, ID: 1001}}, false},
		{"表号重复", []Table{{VRF: 1, ID: 1000}, {VRF: 2, ID: 1000}}, false},
	}
	for _, tc := range tests {
		err := ValidateTables(tc.tables)
		if tc.ok && err != nil {
			t.Errorf("%s: 不该报错: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: 期望报错", tc.name)
		}
	}
}

func TestPlanFrom(t *testing.T) {
	tables := []Table{{VRF: 1, ID: 1000}, {VRF: 2, ID: 1001}}
	routes := []RIBRoute{
		{VRF: 2, Prefix: "10.2.0.0/16", NextHops: []string{"192.168.99.2"}},
		{VRF: 1, Prefix: "10.1.0.0/24", NextHops: []string{"192.168.99.2", "192.168.99.3"}},
		{VRF: 1, Prefix: "10.1.0.1/24", NextHops: []string{"192.168.99.2"}}, // 未规范，收敛成同一个前缀
		{VRF: 1, Prefix: "10.9.0.0/16", NextHops: nil},                      // 没有路径，不导出
	}
	plan, err := PlanFrom(tables, routes, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 2 {
		t.Fatalf("期望 2 条（去重、无路径的跳过），实际 %d: %v", len(plan.Routes), plan.Routes)
	}
	if got := plan.Routes[0]; got.TableID != 1000 || got.Prefix.String() != "10.1.0.0/24" {
		t.Errorf("排序/表号不对: %+v", got)
	}
	if got := plan.Routes[1]; got.TableID != 1001 || got.Prefix.String() != "10.2.0.0/16" {
		t.Errorf("排序/表号不对: %+v", got)
	}
	for _, r := range plan.Routes {
		if r.Distance != 1 || r.Metric != 20 {
			t.Errorf("distance/metric 没带上: %+v", r)
		}
	}
}

func TestPlanFromRejects(t *testing.T) {
	tables := []Table{{VRF: 1, ID: 1000}}
	tests := map[string][]RIBRoute{
		"没配的 VRF": {{VRF: 2, Prefix: "10.2.0.0/16", NextHops: []string{"1.1.1.1"}}},
		"前缀解析失败":  {{VRF: 1, Prefix: "10.1.0.0", NextHops: []string{"1.1.1.1"}}},
		"IPv6":    {{VRF: 1, Prefix: "2001:db8::/32", NextHops: []string{"1.1.1.1"}}},
		"空前缀":     {{VRF: 1, Prefix: "", NextHops: []string{"1.1.1.1"}}},
	}
	for name, routes := range tests {
		if _, err := PlanFrom(tables, routes, 1, 20); err == nil {
			t.Errorf("%s: 期望报错", name)
		}
	}
	if _, err := PlanFrom(tables, nil, 0, 20); err == nil {
		t.Error("distance 0 应报错")
	}
	if _, err := PlanFrom(nil, nil, 1, 20); err == nil {
		t.Error("空表配置应报错")
	}
}

func TestExporterApply(t *testing.T) {
	dp := newFakeDP()
	ex := NewExporter(dp)
	ctx := context.Background()

	plan := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), TableID: 1000, Distance: 1, Metric: 20},
		{Prefix: netip.MustParsePrefix("10.2.0.0/24"), TableID: 1000, Distance: 1, Metric: 20},
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

	// 去掉一条、改掉一条的属性 → 一删一加，顺序是先加后删。
	plan2 := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), TableID: 1000, Distance: 1, Metric: 99},
		{Prefix: netip.MustParsePrefix("10.3.0.0/24"), TableID: 1000, Distance: 1, Metric: 20},
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

	want := []string{"1000|10.1.0.0/24", "1000|10.3.0.0/24"}
	got := ex.Routes()
	if len(got) != 2 || got[0].key() != want[0] || got[1].key() != want[1] {
		t.Fatalf("Routes 快照不对: %v", got)
	}
}

func TestExporterKeepsPrevOnFailure(t *testing.T) {
	dp := newFakeDP()
	ex := NewExporter(dp)
	ctx := context.Background()
	first := Plan{Routes: []Route{{Prefix: netip.MustParsePrefix("10.1.0.0/24"), TableID: 1000, Distance: 1}}}
	if _, err := ex.Apply(ctx, first); err != nil {
		t.Fatal(err)
	}

	dp.failAt = "add"
	second := Plan{Routes: []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), TableID: 1000, Distance: 1},
		{Prefix: netip.MustParsePrefix("10.2.0.0/24"), TableID: 1000, Distance: 1},
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
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), TableID: 1000, Distance: 1},
		{Prefix: netip.MustParsePrefix("10.2.0.0/24"), TableID: 1001, Distance: 1},
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
