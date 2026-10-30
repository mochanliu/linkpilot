package vppapi

// Core interface plumbing: the four messages an agent needs to build the
// overlay side of a tunnel interface. They live here rather than in messages.go
// because they are `vnet/interface.api` and `vnet/mpls.api`, not the sr_wg
// plugin -- exactly the split the MTU/MSS helpers already follow.
//
// CRCs are taken from srwg-node1, which carries this VPP build's own generated
// headers (/usr/include/vnet/interface.api_types.h and mpls.api_types.h), and
// cross-checked against the _CRC macros in the sibling .api.h files:
//
//	sw_interface_set_flags          f5aec1b8   reply e8d4e804
//	sw_interface_add_del_address    5463d73b   reply e8d4e804
//	sw_interface_set_table          df42a577   reply e8d4e804
//	sw_interface_set_mpls_enable    ae6cfcfb   reply e8d4e804
//
// （feature_is_enabled 55db09e2/reply 03f284b5 本来在这张表里：它的 CRC 与 VPP 消息表
// 一致，可这条 API 在这份 26.06 上不回帧，四次请求全部超时，2026-10-30 实测之后退回
// 用 CLI 读弧 —— 见 MPLSEnabled 的注释。）
//
// The packed layouts matter more than they look: flags and vrf ids are NOT
// u8-sized even though they are "one small number", and getting one wrong shifts
// everything after it, which VPP answers with a timeout rather than an error.

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// SetInterfaceUp brings a tunnel interface admin-up (or down).
//
// An interface that is admin-down still accepts addresses, MTU and MPLS config,
// but forwards nothing -- so leaving it down produces the classic "configured
// everywhere, zero traffic" state. bringup.sh sets the state explicitly and the
// agent does the same for the same reason.
func (c *Client) SetInterfaceUp(ctx context.Context, swIfIndex uint32, up bool) error {
	// vl_api_sw_interface_set_flags_t 净体 = sw_if_index(4) + flags(4)。
	// vl_api_if_status_flags_t 在这里占 4 字节（枚举，packed 结构不压缩它）。
	body := make([]byte, 0, 8)
	body = appendU32(body, swIfIndex)
	var flags uint32
	if up {
		flags |= ifStatusAdminUp
	}
	body = appendU32(body, flags)
	frame, err := c.call(ctx, "sw_interface_set_flags", "sw_interface_set_flags_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sw_interface_set_flags", frame)
}

// ifStatusAdminUp 是 IF_STATUS_API_FLAG_ADMIN_UP（interface_types.api）。
//
// 只发 ADMIN_UP：LINK_UP 由 VPP 按接口自身状态置位，客户端写它没有意义。
const ifStatusAdminUp uint32 = 0x1

// AddDelInterfaceAddress 在一张接口上加/删一个 IPv4 地址（含前缀长度）。
//
// 前缀必须是 /32 之类的显式值，不能只给地址：overlay 用的就是 /32，而
// vl_api_address_with_prefix_t 带一个前缀长度字节，省掉它等于加了一条 /0，
// VPP 不会报错，路由表里会多出一条覆盖整个地址空间的 connected 路由。
func (c *Client) AddDelInterfaceAddress(ctx context.Context, swIfIndex uint32, prefix netip.Prefix, add bool) error {
	if !prefix.Addr().Is4() {
		return fmt.Errorf("vppapi: AddDelInterfaceAddress 只做 IPv4，收到 %s", prefix)
	}
	// vl_api_sw_interface_add_del_address_t 净体：
	// sw_if_index(4) + is_add(1) + del_all(1) + vl_api_address_with_prefix_t(18)
	body := make([]byte, 0, 24)
	body = appendU32(body, swIfIndex)
	body = append(body, boolByte(add), 0) // is_add, del_all
	body = appendIPv4Prefix(body, prefix)
	frame, err := c.call(ctx, "sw_interface_add_del_address", "sw_interface_add_del_address_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sw_interface_add_del_address", frame)
}

// SetInterfaceIPTable 把一张接口放进某张 VRF 表。
//
// VPP 拒绝给**带地址**的接口换表（实测 CLI 回 "interface must be unnumbered
// /address-free"），所以调用方必须先摘地址、再换表、最后把地址加回新表。
// 这三步的顺序是硬约束，任何"先换表再说"的写法都会在这一步失败。
func (c *Client) SetInterfaceIPTable(ctx context.Context, swIfIndex uint32, tableID uint32) error {
	// vl_api_sw_interface_set_table_t 净体 = sw_if_index(4) + is_ipv6(1) + vrf_id(4)。
	body := make([]byte, 0, 9)
	body = appendU32(body, swIfIndex)
	body = append(body, 0) // is_ipv6：overlay 至今只有 IPv4
	body = appendU32(body, tableID)
	frame, err := c.call(ctx, "sw_interface_set_table", "sw_interface_set_table_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sw_interface_set_table", frame)
}

