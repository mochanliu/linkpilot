package fib

import (
	"context"
	"strings"
	"testing"
)

func TestResetRemovesManagedState(t *testing.T) {
	dp := newFakeDP()
	a := NewApplier(dp)
	if _, err := a.Apply(context.Background(), plan1()); err != nil {
		t.Fatal(err)
	}
	if len(dp.routes) != 2 || len(dp.labels) != 1 || len(dp.tables) != 2 {
		t.Fatal("前置状态没装上（主表 + 颜色 0 的表）")
	}

	res, err := Reset(context.Background(), dp, []uint8{100})
	if err != nil {
		t.Fatal(err)
	}
	if res.RoutesRemoved != 2 || res.LabelsRemoved != 1 || res.TablesRemoved != 2 {
		t.Errorf("res = %+v, want 路由 2 / 标签 1 / 表 2（颜色表也要撤干净）", res)
	}
	if len(dp.routes) != 0 || len(dp.labels) != 0 || len(dp.tables) != 0 {
		t.Errorf("仍有残留: routes=%v labels=%v tables=%v", dp.routes, dp.labels, dp.tables)
	}
	if len(dp.cli) != 1 {
		t.Errorf("reset 不该触发 mpls-egress（CLI 次数 %d）", len(dp.cli))
	}
}

func TestResetOnCleanStateIsNoop(t *testing.T) {
	// 表/标签都不存在时删除必须是幂等的：运维会重复执行。
	dp := newFakeDP()
	res, err := Reset(context.Background(), dp, []uint8{100})
	if err != nil {
		t.Fatalf("干净状态上 reset 报错: %v", err)
	}
	if res.RoutesRemoved != 0 {
		t.Errorf("res = %+v, want 0 条路由", res)
	}
}

func TestResetRejectsBadVRFs(t *testing.T) {
	dp := newFakeDP()
	for _, c := range []struct {
		vrfs []uint8
		want string
	}{
		{[]uint8{0}, "VRF 0"},
		{[]uint8{100, 100}, "配了两次"},
	} {
		if _, err := Reset(context.Background(), dp, c.vrfs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("vrfs=%v: err = %v, want 包含 %q", c.vrfs, err, c.want)
		}
	}
}
