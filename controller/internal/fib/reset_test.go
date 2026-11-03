package fib

import (
	"context"
	"strings"
	"testing"
)

func TestResetRemovesManagedState(t *testing.T) {
	// 一份计划里同时有颜色 0 和颜色 3 的路径，reset 的两条撤除路径才都覆盖到：
	//   颜色 0 ⇒ 只落主表 100（颜色 0 的落点就是主表，不产生第二张表）；
	//   颜色 3 ⇒ 落 3*1000+100 = 3100，只能靠颜色表循环撤干净。
	// 表数 2 而不是 3 钉的就是"颜色 0 不建表"；主表只撤一遍钉的是 reset 的
	// 循环从颜色 1 起（从 0 起会把主表引用计数减两回）。
	dp := newFakeDP()
	a := NewApplier(dp)
	plan, err := planFrom(
		[]uint8{100},
		[]RIBRoute{
			ribOne(100, "10.1.0.0/24", "10.255.0.2"),
			ribOne(100, "10.1.9.0/24", "10.255.0.3"),
		},
		[]Tunnel{
			{NextHop: "10.255.0.2", Interface: "wg0"},
			{NextHop: "10.255.0.3", Interface: "wg1", Color: 3},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(dp.routes) != 2 || len(dp.labels) != 1 || len(dp.tables) != 2 {
		t.Fatalf("前置状态没装上: routes=%v labels=%v tables=%v, want 2/1/2（主表 100 + 颜色 3 表 3100）",
			dp.routes, dp.labels, dp.tables)
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
		{[]uint8{250}, "overlay 保留表"},
		{[]uint8{100, 100}, "配了两次"},
	} {
		if _, err := Reset(context.Background(), dp, c.vrfs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("vrfs=%v: err = %v, want 包含 %q", c.vrfs, err, c.want)
		}
		if dp.writes != 0 {
			t.Errorf("vrfs=%v: 被拒绝的清单却写了 %d 次 —— reset 必须在动手删之前就把清单验完", c.vrfs, dp.writes)
		}
	}
}

func TestResetRejectsOverlayVrfWithoutTouchingOthers(t *testing.T) {
	// 清单里既有真 VPN 又有 250 时的症状：拒绝，但一张表都不许动。
	// 检查写在删除循环里时，{100, 250} 会先撤掉 VPN 100（连标签带表）再报错，
	// 于是"这次 reset 失败了"实际留下一个被清空的 VPN —— 客户前缀直接消失。
	dp := newFakeDP()
	plan, err := planFrom([]uint8{100},
		[]RIBRoute{ribOne(100, "10.1.0.0/24", "10.255.0.2")},
		[]Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewApplier(dp).Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	before := dp.writes

	if _, err := Reset(context.Background(), dp, []uint8{100, OverlayTable}); err == nil ||
		!strings.Contains(err.Error(), "overlay 保留表") {
		t.Fatalf("err = %v, want 拒绝 VPN 250", err)
	}
	if dp.writes != before {
		t.Errorf("被拒绝的清单产生了 %d 次写入（want 0）", dp.writes-before)
	}
	if len(dp.routes) != 1 || len(dp.labels) != 1 || len(dp.tables) != 1 {
		t.Errorf("VPN 100 的状态被动过: routes=%v labels=%v tables=%v", dp.routes, dp.labels, dp.tables)
	}
}
