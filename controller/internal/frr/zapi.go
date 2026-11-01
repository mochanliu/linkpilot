// Package frr 把控制面的远端前缀送进 FRR，且**不碰内核主表**。
//
// 这是 CPE 上 SD-WAN 与客户路由之间的接缝：控制面（sr-route）只维护 RIB，
// 本包把它翻译成 FRR 能 redistribiute 的"非主表路由"，客户的 BGP/OSPF
// 再按自己的策略把这些前缀发给站点内的设备。三个决定都是 FRR 10.7.1 +
// linux-cp 的实测结果：
//
//   - **用 ZEBRA_ROUTE_TABLE（type 16）+ tableid 停在非主内核表 N**。
//     FRR 的 table 路由最终会（经 zebra）装进内核表 N，而 linux-cp 只把
//     内核表 N 同步成 VPP table N —— N > 255 时就与 sr-fib 的 VRF 表
//     （1..255）不相交，VPP 主表（table 0）也不会被污染。若走 `ip import-table`
//     或把路由放进主表，linux_nl 会把它搬进 VPP table 0，和 sr-fib 的 MPLS
//     路由叠在一起 —— 那是两条互相看不见的转发路径，排障时会很难看。
//   - **黑洞下一跳（NEXTHOP_TYPE_BLACKHOLE / BLACKHOLE_NULL）**。表里的路由
//     只用于再分发，不用于本机转发；黑洞让"由我发起"这句话确定成立：BGP
//     邻居看到的下一跳是本机（next-hop-self），OSPF AS-external 的转发地址
//     是 0.0.0.0，都不依赖 overlay /32 是否可达。
//   - **客户侧实例自己 redistribute**。本包只写表；`redistribute table-direct N`
//     是客户路由进程的策略，BGP/OSPF、带不带 route-map 都由那侧决定。
//
// 协议细节以 FRR 10.7.1 的源码为准（zclient.c zapi_route_encode）。这个包
// 与仓库里其它 Go 代码一样零第三方依赖，因为它要和 VPP 的二进制 API 一样
// 只依赖对端版本这一个事实。
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

	cmdRouteAdd       uint16 = 9
	cmdRouteDelete    uint16 = 10
	cmdRedistAdd      uint16 = 12
	cmdHello          uint16 = 19
	cmdRedistRouteAdd uint16 = 31
	cmdRedistRouteDel uint16 = 32
	cmdError          uint16 = 120

	// routeTypeTable 是 ZEBRA_ROUTE_TABLE：非主内核表里的路由，由 zebra
	// 自己维护（route_types.txt 里的第 17 个类型）。客户侧用
	// `redistribute table-direct N` 订阅它。
	routeTypeTable uint8 = 16

	// routeTypeBGP 是我们反方向订阅的类型：客户侧路由器学来的前缀在
	// zebra 的 RIB 里是 BGP 路由。
	routeTypeBGP uint8 = 10

	// protoSharp 只是"我是谁"的标识。zebra 不检查客户端有没有资格发
	// TABLE 路由；用 sharp 是因为它没有真正的守护进程会和我们抢。
	protoSharp uint8 = 24

	safiUnicast uint8 = 1
	familyIPv4  uint8 = 2
	afiIPv4     uint8 = 1

	nhTypeBlackhole uint8 = 6
	bhTypeNull      uint8 = 1

	nhFlagLabel     uint8 = 0x02
	nhFlagWeight    uint8 = 0x04
	nhFlagBackup    uint8 = 0x08
	nhFlagEvpn      uint8 = 0x40
	nhFlagSeg6      uint8 = 0x10
	nhFlagSeg6Local uint8 = 0x20

	flagAllowRecursion uint32 = 0x01
	flagRRUseDistance  uint32 = 0x40
	flagTableID        uint32 = 0x800

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

// Route 是一条要停进 FRR 非主表的前缀。
//
// Prefix 必须是已规范的 IPv4 前缀：ZAPI 的 family 字段只描述地址族，
// 掩码外的位会被 zebra 原样收下，最后在内核里变成一条莫名其妙的表项。
type Route struct {
	Prefix   netip.Prefix `json:"prefix"`
	TableID  uint32       `json:"table_id"`
	Distance uint8        `json:"distance"`
	Metric   uint32       `json:"metric"`
}

func (r Route) key() string {
	return fmt.Sprintf("%d|%s", r.TableID, r.Prefix)
}

// zapiBuf 是只往前追加的最小写入器；字段顺序错了就是线上协议错了，
// 所以所有编码集中在这一个文件里。
type zapiBuf struct {
	b []byte
}

func (w *zapiBuf) u8(v uint8)   { w.b = append(w.b, v) }
func (w *zapiBuf) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *zapiBuf) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *zapiBuf) raw(p []byte) { w.b = append(w.b, p...) }

// message 在 payload 前面补上 ZAPI 头。头里的 vrf_id 选的是 **FRR 的 VRF**，
// 不是控制面的 VRF：我们把所有表都挂在默认 VRF 下，用 tableid 区分来源；
// 控制面的 VRF 只决定"哪张表"，由调用方在 Route.TableID 里体现。
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

// encodeRoute 编一条 ROUTE_ADD / ROUTE_DELETE。两种命令的 payload 完全相同：
// zebra 按 (type, instance, prefix, tableid) 定位要删的条目，distance/metric
// 不参与匹配，但一起带上省得两份编码器各错一次。
func encodeRoute(cmd uint16, r Route) ([]byte, error) {
	if !r.Prefix.IsValid() {
		return nil, fmt.Errorf("frr: 前缀无效: %s", r.Prefix)
	}
	addr := r.Prefix.Addr()
	if !addr.Is4() || addr.Is4In6() {
		return nil, fmt.Errorf("frr: 只导出 IPv4 前缀，收到 %s", r.Prefix)
	}
	if r.Prefix != r.Prefix.Masked() {
		return nil, fmt.Errorf("frr: 前缀未规范（掩码外有有效位）: %s", r.Prefix)
	}
	if r.TableID == 0 {
		return nil, fmt.Errorf("frr: 表号不能为 0（内核主表）")
	}
	plen := r.Prefix.Bits()

	var p zapiBuf
	p.u8(routeTypeTable)
	p.u16(0) // instance
	p.u32(flagAllowRecursion | flagRRUseDistance | flagTableID)
	p.u32(msgNexthop | msgDistance | msgMetric | msgTableID)
	p.u8(safiUnicast)
	p.u8(familyIPv4)
	p.u8(uint8(plen))
	a4 := addr.As4()
	p.raw(a4[:(plen+7)/8])

	// 下一跳数量是 uint16（不是 uint8）—— 这是线格式里最容易写错的一处。
	p.u16(1)
	p.u32(0) // 下一跳所在 VRF：默认表
	p.u8(nhTypeBlackhole)
	p.u8(0) // 下一跳 flags
	p.u8(bhTypeNull)

	p.u8(r.Distance)
	p.u32(r.Metric)
	p.u32(r.TableID)

	return message(cmd, 0, p.b), nil
}
