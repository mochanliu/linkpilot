package vppapi

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// 真机验证 sw_interface_get_table 这条读回路径。
//
// 为什么必须有它：applyOverlay 靠"先读 local0 在哪张表、不对才写"来保证热更
// 不 churn，同时保证 VPP 重启后能把绑定补回去。读回的字节布局抄错不会报错，
// 只会让 agent 永远觉得"已经是 250"，于是一次 VPP 重启之后多跳 BFD 悄悄回到
// table 0（那份对端 /32 是 dpo-drop），会话恒 down —— 症状和没绑一模一样。
//
// 跑法（容器内，实验室 2026-11-01 起 bringup.sh 会把 local0 与隧道口都放进 250）：
//
//	SRWG_VPP_SOCK=/run/vpp/api.sock SRWG_WG_IF=wg0 SRWG_OVERLAY_TABLE=250 \
//	  go test ./internal/vppapi -run TestLiveInterfaceIPTable -v
//
// 只读，不改任何数据面状态。
func TestLiveInterfaceIPTable(t *testing.T) {
	sock := os.Getenv("SRWG_VPP_SOCK")
	if sock == "" {
		t.Skip("未设 SRWG_VPP_SOCK，跳过真机验证")
	}
	wgIf := envOr("SRWG_WG_IF", "wg0")
	want := uint32(250)
	if v := os.Getenv("SRWG_OVERLAY_TABLE"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			t.Fatalf("SRWG_OVERLAY_TABLE 不是数字: %v", err)
		}
		want = uint32(n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "live-interface-table")
	if err != nil {
		t.Fatalf("连 VPP: %v", err)
	}

	ifs, err := c.DumpInterfaces(ctx)
	if err != nil {
		t.Fatalf("读接口表: %v", err)
	}
	var swIf uint32
	for _, i := range ifs {
		if i.Name == wgIf {
			swIf = i.SWIfIndex
		}
	}
	if swIf == 0 {
		t.Fatalf("接口表里没有 %s（先跑 bringup.sh）", wgIf)
	}

	got, err := c.InterfaceIPTable(ctx, swIf)
	if err != nil {
		t.Fatalf("读 %s(%d) 的表号: %v", wgIf, swIf, err)
	}
	if got != want {
		t.Errorf("%s(%d) 在表 %d，期望 %d —— 隧道口的 overlay 配置没落地，或读回偏移错了",
			wgIf, swIf, got, want)
	}

	// local0 是多跳 BFD 的查表出口：它必须和隧道口在同一张表，否则会话查的是
	// table 0（客户 VPN 0，对端 /32 在那里是 dpo-drop）。
	if got0, err := c.InterfaceIPTable(ctx, 0); err != nil {
		t.Errorf("读 local0 的表号: %v", err)
	} else if got0 != want {
		t.Errorf("local0 在表 %d，期望 %d（多跳 BFD 会查错表）", got0, want)
	}
}
