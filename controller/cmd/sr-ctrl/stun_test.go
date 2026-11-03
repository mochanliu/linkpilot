// 控制器这一侧的三条：反射器起不起来、报给设备的是哪些地址、设备上报的观测里
// 新加的那两半存不存。
//
// 值得单测的理由和 G-1 是同一件事：出口口判定读的就是存下来的 ProbeLocal 与
// SymmetricKnown，存错一个字段，规划器拿到的是"量过了"而不是"没量到"。
package main

import (
	"context"
	"net"
	"testing"
	"time"

	"sdwan/controller/internal/stun"
	"sdwan/controller/internal/wire"
)

func TestParseStunAddrsRequiresExplicitHost(t *testing.T) {
	got, err := parseStunAddrs("127.0.0.1:3478, 127.0.0.1:3479")
	if err != nil {
		t.Fatalf("合法列表被拒：%v", err)
	}
	if len(got) != 2 || got[0] != "127.0.0.1:3478" || got[1] != "127.0.0.1:3479" {
		t.Errorf("解析结果 = %v", got)
	}

	// 每一项都必须写清楚主机地址：绑 0.0.0.0 之后"设备该拨哪个地址"就只剩猜，
	// 而控制器不猜客户的网络（AGENTS.md 第 2 条）。
	for _, bad := range []string{
		":3478",
		"0.0.0.0:3478",
		"3478",
		"10.0.0.5:notaport",
		" , ",
	} {
		if _, err := parseStunAddrs(bad); err == nil {
			t.Errorf("--stun=%q 竟然通过了：这一项会让反射器绑在一个说不清的位置", bad)
		}
	}
	// 非规范写法（多余空格已由 TrimSpace 处理，但 IPv6 必须带括号）要当场指出，
	// 不能悄悄绑到另一个地址上去。
	if _, err := parseStunAddrs("::1:3478"); err == nil {
		t.Error("IPv6 没写中括号却通过")
	}
}

// freeAddr 探一个当前没人用的 UDP 端口然后立刻放开：回滚那条测试要一个能重绑的地址。
func freeAddr(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位：%v", err)
	}
	addr := c.LocalAddr().String()
	_ = c.Close()
	return addr
}

func quiet(string, ...any) {}

// TestServeStunAnswersAndAdvertisesResolvedPorts 是"-stun 打开后设备真能探到"的最小证明。
// 广告出去的必须是绑定后拿到真实端口的地址：绑 ":0" 却广播 0 的话，设备拨的是一个
// 没人听的端口，页面上只会显示"探测失败"。
func TestServeStunAnswersAndAdvertisesResolvedPorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refs, err := serveStun(ctx, []string{"127.0.0.1:0", "127.0.0.1:0"}, quiet)
	if err != nil {
		t.Fatalf("serveStun：%v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("广告列表 = %v, want 两个地址", refs)
	}
	for _, r := range refs {
		host, port, err := net.SplitHostPort(r)
		if err != nil || host != "127.0.0.1" || port == "0" {
			t.Errorf("广告地址 %q 不是可拨的 ip:非零端口", r)
		}
	}

	p, err := stun.NewProber("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	v, err := p.Discover(pctx, refs, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("自家反射器不应答：%v", err)
	}
	if len(v.Mappings) != 2 || !v.SymmetricKnown || v.Symmetric {
		t.Errorf("判定 = %+v, want 两个观测、known、非对称", v)
	}
}

// TestServeStunBindsAllOrNothing 钉住"不许半开"：第二个口被占时整条服务必须失败
// 并放开第一个口。留一个只听一半的反射器在页面上和"这台盒子有防火墙"长得一模一样。
func TestServeStunBindsAllOrNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口：%v", err)
	}
	defer occupied.Close()

	first := freeAddr(t)
	if _, err := serveStun(ctx, []string{first, occupied.LocalAddr().String()}, quiet); err == nil {
		t.Fatal("端口冲突却被当成启动成功")
	}
	// 第一个口必须已经被放开：重绑成功就是回滚做了。
	again, err := net.ListenPacket("udp", first)
	if err != nil {
		t.Fatalf("失败后第一个口还占着（%v）：serveStun 没回滚", err)
	}
	_ = again.Close()
}

