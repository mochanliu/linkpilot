package vppapi

// 隧道内遥测的两个数据面接口：
//
//   - 自研探针（sr_wg_probe_*，UDP 51829）：RTT / 抖动 / 丢包；
//   - core BFD（bfd_udp_*）：可达性（Up/Down）。
//
// 两者的会话键**不一样**，这不是疏忽：
//   - 探针按 sw_if_index 建（插件自己的表，每隧道一条 WG 接口）；
//   - BFD 一律用**多跳**会话（sw_if_index = ~0），只按 (local, peer) 寻址。
//
// 为什么 BFD 不能绑接口：2026-10-24 在实验室实测（干净重启后复现三次），
// `bfd udp session add interface wg0 ...` 在对端没有 BFD 会话时，会把
// 192.168.99.2/32 这条 overlay 路由的 forwarding DPO 从 WG midchain 邻接换成
// `dpo-drop`，隧道两个方向 100% 不通；`show ip fib` 的路径看着仍然完好，
// 只有 `forwarding:` 那一行能看出桶变成了 drop。同一会话改成 multihop
// 就完全正常（单端也不影响转发，两端齐了状态能到 up）。BFD 的语义本来也
// 支持这个选择：overlay 对端不是直连邻居，是穿过 tunnel 才到的，多跳才是
// 它真正在量的东西（RFC 5883）。
//
// 所以上层合成一张视图时按 (local_ip, peer_ip) 对齐两张表，而不是 sw_if_index。
// BFD 一点质量指标都不给（details 里只有状态与间隔），这也是遥测必须自研探针
// 的原因。
//
// 多跳是 VPP 的**全局开关**：先发 bfd_udp_enable_multihop，否则 sw_if_index=~0
// 的 add/del 都被当成非法接口（retval=-2）。Add/Del 每次都先发这一条。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ==================== 自研探针 ====================

// ProbeParams 是一条隧道上的探针会话。LocalIP 留空时由插件取该接口的第一个
// IPv4（正常就是 overlay /32）；TimeoutMS=0 时插件按 interval×3 算。
type ProbeParams struct {
	SwIfIndex  uint32
	PeerIP     net.IP
	LocalIP    net.IP
	IntervalMS uint32
	TimeoutMS  uint32
}

// ProbeStats 是探针会话的一次快照。累计计数（Tx/Rx/Lost/Late）由 agent 换算
// 增量；LossPermille 是插件里 128 槽滑窗的当前值。
type ProbeStats struct {
	SwIfIndex  uint32
	PeerIP     net.IP
	LocalIP    net.IP
	IntervalMS uint32
	TimeoutMS  uint32
	Enabled    bool

	Tx          uint64
	Rx          uint64
	Lost        uint64
	LateReplies uint64

	LastRTTus    uint32
	MinRTTus     uint32
	AvgRTTus     uint32
	MaxRTTus     uint32
	JitterUs     uint32
	LossPermille uint32
}

// SetProbe 建/改一条探针会话（幂等：同 sw_if_index 原地更新）。
// 换了 peer/local 地址会重置统计 —— 新旧地址混在一起的 RTT 没有意义。
func (c *Client) SetProbe(ctx context.Context, p ProbeParams) error {
	return c.probeSet(ctx, 1, p)
}

// DelProbe 删一条探针会话。不存在也算成功（控制器对账不必先 dump）。
func (c *Client) DelProbe(ctx context.Context, swIfIndex uint32) error {
	return c.probeSet(ctx, 0, ProbeParams{SwIfIndex: swIfIndex})
}

func (c *Client) probeSet(ctx context.Context, isAdd uint8, p ProbeParams) error {
	if isAdd != 0 {
		if p.PeerIP.To4() == nil {
			return fmt.Errorf("vppapi: 探针对端地址必须是 IPv4，给了 %v", p.PeerIP)
		}
		if p.LocalIP != nil && p.LocalIP.To4() == nil {
			return fmt.Errorf("vppapi: 探针本地地址必须是 IPv4，给了 %v", p.LocalIP)
		}
	}
	// peer_ip/local_ip 在 .api 里是定长 vl_api_ip4_address_t（不是可选字段）：
	// 删会话时插件不看这两个值，但消息长度必须够，否则 VPP 按 Truncated message
	// 丢弃整帧，表现是"删了但还在 dump 里"。
	body := make([]byte, 0, 21)
	body = append(body, isAdd)
	body = appendU32(body, p.SwIfIndex)
	body = appendIP4Fixed(body, p.PeerIP)
	body = appendIP4Fixed(body, p.LocalIP)
	body = appendU32(body, p.IntervalMS)
	body = appendU32(body, p.TimeoutMS)
	frame, err := c.call(ctx, "sr_wg_probe_set", "sr_wg_probe_set_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_probe_set", frame)
}

