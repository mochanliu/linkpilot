package vppapi

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// 真机验证探针 + core BFD 的整条 Go 客户端路径。
//
// 假端只能证明"我们自己抄的字节布局前后一致"，证明不了它和真 VPP 的注册表
// 一致。这个测试跑在实验室节点上（wg0 已由 setup-node.sh 配好），验证四件事：
//
//  1. CRC 门禁认得 sr_wg_probe_*（插件消息）与 bfd_udp_*（core 消息）；
//  2. packed 结构的偏移正确：Probes 读回的 sw_if_index/间隔/计数器不是垃圾；
//  3. BFD 会话能在真 VPP 上建起来（多跳形式，sw_if_index=~0），dump 读回
//     的定时器/状态字段对得上；
//  4. 探针真的在发、对端真的在折返：tx 与 rx 同步增长，RTT 有具体值。
//
// BFD 不能绑 wg 接口：实测绑接口的单端会话会把 overlay 路由的转发桶换成
// dpo-drop，隧道直接不通（详见 telemetry.go 文件头）。所以这里建的是多跳会话。
//
// 对端也配了会话时（test-metrics.sh 的场景），用 SRWG_BFD_EXPECT_UP=1 要求
// 状态收敛到 up；默认单端只断言状态是认识的枚举值。
//
// 跑法（容器内）：
//
//	SRWG_VPP_SOCK=/run/vpp/api.sock SRWG_WG_IF=wg0 \
//	SRWG_OVERLAY_LOCAL=192.168.99.1 SRWG_OVERLAY_PEER=192.168.99.2 \
//	  go test ./internal/vppapi -run TestLiveTelemetry -v
//
// 结束时把会话删干净：这台节点与别的会话共用。
func TestLiveTelemetryAgainstVPP(t *testing.T) {
	sock := os.Getenv("SRWG_VPP_SOCK")
	if sock == "" {
		t.Skip("未设 SRWG_VPP_SOCK，跳过真机验证")
	}
	wgIf := envOr("SRWG_WG_IF", "wg0")
	local := net.ParseIP(envOr("SRWG_OVERLAY_LOCAL", "192.168.99.1")).To4()
	peer := net.ParseIP(envOr("SRWG_OVERLAY_PEER", "192.168.99.2")).To4()
	if local == nil || peer == nil {
		t.Fatal("overlay 地址必须是 IPv4")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "live-telemetry")
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
		t.Fatalf("接口表里没有 %s（先跑 test-e2e.sh --up-only）", wgIf)
	}

	// bfd_udp_del 对不存在的会话会回错误；测试可能死在建会话之前，所以先 dump。
	// 只清多跳会话：绑接口的会话是别人手配的，不该由这个测试顺手删掉。
	delBFD := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		bs, err := c.BFDSessions(cctx)
		if err != nil {
			t.Errorf("清理时读 BFD: %v", err)
			return
		}
		for _, b := range bs {
			if b.SwIfIndex != BFDMultihopSwIfIndex || !b.PeerIP.Equal(peer) || !b.LocalIP.Equal(local) {
				continue
			}
			if err := c.DelBFDSession(cctx, BFDSessionParams{
				LocalIP: b.LocalIP, PeerIP: b.PeerIP,
			}); err != nil {
				t.Errorf("清理 BFD: %v", err)
			}
		}
	}
	t.Cleanup(func() {
		delBFD()
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		if err := c.DelProbe(cctx, swIf); err != nil {
			t.Errorf("清理探针: %v", err)
		}
	})

	if err := c.SetProbe(ctx, ProbeParams{
		SwIfIndex: swIf, PeerIP: peer, LocalIP: local,
		IntervalMS: 300, TimeoutMS: 900,
	}); err != nil {
		t.Fatalf("建探针: %v", err)
	}
	// SendProbe 不依赖定时器，先点一发确认 API 路径。
	if err := c.SendProbe(ctx, swIf); err != nil {
		t.Fatalf("手动发包: %v", err)
	}
	// BFD 是**双向**的：对端也要有会话才收敛。默认只配本端，所以只断言"会话在、
	// 状态是认识的枚举值"；SRWG_BFD_EXPECT_UP=1（对端也配了会话）时要求 up。
	// SRWG_SKIP_BFD 用来单独观察探针，不受 BFD 流量干扰。
	withBFD := os.Getenv("SRWG_SKIP_BFD") == ""
	if withBFD {
		// 上一次跑到一半留下的会话会让 bfd_udp_add 回 "session exists"。
		delBFD()
		if err := c.AddBFDSession(ctx, BFDSessionParams{
			LocalIP: local, PeerIP: peer,
			DesiredMinTxUS: 50000, RequiredMinRxUS: 50000, DetectMult: 3,
		}); err != nil {
			t.Fatalf("建 BFD 会话: %v", err)
		}
	}

	var (
		lastProbe ProbeStats
		lastBFD   BFDSession
	)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ps, err := c.Probes(ctx)
		if err != nil {
			t.Fatalf("读探针: %v", err)
		}
		bs, err := c.BFDSessions(ctx)
		if err != nil {
			t.Fatalf("读 BFD: %v", err)
		}
		for _, p := range ps {
			if p.SwIfIndex == swIf {
				lastProbe = p
			}
		}
		for _, b := range bs {
			if b.SwIfIndex == BFDMultihopSwIfIndex && b.LocalIP.Equal(local) && b.PeerIP.Equal(peer) {
				lastBFD = b
			}
		}
		if lastProbe.Tx >= 10 && lastProbe.Rx > 0 && (!withBFD || lastBFD.State != 0) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// 布局校验：这些字段只要有一个抄错，读到的是别的字段的字节。
	if lastProbe.SwIfIndex != swIf {
		t.Fatalf("探针读回 sw_if_index=%d，期望 %d", lastProbe.SwIfIndex, swIf)
	}
	if !lastProbe.PeerIP.Equal(peer) || !lastProbe.LocalIP.Equal(local) {
		t.Errorf("探针地址读回 %v -> %v", lastProbe.LocalIP, lastProbe.PeerIP)
	}
	if lastProbe.IntervalMS != 300 || lastProbe.TimeoutMS != 900 || !lastProbe.Enabled {
		t.Errorf("探针参数读回 interval=%d timeout=%d enabled=%v",
			lastProbe.IntervalMS, lastProbe.TimeoutMS, lastProbe.Enabled)
	}
	if lastProbe.Tx <= 1 {
		t.Errorf("探针没在发: tx=%d", lastProbe.Tx)
	}
	if lastProbe.Rx == 0 {
		t.Errorf("对端没折返: tx=%d rx=%d", lastProbe.Tx, lastProbe.Rx)
	}
	if lastProbe.LastRTTus == 0 || lastProbe.LastRTTus > 500000 {
		t.Errorf("RTT 不合理: last=%dus", lastProbe.LastRTTus)
	}
	// 四项指标要互相自洽：last 是最近一次，必然落在 [min,max]；avg 也是同一个
	// 会话的均值，必然落在同一区间。任一条破了都说明读回的字段错位，而不是"网络差"。
	if lastProbe.MinRTTus > lastProbe.AvgRTTus || lastProbe.AvgRTTus > lastProbe.MaxRTTus {
		t.Errorf("RTT 聚合不自洽: min=%dus avg=%dus max=%dus",
			lastProbe.MinRTTus, lastProbe.AvgRTTus, lastProbe.MaxRTTus)
	}
	if lastProbe.LastRTTus < lastProbe.MinRTTus || lastProbe.LastRTTus > lastProbe.MaxRTTus {
		t.Errorf("last RTT 落在 [min,max] 之外: last=%dus min=%dus max=%dus",
			lastProbe.LastRTTus, lastProbe.MinRTTus, lastProbe.MaxRTTus)
	}
	// 健康直连链路上折返率应接近 1。留 80% 的余量给启动阶段：第一发探针
	// 可能赶在对端插件就绪之前，那是真实丢包，不是缺陷。
	if lastProbe.Rx*10 < lastProbe.Tx*8 {
		t.Errorf("折返率过低: tx=%d rx=%d", lastProbe.Tx, lastProbe.Rx)
	}
	if lastProbe.LossPermille > 200 {
		t.Errorf("丢包率过高: %d‰ (tx=%d rx=%d)", lastProbe.LossPermille, lastProbe.Tx, lastProbe.Rx)
	}
	if lastProbe.JitterUs > 50000 {
		t.Errorf("抖动异常: %dus", lastProbe.JitterUs)
	}

	if withBFD {
		if lastBFD.SwIfIndex != BFDMultihopSwIfIndex {
			t.Fatalf("BFD 读回 sw_if_index=%d，期望多跳 ~0（绑接口会打断 overlay 转发）", lastBFD.SwIfIndex)
		}
		if !lastBFD.LocalIP.Equal(local) || !lastBFD.PeerIP.Equal(peer) {
			t.Errorf("BFD 地址读回 %v -> %v", lastBFD.LocalIP, lastBFD.PeerIP)
		}
		bfdName := BFDStateName(lastBFD.State)
		switch {
		case os.Getenv("SRWG_BFD_EXPECT_UP") != "":
			if bfdName != "up" {
				t.Errorf("BFD 状态 = %s，期望 up（对端也要配会话）", bfdName)
			}
		case bfdName != "down" && bfdName != "init" && bfdName != "up":
			t.Errorf("单端 BFD 会话状态 = %s，期望 down/init/up", bfdName)
		}
		if lastBFD.RequiredMinRxUS != 50000 || lastBFD.DesiredMinTxUS != 50000 || lastBFD.DetectMult != 3 {
			t.Errorf("BFD 定时器读回 rx=%d tx=%d mult=%d",
				lastBFD.RequiredMinRxUS, lastBFD.DesiredMinTxUS, lastBFD.DetectMult)
		}
		t.Logf("bfd: state=%s rx=%d tx=%d mult=%d",
			bfdName, lastBFD.RequiredMinRxUS, lastBFD.DesiredMinTxUS, lastBFD.DetectMult)
	}
	t.Logf("probe: tx=%d rx=%d rtt=%.3fms avg=%.3fms jitter=%.3fms loss=%d‰",
		lastProbe.Tx, lastProbe.Rx,
		float64(lastProbe.LastRTTus)/1000,
		float64(lastProbe.AvgRTTus)/1000,
		float64(lastProbe.JitterUs)/1000,
		lastProbe.LossPermille)

	// 删除后必须从 dump 里消失（不是只回 retval=0）。
	if err := c.DelProbe(ctx, swIf); err != nil {
		t.Fatalf("删探针: %v", err)
	}
	ps, err := c.Probes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.SwIfIndex == swIf {
			t.Fatalf("删掉的探针还在 dump 里: %+v", p)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