func TestAdvertiseStunCapsAtTheOffer(t *testing.T) {
	s := newTestServer()
	if got := s.advertiseStun(-1); got != nil {
		t.Errorf("没开反射器却广告了 %v：页面会把每台设备都写成探测失败", got)
	}
	s.reflectors = []string{"10.88.2.200:3478", "10.88.2.200:3479"}

	// 设备说它能探 0 个（没探测 socket）⇒ 一个字都不给。
	if got := s.advertiseStun(0); len(got) != 0 {
		t.Errorf("offer=0 却拿到 %v", got)
	}
	if got := s.advertiseStun(1); len(got) != 1 || got[0] != "10.88.2.200:3478" {
		t.Errorf("offer=1 得到 %v, want 只给第一个", got)
	}
	if got := s.advertiseStun(2); len(got) != 2 {
		t.Errorf("offer=2 得到 %v", got)
	}
	// 心跳那一路没有"名额"可参照（设备早已按自己的上限探测），给全表。
	if got := s.advertiseStun(-1); len(got) != 2 {
		t.Errorf("offer=-1（无名额可参照）得到 %v, want 全表", got)
	}
	// 返回的必须是副本：调用方 append 不许改到控制器自己的列表上。
	got := s.advertiseStun(-1)
	got[0] = "hacked"
	if s.reflectors[0] != "10.88.2.200:3478" {
		t.Errorf("广告列表被调用方改写了：reflectors[0] = %s", s.reflectors[0])
	}
}

// TestMappingStoresProbeLocalAndKnown 是 G-1 的地基：探测的本地源必须存下来，
// 而"没有判定"和"判定为非对称"必须是两个能区分开的存储状态。
func TestMappingStoresProbeLocalAndKnown(t *testing.T) {
	s := newTestServer()
	rec := addNode(s, "t", 1, []string{"10.88.2.11"}, "203.0.113.5:41000")

	mappingReq(t, s, "t", 1, wire.MappingReport{
		UDPMapping:   "203.0.113.5:41000",
		ProbeLocal:   "10.88.2.11:41000",
		LocalPort:    41000,
		Reflector:    "10.88.2.200:3478",
		SymmetricNAT: true, // 自称对称但没说量过 —— 不许当成测量结果
	})
	if rec.ProbeLocal != "10.88.2.11:41000" {
		t.Errorf("ProbeLocal 没存：rec=%q（出口口判定就没有输入可用）", rec.ProbeLocal)
	}
	if rec.SymmetricKnown || rec.Symmetric {
		t.Errorf("known=%v symmetric=%v, want false/false：没量过就不许留下任何结论",
			rec.SymmetricKnown, rec.Symmetric)
	}
	if rec.Reflector != "10.88.2.200:3478" || rec.LocalPort != 41000 {
		t.Errorf("反射器/本地端口没存：reflector=%q local_port=%d", rec.Reflector, rec.LocalPort)
	}
	if rec.MappingAt == 0 {
		t.Error("有观测却没记观测时间：规划器的老化闸门会立刻把它当过期")
	}

	// 两个观测都回来了 ⇒ 这次是测量，symmetric 必须照实存。
	// 观测时间以秒计，所以这里把它压回一个不可能的旧值来验"每轮都刷新"：
	// 规划器的老化闸门读的就是这个数，只刷一次的话它会误杀还在报的设备。
	rec.MappingAt = 1
	mappingReq(t, s, "t", 1, wire.MappingReport{
		UDPMapping:     "203.0.113.5:41000",
		ProbeLocal:     "10.88.2.11:41000",
		LocalPort:      41000,
		Reflector:      "10.88.2.200:3478",
		SymmetricNAT:   true,
		SymmetricKnown: true,
	})
	if !rec.SymmetricKnown || !rec.Symmetric {
		t.Errorf("known=%v symmetric=%v, want true/true：量到的对称必须挡住自动建隧道",
			rec.SymmetricKnown, rec.Symmetric)
	}
	if rec.MappingAt <= 1 {
		t.Errorf("同样的映射没刷新观测时间（留成 %d）：老化闸门会误杀一个还在报的设备", rec.MappingAt)
	}

	// 设备这一轮什么都没探到：映射清空，本地源也必须跟着清空，
	// 否则规划器会拿一个上一轮的出口口去配这一轮的隧道。
	mappingReq(t, s, "t", 1, wire.MappingReport{})
	if rec.ProbeLocal != "" || rec.Symmetric || rec.SymmetricKnown {
		t.Errorf("空报告之后仍留着旧观测：probe_local=%q known=%v symmetric=%v",
			rec.ProbeLocal, rec.SymmetricKnown, rec.Symmetric)
	}
}

// TestStunFlagIsOffByDefault 保证这次改动不会让一个照旧启动的控制器开始应答 UDP：
// 多开一个监听口是要写进部署说明的动作，不是默认状态。
func TestStunFlagIsOffByDefault(t *testing.T) {
	s := newTestServer()
	if len(s.reflectors) != 0 {
		t.Fatalf("新建 server 就自带反射器（%v）：默认必须是不开", s.reflectors)
	}
	if got := s.advertiseStun(2); got != nil {
		t.Errorf("没配置反射器却给了设备 %v", got)
	}
}
