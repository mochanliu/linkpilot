package vppapi

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

// 遥测这层最容易错的地方不是逻辑，而是**字节布局**：probe 是本插件自己的 .api，
// BFD 是 core 的，两份 packed 结构都没有对齐填充。这些测试把实测布局钉住，
// 插件/VPP 的 .api 一动，先在这里炸，而不是等 agent 报出一堆 0 指标。

func TestSetProbeEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	err := c.SetProbe(ctx, ProbeParams{
		SwIfIndex:  7,
		PeerIP:     net.IPv4(192, 168, 99, 2),
		LocalIP:    net.IPv4(192, 168, 99, 1),
		IntervalMS: 300,
		TimeoutMS:  900,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "sr_wg_probe_set" {
		t.Fatalf("请求名 = %s", r.name)
	}
	// is_add(1) + sw_if_index(4) + peer(4) + local(4) + interval(4) + timeout(4)
	if len(r.body) != 21 {
		t.Fatalf("体长 = %d，期望 21", len(r.body))
	}
	if r.body[0] != 1 {
		t.Errorf("is_add = %d，期望 1", r.body[0])
	}
	if got := binary.BigEndian.Uint32(r.body[1:5]); got != 7 {
		t.Errorf("sw_if_index = %d，期望 7", got)
	}
	if got := net.IP(r.body[5:9]); !got.Equal(net.IPv4(192, 168, 99, 2)) {
		t.Errorf("peer_ip = %v", got)
	}
	if got := net.IP(r.body[9:13]); !got.Equal(net.IPv4(192, 168, 99, 1)) {
		t.Errorf("local_ip = %v", got)
	}
	if got := binary.BigEndian.Uint32(r.body[13:17]); got != 300 {
		t.Errorf("interval_ms = %d", got)
	}
	if got := binary.BigEndian.Uint32(r.body[17:21]); got != 900 {
		t.Errorf("timeout_ms = %d", got)
	}
}

// 删会话时插件不看地址，但 peer_ip/local_ip 是定长字段 —— 少写一个字节，
// VPP 就把整帧按 Truncated message 丢掉，调用方看到的是成功之后会话还在。
func TestDelProbeEncodingKeepsFullLength(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	if err := c.DelProbe(ctx, 9); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if len(r.body) != 21 {
		t.Fatalf("体长 = %d，期望 21（地址字段必须补满 4 字节 0）", len(r.body))
	}
	if r.body[0] != 0 {
		t.Errorf("is_add = %d，期望 0", r.body[0])
	}
	if got := binary.BigEndian.Uint32(r.body[1:5]); got != 9 {
		t.Errorf("sw_if_index = %d", got)
	}
	for i := 5; i < 21; i++ {
		if r.body[i] != 0 {
			t.Fatalf("删会话的地址/间隔字段必须全 0，offset %d = %d", i, r.body[i])
		}
	}
}

func TestSetProbeRejectsNonIPv4BeforeSend(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	err := c.SetProbe(ctx, ProbeParams{SwIfIndex: 7, PeerIP: net.ParseIP("2001:db8::1")})
	if err == nil {
		t.Fatal("IPv6 对端必须在发出前报错（inner 是手写的 IPv4 头）")
	}
	// 再发一次合法的，收到的第一条必须是它。
	if err := c.SetProbe(ctx, ProbeParams{SwIfIndex: 8, PeerIP: net.IPv4(10, 0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(f.lastRequest(t).body[1:5]); got != 8 {
		t.Errorf("非法的请求真的发出去了（先收到 sw_if_index=%d）", got)
	}
}

func TestSendProbeEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	if err := c.SendProbe(ctx, 12); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "sr_wg_probe_send" || len(r.body) != 4 {
		t.Fatalf("name=%s len=%d，期望 sr_wg_probe_send 4 字节", r.name, len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body); got != 12 {
		t.Errorf("sw_if_index = %d", got)
	}
}

// 一条真实的 sr_wg_probe_details 净体（不含 msg_id+context，77 字节；
// 客户端读到的整帧是 83 字节）。字段值故意互不相同，抄错偏移会立刻露馅。
func probeDetailBody() []byte {
	b := make([]byte, 77)
	binary.BigEndian.PutUint32(b[0:4], 5) // sw_if_index
	copy(b[4:8], []byte{192, 168, 99, 2})
	copy(b[8:12], []byte{192, 168, 99, 1})
	binary.BigEndian.PutUint32(b[12:16], 300)  // interval
	binary.BigEndian.PutUint32(b[16:20], 900)  // timeout
	b[20] = 1                                  // enabled
	binary.BigEndian.PutUint64(b[21:29], 101)  // tx
	binary.BigEndian.PutUint64(b[29:37], 98)   // rx
	binary.BigEndian.PutUint64(b[37:45], 2)    // lost
	binary.BigEndian.PutUint64(b[45:53], 1)    // late
	binary.BigEndian.PutUint32(b[53:57], 2500) // last rtt
	binary.BigEndian.PutUint32(b[57:61], 1200) // min
	binary.BigEndian.PutUint32(b[61:65], 3100) // avg
	binary.BigEndian.PutUint32(b[65:69], 7000) // max
	binary.BigEndian.PutUint32(b[69:73], 180)  // jitter
	binary.BigEndian.PutUint32(b[73:77], 20)   // loss permille
	return b
}

func TestProbesParsesDetails(t *testing.T) {
	f := newFake(t)
	f.probeRaw = [][]byte{probeDetailBody()}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	got, err := c.Probes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("会话数 = %d，期望 1", len(got))
	}
	p := got[0]
	if p.SwIfIndex != 5 || !p.PeerIP.Equal(net.IPv4(192, 168, 99, 2)) ||
		!p.LocalIP.Equal(net.IPv4(192, 168, 99, 1)) {
		t.Errorf("键字段解析错: %+v", p)
	}
	if p.IntervalMS != 300 || p.TimeoutMS != 900 || !p.Enabled {
		t.Errorf("参数解析错: %+v", p)
	}
	if p.Tx != 101 || p.Rx != 98 || p.Lost != 2 || p.LateReplies != 1 {
		t.Errorf("计数器解析错: %+v", p)
	}
	if p.LastRTTus != 2500 || p.MinRTTus != 1200 || p.AvgRTTus != 3100 ||
		p.MaxRTTus != 7000 || p.JitterUs != 180 || p.LossPermille != 20 {
		t.Errorf("指标解析错: %+v", p)
	}
}

func TestProbesRejectsShortDetails(t *testing.T) {
	f := newFake(t)
	f.probeRaw = [][]byte{make([]byte, 76)} // 少 1 字节
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	_, err := c.Probes(ctx)
	if err == nil || !strings.Contains(err.Error(), "sr_wg_probe_details") {
		t.Fatalf("短帧必须报错并点名消息，got: %v", err)
	}
}

func TestBFDAddEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	err := c.AddBFDSession(ctx, BFDSessionParams{
		LocalIP:         net.IPv4(192, 168, 99, 1),
		PeerIP:          net.IPv4(192, 168, 99, 2),
		DesiredMinTxUS:  50000,
		RequiredMinRxUS: 50000,
		DetectMult:      3,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 多跳不开就直接 add：VPP 回 -2（INVALID_SW_IF_INDEX），所以顺序也是契约。
	if first := f.lastRequest(t); first.name != "bfd_udp_enable_multihop" || len(first.body) != 0 {
		t.Fatalf("第一条请求 = %s (%d B)，期望 bfd_udp_enable_multihop 空体", first.name, len(first.body))
	}
	r := f.lastRequest(t)
	if r.name != "bfd_udp_add" {
		t.Fatalf("请求名 = %s", r.name)
	}
	// sw_if(4) desired(4) required(4) local(17) peer(17) detect(1)
	// is_authenticated(1) bfd_key_id(1) conf_key_id(4)
	if len(r.body) != 53 {
		t.Fatalf("体长 = %d，期望 53", len(r.body))
	}
	// 必须是多跳（~0）：绑 wg 接口的 BFD 会把 overlay 路由的转发桶换成 drop。
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != BFDMultihopSwIfIndex {
		t.Errorf("sw_if_index = %#x，期望多跳 %#x", got, BFDMultihopSwIfIndex)
	}
	if got := binary.BigEndian.Uint32(r.body[4:8]); got != 50000 {
		t.Errorf("desired_min_tx = %d", got)
	}
	if got := binary.BigEndian.Uint32(r.body[8:12]); got != 50000 {
		t.Errorf("required_min_rx = %d", got)
	}
	// vl_api_address_t = af(1) + 16 字节 union；v4 后面必须补 12 个 0。
	if r.body[12] != 0 || !net.IP(r.body[13:17]).Equal(net.IPv4(192, 168, 99, 1)) {
		t.Errorf("local_addr 编码错: af=%d ip=%v", r.body[12], net.IP(r.body[13:17]))
	}
	for i := 17; i < 29; i++ {
		if r.body[i] != 0 {
			t.Fatalf("local_addr 的 v4 尾部必须补 0，offset %d = %d", i, r.body[i])
		}
	}
	if r.body[29] != 0 || !net.IP(r.body[30:34]).Equal(net.IPv4(192, 168, 99, 2)) {
		t.Errorf("peer_addr 编码错: af=%d ip=%v", r.body[29], net.IP(r.body[30:34]))
	}
	if r.body[46] != 3 {
		t.Errorf("detect_mult = %d，期望 3", r.body[46])
	}
	for i := 47; i < 53; i++ {
		if r.body[i] != 0 {
			t.Fatalf("未开认证时 47..52 必须全 0，offset %d = %d", i, r.body[i])
		}
	}
}

func TestBFDAddRejectsBadParamsBeforeSend(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	base := BFDSessionParams{
		LocalIP:    net.IPv4(192, 168, 99, 1),
		PeerIP:     net.IPv4(192, 168, 99, 2),
		DetectMult: 3,
	}
	bad := base
	bad.DetectMult = 0
	if err := c.AddBFDSession(ctx, bad); err == nil {
		t.Fatal("detect_mult=0 必须在发出前报错")
	}
	bad = base
	bad.PeerIP = nil
	if err := c.AddBFDSession(ctx, bad); err == nil {
		t.Fatal("空对端地址必须在发出前报错")
	}
	if err := c.AddBFDSession(ctx, base); err != nil {
		t.Fatal(err)
	}
	if first := f.lastRequest(t); first.name != "bfd_udp_enable_multihop" {
		t.Errorf("非法请求真的发出去了（先收到 %s）", first.name)
	}
	if got := binary.BigEndian.Uint32(f.lastRequest(t).body[0:4]); got != BFDMultihopSwIfIndex {
		t.Errorf("sw_if_index = %#x，期望多跳 %#x", got, BFDMultihopSwIfIndex)
	}
}

func TestBFDDelEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	if err := c.DelBFDSession(ctx, BFDSessionParams{
		LocalIP: net.IPv4(192, 168, 99, 1),
		PeerIP:  net.IPv4(192, 168, 99, 2),
	}); err != nil {
		t.Fatal(err)
	}
	if first := f.lastRequest(t); first.name != "bfd_udp_enable_multihop" {
		t.Fatalf("第一条请求 = %s，期望 bfd_udp_enable_multihop", first.name)
	}
	r := f.lastRequest(t)
	if r.name != "bfd_udp_del" {
		t.Fatalf("请求名 = %s", r.name)
	}
	// sw_if_index(4) + local_addr(17) + peer_addr(17)
	if len(r.body) != 38 {
		t.Fatalf("体长 = %d，期望 38", len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != BFDMultihopSwIfIndex {
		t.Errorf("sw_if_index = %#x，期望多跳 %#x", got, BFDMultihopSwIfIndex)
	}
}

// 真实 bfd_udp_session_details 净体：msg_id+context 之后 57 字节，整帧 63。
func bfdDetailBody() []byte {
	b := make([]byte, 57)
	binary.BigEndian.PutUint32(b[0:4], 5) // sw_if_index
	b[4] = 0                              // local_addr af=ip4
	copy(b[5:9], []byte{192, 168, 99, 1})
	b[21] = 0 // peer_addr af=ip4
	copy(b[22:26], []byte{192, 168, 99, 2})
	binary.BigEndian.PutUint32(b[38:42], BFDStateUp)
	b[42] = 0                                   // is_authenticated
	b[43] = 0                                   // bfd_key_id
	binary.BigEndian.PutUint32(b[44:48], 0)     // conf_key_id
	binary.BigEndian.PutUint32(b[48:52], 50000) // required_min_rx
	binary.BigEndian.PutUint32(b[52:56], 60000) // desired_min_tx
	b[56] = 3                                   // detect_mult
	return b
}

func TestBFDSessionsParsesDetails(t *testing.T) {
	f := newFake(t)
	f.bfdRaw = [][]byte{bfdDetailBody()}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	got, err := c.BFDSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("会话数 = %d，期望 1", len(got))
	}
	s := got[0]
	if s.SwIfIndex != 5 || !s.LocalIP.Equal(net.IPv4(192, 168, 99, 1)) ||
		!s.PeerIP.Equal(net.IPv4(192, 168, 99, 2)) {
		t.Errorf("键字段解析错: %+v", s)
	}
	if s.State != BFDStateUp {
		t.Errorf("state = %d，期望 %d(up)", s.State, BFDStateUp)
	}
	if s.RequiredMinRxUS != 50000 || s.DesiredMinTxUS != 60000 || s.DetectMult != 3 {
		t.Errorf("参数解析错: %+v", s)
	}
}

func TestBFDSessionsRejectsShortDetails(t *testing.T) {
	f := newFake(t)
	f.bfdRaw = [][]byte{make([]byte, 56)}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	_, err := c.BFDSessions(ctx)
	if err == nil || !strings.Contains(err.Error(), "bfd_udp_session_details") {
		t.Fatalf("短帧必须报错并点名消息，got: %v", err)
	}
}

func TestBFDStateName(t *testing.T) {
	for s, want := range map[uint32]string{
		BFDStateAdminDown: "admin-down",
		BFDStateDown:      "down",
		BFDStateInit:      "init",
		BFDStateUp:        "up",
		9:                 "state-9",
	} {
		if got := BFDStateName(s); got != want {
			t.Errorf("BFDStateName(%d) = %q，期望 %q", s, got, want)
		}
	}
}
