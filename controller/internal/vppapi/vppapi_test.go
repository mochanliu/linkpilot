package vppapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
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
	pathDetails int
	tunDetails  int
	portDetails int
	peerDetails int
	swIfDetails int      // core 的 sw_interface_dump 回几条
	peersRaw    [][]byte // 非空时按原样发这些 details 载荷（用来回放真 VPP 抓到的帧）
	swIfRaw     [][]byte // 同上，但发给 sw_interface_dump
	// ipAddrs 是 ip_address_dump 的按接口答案；ipAddrRaw 非空时无视接口号原样发这些
	// 净体（用来造"坏帧/读不全"的现场）。
	ipAddrs    map[uint32][]fakeAddr
	ipAddrRaw  [][]byte
	mssDetails [][]byte // mss_clamp_get 回的明细净体（不含 msg_id+context）
	probeRaw   [][]byte // sr_wg_probe_dump 回的明细净体
	bfdRaw     [][]byte // bfd_udp_session_dump 回的明细净体
	ipRouteRaw [][]byte // ip_route_dump 回的明细净体
	mplsRaw    [][]byte // mpls_route_dump 回的明细净体
	// vrrpRaw 是 vrrp_vr_dump 回的 details 净体。**故意无视请求里的口号、原样全发**：
	// 实测这台盒子会过滤（C7），但客户端不能依赖它，所以假端比真机更宽松，
	// 让"只留被问的那张口"这条由 VRRPDump 自己证明。
	vrrpRaw         [][]byte
	vrrpUpdateIdx   uint32 // vrrp_vr_update_reply 带回来的全局池下标
	vrrpUpdateNoIdx bool   // 只回 retval：复现应答比约定短 4 字节的帧
	cliReply        string // cli_inband 的应答字符串
	requests        chan request
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
		ipAddrs:  map[uint32][]fakeAddr{},
	}
	for _, a := range fakeSwAddrs {
		f.ipAddrs[a.idx] = a.addrs
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
		"wireguard_interface_delete", "wireguard_peer_remove",
		"sr_wg_path_add_del", "sr_wg_tunnel_set_path", "sr_wg_encap_enable",
		"sr_wg_port_add_del", "sr_wg_transit_enable", "sr_wg_transit_disable",
		// core 的改 MTU 应答只有一个 retval（vl_api_sw_interface_set_mtu_reply_t），
		// 和上面这批插件消息同形。本文件里 core 的消息共两条：这条，和下面那条
		// sw_interface_dump（走 details，不是 retval）。
		"sw_interface_set_mtu":
		f.reply(conn, name, reqCtx, uint32Bytes(uint32(f.retvals[name])))
	case "mss_clamp_enable_disable":
		f.reply(conn, name, reqCtx, uint32Bytes(uint32(f.retvals[name])))
	case "mss_clamp_get":
		for _, raw := range f.mssDetails {
			f.sendFrameOf(conn, "mss_clamp_details", reqCtx, raw)
		}
		// 净体 = retval(4) cursor(4)；cursor 0 = 一页读完。
		f.reply(conn, name, reqCtx, append(uint32Bytes(uint32(f.retvals[name])), uint32Bytes(0)...))
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
	case "wireguard_peers_dump":
		if len(f.peersRaw) > 0 {
			for _, raw := range f.peersRaw {
				f.sendFrameOf(conn, "wireguard_peers_details", reqCtx, raw)
			}
			break
		}
		for i := 0; i < f.peerDetails; i++ {
			f.sendFrameOf(conn, "wireguard_peers_details", reqCtx, peerDetailBytes(i))
		}
	case "sr_wg_path_dump":
		for i := 0; i < f.pathDetails; i++ {
			f.sendFrameOf(conn, "sr_wg_path_details", reqCtx, pathDetailBytes(i))
		}
	case "sr_wg_tunnel_dump":
		for i := 0; i < f.tunDetails; i++ {
			f.sendFrameOf(conn, "sr_wg_tunnel_details", reqCtx, tunDetailBytes(i))
		}
	case "sr_wg_port_dump":
		for i := 0; i < f.portDetails; i++ {
			f.sendFrameOf(conn, "sr_wg_port_details", reqCtx, portDetailBytes(i))
		}
	case "sr_wg_probe_set", "sr_wg_probe_send", "bfd_udp_enable_multihop", "bfd_udp_add", "bfd_udp_del":
		f.reply(conn, name, reqCtx, uint32Bytes(uint32(f.retvals[name])))
	case "sr_wg_probe_dump":
		for _, raw := range f.probeRaw {
			f.sendFrameOf(conn, "sr_wg_probe_details", reqCtx, raw)
		}
	case "bfd_udp_session_dump":
		for _, raw := range f.bfdRaw {
			f.sendFrameOf(conn, "bfd_udp_session_details", reqCtx, raw)
		}
	case "ip_table_add_del":
		f.reply(conn, name, reqCtx, uint32Bytes(uint32(f.retvals[name])))
	case "ip_route_add_del", "mpls_route_add_del":
		// 两条 reply 同形：retval(4) + stats_index(4)（CRC 都是 1992deab）。
		f.reply(conn, name, reqCtx, append(uint32Bytes(uint32(f.retvals[name])), uint32Bytes(0)...))
	case "cli_inband":
		out := make([]byte, 0, 4+len(f.cliReply))
		out = appendU32(out, uint32(len(f.cliReply)))
		out = append(out, f.cliReply...)
		f.reply(conn, name, reqCtx, append(uint32Bytes(uint32(f.retvals[name])), out...))
	case "ip_route_dump":
		for _, raw := range f.ipRouteRaw {
			f.sendFrameOf(conn, "ip_route_details", reqCtx, raw)
		}
	case "vrrp_vr_add_del", "vrrp_vr_del", "vrrp_vr_start_stop":
		// 三条应答同形（CRC 都是 e8d4e804）：只有 retval。实测的 -7/-116/-6/-73
		// 就从这里回，客户端必须原样变成 APIError 而不是"没报错"。
		f.reply(conn, name, reqCtx, uint32Bytes(uint32(f.retvals[name])))
	case "vrrp_vr_update":
		// vl_api_vrrp_vr_update_reply_t = retval(4) + vrrp_index(4)，是这套 API 里
		// 唯一能读到那个全局池下标的地方。
		out := uint32Bytes(uint32(f.retvals[name]))
		if !f.vrrpUpdateNoIdx {
			out = append(out, uint32Bytes(f.vrrpUpdateIdx)...)
		}
		f.reply(conn, name, reqCtx, out)
	case "vrrp_vr_dump":
		for _, raw := range f.vrrpRaw {
			f.sendFrameOf(conn, "vrrp_vr_details", reqCtx, raw)
		}
	case "mpls_route_dump":
		for _, raw := range f.mplsRaw {
			f.sendFrameOf(conn, "mpls_route_details", reqCtx, raw)
		}
	case "sw_interface_dump":
		if len(f.swIfRaw) > 0 {
			for _, raw := range f.swIfRaw {
				f.sendFrameOf(conn, "sw_interface_details", reqCtx, raw)
			}
			break
		}
		for i := 0; i < f.swIfDetails; i++ {
			f.sendFrameOf(conn, "sw_interface_details", reqCtx, swInterfaceDetailBytes(i))
		}
	case "ip_address_dump":
		// 真 VPP 对 0xFFFFFFFF（core 惯例里表示"全部"）实测一帧都不回，这个假服务端
		// 照做：只回被问的那个接口，绝不替客户端把"全部"解释成遍历。
		if len(body) < 5 {
			break // 短包 = 不回，客户端超时（与真 VPP 解码失败同形）
		}
		idx := binary.BigEndian.Uint32(body[0:4])
		if body[4] != 0 {
			break // 这个假 VPP 没有 IPv6 地址
		}
		if len(f.ipAddrRaw) > 0 {
			for _, raw := range f.ipAddrRaw {
				f.sendFrameOf(conn, "ip_address_details", reqCtx, raw)
			}
			break
		}
		for _, a := range f.ipAddrs[idx] {
			f.sendFrameOf(conn, "ip_address_details", reqCtx, ipAddressDetailBytes(idx, a.ip, a.len))
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

// vl_api_sr_wg_path_details_t 净体 = path_id(2) hmac_id(2) seg_count(1) active(1)
// segments[8](48)。段的顺序是数据面那套倒序：[0]=终点 VPN 口，[n-1]=首跳中转口。
func pathDetailBytes(i int) []byte {
	b := make([]byte, 54)
	binary.BigEndian.PutUint16(b[0:2], uint16(1+i))
	binary.BigEndian.PutUint16(b[2:4], 1) // hmac_id
	b[4] = 2                              // seg_count
	b[5] = 1                              // active
	copy(b[6:10], []byte{10, 10, 2, 2})
	binary.BigEndian.PutUint16(b[10:12], 51820)
	copy(b[12:16], []byte{10, 10, 1, 3})
	binary.BigEndian.PutUint16(b[16:18], 51821)
	return b
}

// vl_api_sr_wg_tunnel_details_t 净体 = sw_if_index(4) path_id(2) dst_port(2)
// endpoint_ip(4) is_effective(1)。is_effective=0 是"绑了但 underlay 没启 encap"的
// 静默旁路态；dst_port=0 表示端口通配（该 IP 上不分隧道全走这条路径）。
func tunDetailBytes(i int) []byte {
	b := make([]byte, 13)
	binary.BigEndian.PutUint32(b[0:4], uint32(2+i))
	binary.BigEndian.PutUint16(b[4:6], uint16(1+i))
	binary.BigEndian.PutUint16(b[6:8], uint16(51820+i))
	copy(b[8:12], []byte{10, 10, 2, 2})
	b[12] = 1
	return b
}

// vl_api_sr_wg_port_details_t 净体 = kind(1) port(2)。前两条当成 vpn 端口、
// 第三条当成 transit 端口，正好覆盖"集合里两种端口混在一条流里"这件事。
func portDetailBytes(i int) []byte {
	b := make([]byte, 3)
	if i >= 2 {
		b[0] = 1 // kind = transit
	}
	binary.BigEndian.PutUint16(b[1:3], uint16(51820+i))
	return b
}

// peerDetailBytes 造一条 wireguard_peers_details 的载荷：基体 67 字节 + 2 条
// prefix（各 18）。字段与数值刻意取成那次真机探针的同一组，这样这一层与 live
// 层对的是同一份实测布局。
func peerDetailBytes(i int) []byte {
	b := make([]byte, 0, wgPeerBaseLen+2*prefixLen)
	b = appendU32(b, uint32(1+i))
	b = append(b, bytes.Repeat([]byte{byte(0xa0 + i)}, wgKeyLen)...)
	b = appendU16(b, 51820) // 外层目的端口
	b = appendU16(b, 25)    // persistent_keepalive
	b = appendU32(b, 0)     // table_id
	b = appendAddress(b, net.IPv4(192, 0, 2, 7))
	b = appendU32(b, 3) // sw_if_index
	b = append(b, 0)    // flags
	b = append(b, 2)    // n_allowed_ips
	b = appendAddress(b, net.IPv4(198, 51, 100, 1))
	b = append(b, 32)
	b = appendAddress(b, net.IPv4(198, 51, 100, 2))
	b = append(b, 24)
	return b
}

// fakeSwInterfaces 是假 VPP 的接口表，形态照着 srwg-node1 实测那三张口：local0 只有
// ADMIN_UP 且没有 MTU 记录，host-eth0 是 underlay 出口，wg0 是隧道。
var fakeSwInterfaces = []struct {
	idx   uint32
	name  string
	mtu   uint32
	flags uint32
}{
	{1, "local0", 0, 0x1},
	{2, "host-eth0", 1500, 0x3},
	{3, "wg0", 1360, 0x3},
}

type fakeAddr struct {
	ip  string
	len uint8
}

// fakeSwAddrs 照着 srwg-node1 的 `show interface address` 造：只有 underlay 出口和
// wg0 有地址，local0 一个都没有 ——"没有地址"必须能读成空列表，而不是一条 0.0.0.0/0。
var fakeSwAddrs = []struct {
	idx   uint32
	addrs []fakeAddr
}{
	{1, nil},
	{2, []fakeAddr{{"10.10.1.2", 24}}},
	{3, []fakeAddr{{"192.168.99.1", 24}}},
}

// ipAddressDetailBytes 造一条 ip_address_details 净体（不含 msg_id+context）：
// sw_if_index(4) + af(1) + vl_api_address_t(17) + 前缀长度(1) = 22，帧长 28（实测）。
func ipAddressDetailBytes(idx uint32, ip string, plen uint8) []byte {
	b := make([]byte, ipAddressDetailsLen-6)
	binary.BigEndian.PutUint32(b[0:4], idx)
	// b[4] = 地址族 0 = IPv4
	copy(b[5:9], net.ParseIP(ip).To4())
	b[21] = plen
	return b
}

// swInterfaceDetailBytes 造一条 core 的 sw_interface_details 净体。
//
// 偏移是"整帧 offsetof − 6"：sendFrameOf 发的是去掉 msg_id(2)+context(4) 之后的净体，
// 而头文件里的 offsetof 把这两项算在内。这几个数（0/14/32/97）与 swInterfaceDetailsLen
// 同源，都来自容器里 VPP 生成的 interface.api_types.h 加 gcc 的 sizeof/offsetof 实测。
func swInterfaceDetailBytes(i int) []byte {
	f := fakeSwInterfaces[i%len(fakeSwInterfaces)]
	b := make([]byte, swInterfaceDetailsLen-6)
	binary.BigEndian.PutUint32(b[0:4], f.idx)     // sw_if_index  @6
	binary.BigEndian.PutUint32(b[4:8], 0)         // sup_sw_if_index @10
	binary.BigEndian.PutUint32(b[14:18], f.flags) // flags        @20
	binary.BigEndian.PutUint32(b[32:36], f.mtu)   // mtu[0] = L3  @38
	copy(b[97:], f.name)                          // interface_name @103
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

// ==================== SR 路径 / 隧道绑定 / 挂点 ====================

func TestAddPathEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	err := c.AddPath(context.Background(), PathParams{
		PathID: 1, HmacID: 7, Active: true,
		Segments: []Segment{
			{IP: net.IPv4(10, 10, 2, 2), Port: 51820}, // [0] = 终点，打 VPN 口
			{IP: net.IPv4(10, 10, 1, 3), Port: 51821}, // [n-1] = 首跳，打中转口
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	// 体长必须是 55：段数组是定长 8 项。写"实际有几段发几段"会得到一个短请求，
	// VPP 只在日志留一行 Truncated message 就丢掉，运行期表现为莫名超时。
	if len(r.body) != pathBodyLen {
		t.Fatalf("path_add_del 体长 = %d，期望 %d", len(r.body), pathBodyLen)
	}
	if r.body[0] != 1 {
		t.Errorf("is_add = %d", r.body[0])
	}
	if got := binary.BigEndian.Uint16(r.body[1:3]); got != 1 {
		t.Errorf("path_id = %d", got)
	}
	if got := binary.BigEndian.Uint16(r.body[3:5]); got != 7 {
		t.Errorf("hmac_id = %d", got)
	}
	if r.body[5] != 2 {
		t.Errorf("seg_count = %d，期望 2", r.body[5])
	}
	if r.body[6] != 1 {
		t.Errorf("active = %d，期望 1", r.body[6])
	}
	// 段的起点在 is_add(1)+path_id(2)+hmac_id(2)+seg_count(1)+active(1) = 7。
	if !equalBytes(r.body[7:11], []byte{10, 10, 2, 2}) || binary.BigEndian.Uint16(r.body[11:13]) != 51820 {
		t.Errorf("segments[0] 不对: %x（数据面约定 [0]=终点）", r.body[7:13])
	}
	if !equalBytes(r.body[13:17], []byte{10, 10, 1, 3}) || binary.BigEndian.Uint16(r.body[17:19]) != 51821 {
		t.Errorf("segments[1] 不对: %x", r.body[13:19])
	}
	for i := 19; i < len(r.body); i++ {
		if r.body[i] != 0 {
			t.Fatalf("未用的段槽位没补零（offset %d = %d）", i, r.body[i])
		}
	}
}

func TestAddPathRejectsBadSegmentsBeforeSend(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	cases := []struct {
		why  string
		segs []Segment
	}{
		{"零段路径", nil},
		{"超过 8 段", make([]Segment, maxSegments+1)},
		// SR 段是 vl_api_ip4_address_t（4 字节），IPv6 不是"另一种写法"，编不出来。
		{"IPv6 段", []Segment{{IP: net.ParseIP("2001:db8::1"), Port: 51820}}},
		{"空地址段", []Segment{{Port: 51820}}},
	}
	for _, tc := range cases {
		if err := c.AddPath(context.Background(), PathParams{Segments: tc.segs}); err == nil {
			t.Errorf("%s 必须在发出前报错", tc.why)
		}
	}
	select {
	case r := <-f.requests:
		t.Fatalf("校验失败却发出了请求: %s", r.name)
	default:
	}
}

func TestDelPathStillSendsFullStruct(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	if err := c.DelPath(context.Background(), 0x1234); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if len(r.body) != pathBodyLen || r.body[0] != 0 {
		t.Fatalf("删除也要发满结构: 体长 %d is_add %d", len(r.body), r.body[0])
	}
	if got := binary.BigEndian.Uint16(r.body[1:3]); got != 0x1234 {
		t.Errorf("path_id = %x", got)
	}
}

func TestAddPathPropagatesNoSuchEntry(t *testing.T) {
	// 路径存的是密钥副本：hmac_id 不在密钥表里时 VPP 回 NO_SUCH_ENTRY。
	// 若这里吞掉 retval，调用方会以为路径建好了，实际发出去的包对端一律验签失败。
	f := newFake(t)
	f.retvals["sr_wg_path_add_del"] = -8 // VNET_API_ERROR_NO_SUCH_ENTRY
	c := f.dial(t)
	err := c.AddPath(context.Background(), PathParams{
		PathID: 1, HmacID: 99, Segments: []Segment{{IP: net.IPv4(10, 10, 2, 2), Port: 51820}},
	})
	var apiErr APIError
	if !errors.As(err, &apiErr) || apiErr.Retval != -8 {
		t.Fatalf("期望 APIError(-8)，got %T %v", err, err)
	}
}

func TestPathsParsesDetails(t *testing.T) {
	f := newFake(t)
	f.pathDetails = 2
	c := f.dial(t)
	paths, err := c.Paths(context.Background(), ^uint16(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("路径 %d 条", len(paths))
	}
	p := paths[0]
	if p.PathID != 1 || p.HmacID != 1 || !p.Active {
		t.Errorf("头部字段不对: %+v", p)
	}
	if len(p.Segments) != 2 ||
		!equalBytes(p.Segments[0].IP, net.IPv4(10, 10, 2, 2).To4()) || p.Segments[0].Port != 51820 ||
		p.Segments[1].Port != 51821 {
		t.Errorf("段解析不对: %+v", p.Segments)
	}
	// 全量查询的哨兵值要真的发出去：0 是合法 path_id，不能用 0 表示"全部"。
	r := f.lastRequest(t)
	if len(r.body) != 2 || binary.BigEndian.Uint16(r.body) != ^uint16(0) {
		t.Errorf("path_dump 没带全量哨兵: %x", r.body)
	}
}

func TestSetTunnelEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	err := c.SetTunnel(context.Background(), TunnelBinding{
		SWIfIndex: 2, PathID: 1, EndpointIP: net.IPv4(10, 10, 2, 2), DstPort: 51830,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if len(r.body) != tunnelBodyLen {
		t.Fatalf("tunnel_set_path 体长 = %d，期望 %d", len(r.body), tunnelBodyLen)
	}
	if r.body[0] != 1 {
		t.Errorf("is_add = %d", r.body[0])
	}
	if got := binary.BigEndian.Uint32(r.body[1:5]); got != 2 {
		t.Errorf("sw_if_index = %d", got)
	}
	if got := binary.BigEndian.Uint16(r.body[5:7]); got != 1 {
		t.Errorf("path_id = %d", got)
	}
	// dst_port 是绑定身份的另一半：同一对 CPE 开第二条隧道时只有端口能说出
	// "这一包属于哪条隧道"，漏了它两条隧道会抢同一条路径。
	if got := binary.BigEndian.Uint16(r.body[7:9]); got != 51830 {
		t.Errorf("dst_port = %d，期望 51830", got)
	}
	// endpoint 是数据面查绑定的唯一线索：漏了它这条绑定永远不命中，
	// 而计数器只会显示 encap_bypass。
	if !equalBytes(r.body[9:13], net.IPv4(10, 10, 2, 2).To4()) {
		t.Errorf("endpoint_ip = %x，期望按裸 4 字节写", r.body[9:13])
	}

	if err := c.SetTunnel(context.Background(), TunnelBinding{SWIfIndex: 2}); err == nil {
		t.Error("endpoint 为空也必须先拦住")
	}
	if err := c.ClearTunnel(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	r = f.lastRequest(t)
	if len(r.body) != tunnelBodyLen || r.body[0] != 0 ||
		binary.BigEndian.Uint32(r.body[1:5]) != 5 {
		t.Errorf("解绑请求不对: %x", r.body)
	}
}

func TestPortAddDelAndDumpEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)

	if err := c.AddPort(context.Background(), PortKindVPN, 51830); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	// is_add(1) kind(1) port(2)
	if len(r.body) != 4 || r.body[0] != 1 || r.body[1] != PortKindVPN ||
		binary.BigEndian.Uint16(r.body[2:4]) != 51830 {
		t.Errorf("port_add_del(is_add) = %x", r.body)
	}
	if err := c.DelPort(context.Background(), PortKindTransit, 51821); err != nil {
		t.Fatal(err)
	}
	r = f.lastRequest(t)
	if r.body[0] != 0 || r.body[1] != PortKindTransit ||
		binary.BigEndian.Uint16(r.body[2:4]) != 51821 {
		t.Errorf("port_add_del(del) = %x", r.body)
	}
	// 端口 0 不是"另一个端口"，是"没填"：VPP 会当 INVALID_VALUE 拒掉，
	// 但宁可在这里就说清是哪条调用没给端口。
	if err := c.AddPort(context.Background(), PortKindVPN, 0); err == nil {
		t.Error("端口 0 必须被拦住")
	}

	f.portDetails = 3
	ports, err := c.Ports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 3 {
		t.Fatalf("端口 %d 条", len(ports))
	}
	for i, p := range ports {
		if want := uint16(51820 + i); p.Port != want {
			t.Errorf("ports[%d] = %+v，期望端口 %d", i, p, want)
		}
	}
	if ports[0].Kind != PortKindVPN || ports[1].Kind != PortKindVPN ||
		ports[2].Kind != PortKindTransit {
		t.Errorf("kind 分错了: %+v", ports)
	}
	// dump 没有过滤字段：全量就是零长度请求体（VPP 用栅栏判定发完）。
	r = f.lastRequest(t)
	if len(r.body) != 0 {
		t.Errorf("port_dump 不该带参数: %x", r.body)
	}
}

func TestTunnelsParsesEffective(t *testing.T) {
	f := newFake(t)
	f.tunDetails = 1
	c := f.dial(t)
	tuns, err := c.Tunnels(context.Background(), ^uint32(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(tuns) != 1 {
		t.Fatalf("绑定 %d 条", len(tuns))
	}
	tn := tuns[0]
	if tn.SWIfIndex != 2 || tn.PathID != 1 || !tn.Effective {
		t.Errorf("绑定解析不对: %+v", tn)
	}
	// dst_port 读不回来就没法判断这条绑定是"某条隧道的"还是"通配"的，
	// 而通配绑定正是多条隧道互相抢路径的那个坑。
	if tn.DstPort != 51820 {
		t.Errorf("dst_port = %d，期望 51820", tn.DstPort)
	}
	if !equalBytes(tn.EndpointIP, net.IPv4(10, 10, 2, 2).To4()) {
		t.Errorf("endpoint = %v", tn.EndpointIP)
	}
}

func TestEncapAndTransitEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)

	if err := c.EnableEncap(context.Background(), 3, true); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if len(r.body) != 5 || binary.BigEndian.Uint32(r.body[0:4]) != 3 || r.body[4] != 1 {
		t.Errorf("encap_enable 编码不对: %x", r.body)
	}
	if err := c.EnableEncap(context.Background(), 3, false); err != nil {
		t.Fatal(err)
	}
	if r = f.lastRequest(t); r.body[4] != 0 {
		t.Errorf("停用 encap 时 enable 字节 = %d", r.body[4])
	}

	if err := c.SetTransit(context.Background(), 51821); err != nil {
		t.Fatal(err)
	}
	r = f.lastRequest(t)
	if len(r.body) != 2 || binary.BigEndian.Uint16(r.body) != 51821 {
		t.Errorf("transit_enable 端口不对: %x", r.body)
	}

	if err := c.ClearTransit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = f.lastRequest(t)
	if len(r.body) != 0 {
		t.Errorf("transit_disable 不该带字段，got %x", r.body)
	}
}

// ==================== WG peer 读回 / 删除 ====================

func TestRemovePeerEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	if err := c.RemovePeer(context.Background(), 17); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "wireguard_peer_remove" {
		t.Fatalf("请求名 = %s", r.name)
	}
	if len(r.body) != 4 || binary.BigEndian.Uint32(r.body) != 17 {
		t.Errorf("peer_remove 只该带 peer_index，got %x", r.body)
	}
}

func TestPeersParsesDump(t *testing.T) {
	f := newFake(t)
	f.peerDetails = 2
	c := f.dial(t)
	peers, err := c.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("peer %d 条", len(peers))
	}
	p := peers[1]
	if p.PeerIndex != 2 || p.SWIfIndex != 3 {
		t.Errorf("索引不对: %+v", p)
	}
	if !equalBytes(p.PublicKey, bytes.Repeat([]byte{0xa1}, 32)) {
		t.Errorf("公钥不对: %x", p.PublicKey)
	}
	// 端口/keepalive/endpoint/allowed_ips 是 agent 判断"这条 peer 现在打到哪"的
	// 全部依据，读错一个字段，重启后就会把旧 endpoint 当成新的而跳过真正的变化。
	if p.Port != 51820 || p.Keepalive != 25 {
		t.Errorf("port=%d keepalive=%d", p.Port, p.Keepalive)
	}
	if !p.EndpointIP.Equal(net.IPv4(192, 0, 2, 7)) {
		t.Errorf("endpoint = %v", p.EndpointIP)
	}
	if len(p.AllowedIPs) != 2 || p.AllowedIPs[0].Len != 32 ||
		!p.AllowedIPs[1].IP.Equal(net.IPv4(198, 51, 100, 2)) || p.AllowedIPs[1].Len != 24 {
		t.Errorf("allowed_ips 不对: %+v", p.AllowedIPs)
	}
}

func TestPeersDecodesFrameFromRealVPP(t *testing.T) {
	frame, err := hex.DecodeString(realPeersFrame())
	if err != nil {
		t.Fatalf("夹具本身写坏了: %v", err)
	}
	if len(frame) != 109 {
		t.Fatalf("帧长 = %d，期望 109（帧头 6 + 基体 67 + 2 条 prefix）", len(frame))
	}
	f := newFake(t)
	f.peersRaw = [][]byte{frame[6:]}
	c := f.dial(t)
	peers, err := c.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("peer %d 条", len(peers))
	}
	p := peers[0]
	if p.PeerIndex != 1 || p.SWIfIndex != 3 || p.Port != 51820 || p.Keepalive != 25 {
		t.Errorf("头部字段不对: %+v", p)
	}
	if !equalBytes(p.PublicKey, bytes.Repeat([]byte{0xaa}, 32)) {
		t.Errorf("公钥不对: %x", p.PublicKey)
	}
	if !p.EndpointIP.Equal(net.IPv4(192, 0, 2, 7)) {
		t.Errorf("endpoint = %v", p.EndpointIP)
	}
	if len(p.AllowedIPs) != 2 ||
		!p.AllowedIPs[0].IP.Equal(net.IPv4(198, 51, 100, 1)) || p.AllowedIPs[0].Len != 32 ||
		!p.AllowedIPs[1].IP.Equal(net.IPv4(198, 51, 100, 2)) || p.AllowedIPs[1].Len != 24 {
		t.Errorf("allowed_ips 不对: %+v", p.AllowedIPs)
	}
	if p.Flags != 0 {
		t.Logf("flags=%d（VPP 的内部状态位，本客户端只透传不解释）", p.Flags)
	}
}

// realPeersFrame 是从 srwg-node1（VPP 26.06）抓回来的**一帧原文**：先用
// wireguard_peer_add 装一个 port=51820 / keepalive=25 / endpoint=192.0.2.7 /
// sw_if_index=3 / 两条 allowed_ip 的 peer，再用 wireguard_peers_dump 读回来。
// 拿它当夹具，是因为这些偏移只要有一个字节错，我自己写的编码器会跟着一起错，
// 只有真机的字节才能证明"读的是同一种格式"。分段写是为了让每个字段能被单独数清
// —— 一长串 hex 抄错一位，这个测试就会变成假证据。
func realPeersFrame() string {
	zero12 := strings.Repeat("00", 12)
	return "07f400000033" + // msg_id=2036, context
		"00000001" + // peer_index
		strings.Repeat("aa", 32) + // public_key
		"ca6c" + // port 51820
		"0019" + // keepalive 25
		"00000000" + // table_id
		"00" + "c0000207" + zero12 + // endpoint: af=IP4 + 192.0.2.7
		"00000003" + // sw_if_index
		"00" + // flags
		"02" + // n_allowed_ips
		"00" + "c6336401" + zero12 + "20" + // 198.51.100.1/32
		"00" + "c6336402" + zero12 + "18" // 198.51.100.2/24
}

// ==================== core 接口表（写下去的东西能不能读回来） ====================

func TestDumpInterfacesRequestIsUnfiltered(t *testing.T) {
	// 请求体只有 9 字节：sw_if_index=全部、name_filter_valid=0、变长串长度 0。
	// 长度本身就是契约 —— 少发那 4 字节的字符串长度，VPP 解码失败后**一帧都不回**，
	// 客户端只表现为超时。
	f := newFake(t)
	f.swIfDetails = 0
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if _, err := c.DumpInterfaces(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "sw_interface_dump" {
		t.Fatalf("请求名 = %s，应当是 core 的 sw_interface_dump", r.name)
	}
	if len(r.body) != 9 {
		t.Fatalf("体长 = %d，期望 9（sw_if_index 4 + name_filter_valid 1 + 变长串长度 4）", len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != ^uint32(0) {
		t.Errorf("sw_if_index = %#x，期望 0xFFFFFFFF（全部）", got)
	}
	if r.body[4] != 0 {
		t.Errorf("name_filter_valid = %d，期望 0（不用 VPP 侧过滤）", r.body[4])
	}
	if got := binary.BigEndian.Uint32(r.body[5:9]); got != 0 {
		t.Errorf("name_filter 的长度 = %d，期望 0", got)
	}
}

func TestDumpInterfacesParsesCoreLayout(t *testing.T) {
	f := newFake(t)
	f.swIfDetails = len(fakeSwInterfaces)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	got, err := c.DumpInterfaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fakeSwInterfaces) {
		t.Fatalf("读了 %d 条，期望 %d", len(got), len(fakeSwInterfaces))
	}
	for i, g := range got {
		want := fakeSwInterfaces[i]
		if g.SWIfIndex != want.idx {
			t.Errorf("[%d] sw_if_index = %d，期望 %d", i, g.SWIfIndex, want.idx)
		}
		if g.Name != want.name {
			t.Errorf("[%d] 接口名 = %q，期望 %q（偏移错了的话这里会读出一串零）", i, g.Name, want.name)
		}
		if g.L3MTU != want.mtu {
			t.Errorf("[%d] L3 MTU = %d，期望 %d", i, g.L3MTU, want.mtu)
		}
		if g.AdminUp != (want.flags&0x1 != 0) || g.LinkUp != (want.flags&0x2 != 0) {
			t.Errorf("[%d] 状态位 = admin %v link %v，期望 flags=%#x", i, g.AdminUp, g.LinkUp, want.flags)
		}
	}
	// local0 那条是"没有 MTU 记录"的样子：读回 0 必须原样是 0，
	// 不能被谁在半路填成默认值 —— agent 就是靠这个 0 认出"这张口没记录"。
	if got[0].L3MTU != 0 {
		t.Errorf("local0 的 MTU = %d，期望 0", got[0].L3MTU)
	}
}

func TestDumpInterfacesRejectsShortFrame(t *testing.T) {
	// core 的结构比插件的更容易变（每次 VPP 升级都可能加长）。收短了必须报错，
	// 不能返回一条名字为空、MTU 是垃圾的接口 —— 那会被 agent 当成"出口接口不存在"
	// 或者更糟：当成存在。
	f := newFake(t)
	f.swIfRaw = [][]byte{make([]byte, swInterfaceDetailsLen-7)}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	_, err := c.DumpInterfaces(ctx)
	if err == nil {
		t.Fatal("少 1 字节的 details 必须报错")
	}
	if !strings.Contains(err.Error(), "sw_interface_details") {
		t.Errorf("错误要点名哪条消息，got: %v", err)
	}
}

// ==================== 设备侧接口清单 ====================

func TestInventoryPairsEachInterfaceWithItsOwnAddresses(t *testing.T) {
	f := newFake(t)
	f.swIfDetails = len(fakeSwInterfaces)
	c := f.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := c.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fakeSwInterfaces) {
		t.Fatalf("读了 %d 张接口，期望 %d", len(got), len(fakeSwInterfaces))
	}
	want := map[uint32][]string{
		1: {}, // local0 没有地址
		2: {"10.10.1.2/24"},
		3: {"192.168.99.1/24"},
	}
	for _, i := range got {
		var addrs []string
		for _, p := range i.IPv4 {
			addrs = append(addrs, fmt.Sprintf("%s/%d", p.IP, p.Len))
		}
		if strings.Join(addrs, ",") != strings.Join(want[i.SWIfIndex], ",") {
			t.Errorf("接口 %s(%d) 的地址 = %v，期望 %v", i.Name, i.SWIfIndex, addrs, want[i.SWIfIndex])
		}
	}

	// 每个接口单独问一次，而且问的是**具体号**：0xFFFFFFFF 在真 VPP 上什么都不回
	// （2026-10-28 实测），所以把"全部"当遍历用会得到一份空清单而不报错。
	var asked []uint32
	for {
		select {
		case r := <-f.requests:
			if r.name != "ip_address_dump" {
				continue
			}
			if len(r.body) != 5 {
				t.Errorf("ip_address_dump 体长 = %d，期望 5（sw_if_index 4 + is_ipv6 1）", len(r.body))
			}
			if r.body[4] != 0 {
				t.Errorf("is_ipv6 = %d，期望 0（只问 IPv4）", r.body[4])
			}
			asked = append(asked, binary.BigEndian.Uint32(r.body[0:4]))
		default:
			if len(asked) != len(fakeSwInterfaces) {
				t.Fatalf("问了 %d 张接口的地址（%v），期望 %d 张，且不能是 0xFFFFFFFF",
					len(asked), asked, len(fakeSwInterfaces))
			}
			for i, idx := range asked {
				if want := fakeSwInterfaces[i].idx; idx != want {
					t.Errorf("第 %d 次问的是接口 %d，期望 %d", i, idx, want)
				}
			}
			return
		}
	}
}

func TestInventoryRejectsDetailsForAnotherInterface(t *testing.T) {
	// dump 是按号过滤的。收回别人的地址说明过滤没生效，不能当成这张口的地址
	// —— 否则两台设备的 WAN 地址会被混进同一份清单。
	f := newFake(t)
	f.swIfDetails = len(fakeSwInterfaces)
	f.ipAddrRaw = [][]byte{ipAddressDetailBytes(99, "10.0.0.9", 24)}
	c := f.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Inventory(ctx)
	if err == nil {
		t.Fatal("回了别的接口的地址必须报错")
	}
	if !strings.Contains(err.Error(), "sw_if_index=99") {
		t.Errorf("错误要点名回了谁的地址，got: %v", err)
	}
}

func TestInventoryRejectsShortAddressFrame(t *testing.T) {
	f := newFake(t)
	f.swIfDetails = len(fakeSwInterfaces)
	f.ipAddrRaw = [][]byte{make([]byte, ipAddressDetailsLen-1)}
	c := f.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Inventory(ctx)
	if err == nil {
		t.Fatal("少 1 字节的 ip_address_details 必须报错")
	}
	if !strings.Contains(err.Error(), "ip_address_details") {
		t.Errorf("错误要点名哪条消息，got: %v", err)
	}
}

// ==================== TCP MSS 钳制 ====================

func TestSetMSSClampEncodesRX(t *testing.T) {
	// 净体 10 字节：sw_if_index(4) ipv4_mss(2) ipv6_mss(2) ipv4_dir(1) ipv6_dir(1)。
	// 方向位必须是 RX=1 —— 配成 TX 时这个插件读回一切正常但一个包都不改（实测），
	// 所以这里锁的是**字节**，不是"调用发生过"。
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.SetMSSClamp(ctx, 7, 1320); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "mss_clamp_enable_disable" {
		t.Fatalf("请求名 = %s", r.name)
	}
	if len(r.body) != 10 {
		t.Fatalf("体长 = %d，期望 10", len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != 7 {
		t.Errorf("sw_if_index = %d", got)
	}
	if got := binary.BigEndian.Uint16(r.body[4:6]); got != 1320 {
		t.Errorf("ipv4_mss = %d", got)
	}
	if got := binary.BigEndian.Uint16(r.body[6:8]); got != 0 {
		t.Errorf("ipv6_mss = %d，overlay 只有 IPv4，不该动它", got)
	}
	if r.body[8] != MSSClampDirRX {
		t.Errorf("ipv4_direction = %d，期望 RX=%d（TX 在 wg0 上不生效）", r.body[8], MSSClampDirRX)
	}
	if r.body[9] != MSSClampDirNone {
		t.Errorf("ipv6_direction = %d，期望 NONE", r.body[9])
	}
}

func TestSetMSSClampRejectsZeroAndPropagatesRetval(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.SetMSSClamp(ctx, 7, 0); err == nil {
		t.Fatal("MSS=0 必须在发出之前拦掉")
	}

	f.retvals["mss_clamp_enable_disable"] = -2
	if err := c.SetMSSClamp(ctx, 99, 1320); err == nil {
		t.Fatal("retval 非 0 必须上报")
	}
}

func TestMSSClampReadsBack(t *testing.T) {
	// 明细净体 10 字节（消息头之外）：sw_if_index(4) ipv4_mss(2) ipv6_mss(2) 两个方向(1+1)。
	detail := make([]byte, 10)
	binary.BigEndian.PutUint32(detail[0:4], 7)
	binary.BigEndian.PutUint16(detail[4:6], 1320)
	binary.BigEndian.PutUint16(detail[6:8], 0)
	detail[8] = MSSClampDirRX
	detail[9] = MSSClampDirNone

	f := newFake(t)
	f.mssDetails = [][]byte{detail}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	info, ok, err := c.MSSClamp(ctx, 7)
	if err != nil || !ok {
		t.Fatalf("读回失败: ok=%v err=%v", ok, err)
	}
	if info.SWIfIndex != 7 || info.IPv4MSS != 1320 || info.IPv4Dir != MSSClampDirRX {
		t.Errorf("读回 = %+v", info)
	}
	r := f.lastRequest(t)
	if r.name != "mss_clamp_get" || len(r.body) != 8 {
		t.Fatalf("请求 = %s 体长 %d，期望 mss_clamp_get + 8（cursor 4 + sw_if_index 4）", r.name, len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body[4:8]); got != 7 {
		t.Errorf("服务端过滤的 sw_if_index = %d，期望 7", got)
	}

	// 没有记录：ok=false，不是错误 —— 调用方（agent）把它当成"钳制没落地"来报。
	info, ok, err = c.MSSClamp(ctx, 8)
	if err != nil || ok {
		t.Errorf("查一个没配过的接口应当 ok=false err=nil，得到 ok=%v err=%v info=%+v", ok, err, info)
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
