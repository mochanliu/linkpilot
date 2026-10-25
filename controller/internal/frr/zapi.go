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
const (
	zapiHeaderSize   = 10
	zapiHeaderMarker = 0xfe
	zservVersion     = 6

	cmdRouteAdd    uint16 = 9
	cmdRouteDelete uint16 = 10
	cmdHello       uint16 = 19
	cmdError       uint16 = 120

	// routeTypeTable 是 ZEBRA_ROUTE_TABLE：非主内核表里的路由，由 zebra
	// 自己维护（route_types.txt 里的第 17 个类型）。客户侧用
	// `redistribute table-direct N` 订阅它。
	routeTypeTable uint8 = 16

	// protoSharp 只是"我是谁"的标识。zebra 不检查客户端有没有资格发
	// TABLE 路由；用 sharp 是因为它没有真正的守护进程会和我们抢。
	protoSharp uint8 = 24

	safiUnicast uint8 = 1
	familyIPv4  uint8 = 2

	nhTypeBlackhole uint8 = 6
	bhTypeNull      uint8 = 1

	flagAllowRecursion uint32 = 0x01
	flagRRUseDistance  uint32 = 0x40
	flagTableID        uint32 = 0x800

	msgNexthop  uint32 = 0x01
	msgDistance uint32 = 0x02
	msgMetric   uint32 = 0x04
	msgTableID  uint32 = 0x0100
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
