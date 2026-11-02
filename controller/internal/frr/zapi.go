// Package frr 是 CPE 上 SD-WAN 路由与客户侧 FRR 之间的接缝。两个方向各走
// 一条不同的路，都是 FRR 10.7.1 + linux-cp 的实测结论：
//
//   - **SDWAN → 客户**：sr-route 的 RIB 按目标 VPN 直写盒子内核的对应表
//     （internal/frr/krt；VPN 0 = main/254，其余 = vpn<N> 的表 N），出口只用
//     lo / vpn<N>。linux-nl 不镜像这类路由，VPP 完全看不到；zebra 把它们认成
//     K（distance 0），客户 VPN 里的 `redistribute kernel route-map` 再把它们
//     发给客户路由器。为什么不用 ZEBRA_ROUTE_TABLE：那条路会在 VPP 里镜像出
//     一张全 dpo-drop 的死表，对转发毫无用处（docs/o6e-leak-plan.md §1）。
//   - **客户 → SDWAN**：本文件的 ZAPI 部分订阅 zebra 的再分发通知，把客户侧
//     学到的前缀交给 Importer。ZAPI 头里的 vrf_id 是**内核 VRF 设备的
//     ifindex**（实测 vpn100 → 10，默认 VRF 是 0），不是 VPN 号；订阅也必须
//     按 VRF 各发一条。
//
// 协议细节以 FRR 10.7.1 的源码为准。这个包与仓库里其它 Go 代码一样零第三方
// 依赖，因为它要和 VPP 的二进制 API 一样只依赖对端版本这一个事实。
package frr

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// ZAPI 线格式常量。版本号与命令号取自 FRR 10.7.1 的 lib/zclient.h，
// 跨大版本会变 —— 这也是把 FRR 版本 pin 在配置/镜像里的原因。
// 通知命令号不是"导出那条 +18"：enum 里还夹着 CAPABILITIES/BFD_REPLAY，
// 实测 zebra 发来的是 31/32（拿错一个会静默丢整条反方向的路由）。
const (
	zapiHeaderSize   = 10
	zapiHeaderMarker = 0xfe
	zservVersion     = 6

	cmdRedistAdd      uint16 = 12
	cmdHello          uint16 = 19
	cmdRedistRouteAdd uint16 = 31
	cmdRedistRouteDel uint16 = 32
	cmdError          uint16 = 120

	// routeTypeBGP 是我们反方向订阅的类型：客户侧路由器学来的前缀在
	// zebra 的 RIB 里是 BGP 路由。
	routeTypeBGP uint8 = 10

	// protoSharp 只是"我是谁"的标识；订阅再分发不需要真实守护进程的身份，
	// 用 sharp 是因为没有真守护进程会和我们抢。
	protoSharp uint8 = 24

	safiUnicast uint8 = 1
	familyIPv4  uint8 = 2
	afiIPv4     uint8 = 1

	nhFlagLabel     uint8 = 0x02
	nhFlagWeight    uint8 = 0x04
	nhFlagBackup    uint8 = 0x08
	nhFlagEvpn      uint8 = 0x40
	nhFlagSeg6      uint8 = 0x10
	nhFlagSeg6Local uint8 = 0x20

	msgNexthop  uint32 = 0x01
	msgDistance uint32 = 0x02
	msgMetric   uint32 = 0x04
	msgTag      uint32 = 0x08
	msgMTU      uint32 = 0x10
	msgSrcPfx   uint32 = 0x20
	msgBackup   uint32 = 0x40
	msgNHG      uint32 = 0x80
	msgTableID  uint32 = 0x0100
	msgSRTE     uint32 = 0x0200
	msgOpaque   uint32 = 0x0400
)

// zapiBuf 是只往前追加的最小写入器；字段顺序错了就是线上协议错了，
// 所以所有编码集中在这一个文件里。
type zapiBuf struct {
	b []byte
}

func (w *zapiBuf) u8(v uint8)   { w.b = append(w.b, v) }
func (w *zapiBuf) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *zapiBuf) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *zapiBuf) raw(p []byte) { w.b = append(w.b, p...) }

// message 在 payload 前面补上 ZAPI 头。头里的 vrf_id 是这条消息所属的
// **FRR VRF**（内核 VRF 设备的 ifindex），不是控制面的 VPN 号；当前导入只
// 订默认 VRF（0），多 VRF 的 ifindex 映射在订阅侧完成。
func message(cmd uint16, vrfID uint32, payload []byte) []byte {
	w := zapiBuf{b: make([]byte, 0, zapiHeaderSize+len(payload))}
	w.u16(uint16(zapiHeaderSize + len(payload)))
	w.u8(zapiHeaderMarker)
	w.u8(zservVersion)
	w.u32(vrfID)
	w.u16(cmd)
	w.raw(payload)
	return w.b
}

// encodeHello 是连接建立后的第一帧。instance 用 0；notify_owner 关掉 ——
// 我们只在轮询里重算整份计划，不消费 zebra 的单条变更通知。
func encodeHello(instance uint16) []byte {
	var p zapiBuf
	p.u8(protoSharp)
	p.u16(instance)
	p.u32(0) // session_id
	p.u8(0)  // notify_owner
	return message(cmdHello, 0, p.b)
}

// encodeRedistributeAdd 订阅某类路由的再分发通知。zebra 收到后会先把该类型
// 已经选中的路由整份回放一遍，再逐条推增量 —— 重连后不需要另外要快照。
func encodeRedistributeAdd(afi, routeType uint8, instance uint16) []byte {
	var p zapiBuf
	p.u8(afi)
	p.u8(routeType)
	p.u16(instance)
	return message(cmdRedistAdd, 0, p.b)
}

