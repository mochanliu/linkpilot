package vppapi

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveBFDState 是 test-metrics.sh 的状态探针：它什么都不配置，只读一次
// （或轮询到超时）多跳 BFD 会话的状态，用退出码回答"现在是不是期望的样子"。
//
// 为什么状态要借 Go 测试来读：这个 VPP 构建的 `show bfd udp session` 只打印
// 会话计数，状态字段只能从 API dump 拿（2026-10-24 实测）。脚本负责用 CLI
// 建/删会话，状态断言走 agent 将来走的同一条 API 路径 —— 脚本看到的和 agent
// 看到的因此不是两套判据。
//
// 环境变量：
//
//	SRWG_VPP_SOCK             必填，没有就 skip
//	SRWG_OVERLAY_LOCAL/PEER   默认 192.168.99.1 / 192.168.99.2
//	SRWG_BFD_WANT             up | down | absent，默认 up
//	SRWG_BFD_WAIT_MS          默认 0：只读一次；>0 时每 100ms 重读到超时
//
// 只认多跳会话：绑接口的会话不是遥测建的那条，把它当答案会让"断链检测"在
// 一个错误的前提下通过。
func TestLiveBFDState(t *testing.T) {
	sock := os.Getenv("SRWG_VPP_SOCK")
	if sock == "" {
		t.Skip("未设 SRWG_VPP_SOCK，跳过真机状态探针")
	}
	local := net.ParseIP(envOr("SRWG_OVERLAY_LOCAL", "192.168.99.1")).To4()
	peer := net.ParseIP(envOr("SRWG_OVERLAY_PEER", "192.168.99.2")).To4()
	if local == nil || peer == nil {
		t.Fatal("overlay 地址必须是 IPv4")
	}
	want := envOr("SRWG_BFD_WANT", "up")
	switch want {
	case "up", "down", "absent":
	default:
		t.Fatalf("SRWG_BFD_WANT=%q，只接受 up/down/absent", want)
	}
	waitMS := 0
	if v := os.Getenv("SRWG_BFD_WAIT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 15000 {
			t.Fatalf("SRWG_BFD_WAIT_MS=%q，需要 0..15000 的整数", v)
		}
		waitMS = n
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "live-bfd-state")
	if err != nil {
		t.Fatalf("连 VPP: %v", err)
	}

	state := func() string {
		bs, err := c.BFDSessions(ctx)
		if err != nil {
			t.Fatalf("读 BFD 会话: %v", err)
		}
		for _, b := range bs {
			if b.SwIfIndex == BFDMultihopSwIfIndex && b.LocalIP.Equal(local) && b.PeerIP.Equal(peer) {
				return BFDStateName(b.State)
			}
		}
		return "absent"
	}

	start := time.Now()
	deadline := start.Add(time.Duration(waitMS) * time.Millisecond)
	for {
		got := state()
		if got == want {
			t.Logf("bfd state=%s (waited %dms)", got, time.Since(start).Milliseconds())
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("bfd state=%s (waited %dms), 期望 %s",
				got, time.Since(start).Milliseconds(), want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
