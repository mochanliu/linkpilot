package agent

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"sdwan/controller/internal/stun"
	"sdwan/controller/internal/wire"
)

// testReflector 是测试自己的应答器：既回答又数请求。产品里那台反射器没有把计数
// 暴露给进程外，而这几条测试要的恰恰是"这一轮到底发了几个包"。
type testReflector struct {
	addr     string
	requests atomic.Int64
	conn     net.PacketConn
}

func startReflector(t *testing.T) *testReflector {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起反射器：%v", err)
	}
	r := &testReflector{addr: conn.LocalAddr().String(), conn: conn}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if !stun.IsBindingRequest(buf[:n]) {
				continue
			}
			r.requests.Add(1)
			tx := make([]byte, 12)
			copy(tx, buf[8:20])
			udp, ok := from.(*net.UDPAddr)
			if !ok || udp.IP.To4() == nil {
				continue
			}
			resp, err := stun.Response(tx, udp.IP.To4().String(), udp.Port)
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(resp, from)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return r
}

// kill 让这台反射器"挂了"：端口还在编号但没人答，等价于反射器重启或链路黑洞。
func (r *testReflector) kill() { _ = r.conn.Close() }

func fakeClock() (func() time.Time, func(time.Duration)) {
	base := time.Unix(1_700_000_000, 0)
	return func() time.Time { return base }, func(d time.Duration) { base = base.Add(d) }
}

func newTestSync(t *testing.T, max int) (*StunSync, func(time.Duration)) {
	t.Helper()
	now, advance := fakeClock()
	s := NewStunSync("127.0.0.1:0", max, nil)
	s.now = now
	t.Cleanup(func() { _ = s.Close() })
	return s, advance
}

// TestStunApplyCarriesBothHalves 是这次增量的判据：报告里必须同时有
// "别人该拨哪儿"（UDPMapping）和"我从哪儿出去"（ProbeLocal）。少了后半，
// NAT 后面的隧道永远定不下出口口 —— 那正是 G-1 的根。
func TestStunApplyCarriesBothHalves(t *testing.T) {
	ref := startReflector(t)
	s, _ := newTestSync(t, 2)
	s.SetReflectors([]string{ref.addr})
	s.Sync(context.Background())

	var rep wire.MappingReport
	s.Apply(&rep)
	if rep.UDPMapping == "" || rep.ProbeLocal == "" {
		t.Fatalf("报告空了：mapping=%q probe_local=%q", rep.UDPMapping, rep.ProbeLocal)
	}
	if rep.Reflector != ref.addr {
		t.Errorf("Reflector = %q, want %q", rep.Reflector, ref.addr)
	}
	host, portStr, err := net.SplitHostPort(rep.ProbeLocal)
	if err != nil {
		t.Fatalf("ProbeLocal 不是 ip:port：%q", rep.ProbeLocal)
	}
	if host != "127.0.0.1" {
		t.Errorf("ProbeLocal 源地址 = %s, want 127.0.0.1（未指定地址不许上报）", host)
	}
	if p, _ := strconv.Atoi(portStr); p != rep.LocalPort {
		t.Errorf("ProbeLocal 端口 = %d 而 LocalPort = %d：同一个 socket 不该有两个说法", p, rep.LocalPort)
	}
	if rep.SymmetricKnown {
		t.Error("只有一个反射器答却报 SymmetricKnown=true")
	}

	// 第二个反射器进来：同一 socket、同一映射 ⇒ 判定为"已知且非对称"。
	ref2 := startReflector(t)
	s.SetReflectors([]string{ref.addr, ref2.addr})
	s.Sync(context.Background())
	var rep2 wire.MappingReport
	s.Apply(&rep2)
	if !rep2.SymmetricKnown || rep2.SymmetricNAT {
		t.Errorf("known=%v symmetric=%v, want true/false（环回上两处观测必然一致）",
			rep2.SymmetricKnown, rep2.SymmetricNAT)
	}
}

// TestStunCadence 钉住节奏：一轮量完不许每个 tick 重来（上一版的缺陷是 dirty
// 从不清零，20s 间隔形同虚设），而控制器换了名单必须当轮就重来。
func TestStunCadence(t *testing.T) {
	ref := startReflector(t)
	s, advance := newTestSync(t, 2)
	s.SetReflectors([]string{ref.addr})
	s.Sync(context.Background())
	if got := ref.requests.Load(); got != 1 {
		t.Fatalf("第一轮请求数 = %d, want 1", got)
	}

	for i := 0; i < 5; i++ {
		s.Sync(context.Background())
	}
	if got := ref.requests.Load(); got != 1 {
		t.Errorf("间隔未到就重探了：请求数 = %d, want 1", got)
	}

	advance(stunProbeGap)
	s.Sync(context.Background())
	if got := ref.requests.Load(); got != 2 {
		t.Errorf("间隔到了却没重探：请求数 = %d, want 2", got)
	}

	ref2 := startReflector(t)
	s.SetReflectors([]string{ref2.addr})
	s.Sync(context.Background())
	if got := ref2.requests.Load(); got != 1 {
		t.Errorf("换了名单没立刻探测：新反射器请求数 = %d, want 1", got)
	}
}