// appendIP4Fixed 写满 4 字节的 vl_api_ip4_address_t：nil 编成 0.0.0.0。
// appendIP4 对 nil 一个字节都不写（其余消息的字段确实可选），这里不能这么用。
func appendIP4Fixed(b []byte, ip net.IP) []byte {
	var v4 [4]byte
	if x := ip.To4(); x != nil {
		copy(v4[:], x)
	}
	return append(b, v4[:]...)
}

// SendProbe 立刻补发一个探针（CLI/e2e 用来确认路径，不等定时器）。
func (c *Client) SendProbe(ctx context.Context, swIfIndex uint32) error {
	frame, err := c.call(ctx, "sr_wg_probe_send", "sr_wg_probe_send_reply",
		appendU32(nil, swIfIndex))
	if err != nil {
		return err
	}
	return c.retval("sr_wg_probe_send", frame)
}

// Probes 读回全部探针会话。空表返回空切片而不是错误（dump 对空结果一帧不发）。
func (c *Client) Probes(ctx context.Context) ([]ProbeStats, error) {
	frames, err := c.dump(ctx, "sr_wg_probe_dump", "sr_wg_probe_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]ProbeStats, 0, len(frames))
	for _, f := range frames {
		// vl_api_sr_wg_probe_details_t 是 packed 结构，字段依次紧排：
		//   msg_id(2) context(4) sw_if_index(4) peer_ip(4) local_ip(4)
		//   interval_ms(4) timeout_ms(4) enabled(1)
		//   tx(8) rx(8) lost(8) late(8)
		//   last/min/avg/max/jitter(各4) loss_permille(4)
		// 合计 83 字节；u64 前没有对齐填充（packed），别按 C 的自然对齐去算。
		if len(f) < 83 {
			return nil, fmt.Errorf("vppapi: sr_wg_probe_details 太短 (%d B)，插件结构变了", len(f))
		}
		out = append(out, ProbeStats{
			SwIfIndex:    binary.BigEndian.Uint32(f[6:10]),
			PeerIP:       net.IP(append([]byte(nil), f[10:14]...)),
			LocalIP:      net.IP(append([]byte(nil), f[14:18]...)),
			IntervalMS:   binary.BigEndian.Uint32(f[18:22]),
			TimeoutMS:    binary.BigEndian.Uint32(f[22:26]),
			Enabled:      f[26] != 0,
			Tx:           binary.BigEndian.Uint64(f[27:35]),
			Rx:           binary.BigEndian.Uint64(f[35:43]),
			Lost:         binary.BigEndian.Uint64(f[43:51]),
			LateReplies:  binary.BigEndian.Uint64(f[51:59]),
			LastRTTus:    binary.BigEndian.Uint32(f[59:63]),
			MinRTTus:     binary.BigEndian.Uint32(f[63:67]),
			AvgRTTus:     binary.BigEndian.Uint32(f[67:71]),
			MaxRTTus:     binary.BigEndian.Uint32(f[71:75]),
			JitterUs:     binary.BigEndian.Uint32(f[75:79]),
			LossPermille: binary.BigEndian.Uint32(f[79:83]),
		})
	}
	return out, nil
}

// ==================== core BFD ====================

// BFD 状态值（vl_api_bfd_state_t，u32 枚举）。AdminDown 是我们自己没开会话，
// Down/Init 是"会话在但没收敛"，只有 Up 才算可达。
const (
	BFDStateAdminDown uint32 = 0
	BFDStateDown      uint32 = 1
	BFDStateInit      uint32 = 2
	BFDStateUp        uint32 = 3
)

// BFDStateName 给 CLI/北向用：数字对排障没用。
func BFDStateName(s uint32) string {
	switch s {
	case BFDStateAdminDown:
		return "admin-down"
	case BFDStateDown:
		return "down"
	case BFDStateInit:
		return "init"
	case BFDStateUp:
		return "up"
	}
	return fmt.Sprintf("state-%d", s)
}

// BFDMultihopSwIfIndex 是 bfd_udp_* 里表示"多跳会话"的 sw_if_index（~0）。
// 实测依据：CLI 的 `bfd udp session add multihop ...` 建出来的会话，dump 回来
// 的 sw_if_index 正是 0xffffffff（state=up，定时器字段时间单位都对得上）。
const BFDMultihopSwIfIndex = ^uint32(0)

// BFDSessionParams 是一条**多跳** UDP BFD 会话。间隔单位 µs（VPP 的原生单位）。
//
// 故意没有 sw_if_index：绑接口的 BFD 在这套数据面上会把隧道自己打死（见文件头），
// 留一个"能用但千万别用"的字段，迟早有人用。
type BFDSessionParams struct {
	LocalIP         net.IP
	PeerIP          net.IP
	DesiredMinTxUS  uint32
	RequiredMinRxUS uint32
	DetectMult      uint8
}

// BFDSession 是一条会话的读回值。SwIfIndex 会保留下来：多跳是 ~0，绑接口的
// 是真实索引 —— 运维手敲过一条绑 wg 的会话时，上层至少能看见它。注意：这里
// **没有** RTT/抖动/丢包 —— core BFD 不提供任何质量指标（见文件头）。
type BFDSession struct {
	SwIfIndex       uint32
	LocalIP         net.IP
	PeerIP          net.IP
	State           uint32
	DesiredMinTxUS  uint32
	RequiredMinRxUS uint32
	DetectMult      uint8
}

// EnableBFDMultihop 打开 VPP 的多跳 BFD 支持（全局开关，幂等）。
//
// 必须先调：多跳未启用时 bfd_udp_add 的 sw_if_index=~0 会被当成非法接口，
// 回 retval=-2（INVALID_SW_IF_INDEX）—— 2026-10-24 实测。CLI 的
// `bfd udp session add multihop` 不走这条 API 路径，所以手敲能建、只发 add 不能。
func (c *Client) EnableBFDMultihop(ctx context.Context) error {
	frame, err := c.call(ctx, "bfd_udp_enable_multihop", "bfd_udp_enable_multihop_reply", nil)
	if err != nil {
		return err
	}
	return c.retval("bfd_udp_enable_multihop", frame)
}

// AddBFDSession 建/改一条多跳 BFD 会话。这里不开认证：隧道内本身只承载已封装的
// 业务流，BFD 包不参与转发决策；要认证就得再管一套 key id 生命周期。
func (c *Client) AddBFDSession(ctx context.Context, p BFDSessionParams) error {
	if !isAPIAddress(p.LocalIP) || !isAPIAddress(p.PeerIP) {
		return fmt.Errorf("vppapi: BFD 本地/对端地址无效 (%v -> %v)", p.LocalIP, p.PeerIP)
	}
	if p.DetectMult == 0 {
		return errors.New("vppapi: BFD detect_mult 不能为 0（0 乘出来的检测窗口是 0）")
	}
	if err := c.EnableBFDMultihop(ctx); err != nil {
		return err
	}
	body := make([]byte, 0, 53)
	body = appendU32(body, BFDMultihopSwIfIndex)
	body = appendU32(body, p.DesiredMinTxUS)
	body = appendU32(body, p.RequiredMinRxUS)
	body = appendAddress(body, p.LocalIP)
	body = appendAddress(body, p.PeerIP)
	body = append(body, p.DetectMult)
	body = append(body, 0, 0) // is_authenticated=0, bfd_key_id=0
	body = appendU32(body, 0) // conf_key_id
	frame, err := c.call(ctx, "bfd_udp_add", "bfd_udp_add_reply", body)
	if err != nil {
		return err
	}
	return c.retval("bfd_udp_add", frame)
}

// DelBFDSession 删一条多跳会话。
func (c *Client) DelBFDSession(ctx context.Context, p BFDSessionParams) error {
	if !isAPIAddress(p.LocalIP) || !isAPIAddress(p.PeerIP) {
		return fmt.Errorf("vppapi: BFD 本地/对端地址无效 (%v -> %v)", p.LocalIP, p.PeerIP)
	}
	// 删也要先开多跳：agent 重启后清理旧会话时，这条路径可能早于任何一次 Add，
	// 而 ~0 在未启用多跳的 VPP 上同样被拒（-2），表现成"每次清理都失败"。
	if err := c.EnableBFDMultihop(ctx); err != nil {
		return err
	}
	body := make([]byte, 0, 38)
	body = appendU32(body, BFDMultihopSwIfIndex)
	body = appendAddress(body, p.LocalIP)
	body = appendAddress(body, p.PeerIP)
	frame, err := c.call(ctx, "bfd_udp_del", "bfd_udp_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("bfd_udp_del", frame)
}

// BFDSessions 读回全部会话（packed 结构，逐字段偏移见下）。
func (c *Client) BFDSessions(ctx context.Context) ([]BFDSession, error) {
	frames, err := c.dump(ctx, "bfd_udp_session_dump", "bfd_udp_session_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]BFDSession, 0, len(frames))
	for _, f := range frames {
		// msg_id(2) context(4) sw_if_index(4) local_addr(17) peer_addr(17)
		// state(4) is_authenticated(1) bfd_key_id(1) conf_key_id(4)
		// required_min_rx(4) desired_min_tx(4) detect_mult(1) = 63 B
		if len(f) < 63 {
			return nil, fmt.Errorf("vppapi: bfd_udp_session_details 太短 (%d B)，VPP 结构变了", len(f))
		}
		out = append(out, BFDSession{
			SwIfIndex:       binary.BigEndian.Uint32(f[6:10]),
			LocalIP:         readAddress(f[10:27]),
			PeerIP:          readAddress(f[27:44]),
			State:           binary.BigEndian.Uint32(f[44:48]),
			RequiredMinRxUS: binary.BigEndian.Uint32(f[54:58]),
			DesiredMinTxUS:  binary.BigEndian.Uint32(f[58:62]),
			DetectMult:      f[62],
		})
	}
	return out, nil
}
