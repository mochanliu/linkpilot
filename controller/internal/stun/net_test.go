package stun

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// 这一批测试只覆盖"要发收报文"的那三块：Response 编码、IsBindingRequest 闸门、
// reflector 与 prober 的配合。编解码的纯函数部分在 stun_test.go 里。

func serveReflector(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起反射器失败：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	r := &Reflector{}
	go func() { done <- r.Serve(ctx, conn) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("反射器没在 ctx 取消后退出")
		}
	})
	return conn.LocalAddr().String()
}

// serveLiar 起一个"说谎的"应答器：不管真实源是谁，都按 given 的 ip:port 回答。
// 对称 NAT 在家里造不出来，只能靠它把判定规则钉住。
func serveLiar(t *testing.T, ip string, port int) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假应答器失败：%v", err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if !IsBindingRequest(buf[:n]) {
				continue
			}
			resp, err := Response(txIDOf(buf), ip, port)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(resp, from)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return conn.LocalAddr().String()
}

// serveForeignTxid 永远按一个固定且不属于本次探测的 txid 回答：
// 客户端必须把它当作"别人家的事务"，不能拿它凑出一个映射。
func serveForeignTxid(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起应答器失败：%v", err)
	}
	foreign := make([]byte, 12)
	for i := range foreign {
		foreign[i] = 0x5A
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if !IsBindingRequest(buf[:n]) {
				continue
			}
			resp, err := Response(foreign, "203.0.113.66", 40000)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(resp, from)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return conn.LocalAddr().String()
}

func serveSilent(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起静默反射器：%v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.LocalAddr().String()
}

func TestResponseRoundTrip(t *testing.T) {
	tx := testTxid()
	resp, err := Response(tx, "203.0.113.9", 41247)
	if err != nil {
		t.Fatalf("Response：%v", err)
	}
	// 20 头 + 4 属性头 + 8 属性值：短了会在编码时越界，长了客户端解析不到。
	if len(resp) != 32 {
		t.Errorf("报文长 = %d, want 32", len(resp))
	}
	if IsBindingRequest(resp) {
		t.Error("应答被当作请求 —— 反射器会回答自己")
	}
	ip, port, ok := MappedAddress(resp, tx)
	if !ok || ip != "203.0.113.9" || port != 41247 {
		t.Errorf("回读 = %s:%d ok=%v, want 203.0.113.9:41247 ok=true", ip, port, ok)
	}
}

func TestResponseRejects(t *testing.T) {
	tx := testTxid()
	if _, err := Response(tx[:8], "203.0.113.9", 1); err == nil {
		t.Error("txid 长度不足却编成功了")
	}
	if _, err := Response(tx, "::1", 1); err == nil {
		t.Error("IPv6 观测不该被编成 IPv4 报文")
	}
	if _, err := Response(tx, "203.0.113.9", 70000); err == nil {
		t.Error("端口越界却没报错")
	}
}

func TestIsBindingRequestGate(t *testing.T) {
	tx := testTxid()
	if !IsBindingRequest(BindingRequest(tx)) {
		t.Error("真的 Binding Request 被闸门挡了 —— 反射器会一个都不答")
	}
	if IsBindingRequest([]byte{0x00, 0x01, 0x00}) {
		t.Error("短于 20 字节的垃圾被当作请求")
	}
	other := BindingRequest(tx)
	other[1] = 0x11 // 不再是 Binding Request 的类型号
	if IsBindingRequest(other) {
		t.Error("别的类型被当作请求")
	}
	cookie := BindingRequest(tx)
	cookie[4] ^= 0xFF
	if IsBindingRequest(cookie) {
		t.Error("magic cookie 不符也被当作请求")
	}
	// 声称的长度比报文还长：这是截断/伪造，回它只会放大垃圾。
	lying := make([]byte, 24)
	copy(lying, BindingRequest(tx))
	lying[2], lying[3] = 0x00, 0x40
	if IsBindingRequest(lying) {
		t.Error("声明长度超出报文却没被拒")
	}
}

