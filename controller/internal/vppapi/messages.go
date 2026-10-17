package vppapi

import (
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
// 值取自 srwg-node1（VPP 26.06 + sr_wg 1.0.0）的实测注册表。
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
// "destination port"，即外层 UDP 目的端口：51820 打对端 CPE，51821 打中转
// （定案 2 显式下发的那两个端口）。
type PeerParams struct {
	PublicKey  []byte // wgKeyLen 字节 X25519 公钥
	EndpointIP net.IP
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

// RebindPorts 按当前 vpn_port / transit_port 重新注册 UDP 目的端口。
//
// 必须在**创建 WG 接口之后**调用：建接口时 wireguard 插件会把 51820 重新注册给
// 自己的 wg4-input，把本插件的旁路入口摘掉。这是数据面挂点的顺序约束，不是可选
// 优化 —— 漏掉它，SR 隧道会静默退化成普通 WG。
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
