package vppapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeVPP 是一个按实测格式行事的假 VPP api.sock 服务端：16 字节帧前缀、
// sockclnt_create(15)/reply(16) 握手、**index 才是后续请求的 client_index**、
// dump 没有结束标记。用它把这些格式事实锁成测试，而不是等真机上超时才发现。
type fakeVPP struct {
	path     string
	ln       net.Listener
	versions map[string]string
	ids      map[string]uint16
	names    map[uint16]string

	// 行为开关
	badReplyFor string           // 这个名字的应答故意写错 msg_id（复现修掉的插件 bug）
	noiseFor    string           // 应答前先回一帧同 context 的异步通知
	retvals     map[string]int32 // 让某条请求回非 0 retval
	obsDetails  int
	ifDetails   int
	requests    chan request
}

type request struct {
	name string
	body []byte
	ctx  uint32
}

const fakeClientIndex = 0x80000002

func newFake(t *testing.T) *fakeVPP {
	f := &fakeVPP{
		requests: make(chan request, 64),
		versions: map[string]string{},
		retvals:  map[string]int32{},
	}
	for name, crc := range requiredVersions {
		f.versions[name] = crc
	}
	f.buildIDs()

	f.path = filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatalf("假 VPP 起不来: %v", err)
	}
	f.ln = ln
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

// buildIDs 必须稳定：Go 的 map 遍历顺序是随机的，若每次都按遍历顺序发号，
// 注册表里的 id 和 nameOf() 的反查会不一致，测试就会莫名其妙地偶发失败。
func (f *fakeVPP) buildIDs() {
	names := make([]string, 0, len(f.versions))
	for n := range f.versions {
		names = append(names, n)
	}
	sort.Strings(names)
	f.ids = map[string]uint16{}
	f.names = map[uint16]string{}
	for i, n := range names {
		id := uint16(700 + i)
		f.ids[n] = id
		f.names[id] = n
	}
}

// withVersion 改一条 CRC，模拟"插件 .api 动过、客户端没跟上"。
func (f *fakeVPP) withVersion(name, crc string) *fakeVPP {
	f.versions[name] = crc
	return f
}

func (f *fakeVPP) dropVersion(name string) *fakeVPP {
	delete(f.versions, name)
	f.buildIDs()
	return f
}

func (f *fakeVPP) id(name string) uint16 { return f.ids[name] }

func (f *fakeVPP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		frame, err := readFrame(conn)
		if err != nil {
			return
		}
		if msgID := binary.BigEndian.Uint16(frame[0:2]); msgID == sockclntCreate {
			// vl_api_sockclnt_create_t = msg_id(2) + context(4) + name[64]，
			// **没有 client_index**（memclnt.api_types.h:136）——所以注册请求的
			// context 在第 2 字节起，而普通请求在第十。
			ctxVal := binary.BigEndian.Uint32(frame[2:6])
			if err := writeFrame(conn, f.registerReply(ctxVal)); err != nil {
				return
			}
			continue
		}
		f.handle(conn, frame)
	}
}

func (f *fakeVPP) registerReply(ctxVal uint32) []byte {
	body := make([]byte, sockclntReplyLen+len(f.versions)*tableEntryLen)
	binary.BigEndian.PutUint16(body[0:2], sockclntReplyID)
	binary.BigEndian.PutUint32(body[2:6], 0) // 应答头的 client_index 恒为 0，不能用
	binary.BigEndian.PutUint32(body[6:10], ctxVal)
	binary.BigEndian.PutUint32(body[10:14], 0) // response = 成功
	binary.BigEndian.PutUint32(body[14:18], fakeClientIndex)
	binary.BigEndian.PutUint16(body[18:20], uint16(len(f.versions)))
	i := 0
	for name := range f.versions {
		at := sockclntReplyLen + i*tableEntryLen
		binary.BigEndian.PutUint16(body[at:at+2], f.id(name))
		copy(body[at+2:], fmt.Sprintf("%s_%s", name, f.versions[name]))
		i++
	}
	return body
}