// TestStunCapsAtOfferedCount 保证 -stun-reflectors 是这台盒子的上限而不是建议值：
// 一个只声明能探 1 个的设备被塞两个地址，也只许发一个包。
func TestStunCapsAtOfferedCount(t *testing.T) {
	a := startReflector(t)
	b := startReflector(t)
	s, _ := newTestSync(t, 1)
	s.SetReflectors([]string{a.addr, b.addr})
	if got := s.Reflectors(); len(got) != 1 || got[0] != a.addr {
		t.Fatalf("Reflectors = %v, want 只留第一个", got)
	}
	s.Sync(context.Background())
	if b.requests.Load() != 0 {
		t.Errorf("越界的反射器也被探测了（请求数 %d）", b.requests.Load())
	}

	// 空条目不许占名额：控制器万一给出 ["", addr]，仍然要能探测那一个。
	s.SetReflectors([]string{"", a.addr})
	if got := s.Reflectors(); len(got) != 1 || got[0] != a.addr {
		t.Errorf("空条目占了名额：%v", got)
	}
}

// TestStunFailedRoundThenAgeOut 定住两件事：一次探不到不许立刻把已有结论抹掉
// （反射器重启、瞬时丢包都算这种），但结论老了以后必须不再上报 —— 让设备自己
// 沉默，控制器那一行才会老化掉，而不是把隧道建在一个已经不存在的 NAT 端口上。
func TestStunFailedRoundThenAgeOut(t *testing.T) {
	ref := startReflector(t)
	s, advance := newTestSync(t, 2)
	s.SetReflectors([]string{ref.addr})
	s.Sync(context.Background())
	var first wire.MappingReport
	s.Apply(&first)
	if first.UDPMapping == "" {
		t.Fatal("第一轮就没量到")
	}

	ref.kill()
	advance(stunProbeGap)
	syncCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(len(s.Reflectors()))*time.Duration(stunTries)*stunGap+2*time.Second)
	defer cancel()
	s.Sync(syncCtx)
	if snap := s.Snapshot(); snap.Err == "" {
		t.Error("探测失败却没留下任何原因：状态页会显示一切正常")
	}
	var kept wire.MappingReport
	s.Apply(&kept)
	if kept.UDPMapping != first.UDPMapping {
		t.Errorf("一轮失败就动了结论：got %q, want 沿用上一轮 %q", kept.UDPMapping, first.UDPMapping)
	}

	// 老化：过 stunMaxAge 之后 Apply 必须一个字都不写，此后设备对映射保持沉默。
	advance(stunMaxAge)
	var aged wire.MappingReport
	s.Apply(&aged)
	if aged.UDPMapping != "" || aged.ProbeLocal != "" || aged.Reflector != "" || aged.SymmetricKnown {
		t.Errorf("结论过期还在上报：%+v", aged)
	}
}

// TestStunSnapshotIsHonest 是控制台"未上报"的来源（AGENTS.md 第 2 条）：
// 没探过就没有数，不许填 0 或占位。
func TestStunSnapshotIsHonest(t *testing.T) {
	s, _ := newTestSync(t, 2)
	snap := s.Snapshot()
	if snap.Mapping != "" || snap.ProbeLocal != "" || snap.Symmetric != nil || snap.SymmetricKnown {
		t.Errorf("什么都没测却有了结果：%+v", snap)
	}
	var rep wire.MappingReport
	s.Apply(&rep)
	if rep.UDPMapping != "" || rep.ProbeLocal != "" || rep.LocalPort != 0 {
		t.Errorf("还没探测就往报告里写数：%+v", rep)
	}

	// 名单为空：不许启动探测，也不许留下"探测失败"这种噪声。
	s.Sync(context.Background())
	if got := s.Snapshot(); got.Err != "" {
		t.Errorf("没给反射器却记了错：%q（应当安静地什么都没测）", got.Err)
	}

	var nilSync *StunSync
	nilSync.SetReflectors([]string{"127.0.0.1:1"})
	nilSync.Sync(context.Background())
	if len(nilSync.Reflectors()) != 0 || nilSync.Snapshot().SymmetricKnown {
		t.Error("nil 接收者不该有结果")
	}
	var nilRep wire.MappingReport
	nilSync.Apply(&nilRep)
	if nilRep.UDPMapping != "" {
		t.Error("nil 接收者写了报告")
	}
	if err := nilSync.Close(); err != nil {
		t.Errorf("nil Close：%v", err)
	}
}

// TestStunLoopProbes 只看 Loop 会不会真的把探测驱动起来，以及预算够不够一轮：
// 预算小的话，黑洞反射器会让每轮都在半途被自己取消，永远量不到。
func TestStunLoopProbes(t *testing.T) {
	ref := startReflector(t)
	s, _ := newTestSync(t, 2)
	s.SetReflectors([]string{ref.addr})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Loop(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for ref.requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ref.requests.Load() == 0 {
		t.Error("Loop 跑起来了却没探测")
	}
	if snap := s.Snapshot(); snap.Mapping == "" {
		t.Errorf("Loop 探测后没有可用结论：%+v", snap)
	}
}