// TestProbeLoopback 钉住 Mapping 的两半：反射器观测到的外层地址、和本地源。
// 少了后半，出口口判定就没有任何可核对的东西。
// 这里绑 ":0"（源地址由路由决定），走的是 sourceFor 每次现查那条路。
func TestProbeLoopback(t *testing.T) {
	ref := serveReflector(t)
	p, err := NewProber(":0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := p.Probe(ctx, ref, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Probe：%v", err)
	}
	if m.Addr != "127.0.0.1" {
		t.Errorf("观测地址 = %s, want 127.0.0.1", m.Addr)
	}
	if m.Port != p.LocalPort() {
		t.Errorf("观测端口 = %d, want 本地端口 %d（环回没有 NAT，两者必须一致）", m.Port, p.LocalPort())
	}
	// Local 必须是这条 socket 真正用的源地址，不能是 0.0.0.0：
	// 拿 0.0.0.0 去对接口清单，要么永远对不上，要么全都对得上。
	host, portStr, err2 := net.SplitHostPort(m.Local)
	if err2 != nil {
		t.Fatalf("Local 不是 ip:port：%q", m.Local)
	}
	if host != "127.0.0.1" {
		t.Errorf("Local 的源地址 = %s, want 127.0.0.1（未指定地址不许上报）", host)
	}
	if p2, _ := strconv.Atoi(portStr); p2 != p.LocalPort() {
		t.Errorf("Local 的端口 = %d, want %d", p2, p.LocalPort())
	}
	if m.Reflector != ref {
		t.Errorf("Reflector = %s, want %s", m.Reflector, ref)
	}
}

// TestProberExplicitBind 走另一条路：绑死源地址时不许再由路由另选一个，
// 复测"某个具体口的探测"全靠这条。
func TestProberExplicitBind(t *testing.T) {
	ref := serveReflector(t)
	p, err := NewProber("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := p.Probe(ctx, ref, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Probe：%v", err)
	}
	host, _, err := net.SplitHostPort(m.Local)
	if err != nil || host != "127.0.0.1" {
		t.Errorf("显式绑定下 Local = %q, want 源地址 127.0.0.1", m.Local)
	}
}

func TestProbeIgnoresForeignTxid(t *testing.T) {
	ref := serveForeignTxid(t)
	p, err := NewProber("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := p.Probe(ctx, ref, 2, 200*time.Millisecond); err == nil {
		t.Error("事务号对不上的应答被当成了映射")
	}
}

// TestDiscoverSymmetricRule 是对称判定的全部依据：同一个本地 socket，
// 两个目的地给了不同的 (ip:port) ⇒ 对称。任何"别人拨这个映射"的隧道都不成立。
func TestDiscoverSymmetricRule(t *testing.T) {
	a := serveLiar(t, "203.0.113.10", 41001)
	b := serveLiar(t, "203.0.113.10", 41002)
	p, err := NewProber("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := p.Discover(ctx, []string{a, b}, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(v.Mappings) != 2 {
		t.Fatalf("映射数 = %d, want 2", len(v.Mappings))
	}
	if !v.SymmetricKnown {
		t.Fatal("两个目的地都答了，却说没有判定可给")
	}
	if !v.Symmetric {
		t.Fatal("观测端口不同却判成非对称 —— 这正是 NAT 后面隧道建不起来的形态")
	}

	// 只有一处答：判定必须退回"不知道"，控制器不许把它当成非对称。
	silent := serveSilent(t)
	v2, err := p.Discover(ctx, []string{a, silent}, 1, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("单目的地 Discover：%v", err)
	}
	if v2.SymmetricKnown || v2.Symmetric {
		t.Errorf("只有一个反射器应答：known=%v symmetric=%v, want 都为 false", v2.SymmetricKnown, v2.Symmetric)
	}
	if len(v2.Failed) != 1 {
		t.Errorf("Failed = %v, want 一条没答的", v2.Failed)
	}

	// 全静默：这不是"没有映射"而是"这一轮没量到"，必须以错误返回。
	if _, err := p.Discover(ctx, []string{silent}, 1, 100*time.Millisecond); err == nil {
		t.Error("所有反射器都没答却返回了 nil error")
	}
}