func (f *fakeVPP) handle(conn net.Conn, frame []byte) {
	// 请求头 = msg_id(2) + client_index(4) + context(4)，应答头 = msg_id(2) +
	// context(4)：应答没有 client_index。这两处的 context 偏移不同，
	// 搞混了客户端只会安静地超时（本测试最初就是这样挂住的）。
	msgID := binary.BigEndian.Uint16(frame[0:2])
	reqCtx := binary.BigEndian.Uint32(frame[6:10])
	body := append([]byte(nil), frame[reqHeaderLen:]...)
	name := f.names[msgID]

	select {
	case f.requests <- request{name: name, body: body, ctx: reqCtx}:
	default:
	}

	if f.noiseFor != "" && f.noiseFor == name {
		// 异步通知：同 context、但不是这条请求的应答，客户端必须识别并丢弃。
		noise := make([]byte, 10)
		binary.BigEndian.PutUint16(noise[0:2], 5) // 全局表里的 rx_thread_exit
		binary.BigEndian.PutUint32(noise[2:6], reqCtx)
		writeFrame(conn, noise)
	}

	switch name {
	case "sr_wg_key_set", "sr_wg_rebind_ports", "sr_wg_obs_clear",
		"wireguard_interface_delete":
		f.reply(conn, name, reqCtx, uint32Bytes(uint32(f.retvals[name])))
	case "sr_wg_stats_get":
		// 真实尺寸 138 = msg_id(2)+context(4)+retval(4)+16 个 u64
		f.reply(conn, name, reqCtx, append(uint32Bytes(0), make([]byte, 128)...))
	case "wireguard_interface_create":
		f.reply(conn, name, reqCtx, append(uint32Bytes(0), uint32Bytes(7)...))
	case "wireguard_peer_add":
		f.reply(conn, name, reqCtx, append(uint32Bytes(0), uint32Bytes(3)...))
	case "sr_wg_obs_dump":
		// 只发 details；"发完了"由后面那条栅栏应答体现（实测 VPP 就是这么做的）。
		for i := 0; i < f.obsDetails; i++ {
			f.sendFrameOf(conn, "sr_wg_obs_details", reqCtx, obsDetailBytes(i))
		}
	case "wireguard_interface_dump":
		for i := 0; i < f.ifDetails; i++ {
			f.sendFrameOf(conn, "wireguard_interface_details", reqCtx, ifDetailBytes(i))
		}
	}
	// 未知请求：不回。客户端应当超时，而不是永远挂着。
}

// reply 发一条 <name>_reply。badReplyFor 命中时刻意写错 msg_id，复现本插件
// 修掉的那个 bug（应答带的是插件内部下标）。
func (f *fakeVPP) reply(conn net.Conn, reqName string, ctxVal uint32, payload []byte) {
	id := f.id(reqName + "_reply")
	if f.badReplyFor == reqName {
		id = 5
	}
	f.sendFrameRaw(conn, id, ctxVal, payload)
}

func (f *fakeVPP) sendFrameOf(conn net.Conn, msgName string, ctxVal uint32, payload []byte) {
	f.sendFrameRaw(conn, f.id(msgName), ctxVal, payload)
}

func (f *fakeVPP) sendFrameRaw(conn net.Conn, msgID uint16, ctxVal uint32, payload []byte) {
	frame := make([]byte, 6+len(payload))
	binary.BigEndian.PutUint16(frame[0:2], msgID)
	binary.BigEndian.PutUint32(frame[2:6], ctxVal)
	copy(frame[6:], payload)
	writeFrame(conn, frame)
}

func uint32Bytes(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// details 帧去掉 msg_id+context 后的净体：
// vl_api_sr_wg_obs_details_t = ip4(4) sport(2) dport(2) rx_if(4) hits(4) age(4) verified(1)
func obsDetailBytes(i int) []byte {
	b := make([]byte, 21)
	b[0], b[1], b[2], b[3] = 10, 10, 1, byte(2+i)
	binary.BigEndian.PutUint16(b[4:6], uint16(41781+i))
	binary.BigEndian.PutUint16(b[6:8], 51820)
	binary.BigEndian.PutUint32(b[8:12], 2)
	binary.BigEndian.PutUint32(b[12:16], uint32(6+i))
	b[20] = 1
	return b
}

// vl_api_wireguard_interface_details_t = interface(91)
func ifDetailBytes(i int) []byte {
	b := make([]byte, 91)
	binary.BigEndian.PutUint32(b[4:8], uint32(1+i))
	for k := range b[8:40] {
		b[8+k] = byte(k)
	}
	binary.BigEndian.PutUint16(b[72:74], uint16(51999+i))
	copy(b[75:79], []byte{10, 10, 1, 2})
	return b
}

func readFrame(conn net.Conn) ([]byte, error) {
	h := make([]byte, frameHeaderLen)
	if _, err := io.ReadFull(conn, h); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint32(h[8:12]))
	if length < 2 {
		return nil, fmt.Errorf("假 VPP: 帧长度 %d 不合理", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (f *fakeVPP) dial(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, f.path, "test-agent")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (f *fakeVPP) lastRequest(t *testing.T) request {
	t.Helper()
	select {
	case r := <-f.requests:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("假 VPP 没收到请求")
		return request{}
	}
}

func shortCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 400*time.Millisecond)
}

// ==================== 握手与版本 ====================

func TestDialHandshakeAndTable(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	if c.clientIndex != fakeClientIndex {
		t.Errorf("client_index 取错了：必须用应答里的 index 字段(0x%x)，不是恒为 0 的头字段，got %#x",
			fakeClientIndex, c.clientIndex)
	}
	if _, ok := c.MsgID("sr_wg_key_set"); !ok {
		t.Error("消息表没解析出 sr_wg_key_set")
	}
}

func TestDialRefusesCRCDrift(t *testing.T) {
	f := newFake(t).withVersion("sr_wg_key_set", "deadbeef")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Dial(ctx, f.path, "test-agent")
	if err == nil {
		t.Fatal("CRC 变了必须拒绝继续：否则只会发出尺寸不对的包，然后 VPP 静默丢弃")
	}
	if !strings.Contains(err.Error(), "sr_wg_key_set") || !strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("错误要点名哪条消息、实际值是什么，got: %v", err)
	}
}