// RouteEvent 是 zebra 再分发通知里我们关心的部分：前缀与增删。
// 下一跳、tag 等字段解码时跳过 —— 反向导入只关心"客户侧宣告了哪些前缀"。
type RouteEvent struct {
	Prefix netip.Prefix
	Add    bool
}

// zapiReader 是边界受检的读取器：任何字段越界都会把 err 置上，后续读取
// 变成空操作。线格式与 FRR 10.7.1 的 zapi_route_decode 逐字段对应。
type zapiReader struct {
	b   []byte
	off int
	err error
}

func (r *zapiReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.off+n > len(r.b) {
		r.err = fmt.Errorf("ZAPI 再分发通知被截断（需要 %d 字节，剩 %d）", n, len(r.b)-r.off)
		return nil
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p
}

func (r *zapiReader) u8() uint8 {
	p := r.take(1)
	if p == nil {
		return 0
	}
	return p[0]
}

func (r *zapiReader) u16() uint16 {
	p := r.take(2)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint16(p)
}

func (r *zapiReader) u32() uint32 {
	p := r.take(4)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint32(p)
}

func (r *zapiReader) u64() uint64 {
	p := r.take(8)
	if p == nil {
		return 0
	}
	return binary.BigEndian.Uint64(p)
}

const knownMessageFlags = msgNexthop | msgDistance | msgMetric | msgTag | msgMTU |
	msgSrcPfx | msgBackup | msgNHG | msgTableID | msgSRTE | msgOpaque

// decodeRedistributeRoute 解一条 ZEBRA_REDISTRIBUTE_ROUTE_ADD/DEL 的 payload。
func decodeRedistributeRoute(body []byte, add bool) (RouteEvent, error) {
	var ev RouteEvent
	r := &zapiReader{b: body}
	typ := r.u8()
	_ = r.u16() // instance
	_ = r.u32() // route flags
	message := r.u32()
	safi := r.u8()
	family := r.u8()
	plen := r.u8()
	if r.err != nil {
		return ev, r.err
	}
	if typ != routeTypeBGP {
		return ev, fmt.Errorf("再分发通知的路由类型是 %d，不是 BGP", typ)
	}
	if safi != safiUnicast || family != familyIPv4 {
		return ev, fmt.Errorf("再分发通知是 safi %d family %d，只处理 IPv4 unicast", safi, family)
	}
	if plen > 32 {
		return ev, fmt.Errorf("再分发通知的 IPv4 前缀长度 %d 非法", plen)
	}
	if unknown := message &^ knownMessageFlags; unknown != 0 {
		return ev, fmt.Errorf("再分发通知带了不认识的 message 标志 0x%x", unknown)
	}
	var a [4]byte
	copy(a[:], r.take((int(plen)+7)/8))
	if r.err != nil {
		return ev, r.err
	}
	ev.Prefix = netip.PrefixFrom(netip.AddrFrom4(a), int(plen)).Masked()
	ev.Add = add

	if message&msgSrcPfx != 0 {
		splen := r.u8()
		r.take((int(splen) + 7) / 8)
	}
	if message&msgNHG != 0 {
		_ = r.u32() // nhgid
	}
	if message&msgNexthop != 0 {
		n := r.u16()
		for i := 0; i < int(n) && r.err == nil; i++ {
			decodeNexthop(r, message)
		}
	}
	if message&msgBackup != 0 {
		n := r.u16()
		for i := 0; i < int(n) && r.err == nil; i++ {
			decodeNexthop(r, message)
		}
	}
	if message&msgDistance != 0 {
		_ = r.u8()
	}
	if message&msgMetric != 0 {
		_ = r.u32()
	}
	if message&msgTag != 0 {
		_ = r.u32()
	}
	if message&msgMTU != 0 {
		_ = r.u32()
	}
	if message&msgTableID != 0 {
		_ = r.u32()
	}
	if message&msgOpaque != 0 {
		r.take(int(r.u16()))
	}
	if r.err != nil {
		return RouteEvent{}, r.err
	}
	if r.off != len(body) {
		return RouteEvent{}, fmt.Errorf("再分发通知解完后还剩 %d 字节（线格式变了？）", len(body)-r.off)
	}
	return ev, nil
}

func decodeNexthop(r *zapiReader, message uint32) {
	_ = r.u32() // vrf_id
	typ := r.u8()
	flags := r.u8()
	switch typ {
	case 1: // IFINDEX
		_ = r.u32()
	case 2, 3: // IPV4 / IPV4_IFINDEX
		r.take(4)
		_ = r.u32()
	case 4, 5: // IPV6 / IPV6_IFINDEX
		r.take(16)
		_ = r.u32()
	case 6: // BLACKHOLE
		_ = r.u8()
	default:
		r.err = fmt.Errorf("再分发通知里有未知的下一跳类型 %d", typ)
		return
	}
	if flags&nhFlagLabel != 0 {
		n := r.u8()
		_ = r.u8() // label type
		r.take(int(n) * 4)
	}
	if flags&nhFlagWeight != 0 {
		_ = r.u64()
	}
	if flags&nhFlagEvpn != 0 {
		r.take(6)
	}
	if message&msgSRTE != 0 {
		_ = r.u32()
	}
	if flags&nhFlagBackup != 0 {
		n := r.u8()
		r.take(int(n))
	}
	if flags&(nhFlagSeg6|nhFlagSeg6Local) != 0 {
		// SRv6 的字段长度取决于内部结构；客户侧普通 BGP 路由不会带。
		// 与其猜错把后面的字段全解歪，不如明确拒绝。
		r.err = fmt.Errorf("再分发通知带了 SRv6 下一跳（flags 0x%x），暂不支持", flags)
	}
}
