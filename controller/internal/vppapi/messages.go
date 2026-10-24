package vppapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
)

// 栅栏消息：dump 用它判断"发完了"（见 dump 的注释）。选 sr_wg_stats_get 是因为
// 它是本插件里最便宜的一条只读、定长、有显式应答的 rpc。
const (
	barrierMsg   = "sr_wg_stats_get"
	barrierReply = "sr_wg_stats_get_reply"
)

// requiredVersions 是本客户端会用到的消息与其结构 CRC（VPP 按结构算的版本号）。
// 连上就整体核对：某条缺失或 CRC 变了，说明插件的 .api 改过而这里没跟上 ——
// 此时必须拒绝继续，因为尺寸不对的请求会被 VPP 静默丢弃（只在日志里留一行
// Truncated message），运行期表现为莫名其妙地超时。
//
// 值取自 srwg-node1（VPP 26.06 + sr_wg 1.0.0）的实测注册表；改过 .api 之后可以
// 离线重算：插件构建产出的 build/sr_wg.api.c 里有
// vl_api_add_msg_name_crc("sr_wg_xxx_<crc>", …)，那就是 VPP 运行期登记的同一个值。
var requiredVersions = map[string]string{
	"sr_wg_key_set":                    "e7289514",
	"sr_wg_key_set_reply":              "e8d4e804",
	"sr_wg_key_del":                    "0306271d",
	"sr_wg_key_del_reply":              "e8d4e804",
	"sr_wg_rebind_ports":               "51077d14",
	"sr_wg_rebind_ports_reply":         "e8d4e804",
	"sr_wg_obs_clear":                  "51077d14",
	"sr_wg_obs_clear_reply":            "e8d4e804",
	"sr_wg_obs_dump":                   "51077d14",
	"sr_wg_obs_details":                "ee252d6e",
	"sr_wg_stats_get":                  "51077d14",
	"sr_wg_stats_get_reply":            "ab7395b2",
	"wireguard_interface_create":       "a530137e",
	"wireguard_interface_create_reply": "5383d31f",
	"wireguard_interface_delete":       "f9e6675e",
	"wireguard_interface_delete_reply": "e8d4e804",
	"wireguard_interface_dump":         "2c954158",
	"wireguard_interface_details":      "0dd4865d",
	"wireguard_peer_add":               "9b8aad61",
	"wireguard_peer_add_reply":         "084a0cd3",
	"wireguard_peer_remove":            "3b74607a",
	"wireguard_peer_remove_reply":      "e8d4e804",
	"wireguard_peers_dump":             "3b74607a",
	"wireguard_peers_details":          "6a9f6bc3",
	"sr_wg_path_add_del":               "f1579105",
	"sr_wg_path_add_del_reply":         "e8d4e804",
	"sr_wg_path_dump":                  "e7177ea0",
	"sr_wg_path_details":               "e4624a51",
	"sr_wg_tunnel_set_path":            "75d015ec",
	"sr_wg_tunnel_set_path_reply":      "e8d4e804",
	"sr_wg_tunnel_dump":                "529cb13f",
	"sr_wg_tunnel_details":             "dbfc98ee",
	"sr_wg_port_add_del":               "5495c953",
	"sr_wg_port_add_del_reply":         "e8d4e804",
	"sr_wg_port_dump":                  "51077d14",
	"sr_wg_port_details":               "ed9c3dfd",
	"sr_wg_encap_enable":               "a36fadc0",
	"sr_wg_encap_enable_reply":         "e8d4e804",
	"sr_wg_transit_enable":             "42bb04c5",
	"sr_wg_transit_enable_reply":       "e8d4e804",
	"sr_wg_transit_disable":            "51077d14",
	"sr_wg_transit_disable_reply":      "e8d4e804",
	// core（vnet/interface.api）也要核对：MTU 是 VPP 自己的消息，不是插件的，
	// 值取自 srwg-node1 里装的 /usr/share/vpp/api/core/interface.api.json。
	"sw_interface_set_mtu":       "5cbe85e5",
	"sw_interface_set_mtu_reply": "e8d4e804",
	// 同一份 JSON 里的 dump/details：结构体一旦变，CRC 一定先于超时告诉我们。
	"sw_interface_dump":    "aa610c27",
	"sw_interface_details": "6c221fc7",
	// TCP MSS 钳制是 VPP 自带的 mss_clamp 插件（VPP 26.06 默认就装），同样不是
	// sr_wg 的。值取自 srwg-node1 的 /usr/share/vpp/api/plugins/mss_clamp.api.json，
	// 并与生成的 mss_clamp.api_types.h 里的 *_CRC 宏逐条对上。
	"mss_clamp_enable_disable":       "d31b44e3",
	"mss_clamp_enable_disable_reply": "e8d4e804",
	"mss_clamp_get":                  "47250981",
	"mss_clamp_get_reply":            "53b48f5d",
	"mss_clamp_details":              "d3a4de61",
}