func TestDialRefusesMissingMessage(t *testing.T) {
	f := newFake(t).dropVersion("wireguard_peer_add")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Dial(ctx, f.path, "test-agent")
	if err == nil || !strings.Contains(err.Error(), "wireguard_peer_add") {
		t.Fatalf("缺消息要点名报错，got: %v", err)
	}
}

func TestDialSocketMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Dial(ctx, filepath.Join(t.TempDir(), "nope.sock"), "x"); err == nil {
		t.Fatal("连不存在的 socket 必须报错")
	}
}

// ==================== 请求编码（字节级） ====================

func TestSetPKeyEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(0xA0 + i)
	}
	if err := c.SetPKey(context.Background(), 1, key); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "sr_wg_key_set" {
		t.Fatalf("请求名 = %s", r.name)
	}
	if len(r.body) != 18 {
		t.Errorf("sr_wg_key_set 体长 = %d, 期望 18 (hmac_id 2 + key 16)", len(r.body))
	}
	if got := binary.BigEndian.Uint16(r.body[0:2]); got != 1 {
		t.Errorf("hmac_id = %d", got)
	}
	if !equalBytes(r.body[2:], key) {
		t.Errorf("key 原文没送到: %x", r.body[2:])
	}
}

func TestSetPKeyRejectsWrongKeyLength(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	if err := c.SetPKey(context.Background(), 1, make([]byte, 32)); err == nil {
		t.Fatal("P-Key 长度不是 16 必须在发出前就报错")
	}
}

func TestSetPKeyPropagatesRetval(t *testing.T) {
	// VPP 用应答里的 retval 表达失败（例如密钥表满）。客户端不能当成功返回，
	// 否则控制器以为钥匙装上了，数据面却在用旧的那把。
	f := newFake(t)
	f.retvals["sr_wg_key_set"] = -2
	c := f.dial(t)
	err := c.SetPKey(context.Background(), 1, make([]byte, 16))
	var apiErr APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 APIError，got: %T %v", err, err)
	}
	if apiErr.Retval != -2 {
		t.Errorf("retval = %d, 期望 -2", apiErr.Retval)
	}
}

func TestCreateInterfaceEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i)
	}
	idx, err := c.CreateInterface(context.Background(), InterfaceParams{
		ListenPort:  51820,
		PrivateKeys: priv,
		SrcIP:       net.IPv4(10, 10, 1, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 7 {
		t.Errorf("sw_if_index = %d, 期望从应答解析出 7", idx)
	}
	r := f.lastRequest(t)
	if len(r.body) != 92 {
		t.Fatalf("create 体长 = %d, 期望 92（整帧 102，实测值）", len(r.body))
	}
	if !equalBytes(r.body[8:40], priv) {
		t.Error("私钥没原样交给 VPP —— 定案 3 就白做了")
	}
	if !allZero(r.body[40:72]) {
		t.Error("public_key 应留空，让 VPP 从私钥自己推")
	}
	if got := binary.BigEndian.Uint16(r.body[72:74]); got != 51820 {
		t.Errorf("port = %d", got)
	}
	if r.body[74] != apiAddressFamilyIP4 {
		t.Errorf("af = %d, 期望 ADDRESS_IP4=0", r.body[74])
	}
	if !equalBytes(r.body[75:79], []byte{10, 10, 1, 2}) {
		t.Errorf("ip4 = %v", r.body[75:79])
	}
	if !allZero(r.body[79:91]) {
		t.Error("union 尾部必须补满 12 个 0，否则整帧尺寸就不对（address 是 17 字节不是 8）")
	}
	if r.body[91] != 0 {
		t.Error("generate_key 必须是 0：私钥由 agent 生成，不许 VPP 自己造")
	}
}

func TestCreateInterfaceNeedsSrcIP(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	if _, err := c.CreateInterface(context.Background(), InterfaceParams{
		PrivateKeys: make([]byte, 32),
	}); err == nil {
		t.Fatal("没有 underlay 源地址必须报错")
	}
}

func TestAddPeerEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(0x40 + i)
	}
	peerIdx, err := c.AddPeer(context.Background(), PeerParams{
		PublicKey:  pub,
		EndpointIP: net.IPv4(10, 10, 2, 2),
		Port:       51821,
		Keepalive:  25,
		SWIfIndex:  7,
		AllowedIPs: []Prefix{{IP: net.IPv4(192, 168, 99, 2), Len: 32}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if peerIdx != 3 {
		t.Errorf("peer_index = %d", peerIdx)
	}
	r := f.lastRequest(t)
	if len(r.body) != 67+18 {
		t.Fatalf("peer_add 体长 = %d, 期望 85（基体 67 + 一条 prefix 18，packed）", len(r.body))
	}
	// 偏移取自 /usr/include/vpp_plugins/wireguard/wireguard.api_types.h 里
	// vl_api_wireguard_peer_t 的字段顺序：
	// peer_index(4) public_key[32] port(2) keepalive(2) table_id(4)
	// endpoint(17) sw_if_index(4) flags(1) n_allowed_ips(1) prefix(18)
	if !equalBytes(r.body[4:36], pub) {
		t.Error("peer 公钥错了")
	}
	if got := binary.BigEndian.Uint16(r.body[36:38]); got != 51821 {
		t.Errorf("外层目的端口 = %d, 期望 51821（打中转）", got)
	}
	if got := binary.BigEndian.Uint16(r.body[38:40]); got != 25 {
		t.Errorf("persistent-keepalive = %d", got)
	}
	want := append([]byte{apiAddressFamilyIP4}, padded16(net.IPv4(10, 10, 2, 2).To4())...)
	if !equalBytes(r.body[44:61], want) {
		t.Errorf("endpoint address 编码错了: %x", r.body[44:61])
	}
	if got := binary.BigEndian.Uint32(r.body[61:65]); got != 7 {
		t.Errorf("sw_if_index = %d", got)
	}
	if r.body[65] != 0 || r.body[66] != 1 {
		t.Errorf("flags/n_allowed_ips = %d/%d", r.body[65], r.body[66])
	}
	if r.body[84] != 32 {
		t.Errorf("prefix len = %d, 期望在第 84 字节", r.body[84])
	}
	if !equalBytes(r.body[67:84], append([]byte{apiAddressFamilyIP4}, padded16(net.IPv4(192, 168, 99, 2).To4())...)) {
		t.Errorf("allowed_ips[0] 地址错了: %x", r.body[67:84])
	}
}

// ==================== dump 与栅栏 ====================

func TestDumpOverflowIsNotSilentlyTruncated(t *testing.T) {
	// 队列只有 detailQueueLen 格。超过它时 VPP 那边一切正常、客户端只是少收，
	// 拿到手的就是一份"合法但不全"的观测表 —— 必须报错，不能返回半份。
	f := newFake(t)
	f.obsDetails = detailQueueLen + 5
	c := f.dial(t)
	_, err := c.Observations(context.Background())
	if err == nil {
		t.Fatalf("截断必须报错，got %d 条", detailQueueLen)
	}
	if !strings.Contains(err.Error(), "截断") {
		t.Errorf("错误要说明是截断, got: %v", err)
	}
}

func TestBadAddressRefusedBeforeSend(t *testing.T) {
	// 地址写错时 VPP 不报错，它照单收下配成 0.0.0.0：隧道"配好了"却永远不通。
	// 所以这个必须在客户端就挡住，而且请求根本不许发出去。
	f := newFake(t)
	c := f.dial(t)
	bad := net.IP([]byte{1, 2, 3, 4, 5}) // 既非 v4 也非 v6 的表示长度
	if _, err := c.CreateInterface(context.Background(), InterfaceParams{
		PrivateKeys: make([]byte, wgKeyLen), SrcIP: bad,
	}); err == nil {
		t.Error("CreateInterface 接受了非法源地址")
	}
	if _, err := c.AddPeer(context.Background(), PeerParams{
		PublicKey: make([]byte, wgKeyLen), EndpointIP: bad,
	}); err == nil {
		t.Error("AddPeer 接受了非法 endpoint 地址")
	}
	if _, err := c.AddPeer(context.Background(), PeerParams{
		PublicKey: make([]byte, wgKeyLen), EndpointIP: net.IPv4(10, 10, 2, 2),
		AllowedIPs: []Prefix{{IP: bad, Len: 32}},
	}); err == nil {
		t.Error("AddPeer 接受了非法 allowed_ip")
	}
}

func TestObservationsParsesDetails(t *testing.T) {
	f := newFake(t)
	f.obsDetails = 2
	c := f.dial(t)
	obs, err := c.Observations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("收到 %d 条观测, 期望 2", len(obs))
	}
	if obs[0].SrcPort != 41781 || !obs[0].Verified || obs[0].Hits != 6 {
		t.Errorf("第一条观测解码错了: %+v", obs[0])
	}
	if !obs[0].SrcIP.Equal(net.IPv4(10, 10, 1, 2)) {
		t.Errorf("src ip = %v", obs[0].SrcIP)
	}
	if obs[1].SrcPort != 41782 {
		t.Errorf("第二条 src port = %d", obs[1].SrcPort)
	}
}

func TestEmptyDumpIsNotAnError(t *testing.T) {
	// 实测：VPP 的 dump 空结果集一帧都不发。客户端不能把它当失败，
	// 否则 agent 每轮"还没观测到任何东西"都会报错。
	f := newFake(t)
	f.obsDetails = 0
	c := f.dial(t)
	obs, err := c.Observations(context.Background())
	if err != nil {
		t.Fatalf("空 dump 不该报错: %v", err)
	}
	if len(obs) != 0 {
		t.Fatalf("got %d 条", len(obs))
	}
}

func TestInterfacesShowPrivate(t *testing.T) {
	f := newFake(t)
	f.ifDetails = 1
	c := f.dial(t)
	ifs, err := c.Interfaces(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ifs) != 1 {
		t.Fatalf("got %d", len(ifs))
	}
	if ifs[0].Port != 51999 || ifs[0].SWIfIndex != 1 {
		t.Errorf("解码错了: %+v", ifs[0])
	}
	if len(ifs[0].PrivateKey) != 32 || !ifs[0].SrcIP.Equal(net.IPv4(10, 10, 1, 2)) {
		t.Errorf("私钥/源地址解码错了: %+v", ifs[0])
	}
}

// ==================== 错帧与噪声 ====================

func TestWrongReplyMsgIDTimesOut(t *testing.T) {
	// 复现本插件修掉的那个 bug：应答 msg_id 写成插件内部下标。
	// 客户端的正确表现是超时 + 计数，而不是把错帧当好帧收下。
	f := newFake(t)
	f.badReplyFor = "sr_wg_key_set"
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	err := c.SetPKey(ctx, 1, make([]byte, 16))
	if err == nil {
		t.Fatal("错 id 的应答必须让调用失败")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应当是超时错误, got: %v", err)
	}
	if c.Drops() == 0 {
		t.Error("丢弃的帧要计数，否则线上没法判断是消息号错了")
	}
}

func TestAsyncNotificationWithSameContextIsIgnored(t *testing.T) {
	f := newFake(t)
	f.noiseFor = "sr_wg_rebind_ports"
	c := f.dial(t)
	if err := c.RebindPorts(context.Background()); err != nil {
		t.Fatalf("异步噪声不该影响请求: %v", err)
	}
	if c.Drops() != 1 {
		t.Errorf("噪声应当计数, Drops=%d", c.Drops())
	}
}

func TestConnectionCloseFailsCalls(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// 关闭之后任何调用都应立刻拿到 ErrClosed，而不是挂到超时。
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.SetPKey(ctx, 1, make([]byte, 16))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("期望 ErrClosed, got: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("等了 %s，连接断开应当立刻暴露", time.Since(start))
	}
}

// ==================== helpers ====================

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func padded16(v4 []byte) []byte {
	out := make([]byte, 16)
	copy(out, v4)
	return out
}