// TestDiscoverOrderStable 钉住返回形状：控制器拿 Mappings[0] 建隧道，
// 顺序漂了就把"对端该拨哪儿"换成了另一个观测点的结论。
func TestDiscoverOrderStable(t *testing.T) {
	a := serveReflector(t)
	b := serveReflector(t)
	p, err := NewProber("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := p.Discover(ctx, []string{a, b}, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(v.Mappings) != 2 || v.Mappings[0].Reflector != a || v.Mappings[1].Reflector != b {
		t.Errorf("映射顺序不符：%+v", v.Mappings)
	}
	if v.Symmetric {
		t.Error("环回上两个反射器看到的是同一个映射，判成对称说明规则错")
	}
	// 真反射器报的是内核给的源端口，本地端口只有一个：两处都必须等于它。
	for _, m := range v.Mappings {
		if m.Port != p.LocalPort() {
			t.Errorf("%s 观测到的端口 = %d, want %d", m.Reflector, m.Port, p.LocalPort())
		}
		if m.Local != v.Mappings[0].Local {
			t.Errorf("两轮 Local 不一致：%q vs %q", m.Local, v.Mappings[0].Local)
		}
	}
}

func TestProberReuseKeepsPort(t *testing.T) {
	ref := serveReflector(t)
	p, err := NewProber("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewProber：%v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := p.Probe(ctx, ref, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("第一轮：%v", err)
	}
	second, err := p.Probe(ctx, ref, 2, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("第二轮：%v", err)
	}
	// 每轮换端口的话，"同一 socket 打两个目的地"就不成立，对称判定只剩猜。
	if first.Port != second.Port || first.Local != second.Local {
		t.Errorf("两轮之间源变了：第一轮 %s/%d，第二轮 %s/%d", first.Local, first.Port, second.Local, second.Port)
	}
}

// TestReflectorIgnoresNonRequests 保证反射器不是回声服务：乱七八糟的东西
// 只会变成一个看得见的计数，不会变成应答。
func TestReflectorIgnoresNonRequests(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Reflector{}
	go func() { _ = r.Serve(ctx, conn) }()

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("客户端监听：%v", err)
	}
	defer client.Close()
	raddr, err := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("resolve：%v", err)
	}
	buf := make([]byte, 2048)

	if _, err := client.WriteTo([]byte("hello"), raddr); err != nil {
		t.Fatalf("写垃圾：%v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	if n, _, err := client.ReadFrom(buf); err == nil {
		t.Errorf("垃圾换来了 %d 字节应答", n)
	}

	tx := testTxid()
	if _, err := client.WriteTo(BindingRequest(tx), raddr); err != nil {
		t.Fatalf("写请求：%v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatalf("正常请求没等到应答：%v", err)
	}
	if _, _, ok := MappedAddress(buf[:n], tx); !ok {
		t.Fatal("应答解析不出来")
	}
	// 观测到的就是客户端这条 socket 自己的源：反射器不许看报文里的任何自称。
	if p2, _, _ := net.SplitHostPort(client.LocalAddr().String()); p2 != "127.0.0.1" {
		t.Errorf("客户端源地址 = %s, want 127.0.0.1", p2)
	}
	req, rep, mal, v6 := r.Stats()
	if req != 1 || rep != 1 {
		t.Errorf("请求=%d 应答=%d, want 1/1（垃圾不该计入请求）", req, rep)
	}
	if mal != 1 {
		t.Errorf("无法解析=%d, want 1（那句 hello 必须看得见）", mal)
	}
	if v6 != 0 {
		t.Errorf("非 IPv4 源=%d, want 0", v6)
	}
}