func checkVersions(ids map[string]uint16, crcs map[string]string, want map[string]string) error {
	var problems []string
	for name, wantCRC := range want {
		if _, ok := ids[name]; !ok {
			problems = append(problems, fmt.Sprintf("缺失 %s", name))
			continue
		}
		if got := crcs[name]; got != wantCRC {
			problems = append(problems, fmt.Sprintf("%s CRC=%s 期望 %s", name, got, wantCRC))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	// map 遍历顺序不定，排一遍才看得出这一轮到底比上一轮多了哪条。
	sort.Strings(problems)
	return fmt.Errorf("vppapi: VPP 消息表与本客户端不匹配: %v", problems)
}

// 长度都是 VPP 头文件里写死的字节数（u8 private_key[32] / u8 key[16]）。
const (
	wgKeyLen = 32 // X25519 私钥与公钥
	pKeyLen  = 16 // SR HMAC 的 P-Key = SR_PKEY_LEN
)

// InterfaceParams 是建 WG 隧道接口的参数。定案 3：私钥由 agent 自己生成并原样
// 交给 VPP（generate_key=0），接口公钥由 VPP 从这把私钥推出，agent 再把公钥签名
// 后上报控制器 —— 控制器只是中继，换不了钥匙。
type InterfaceParams struct {
	UserInstance uint32
	ListenPort   uint16
	PrivateKeys  []byte // wgKeyLen 字节 X25519 私钥
	SrcIP        net.IP // underlay 源地址（VPP 用它做 WG 的 src）
}

// PeerParams 是一个 WG peer。Port 就是 vl_api_wireguard_peer_t.port，注释写的是
// "destination port"，即外层 UDP 目的端口 —— 它是**本条隧道的** vpn_port
// （定案 2：每隧道一个端口，所以这个值来自隧道自己，不是一个全局常数）。
type PeerParams struct {
	PublicKey  []byte // wgKeyLen 字节 X25519 公钥
	EndpointIP net.IP
	// Port 是**封装前**那个 WG 包的外层目的端口。走 SR 时必须填租户的 vpn_port：
	// encap 节点在 Step 4 只处理 udp dst 命中 vpn_ports 集合的包，Step 12 才把
	// 目的端口改写成段端口（vpp-plugin/sr_wg4_encap.c）。填成 NAT 映射端口的话
	// 包根本不会被加 SR 头，计数器上是 encap_bypass —— 不报错，但源路由没了。
	Port       uint16
	Keepalive  uint16
	TableID    uint32
	SWIfIndex  uint32
	AllowedIPs []Prefix
}

type Prefix struct {
	IP  net.IP
	Len uint8
}

// InterfaceInfo 是 wireguard_interface_dump 的一条结果。
type InterfaceInfo struct {
	UserInstance uint32
	SWIfIndex    uint32
	PrivateKey   []byte // 只有 ShowPrivate 时才非空
	PublicKey    []byte
	Port         uint16
	SrcIP        net.IP
}

// Observation 是数据面观测到的一条外层源（vBOND 地址发现的现场证据）。
type Observation struct {
	SrcIP    net.IP
	SrcPort  uint16
	DstPort  uint16
	RXIf     uint32
	Hits     uint32
	AgeSec   uint32
	Verified bool
}

// SetPKey 下发/轮换一把 SR HMAC 的 P-Key（hmac_id 标识，16 字节）。
// 路径表里存的是密钥副本，这一条会同时刷新所有引用该 hmac_id 的路径。
func (c *Client) SetPKey(ctx context.Context, hmacID uint16, key []byte) error {
	if len(key) != pKeyLen {
		return fmt.Errorf("vppapi: P-Key 必须 %d 字节，给了 %d", pKeyLen, len(key))
	}
	body := make([]byte, 0, 2+pKeyLen)
	body = appendU16(body, hmacID)
	body = append(body, key...)
	frame, err := c.call(ctx, "sr_wg_key_set", "sr_wg_key_set_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_key_set", frame)
}

// DelPKey 删掉一把 P-Key。仍有路径引用该 hmac_id 时 VPP 回 INVALID_VALUE
// （不是静默删除），所以这个错误要原样露给调用方。
// 换密钥请直接用 SetPKey，不要先删再设。
func (c *Client) DelPKey(ctx context.Context, hmacID uint16) error {
	frame, err := c.call(ctx, "sr_wg_key_del", "sr_wg_key_del_reply", appendU16(nil, hmacID))
	if err != nil {
		return err
	}
	return c.retval("sr_wg_key_del", frame)
}

// RebindPorts 把**当前端口集合**（vpn_ports + transit_ports）重新注册给本插件的
// 输入节点。
//
// 必须在**创建 WG 接口之后**调用：建接口时 wireguard 插件会把那个接口的
// listen-port 重新注册给自己的 wg4-input，把本插件的旁路入口摘掉 ——
// 每隧道一个端口之后，被覆盖的是那一条隧道的端口，其余不受影响。
// 这是数据面挂点的顺序约束，不是可选优化：漏掉它，SR 隧道会静默退化成普通 WG。
func (c *Client) RebindPorts(ctx context.Context) error {
	frame, err := c.call(ctx, "sr_wg_rebind_ports", "sr_wg_rebind_ports_reply", nil)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_rebind_ports", frame)
}

// ClearObservations 清空观测表（"从这里开始重新观测"）。
func (c *Client) ClearObservations(ctx context.Context) error {
	frame, err := c.call(ctx, "sr_wg_obs_clear", "sr_wg_obs_clear_reply", nil)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_obs_clear", frame)
}

// Observations 读回本节点观测到的外层源元组。空表返回空切片而不是错误 ——
// VPP 对空结果一帧都不发，"没有帧"是正常状态。
func (c *Client) Observations(ctx context.Context) ([]Observation, error) {
	frames, err := c.dump(ctx, "sr_wg_obs_dump", "sr_wg_obs_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]Observation, 0, len(frames))
	for _, f := range frames {
		// vl_api_sr_wg_obs_details_t = msg_id(2) context(4) ip4(4) sport(2)
		//                              dport(2) rx_if(4) hits(4) age_sec(4) verified(1)
		if len(f) < 27 {
			return nil, fmt.Errorf("vppapi: sr_wg_obs_details 太短 (%d B)，插件结构变了", len(f))
		}
		o := Observation{
			SrcIP:    net.IP(append([]byte(nil), f[6:10]...)),
			SrcPort:  binary.BigEndian.Uint16(f[10:12]),
			DstPort:  binary.BigEndian.Uint16(f[12:14]),
			RXIf:     binary.BigEndian.Uint32(f[14:18]),
			Hits:     binary.BigEndian.Uint32(f[18:22]),
			AgeSec:   binary.BigEndian.Uint32(f[22:26]),
			Verified: f[26] != 0,
		}
		out = append(out, o)
	}
	return out, nil
}

// CreateInterface 用**给定的静态私钥**建 WG 接口，返回 sw_if_index。
//
// 失败以 APIError 露出 retval。实测一个坑：UserInstance 已被占用时，wireguard
// 插件回的是 VNET_API_ERROR_INVALID_REGISTRATION = -31（"实例注册失败"），
// 不是 -79 interface already exists —— agent 判重时得认 -31。
func (c *Client) CreateInterface(ctx context.Context, p InterfaceParams) (uint32, error) {
	if len(p.PrivateKeys) != wgKeyLen {
		return 0, fmt.Errorf("vppapi: WG 私钥必须 %d 字节，给了 %d", wgKeyLen, len(p.PrivateKeys))
	}
	if !isAPIAddress(p.SrcIP) {
		return 0, fmt.Errorf("vppapi: WG 接口必须有合法的 underlay 源地址，got %v", p.SrcIP)
	}
	body := make([]byte, 0, 92)
	body = appendU32(body, p.UserInstance)
	body = appendU32(body, ^uint32(0)) // sw_if_index：让 VPP 分配
	body = append(body, p.PrivateKeys...)
	body = append(body, make([]byte, 32)...) // public_key 留空，VPP 从私钥推
	body = appendU16(body, p.ListenPort)
	body = appendAddress(body, p.SrcIP)
	body = append(body, 0) // generate_key = false —— 定案 3 的全部意义在这一字节

	frame, err := c.call(ctx, "wireguard_interface_create", "wireguard_interface_create_reply", body)
	if err != nil {
		return 0, err
	}
	if err := c.retval("wireguard_interface_create", frame); err != nil {
		return 0, err
	}
	if len(frame) < 14 {
		return 0, fmt.Errorf("vppapi: create_reply 太短 (%d B)", len(frame))
	}
	return binary.BigEndian.Uint32(frame[10:14]), nil
}

// DeleteInterface 删掉 WG 接口（连带它的 peers）。
func (c *Client) DeleteInterface(ctx context.Context, swIfIndex uint32) error {
	frame, err := c.call(ctx, "wireguard_interface_delete", "wireguard_interface_delete_reply", appendU32(nil, swIfIndex))
	if err != nil {
		return err
	}
	return c.retval("wireguard_interface_delete", frame)
}

// AddPeer 装一个 WG peer，返回 peer_index。
//
// 按公钥去重，而且**去重键不含接口**：同公钥再加一次回 -116 ENTRY_ALREADY_EXISTS，
// 换 endpoint 救不回来，换一张接口装它同样救不回来（真机在两张接口上试同一把公钥，
// 第二次 -116）。所以对端地址变了要先 RemovePeer 再 AddPeer；而"别的接口上已经有
// 这个人"不是本函数能自愈的事，调用方只能把那张接口指出来交给人判。
//
// AllowedIPs 不能为空：实测 VPP 回 -7 INVALID_VALUE。Keepalive 给 0 是合法的
// （VPP 不拦），但 0 意味着没有探针 —— 定案 1 把 WG 的 persistent keepalive 当
// NAT 保活/观测探针用，所以必须由调用方给一个非 0 值，别指望数据面兜底。
func (c *Client) AddPeer(ctx context.Context, p PeerParams) (uint32, error) {
	if len(p.PublicKey) != wgKeyLen {
		return 0, fmt.Errorf("vppapi: peer 公钥必须 %d 字节，给了 %d", wgKeyLen, len(p.PublicKey))
	}
	if !isAPIAddress(p.EndpointIP) {
		return 0, fmt.Errorf("vppapi: peer 必须有合法的外层 endpoint 地址，got %v", p.EndpointIP)
	}
	if len(p.AllowedIPs) > 255 {
		return 0, errors.New("vppapi: peer 的 allowed_ips 超过 255 条")
	}
	for i, pre := range p.AllowedIPs {
		if !isAPIAddress(pre.IP) {
			return 0, fmt.Errorf("vppapi: allowed_ips[%d] 地址不合法，got %v", i, pre.IP)
		}
	}
	// vl_api_wireguard_peer_t（wireguard.api_types.h，packed，实测 67 字节基体）：
	//   peer_index(4) public_key[32] port(2) persistent_keepalive(2) table_id(4)
	//   endpoint(17) sw_if_index(4) flags(1) n_allowed_ips(1) allowed_ips[18*n]
	// 同头文件里还有带 preshared_key 的 _v2；本设计不用 PSK，走 v1，
	// requiredVersions 里的 CRC 就是这条布局的看门人。
	body := make([]byte, 0, 67+18*len(p.AllowedIPs))
	body = appendU32(body, ^uint32(0)) // peer_index：让 VPP 分配
	body = append(body, p.PublicKey...)
	body = appendU16(body, p.Port)
	body = appendU16(body, p.Keepalive)
	body = appendU32(body, p.TableID)
	body = appendAddress(body, p.EndpointIP)
	body = appendU32(body, p.SWIfIndex)
	body = append(body, 0) // flags
	body = append(body, byte(len(p.AllowedIPs)))
	for _, pre := range p.AllowedIPs {
		body = appendAddress(body, pre.IP)
		body = append(body, pre.Len)
	}

	frame, err := c.call(ctx, "wireguard_peer_add", "wireguard_peer_add_reply", body)
	if err != nil {
		return 0, err
	}
	if err := c.retval("wireguard_peer_add", frame); err != nil {
		return 0, err
	}
	if len(frame) < 14 {
		return 0, fmt.Errorf("vppapi: peer_add_reply 太短 (%d B)", len(frame))
	}
	return binary.BigEndian.Uint32(frame[10:14]), nil
}

// Interfaces 读回 WG 接口。showPrivate=true 才能拿到 VPP 实际持有的那把私钥，
// 用来证明"装进去的就是我们生成的那把"（定案 3 的现场核验，探针里逐字节比过）。
func (c *Client) Interfaces(ctx context.Context, showPrivate bool) ([]InterfaceInfo, error) {
	body := make([]byte, 5)
	if showPrivate {
		body[0] = 1
	}
	binary.BigEndian.PutUint32(body[1:5], ^uint32(0)) // sw_if_index = all

	frames, err := c.dump(ctx, "wireguard_interface_dump", "wireguard_interface_details", body)
	if err != nil {
		return nil, err
	}
	out := make([]InterfaceInfo, 0, len(frames))
	for _, f := range frames {
		// vl_api_wireguard_interface_details_t = msg_id(2) context(4) interface(91)
		if len(f) < 97 {
			return nil, fmt.Errorf("vppapi: wireguard_interface_details 太短 (%d B)，插件结构变了", len(f))
		}
		at := 6 // 跳过 msg_id + context，进入 vl_api_wireguard_interface_t
		out = append(out, InterfaceInfo{
			UserInstance: binary.BigEndian.Uint32(f[at : at+4]),
			SWIfIndex:    binary.BigEndian.Uint32(f[at+4 : at+8]),
			PrivateKey:   append([]byte(nil), f[at+8:at+40]...),
			PublicKey:    append([]byte(nil), f[at+40:at+72]...),
			Port:         binary.BigEndian.Uint16(f[at+72 : at+74]),
			SrcIP:        net.IP(append([]byte(nil), f[at+75:at+79]...)),
		})
	}
	return out, nil
}

// ==================== WG peer 的读回与删除 ====================

// WgPeer 是 wireguard_peers_dump 回来的一条 peer，即数据面**当下真正在用**的状态。
//
// 为什么非要有这条：agent 重启（或崩在配置中途）时，VPP 里的接口和 peer 并不会
// 跟着消失，而 wireguard 插件对重复的 create/add 回的是错误（接口实例号被占
// 实测 -31 INVALID_REGISTRATION；同公钥重复 add peer 实测 -116 ENTRY_ALREADY_EXISTS，
// 换 endpoint 也一样 —— 去重键是公钥）。只会"从零配一遍"的 agent 第二次就起不来，
// 只能人工去 vppctl 里删干净 —— 那不是
// 一个能长期跑的东西。另外外层 endpoint 变了要重装 peer，也得先知道旧的 peer_index。
type WgPeer struct {
	PeerIndex  uint32
	PublicKey  []byte
	EndpointIP net.IP
	Port       uint16
	Keepalive  uint16
	TableID    uint32
	SWIfIndex  uint32
	Flags      uint8
	AllowedIPs []Prefix
}

// wgPeerBaseLen 是 vl_api_wireguard_peer_t 去掉变长尾部后的定长部分（67 字节，
// 含 flags 这一字节 —— flags 在 vapi 的头文件里是 enum，但 VPP 实际按 u8 发送：
// 实测一条带 1 个 allowed_ip 的 details 帧长 91 = 帧头 6 + 67 + 18）。
const (
	wgPeerBaseLen = 67
	prefixLen     = 18 // vl_api_prefix_t = address(17) + len(1)
)

// Peers 读回全部 WG peer。VPP 的过滤字段是 peer_index（~0 为全量），接口维度
// 的筛选在调用方按 SWIfIndex 做 —— 少一个会骗人的参数比多一个用不上的参数好。
func (c *Client) Peers(ctx context.Context) ([]WgPeer, error) {
	frames, err := c.dump(ctx, "wireguard_peers_dump", "wireguard_peers_details",
		appendU32(nil, ^uint32(0)))
	if err != nil {
		return nil, err
	}
	out := make([]WgPeer, 0, len(frames))
	for i, f := range frames {
		if len(f) < 6+wgPeerBaseLen {
			return nil, fmt.Errorf("vppapi: wireguard_peers_details[%d] 太短 (%d B)，插件结构变了", i, len(f))
		}
		p := WgPeer{
			PeerIndex:  binary.BigEndian.Uint32(f[6:10]),
			PublicKey:  append([]byte(nil), f[10:42]...),
			Port:       binary.BigEndian.Uint16(f[42:44]),
			Keepalive:  binary.BigEndian.Uint16(f[44:46]),
			TableID:    binary.BigEndian.Uint32(f[46:50]),
			EndpointIP: readAddress(f[50:67]),
			SWIfIndex:  binary.BigEndian.Uint32(f[67:71]),
			Flags:      f[71],
		}
		n := int(f[72])
		if len(f) < 73+n*prefixLen {
			return nil, fmt.Errorf("vppapi: wireguard_peers_details[%d] 的 allowed_ips 被截断 (声明 %d 条，帧只有 %d B)", i, n, len(f))
		}
		for j := 0; j < n; j++ {
			at := 73 + j*prefixLen
			p.AllowedIPs = append(p.AllowedIPs, Prefix{
				IP:  readAddress(f[at : at+17]),
				Len: f[at+17],
			})
		}
		out = append(out, p)
	}
	return out, nil
}

// RemovePeer 按 peer_index 摘掉一个 peer。VPP 没有"改 peer 的 endpoint"这条消息
// （26.06 的 wireguard.api 只有 add/remove/dump），所以外层地址变了只能先删再加。
//
// peer_index 不存在时回 -6（VNET_API_ERROR_NO_SUCH_ENTRY，真机实测），不是静默成功：
// reconcile 路径要用 errors.As 认出它、当"已经不在了"继续往下走，其它错误不能吞。
func (c *Client) RemovePeer(ctx context.Context, peerIndex uint32) error {
	frame, err := c.call(ctx, "wireguard_peer_remove", "wireguard_peer_remove_reply",
		appendU32(nil, peerIndex))
	if err != nil {
		return err
	}
	return c.retval("wireguard_peer_remove", frame)
}

// ==================== SR 路径 / 隧道绑定 / 挂点 ====================

// 段的字节数：vl_api_sr_wg_segment_t = ip4(4) + port(2)，且数组是**固定 8 项**
// 的 segments[8]，不是变长。少写了未用那几项的填充，请求就短于 VPP 注册的结构，
// 而 VPP 对短请求只留一行 Truncated message 便丢弃。
const (
	segmentLen    = 6
	maxSegments   = 8
	pathBodyLen   = 1 + 2 + 2 + 1 + 1 + segmentLen*maxSegments // 55
	pathDetailLen = 2 + 4 + 2 + 2 + 1 + 1 + segmentLen*maxSegments
)

// Segment 是 SR 路径上的一跳外层地址。
//
// 段的顺序照数据面的约定（vpp-plugin/sr_wg4_encap.c 头部注释）：
//
//	Segments[0]      = 最终目的节点（打它的 VPN 端口 51820）
//	Segments[n-1]    = 第一跳中转（"首跳免费"，由外层 IP/UDP 承载）
//
// 也就是**倒序**，与 sr-wg CLI 的 `via` 顺序一致。写反了不会报错：包照样发出去，
// 只是发往错误的第一跳，症状是"路径配上了但隧道不通"。
type Segment struct {
	IP   net.IP // 只支持 IPv4：vl_api_ip4_address_t 就是 4 字节
	Port uint16
}

// PathParams 是一条 SR 路径。HmacID 引用的 P-Key **必须已经装进密钥表**，
// 否则 VPP 回 NO_SUCH_ENTRY —— 路径存的是密钥副本，缺钥匙时若用全零密钥建路径，
// 包能正常发出去但对端一律验签失败，控制器完全看不出来。
type PathParams struct {
	PathID   uint16
	HmacID   uint16
	Active   bool
	Segments []Segment
}

// PathInfo 是 sr_wg_path_dump 回来的一条路径。
type PathInfo struct {
	PathID   uint16
	HmacID   uint16
	Active   bool
	Segments []Segment
}

// AddPath 新增或按 PathID 覆盖一条路径（is_add=1）。
func (c *Client) AddPath(ctx context.Context, p PathParams) error {
	if len(p.Segments) == 0 || len(p.Segments) > maxSegments {
		return fmt.Errorf("vppapi: 路径段数必须 1..%d，给了 %d", maxSegments, len(p.Segments))
	}
	for i, s := range p.Segments {
		if s.IP.To4() == nil {
			return fmt.Errorf("vppapi: segments[%d] 地址 %v 不是 IPv4，SR 段只支持 IPv4", i, s.IP)
		}
	}
	body := make([]byte, 0, pathBodyLen)
	body = append(body, 1) // is_add
	body = appendU16(body, p.PathID)
	body = appendU16(body, p.HmacID)
	body = append(body, byte(len(p.Segments)))
	if p.Active {
		body = append(body, 1)
	} else {
		body = append(body, 0)
	}
	body = appendSegments(body, p.Segments)
	frame, err := c.call(ctx, "sr_wg_path_add_del", "sr_wg_path_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_path_add_del", frame)
}

// DelPath 删一条路径。**指向该路径的隧道绑定会被一并摘除**：若只删路径而留着
// 绑定，收包时只会按 encap_no_path 静默丢包，看不出是谁的问题。
func (c *Client) DelPath(ctx context.Context, pathID uint16) error {
	body := make([]byte, pathBodyLen)
	// body[0] = is_add = 0；只有 path_id 有意义，其余补零凑满注册结构。
	binary.BigEndian.PutUint16(body[1:3], pathID)
	frame, err := c.call(ctx, "sr_wg_path_add_del", "sr_wg_path_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_path_add_del", frame)
}

// Paths 读回路径表；pathID 传 ^uint16(0) 表示全量。
// 0 是合法 path_id，所以"全量"只能靠这个哨兵值表达。
func (c *Client) Paths(ctx context.Context, pathID uint16) ([]PathInfo, error) {
	frames, err := c.dump(ctx, "sr_wg_path_dump", "sr_wg_path_details", appendU16(nil, pathID))
	if err != nil {
		return nil, err
	}
	out := make([]PathInfo, 0, len(frames))
	for _, f := range frames {
		if len(f) < pathDetailLen {
			return nil, fmt.Errorf("vppapi: sr_wg_path_details 太短 (%d B)，插件结构变了", len(f))
		}
		out = append(out, PathInfo{
			PathID:   binary.BigEndian.Uint16(f[6:8]),
			HmacID:   binary.BigEndian.Uint16(f[8:10]),
			Active:   f[11] != 0,
			Segments: readSegments(f[12:], f[10]),
		})
	}
	return out, nil
}

// TunnelBinding 把一条 WG 隧道接口绑到一条 SR 路径上。
//
// EndpointIP 是 WG peer 的**外层** IPv4：encap 挂点挂在 underlay 出口接口上，
// 数据面按外层目的地址查绑定，所以漏了它这条绑定永远不会命中 —— 而"没命中"在
// 计数器上表现为 encap_bypass，看起来像"配置根本没生效"。
// DstPort 是**封装前**那个 WG 包的外层 UDP 目的端口，也就是这条隧道的 vpn_port。
// 一对 CPE 可以开多条隧道（定案 2：每隧道一个端口），此时 endpoint IP 相同、
// 只有端口能区分"哪条隧道"，所以绑定身份是 (EndpointIP, DstPort)。填 0 表示
// 端口通配 = 该 IP 上所有隧道共用这一条路径，只在单隧道时才是安全写法。
type TunnelBinding struct {
	SWIfIndex  uint32
	PathID     uint16
	EndpointIP net.IP
	DstPort    uint16
}

// TunnelInfo 是 sr_wg_tunnel_dump 回来的一条绑定。Effective=false 意味着
// endpoint 配了但 underlay 上没启用 sr-wg4-encap：包会静默旁路 SR 头。
// DstPort=0 是通配绑定（见 TunnelBinding）。
type TunnelInfo struct {
	SWIfIndex  uint32
	PathID     uint16
	EndpointIP net.IP
	DstPort    uint16
	Effective  bool
}

// tunnelBodyLen 是 sr_wg_tunnel_set_path 的请求体长度：
// is_add(1) sw_if_index(4) path_id(2) dst_port(2) endpoint_ip(4)。
const tunnelBodyLen = 1 + 4 + 2 + 2 + 4

// SetTunnel 绑定隧道到路径（is_add=1）。它**不会**自动启用 feature，
// 那是 EnableEncap 的活儿；两步都做完才是"这条隧道走 SR"。
//
// 同一个 (endpoint, dst_port) 身份已被别的接口占用时 VPP 回 ENTRY_ALREADY_EXISTS
// 而不是覆盖：两条绑定抢同一个包，"哪条隧道走哪条路径"就变成非确定的，
// 调用方必须把它当错误处理。
func (c *Client) SetTunnel(ctx context.Context, b TunnelBinding) error {
	if b.EndpointIP.To4() == nil {
		return fmt.Errorf("vppapi: 隧道绑定的 endpoint %v 不是 IPv4", b.EndpointIP)
	}
	body := make([]byte, 0, tunnelBodyLen)
	body = append(body, 1)
	body = appendU32(body, b.SWIfIndex)
	body = appendU16(body, b.PathID)
	body = appendU16(body, b.DstPort)
	body = appendIP4(body, b.EndpointIP)
	frame, err := c.call(ctx, "sr_wg_tunnel_set_path", "sr_wg_tunnel_set_path_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_tunnel_set_path", frame)
}

// ClearTunnel 解绑某接口上的隧道绑定（路径仍在表里，只是不再被这条隧道引用）。
func (c *Client) ClearTunnel(ctx context.Context, swIfIndex uint32) error {
	body := make([]byte, tunnelBodyLen)
	binary.BigEndian.PutUint32(body[1:5], swIfIndex) // body[0] = is_add = 0
	frame, err := c.call(ctx, "sr_wg_tunnel_set_path", "sr_wg_tunnel_set_path_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_tunnel_set_path", frame)
}

// Tunnels 读回隧道绑定；swIfIndex 传 ^uint32(0) 表示全量。
func (c *Client) Tunnels(ctx context.Context, swIfIndex uint32) ([]TunnelInfo, error) {
	frames, err := c.dump(ctx, "sr_wg_tunnel_dump", "sr_wg_tunnel_details", appendU32(nil, swIfIndex))
	if err != nil {
		return nil, err
	}
	out := make([]TunnelInfo, 0, len(frames))
	for _, f := range frames {
		// msg_id(2) context(4) sw_if_index(4) path_id(2) dst_port(2) endpoint_ip(4) is_effective(1)
		if len(f) < 19 {
			return nil, fmt.Errorf("vppapi: sr_wg_tunnel_details 太短 (%d B)，插件结构变了", len(f))
		}
		out = append(out, TunnelInfo{
			SWIfIndex:  binary.BigEndian.Uint32(f[6:10]),
			PathID:     binary.BigEndian.Uint16(f[10:12]),
			DstPort:    binary.BigEndian.Uint16(f[12:14]),
			EndpointIP: net.IP(append([]byte(nil), f[14:18]...)),
			Effective:  f[18] != 0,
		})
	}
	return out, nil
}

// ==================== UDP 端口集合（定案 2：每隧道一个端口） ====================

// 端口集合的种类，值即 sr_wg_port_add_del.kind。
const (
	PortKindVPN     uint8 = 0
	PortKindTransit uint8 = 1
)

// PortInfo 是 sr_wg_port_dump 回来的一个端口。
type PortInfo struct {
	Kind uint8
	Port uint16
}

// AddPort 把一个 UDP 目的端口加入集合并挂到本插件的输入节点。
//
// 幂等，而且每次都会重挂 —— 这正是它必须跟在 CreateInterface 之后的原因：
// 建 WG 接口时 wireguard 插件会把**那个接口的** listen-port 抢回 wg4-input，
// 漏掉这一步，那条隧道会静默退化成普通 WG（计数器上全是 encap_bypass）。
func (c *Client) AddPort(ctx context.Context, kind uint8, port uint16) error {
	return c.portAddDel(ctx, 1, kind, port)
}

// DelPort 从集合里去掉一个端口。VPN 端口是被**交回 wg4-input** 的（那条隧道
// 退化成普通 WG 仍可用），不是单纯反注册；del transit 端口才反注册。
func (c *Client) DelPort(ctx context.Context, kind uint8, port uint16) error {
	return c.portAddDel(ctx, 0, kind, port)
}

func (c *Client) portAddDel(ctx context.Context, isAdd uint8, kind uint8, port uint16) error {
	if port == 0 {
		return errors.New("vppapi: UDP 端口不能是 0")
	}
	body := make([]byte, 0, 4)
	body = append(body, isAdd, kind)
	body = appendU16(body, port)
	frame, err := c.call(ctx, "sr_wg_port_add_del", "sr_wg_port_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_port_add_del", frame)
}

// Ports 读回本节点登记的 UDP 端口集合（vpn 与 transit 混在一条流里，按 Kind 分）。
func (c *Client) Ports(ctx context.Context) ([]PortInfo, error) {
	frames, err := c.dump(ctx, "sr_wg_port_dump", "sr_wg_port_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]PortInfo, 0, len(frames))
	for _, f := range frames {
		// msg_id(2) context(4) kind(1) port(2)
		if len(f) < 9 {
			return nil, fmt.Errorf("vppapi: sr_wg_port_details 太短 (%d B)，插件结构变了", len(f))
		}
		out = append(out, PortInfo{Kind: f[6], Port: binary.BigEndian.Uint16(f[7:9])})
	}
	return out, nil
}

// EnableEncap 在 **underlay 出口接口**上启/停 ip4-output 的 sr-wg4-encap。
//
// 传 wg0 是错的，而且是静默的：wg4-output-tun 不进入 feature 链，挂 wg0 收不到
// 任何包，症状是"启用成功了但一个包都没被 encap"。sw_if_index 必须是 host-eth1
// 这类真正的出口。
func (c *Client) EnableEncap(ctx context.Context, swIfIndex uint32, enable bool) error {
	body := make([]byte, 5)
	binary.BigEndian.PutUint32(body, swIfIndex)
	if enable {
		body[4] = 1
	}
	frame, err := c.call(ctx, "sr_wg_encap_enable", "sr_wg_encap_enable_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_encap_enable", frame)
}

// SetTransit 让本机按 transit 角色收 SR 包：只验签 + 弹一段 + 改外层目的，
// 不解密、**绝不改外层源**。port 是租户的 transit 端口（定案 2 下发的那个），
// 改端口会同时重注册 UDP 分发表。
func (c *Client) SetTransit(ctx context.Context, port uint16) error {
	frame, err := c.call(ctx, "sr_wg_transit_enable", "sr_wg_transit_enable_reply", appendU16(nil, port))
	if err != nil {
		return err
	}
	return c.retval("sr_wg_transit_enable", frame)
}

// ClearTransit 关掉本机中转。
func (c *Client) ClearTransit(ctx context.Context) error {
	frame, err := c.call(ctx, "sr_wg_transit_disable", "sr_wg_transit_disable_reply", nil)
	if err != nil {
		return err
	}
	return c.retval("sr_wg_transit_disable", frame)
}

// ==================== 接口 MTU（core 消息，不是插件的） ====================

// vl_api_mtu_proto_t 的槽位下标：sw_interface_set_mtu 一次给四个值。
const (
	MTUSlotL3   = 0
	MTUSlotIP4  = 1
	MTUSlotIP6  = 2
	MTUSlotMPLS = 3
)

// SetInterfaceMTU 设一张接口的 **L3** MTU，其余三个槽位写 0。
//
// 写 0 不是"不改"而是"回落到 L3"：实测 `set interface mtu ip4 1400 wg0` 之后
// 再 `set interface mtu packet 1360 wg0`，VPP 显示的是 1360/0/0/0，IP4 那位被
// 清零了。四个槽位一起发、只填 L3，就等于 CLI 的 `set interface mtu packet N`，
// 这也是 VPP 对隧道接口约定的写法。真机复现见 live_test.go 的 interface_mtu。
//
// 0 要拦掉，而且要拦在**发出之前**：mtu[4] 全 0 等于取消这张口的 MTU 记录 ——
// 实测 CLI 写 `packet 0` 被照单收下、显示 0/0/0/0，VPP 对值毫不校验，所以这层
// 只能我们自己加。没有 MTU 记录的 underlay 拿不到分片依据；实验室里真实发生过
// 的形态是 af-packet 的 VIF 记着 9000 而容器网卡只有 1500，大包在 sendto 上失败
// 并把那个口的发送侧卡死、计数器不再动（2026-10-18 的带宽基线跑过一次，本轮做
// MTU 测量时在 node2 又复现一次：host-eth0-tx 连报 5 次 tx sendto failure）。
//
// 接口号写错不会静默：实测不存在的 sw_if_index 当场回 retval=-2，所以操作员把
// EncapIfIndex 填错时这里是报错，而不是"启用成功但一个包都没动"。
//
// 读不回这一条已经补上了：core 的 sw_interface_dump 就是下面的 DumpInterfaces，
// mtu[0] 正是 `show interface` 那一列，agent 在 Open/adopt 结尾会核对写进去的值真的
// 落地（见 agent.verifyLanded）。幂等，重复设同一个值没有副作用。
func (c *Client) SetInterfaceMTU(ctx context.Context, swIfIndex uint32, l3MTU uint16) error {
	if l3MTU == 0 {
		return errors.New("vppapi: MTU 不能是 0（四个槽位全清零等于取消 MTU 记录）")
	}
	body := make([]byte, 0, 20)
	body = appendU32(body, swIfIndex)
	body = appendU32(body, uint32(l3MTU))
	body = appendU32(body, 0) // IP4 → 回落 L3
	body = appendU32(body, 0) // IP6
	body = appendU32(body, 0) // MPLS
	frame, err := c.call(ctx, "sw_interface_set_mtu", "sw_interface_set_mtu_reply", body)
	if err != nil {
		return err
	}
	return c.retval("sw_interface_set_mtu", frame)
}

// SwInterface 是 core 的接口表里本数据面真正要用的那几项。
//
// L3MTU 就是 `show interface` 那个 MTU 列（mtu[0]，vl_api_mtu_proto_t 的 L3 槽）：
// 写进去的 MTU 能不能读回来，看的就是它。
type SwInterface struct {
	SWIfIndex uint32
	Name      string
	L3MTU     uint32
	AdminUp   bool
	LinkUp    bool
}

// swInterfaceDetailsLen 是 vl_api_sw_interface_details_t 的字节数。
//
// 这个数和下面每个偏移都不是推的：取自 srwg-node1 里 VPP 自己生成的
// /usr/include/vnet/interface.api_types.h（那张 packed 结构），并用容器里的 gcc
// 打印 sizeof/offsetof 复核过 —— sizeof=295，interface_name 在 103。值得记一笔的是
// flags/type/link_duplex/sub_if_flags **各占 4 字节**：VPP 的头文件只对 u8 枚举写
// STATIC_ASSERT（direction、address_family 有），这几个没有，所以按 u8 算会把后面
// 所有字段往前挪 6~10 字节 —— 症状是接口名读出来一串零、MTU 读出一个没见过的数。
const swInterfaceDetailsLen = 295

// DumpInterfaces 读回 VPP 的接口表，用来验证写下去的东西真的落了地。
//
// 为什么需要它：EnableEncap 对"挂错接口"是静默的（encap 挂到 WG 接口上照样
// retval=0，一个包都不改写），而 MTU 写下去之后 VPP 从不回读。这两件事在
// sw_interface_dump 之前只能靠操作员盯 vppctl，agent 自己看不见。
//
// 只发**无过滤**的 dump：sw_if_index=0xFFFFFFFF（全部）、name_filter_valid=0、
// 变长串长度 0，请求体共 9 字节（帧共 19）。不用 VPP 侧的按名过滤有两个理由：
// 一台 CPE 就几张口，全收回来在 Go 里筛更便宜；而 vl_api_string_t 后面要不要补零
// 到 4 字节对齐，取决于 VPP 解码器版本，少发一个字节就是"超时且没有任何错误"。
func (c *Client) DumpInterfaces(ctx context.Context) ([]SwInterface, error) {
	body := make([]byte, 9)
	binary.BigEndian.PutUint32(body[0:4], ^uint32(0)) // sw_if_index = all
	// body[4] = name_filter_valid（0），body[5:9] = name_filter 的 u32 长度（0）。

	frames, err := c.dump(ctx, "sw_interface_dump", "sw_interface_details", body)
	if err != nil {
		return nil, err
	}
	out := make([]SwInterface, 0, len(frames))
	for _, f := range frames {
		if len(f) < swInterfaceDetailsLen {
			return nil, fmt.Errorf("vppapi: sw_interface_details 太短 (%d B < %d B)，core 的接口结构变了",
				len(f), swInterfaceDetailsLen)
		}
		// 帧自带 msg_id(2)+context(4)，正好是那张 packed 结构的前 6 字节，
		// 所以下面直接用头文件里的 offsetof，不再另加 6。
		flags := binary.BigEndian.Uint32(f[20:24])
		out = append(out, SwInterface{
			SWIfIndex: binary.BigEndian.Uint32(f[6:10]),
			Name:      fixedString(f[103:167]),
			L3MTU:     binary.BigEndian.Uint32(f[38:42]),
			AdminUp:   flags&0x1 != 0, // IF_STATUS_API_FLAG_ADMIN_UP
			LinkUp:    flags&0x2 != 0, // IF_STATUS_API_FLAG_LINK_UP
		})
	}
	return out, nil
}

// fixedString 读 API 里那种"定长数组、NUL 结尾"的字符串字段。
func fixedString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// FindInterface 在 DumpInterfaces 的结果里按号找一张口。
//
// 找不到就是找不到（false），调用方自己决定这是不是错误 —— 本包不去猜"这个号
// 本该是谁"，因为出口接口该由谁定、按什么定，是 agent 那一层的事。
func FindInterface(ifaces []SwInterface, swIfIndex uint32) (SwInterface, bool) {
	for _, i := range ifaces {
		if i.SWIfIndex == swIfIndex {
			return i, true
		}
	}
	return SwInterface{}, false
}

// ==================== TCP MSS 钳制（mss_clamp 插件，不是 sr_wg 的） ====================

// vl_api_mss_clamp_dir_t 是 enumflags(u8)：NONE=0、RX=1、TX=2。
const (
	MSSClampDirNone uint8 = 0
	MSSClampDirRX   uint8 = 1
	MSSClampDirTX   uint8 = 2
)

// SetMSSClamp 把这张接口的 IPv4 TCP MSS 钳到 mss，方向固定 **RX**。
//
// 为什么是 RX 而不是 TX（2026-10-24 在实验室按 A/B/A 实测）：
//
//   - RX 挂在 ip4-input 弧上，改的是**从隧道进来**的内层包。实测：node2 的 wg0
//     设 ip4 RX 1320 后，node1 的终端发来的 SYN 到 node2 时 MSS 已从 1460 变 1320。
//   - TX 挂在 ip4-output 弧上，而这张口的 ip4-output 弧会被 wg4-output-tun 先吃掉
//     （`show interface features wg0` 的顺序就是 wg4-output-tun → tcp-mss-clamping-
//     ip4-out；整条隧道路径的另一处实测也证明 wg0 的 ip4-output 不跑 feature 链）。
//     实测同一台 node1 设 ip4 TX 1320 后，SYN 到 node2 仍是 1460 —— 配置读回来
//     一切正常，包一个字节没改。**这正是本插件最容易配错的地方**。
//
// 两端都要设：RX 只管"我收到的那个 SYN/SYN-ACK"，所以 A 的 MSS 由 B 的 RX 钳、
// B 的 MSS 由 A 的 RX 钳，各自改的都是对方要发给自己的段长。
//
// mss=0 在发出之前拦掉：钳到 0 等于把每个段的载荷压成 0，隧道看起来"通"但吞吐
// 是每次一个纯 ACK 的量级。VPP 对这个值不做校验（和 MTU 一样）。
func (c *Client) SetMSSClamp(ctx context.Context, swIfIndex uint32, mss uint16) error {
	if mss == 0 {
		return errors.New("vppapi: MSS 不能是 0（钳到 0 等于每个 TCP 段只能带纯 ACK）")
	}
	// vl_api_mss_clamp_enable_disable_t 净体 = sw_if_index(4) ipv4_mss(2)
	// ipv6_mss(2) ipv4_direction(1) ipv6_direction(1)。
	body := make([]byte, 0, 10)
	body = appendU32(body, swIfIndex)
	body = appendU16(body, mss)
	body = appendU16(body, 0) // IPv6 不碰：overlay 至今只有 IPv4
	body = append(body, MSSClampDirRX, MSSClampDirNone)
	frame, err := c.call(ctx, "mss_clamp_enable_disable", "mss_clamp_enable_disable_reply", body)
	if err != nil {
		return err
	}
	return c.retval("mss_clamp_enable_disable", frame)
}

// MSSClampInfo 是一条 `mss_clamp_get` 明细（vl_api_mss_clamp_details_t）。
type MSSClampInfo struct {
	SWIfIndex uint32
	IPv4MSS   uint16
	IPv6MSS   uint16
	IPv4Dir   uint8 // MSSClampDir* 的位组合
	IPv6Dir   uint8
}

// MSSClampDetailsLen 是 vl_api_mss_clamp_details_t 的字节数：
// msg_id(2)+context(4)+sw_if_index(4)+ipv4_mss(2)+ipv6_mss(2)+两个方向(1+1)。
const MSSClampDetailsLen = 16

// MSSClamp 读回一张接口的 MSS 钳制状态；没有记录返回 ok=false。
//
// 存在的理由和 DumpInterfaces 一样：mss_clamp_enable_disable 的 retval=0 只说明
// "请求被收下"，读回来才知道方向真的是 RX、数真的是那个数。真机实测这个插件对
// 值不做校验。
//
// 按 sw_if_index 服务端过滤（`mss_clamp_get` 的 sw_if_index 字段，不过滤是 ~0），
// 所以一张口最多一条明细；cursor 循环照抄 VPP 的分页语义，避免"第一页没命中就
// 报没有"的假阴性。
func (c *Client) MSSClamp(ctx context.Context, swIfIndex uint32) (MSSClampInfo, bool, error) {
	var out MSSClampInfo
	found := false
	cursor := uint32(0)
	for {
		body := make([]byte, 0, 8)
		body = appendU32(body, cursor)
		body = appendU32(body, swIfIndex)
		frames, reply, err := c.callStream(ctx, "mss_clamp_get", "mss_clamp_details",
			"mss_clamp_get_reply", body)
		if err != nil {
			return MSSClampInfo{}, false, err
		}
		// 应答净体 = retval(4) cursor(4)，接在 msg_id(2)+context(4) 之后。
		if len(reply) < 14 {
			return MSSClampInfo{}, false, fmt.Errorf("vppapi: mss_clamp_get_reply 太短 (%d B)", len(reply))
		}
		if rv := int32(binary.BigEndian.Uint32(reply[6:10])); rv != 0 {
			return MSSClampInfo{}, false, APIError{Op: "mss_clamp_get", Retval: rv}
		}
		for _, f := range frames {
			if len(f) < MSSClampDetailsLen {
				return MSSClampInfo{}, false, fmt.Errorf(
					"vppapi: mss_clamp_details 太短 (%d B < %d B)，mss_clamp 的结构变了",
					len(f), MSSClampDetailsLen)
			}
			info := MSSClampInfo{
				SWIfIndex: binary.BigEndian.Uint32(f[6:10]),
				IPv4MSS:   binary.BigEndian.Uint16(f[10:12]),
				IPv6MSS:   binary.BigEndian.Uint16(f[12:14]),
				IPv4Dir:   f[14],
				IPv6Dir:   f[15],
			}
			if info.SWIfIndex == swIfIndex {
				out, found = info, true
			}
		}
		next := binary.BigEndian.Uint32(reply[10:14])
		// next <= cursor 防的是"永远回同一个 cursor"的坏实现把读回挂死。
		if next == 0 || next <= cursor {
			return out, found, nil
		}
		cursor = next
	}
}

// appendSegments 写满定长数组 segments[8]：未用的槽位补零。
func appendSegments(b []byte, segs []Segment) []byte {
	for _, s := range segs {
		b = appendIP4(b, s.IP)
		b = appendU16(b, s.Port)
	}
	return append(b, make([]byte, segmentLen*(maxSegments-len(segs)))...)
}

func readSegments(raw []byte, count uint8) []Segment {
	if int(count) > maxSegments {
		count = maxSegments
	}
	out := make([]Segment, 0, count)
	for i := 0; i < int(count); i++ {
		at := raw[i*segmentLen : i*segmentLen+segmentLen]
		out = append(out, Segment{
			IP:   net.IP(append([]byte(nil), at[:4]...)),
			Port: binary.BigEndian.Uint16(at[4:6]),
		})
	}
	return out
}

// ==================== 编码小工具 ====================

func appendU16(b []byte, v uint16) []byte {
	var t [2]byte
	binary.BigEndian.PutUint16(t[:], v)
	return append(b, t[:]...)
}

func appendU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}

// isAPIAddress 报告这个 IP 编不出 vl_api_address_t（既不是 v4 也不是 v6）。
// 调用方在发出前逐个挡掉：地址写错时 VPP 不报错，它照单收下配成 0.0.0.0，
// 表现为"配置成功了但隧道不通"——那正是本项目最贵的一类故障。
func isAPIAddress(ip net.IP) bool {
	return ip != nil && (ip.To4() != nil || len(ip) == net.IPv6len)
}

// appendIP4 写 vl_api_ip4_address_t：就是裸的 4 字节，没有 af 前缀。
// 本插件的路径段与隧道 endpoint 用的都是它（vpp-plugin/sr_wg.api），
// 所以 IPv6 地址在这里是错误，不是"另一种写法"。调用方须先验过 To4()。
func appendIP4(b []byte, ip net.IP) []byte {
	return append(b, ip.To4()...)
}

// appendAddress 写 vl_api_address_t：u8 af + union{ip4[4], ip6[16]} = 17 字节。
// union 从偏移 0 开始，所以 v4 的 4 字节后面必须补 12 个 0 才能凑满 16。
// 调用方必须先用 isAPIAddress 验过。
func appendAddress(b []byte, ip net.IP) []byte {
	pad := make([]byte, 16)
	if v4 := ip.To4(); v4 != nil {
		copy(pad, v4)
		return append(append(b, apiAddressFamilyIP4), pad...)
	}
	copy(pad, ip.To16())
	return append(append(b, apiAddressFamilyIP6), pad...)
}

// readAddress 读回 vl_api_address_t（17 字节）。af 不是 v4/v6 时返回 nil：
// VPP 用"没有地址"和"地址是 0.0.0.0"表达两件事，把它们读成同一个值，
// 调用方就没法判断 peer 到底有没有 endpoint 了。
func readAddress(f []byte) net.IP {
	switch f[0] {
	case apiAddressFamilyIP4:
		return net.IP(append([]byte(nil), f[1:5]...))
	case apiAddressFamilyIP6:
		return net.IP(append([]byte(nil), f[1:17]...))
	}
	return nil
}