// SetInterfaceMPLS 打开/关闭一张接口的 MPLS 处理。
//
// 只影响**标签**方向（VPN 的内层 MPLS 帧）：没有它，收端 wg4-input 解出标签帧
// 之后在接口上没有 mpls-input 可进，包被丢在这一跳。overlay 的纯 IPv4 转发
// 不依赖它，所以少配这一条的隧道"能 ping 通、跑不了 VPN 流量"。
func (c *Client) SetInterfaceMPLS(ctx context.Context, swIfIndex uint32, enable bool) error {
	// vl_api_sw_interface_set_mpls_enable_t 净体 = sw_if_index(4) + enable(1)。
	body := make([]byte, 0, 5)
	body = appendU32(body, swIfIndex)
	body = append(body, boolByte(enable))
	frame, err := c.call(ctx, "sw_interface_set_mpls_enable", "sw_interface_set_mpls_enable_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sw_interface_set_mpls_enable", frame)
}

// MPLSEnabled 回答"这张口的 mpls-input 弧现在还能丢标签包吗"：true = 弧已摘掉
// 丢弃路径，带标签的帧能进 MPLS 转发；false = 弧上还挂着 mpls-not-enabled。
//
// 为什么必须单独问弧：`sw_interface_set_mpls_enable` 在一张**继承了别人 sw_if_index**
// 的口上是静默空操作（VPP 的 MPLS 开关按 index 记引用计数、只在 0↔1 翻转时才动弧，
// 而计数不随接口删除清零），retval 照样是 0。而 `show mpls interface` 读的正是那份
// 计数，所以它也撒谎：2026-10-30 在 lab-cpe1 上 wg1001 被列成 "MPLS enabled"，弧上
// 却还挂着 mpls-not-enabled（同一次测量里 loop0 enable 之后弧变成 none configured，
// disable 又变回来 —— 弧这一段是唯一说真话的）。判据与坑的来龙去脉见
// docs/overlay-design-draft.md §2.8。
//
// 为什么走 CLI 而不是 feature_is_enabled 那条 API：这条 API 在这份 VPP 26.06 上
// **根本不回帧**（同日实测：消息表里它的 CRC 与我们注册的一致，所以不是版本对不上，
// 但 local0/wg0..wg1003 四次请求全部超时）。`show interface features` 的 mpls-input
// 段是现成、可读、且已经被 harness 用同一条判据验过的口子：
//
//	mpls-input:
//	  none configured      ← 开好了
//	  mpls-not-enabled     ← 还挂着，带标签的包到这里就丢
//
// 注意 CLI 会把错误吞成 retval=0（见 Client.CLI 的注释），所以"读不到 mpls-input 段"
// 一律当错误返回，不能当成"没开"。
func (c *Client) MPLSEnabled(ctx context.Context, swIfIndex uint32) (bool, error) {
	out, err := c.CLI(ctx, "show interface features "+strconv.FormatUint(uint64(swIfIndex), 10))
	if err != nil {
		return false, fmt.Errorf("vppapi: 读接口 %d 的 feature 弧: %w", swIfIndex, err)
	}
	lines := strings.Split(out, "\n")
	for i, raw := range lines {
		if strings.TrimSpace(raw) != "mpls-input:" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			body := strings.TrimSpace(lines[j])
			if body == "" {
				continue
			}
			return !strings.Contains(body, "mpls-not-enabled"), nil
		}
		// 段头是最后一行：这份输出被截断了，宁可报错也不猜。
		return false, fmt.Errorf("vppapi: 接口 %d 的 mpls-input 段没有正文: %q", swIfIndex, out)
	}
	return false, fmt.Errorf("vppapi: 接口 %d 的输出里没有 mpls-input 段: %q", swIfIndex, out)
}

// IPv4Addresses 读一张接口上的 IPv4 地址，按 netip 前缀返回。
//
// 这是 AddDelInterfaceAddress 的读回面：地址加没加上、加在哪张前缀下，只有
// dump 说得清（retval=0 只代表 VPP 收下了请求）。注意 ip_address_details 不带
// 表号 —— 地址在不在接口上，和地址属于哪张表是两个问题，后者只能查 FIB。
func (c *Client) IPv4Addresses(ctx context.Context, swIfIndex uint32) ([]netip.Prefix, error) {
	ps, err := c.DumpIPv4Addresses(ctx, swIfIndex)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		a, ok := netip.AddrFromSlice(p.IP.To4())
		if !ok {
			return nil, fmt.Errorf("vppapi: 接口 %d 读回了非 IPv4 地址 %v", swIfIndex, p.IP)
		}
		out = append(out, netip.PrefixFrom(a, int(p.Len)))
	}
	return out, nil
}
