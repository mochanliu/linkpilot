package vppapi

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// x25519Public 由私钥推算 WireGuard 公钥，只用标准库。
//
// 不能用 golang.org/x/crypto/curve25519：本模块定死了零第三方依赖。
// crypto/ecdh 的 X25519 私钥就是那个未夹克的 32 字节标量，PublicKey() 做的
// 正是 scalarmult(标量, u=9)，与 VPP 里 wireguard 插件的推导同一套。
func x25519Public(t *testing.T, priv []byte) []byte {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		t.Fatalf("ecdh X25519 私钥不合法: %v", err)
	}
	return k.PublicKey().Bytes()
}

// 真机验证专用的号段：躲开实验室约定的 51820/51821，也躲开 instance 0（wg0）。
// 段的地址一律用 TEST-NET-1（192.0.2.0/24，RFC 5737）：不可路由，所以在这台
// 与别的会话**共用**的节点上不会把任何真实流量带偏。
const (
	liveInstance  = 9001
	livePort      = 51999
	liveInstance2 = 9002
	livePort2     = 51998
	liveHmacID    = 9001
	livePathID    = 9001
)

// TestLiveAgainstVPP 是真机验证，默认跳过：只有显式给出 socket 才跑。
//
// 在容器里跑（macOS 上没有 VPP）：
//
//	SRWG_VPP_SOCK=/run/vpp/api.sock SRWG_UNDERLAY_IP=10.10.1.2 \
//	  go test ./internal/vppapi -run TestLive -v
//
// 它证明的是这个包最要紧的六件事，全部用真 VPP 而不是假端：
//  1. 握手、消息尺寸与 CRC 校验对得上真实注册表（假端只能锁住我们自己抄的格式）；
//  2. 定案 3：agent 自己生成的 X25519 私钥能原样装进 VPP —— dump 回来的私钥
//     逐字节等于装进去的那把，公钥等于本地用 crypto/ecdh 推算的那把；
//  3. 观测表为空时返回空切片而不是报错（VPP 的 dump 对空结果一帧都不发）；
//  4. SR 路径/隧道绑定/encap 挂点这一整套能配进真数据面，且 API 读回与
//     `vppctl show sr-wg` 读到的是同一张表、同一段序（relay[0]=离终点最近的中转、
//     relay[n-1]=第一跳；终点段不在这张表里，由 encap 从报文外层现取）；
//  5. peer 与路径的**重复下发语义**（agent 重启与 NAT 重绑全靠它）：同公钥
//     重复 add peer 回 -116 且**跨接口也回 -116**（去重键不含接口，换 endpoint
//     也救不回来）、0 条 allowed_ip 回 -7、删不存在的 peer 回 -6、
//     同 path_id 再 AddPath 是原地替换中转链、同接口再 SetTunnel 也是原地更新
//     （旧的本端源端口键会被摘走）、DelPath 会静默级联解掉引用它的绑定；
//  6. core 的 sw_interface_set_mtu（MTU 唯一的写入口，实测三件事）：新建的 WG 接口
//     带着 0/0/0/0 出生（所以"不写"不等于"有默认值"）；写 1360 得到 1360/0/0/0
//     —— IP4 槽回落 L3，与 CLI `packet N` 同一形态；VPP 对值**毫不校验**（0 照收并
//     清零四槽），但接口号写错会当场回 retval=-2，所以出口接口填错不会是静默的。
//  7. core 的 sw_interface_dump（写入口的对面）：接口名、mtu[0]、ADMIN_UP/LINK_UP
//     能读回来，且与 `vppctl show interface` **逐条对上**。这一条是那份线布局的
//     存在理由 —— 偏移照着 VPP 生成的头文件抄，只有拿另一个独立实现来对，才不是
//     自证。CLI 清零 MTU 之后这里也必须读出 0，否则读的是缓存。
//
// 号段全部走 9001/9002 与 51998/51999，路径段地址一律 TEST-NET-1，跑完自己
// 删干净：这台节点是和别的会话共用的，不能留下任何会影响真实流量的配置。
func TestLiveAgainstVPP(t *testing.T) {
	sock := os.Getenv("SRWG_VPP_SOCK")
	if sock == "" {
		t.Skip("未设 SRWG_VPP_SOCK，跳过真机验证")
	}
	src := net.ParseIP(os.Getenv("SRWG_UNDERLAY_IP"))
	if src == nil {
		src = net.IPv4(10, 10, 1, 2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := Dial(ctx, sock, "srwg-vppapi-livetest")
	if err != nil {
		t.Fatalf("Dial(%s): %v", sock, err)
	}
	defer c.Close()

	t.Run("pkey_set_del", func(t *testing.T) {
		key := make([]byte, 16)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		if err := c.SetPKey(ctx, 99, key); err != nil {
			t.Fatalf("SetPKey: %v", err)
		}
		if err := c.DelPKey(ctx, 99); err != nil {
			t.Fatalf("DelPKey: %v", err)
		}
	})

	t.Run("empty_observation_dump", func(t *testing.T) {
		if err := c.ClearObservations(ctx); err != nil {
			t.Fatalf("ClearObservations: %v", err)
		}
		obs, err := c.Observations(ctx)
		if err != nil {
			t.Fatalf("空观测表必须返回空切片而不是错误: %v", err)
		}
		t.Logf("清空后观测 %d 条", len(obs))
	})

	t.Run("static_private_key_interface", func(t *testing.T) {
		priv := make([]byte, wgKeyLen)
		if _, err := rand.Read(priv); err != nil {
			t.Fatal(err)
		}
		wantPub := x25519Public(t, priv)

		// user_instance 必须挑一个没人用过的：实验室的 node 上已经有 wg0
		// （instance 0），而 wireguard 插件在**实例号被占用**时回的是
		// VNET_API_ERROR_INVALID_REGISTRATION = -31（实测），不是 -79 那种
		// "interface already exists"。用 0 会让这条测试看起来像客户端坏了。
		idx, err := c.CreateInterface(ctx, InterfaceParams{
			UserInstance: liveInstance,
			ListenPort:   livePort,
			PrivateKeys:  priv,
			SrcIP:        src,
		})
		if err != nil {
			t.Fatalf("CreateInterface: %v", err)
		}
		t.Logf("wireguard_interface_create 返回 sw_if_index=%d", idx)

		// t.Cleanup 在子测试函数**返回之后**才跑，所以清理用的 context 必须在
		// 里面现造：放外面配 defer cancel 的话，清理拿到的是一个已取消的 ctx，
		// 结果是接口没删掉、测试却"通过"——下次再跑就会撞上自己留下的实例。
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.DeleteInterface(ctx, idx); err != nil {
				t.Errorf("清理 WG 接口 idx=%d 失败: %v（手工 vppctl wireguard delete）", idx, err)
			}
			// 建接口会让 wireguard 插件把端口重新注册给自己的 wg4-input；
			// 删完必须把本插件的挂点补回来，否则实验室的 SR 旁路口就少了。
			if err := c.RebindPorts(ctx); err != nil {
				t.Errorf("RebindPorts: %v", err)
			}
		})

		ifs, err := c.Interfaces(ctx, true)
		if err != nil {
			t.Fatalf("Interfaces: %v", err)
		}
		var found *InterfaceInfo
		for i := range ifs {
			if ifs[i].SWIfIndex == idx {
				found = &ifs[i]
			}
		}
		if found == nil {
			t.Fatalf("dump 里找不到刚建的接口 idx=%d（共 %d 条）", idx, len(ifs))
		}
		if string(found.PrivateKey) != string(priv) {
			t.Errorf("VPP 持有的私钥不是我们装进去的那把 —— 定案 3 不成立\n 装进去 %x\n dump 回来 %x",
				priv, found.PrivateKey)
		}
		if string(found.PublicKey) != string(wantPub) {
			t.Errorf("公钥不是由这把私钥推出来的：VPP 推 %x，本地 ecdh 推 %x",
				found.PublicKey, wantPub)
		}
		t.Logf("定案 3 实测：VPP 持有 agent 生成的私钥（%x…），公钥 %x… 与本地推算一致",
			found.PrivateKey[:4], found.PublicKey[:4])

		peerPub := make([]byte, wgKeyLen)
		if _, err := rand.Read(peerPub); err != nil {
			t.Fatal(err)
		}
		peerIdx, err := c.AddPeer(ctx, PeerParams{
			PublicKey:  peerPub,
			EndpointIP: net.IPv4(10, 10, 2, 2),
			Port:       51821,
			Keepalive:  25,
			SWIfIndex:  idx,
			AllowedIPs: []Prefix{{IP: net.IPv4(192, 168, 99, 3), Len: 32}},
		})
		if err != nil {
			// 错误要原样露出来：静默跳过会让"隧道能配好"变成假话。
			t.Errorf("AddPeer: %v", err)
			return
		}
		t.Logf("wireguard_peer_add 返回 peer_index=%d", peerIdx)

		// 没有 allowed_ip 的 peer VPP **不收**：实测 -7 = INVALID_VALUE。
		// 这条对上层不是细节而是硬门槛 —— 契约里现在还没有 overlay 地址
		// （见 internal/wire：PeerInfo 没有任何隧道 IP 字段），所以 agent 在
		// 拿到地址之前根本装不出 peer，"配好隧道"必须等这个字段落地。
		_, err = c.AddPeer(ctx, PeerParams{
			PublicKey:  mustRand(t, wgKeyLen),
			EndpointIP: net.IPv4(10, 10, 2, 9),
			Port:       51820,
			SWIfIndex:  idx,
		})
		var noIPs APIError
		if !errors.As(err, &noIPs) || noIPs.Retval != -7 {
			t.Fatalf("空 allowed_ips 应该被 VPP 拒掉（-7 INVALID_VALUE），got: %v", err)
		}

		// 同公钥再加一次，**换 endpoint 也不行**：实测 -116 =
		// VNET_API_ERROR_ENTRY_ALREADY_EXISTS（对照 /usr/include/vnet/error.h）。
		// 去重键是公钥而不是 (公钥, endpoint)，所以"对端换了 NAT 端口"这件事
		// 在数据面只能靠先 remove 再 add 完成 —— agent 的 reconcile 必须这么写，
		// 否则它会以为 add 失败是自己没配好，隧道永远停在旧地址上。
		_, err = c.AddPeer(ctx, PeerParams{
			PublicKey:  peerPub,
			EndpointIP: net.IPv4(10, 10, 2, 3),
			Port:       51821,
			Keepalive:  25,
			SWIfIndex:  idx,
			AllowedIPs: []Prefix{{IP: net.IPv4(192, 168, 99, 4), Len: 32}},
		})
		var dup APIError
		if !errors.As(err, &dup) || dup.Retval != -116 {
			t.Fatalf("重复 add（换 endpoint）应该回 -116 ENTRY_ALREADY_EXISTS，got: %v", err)
		}
		t.Logf("同公钥换 endpoint 再 add = %v（去重键是公钥，只能先删后加）", err)

		// peer 的读回是 agent 重启时"认回已有隧道"的唯一依据：认不回来就会
		// 第二次 add 撞上 -31，或者更糟——把旧 endpoint 当成新的一直不去改。
		// 这里按公钥找（不是按下标），因为 VPP 的 peer_index 会回收复用，
		// 只有公钥是"这条 peer 是谁"的稳定答案。
		peers, err := c.Peers(ctx)
		if err != nil {
			t.Fatalf("Peers: %v", err)
		}
		var got *WgPeer
		onMine := 0
		for i := range peers {
			if equalBytes(peers[i].PublicKey, peerPub) {
				got = &peers[i]
			}
			if peers[i].SWIfIndex == idx {
				onMine++
			}
		}
		if got == nil {
			t.Fatalf("刚装的 peer 不在 dump 里（共 %d 条 peer）", len(peers))
		}
		if got.PeerIndex != peerIdx {
			t.Errorf("peer_index 对不上：add 回 %d，dump 回 %d", peerIdx, got.PeerIndex)
		}
		if got.SWIfIndex != idx {
			t.Errorf("挂在别的接口上：want %d, got %d", idx, got.SWIfIndex)
		}
		if onMine != 1 {
			t.Errorf("我这个接口上应该有且只有 1 条 peer，dump 出 %d 条（dump 不带接口过滤，客户端按 SWIfIndex 筛）", onMine)
		}
		// endpoint / port / keepalive 三个字段读错，agent 就会判定"没变化"而
		// 不重配 —— 那正是"NAT 重绑之后隧道再也不通"的成因。
		if !got.EndpointIP.Equal(net.IPv4(10, 10, 2, 2)) {
			t.Errorf("endpoint = %v，want 10.10.2.2", got.EndpointIP)
		}
		if got.Port != 51821 || got.Keepalive != 25 {
			t.Errorf("port=%d keepalive=%d，want 51821/25", got.Port, got.Keepalive)
		}
		if len(got.AllowedIPs) != 1 || !got.AllowedIPs[0].IP.Equal(net.IPv4(192, 168, 99, 3)) ||
			got.AllowedIPs[0].Len != 32 {
			t.Errorf("allowed_ips = %+v，want 192.168.99.3/32", got.AllowedIPs)
		}
		t.Logf("peer 读回：index=%d endpoint=%v:%d keepalive=%d allowed=%v",
			got.PeerIndex, got.EndpointIP, got.Port, got.Keepalive, got.AllowedIPs)

		// CLI 是第二只眼睛：show wireguard peer 打的是十六进制公钥。
		if out, err := vppctl(ctx, "show", "wireguard", "peer"); err == nil {
			line := cliLine(out, hex.EncodeToString(peerPub))
			if line == "" {
				t.Errorf("CLI 里看不到这条 peer（API 与 CLI 读的是同一张表）:\n%s", out)
			} else if !strings.Contains(line, "public key:") {
				t.Errorf("CLI 那行不像公钥行: %q", line)
			} else {
				t.Logf("CLI 佐证: %s", strings.TrimSpace(line))
			}
		} else {
			t.Logf("没有 vppctl，跳过 CLI 佐证: %v", err)
		}

		// VPP 26.06 没有"改 peer endpoint"这条消息，只有 add/remove/dump，
		// 所以换 endpoint 必须是先删后加。删除走不通的话，这条路径整个不成立。
		if err := c.RemovePeer(ctx, got.PeerIndex); err != nil {
			t.Fatalf("RemovePeer(%d): %v", got.PeerIndex, err)
		}
		peers, err = c.Peers(ctx)
		if err != nil {
			t.Fatalf("删除后 Peers: %v", err)
		}
		for _, p := range peers {
			if equalBytes(p.PublicKey, peerPub) {
				t.Fatalf("删掉了 yet dump 里还在: %+v", p)
			}
		}
		if out, err := vppctl(ctx, "show", "wireguard", "peer"); err == nil {
			if l := cliLine(out, hex.EncodeToString(peerPub)); l != "" {
				t.Errorf("CLI 里这条 peer 还在: %q", l)
			}
		}

		// 删不存在的 peer：VPP 不会当成成功。把 retval 钉成测试，是因为 agent 的
		// reconcile 逻辑必须区分"这条 peer 已经不在了"（良性，继续 add）和
		// "连接/参数出错了"（不能忽略）。-6 = VNET_API_ERROR_NO_SUCH_ENTRY，
		// 由 /usr/include/vnet/error.h 的 foreach_vnet_error 对照实测值得出。
		err = c.RemovePeer(ctx, peerIdx)
		var apiErr APIError
		if !errors.As(err, &apiErr) || apiErr.Retval != -6 {
			t.Fatalf("重复删除应该回 -6 NO_SUCH_ENTRY，got: %v", err)
		}
		t.Logf("重复 RemovePeer(%d) = %v（agent 要把这个当良性）", peerIdx, err)
	})

	t.Run("path_tunnel_binding", func(t *testing.T) {
		// 这一段验的是"CPE agent 能不能把一条 SR 隧道配出来"，而且是拿真 VPP 验：
		// 建接口 → 装 P-Key → 建路径 → 绑隧道 → 读回 → 用 CLI 交叉核对同一张表。
		// CLI 与 API 读的是同一份 path_table / tunnel 绑定，所以"API 说自己配好了"
		// 必须有第二方作证，否则字节抄错也可能自洽地通过。
		key := make([]byte, pKeyLen)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		if err := c.SetPKey(ctx, liveHmacID, key); err != nil {
			t.Fatalf("SetPKey(%d): %v", liveHmacID, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.DelPKey(ctx, liveHmacID); err != nil {
				t.Errorf("清理 P-Key %d: %v", liveHmacID, err)
			}
		})

		// 先建路径再建接口：路径要求 hmac_id 已在密钥表里（否则 NO_SUCH_ENTRY），
		// 这条约束本身就是要验的东西之一。
		//
		// 这张表里只有**中转跳**：终点段不进来，是 encap 在封装前从即将发出的那个
		// 包的外层 dst IP + UDP 目的端口现取的（A 方案）。所以这里没有、也不该有
		// 对端地址可对。顺序约定 relay[0]=离终点最近的中转、relay[n-1]=第一跳，
		// 抄反了不会报错，只会让隧道"配上了但不通"，因此必须原样读回来核对。
		relays := []Segment{
			{IP: net.IPv4(192, 0, 2, 3), Port: 51822}, // [0] 离终点最近的中转
			{IP: net.IPv4(192, 0, 2, 2), Port: 51821}, // [n-1] 第一跳
		}
		if err := c.AddPath(ctx, PathParams{
			PathID: livePathID, HmacID: liveHmacID, Active: true, Relays: relays,
		}); err != nil {
			t.Fatalf("AddPath(%d): %v", livePathID, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// 删路径会连带摘掉指向它的绑定（插件文档写明），所以这里只删路径；
			// 反过来先删绑定再删路径也一样干净。
			if err := c.DelPath(ctx, livePathID); err != nil {
				t.Errorf("清理路径 %d: %v", livePathID, err)
			}
		})

		// 缺钥匙的路径必须被拒：这是"包发得出去但对端一律验签失败"的唯一防线。
		if err := c.AddPath(ctx, PathParams{
			PathID: livePathID + 1, HmacID: liveHmacID + 1,
			Relays: relays,
		}); err == nil {
			t.Error("引用不存在的 hmac_id 却建成了路径 —— 数据面回 NO_SUCH_ENTRY 才算对")
			_ = c.DelPath(ctx, livePathID+1)
		}

		// 零中转跳必须能建：直连隧道也走 SR，为的是这条头证明"发端持有这把 P-Key"
		// —— 收端把外层源记成 verified 观测的唯一凭证。真机要证的正是 handler
		// 没把它当 INVALID_VALUE 拒掉（桩测只锁得住我们这边的编码）。
		if err := c.AddPath(ctx, PathParams{
			PathID: livePathID + 2, HmacID: liveHmacID, Active: true,
		}); err != nil {
			t.Errorf("零中转跳的路径建不上: %v", err)
		} else {
			t.Cleanup(func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.DelPath(cctx, livePathID+2); err != nil {
					t.Errorf("清理路径 %d: %v", livePathID+2, err)
				}
			})
			ps, err := c.Paths(ctx, livePathID+2)
			if err != nil || len(ps) != 1 {
				t.Fatalf("Paths(%d) = %v, %v", livePathID+2, ps, err)
			}
			if len(ps[0].Relays) != 0 {
				t.Errorf("零中转跳读回了 %d 跳: %+v", len(ps[0].Relays), ps[0].Relays)
			}
		}

		idx, err := c.CreateInterface(ctx, InterfaceParams{
			UserInstance: liveInstance2,
			ListenPort:   livePort2,
			PrivateKeys:  mustRand(t, wgKeyLen),
			SrcIP:        src,
		})
		if err != nil {
			t.Fatalf("CreateInterface: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.DeleteInterface(ctx, idx); err != nil {
				t.Errorf("清理 WG 接口 idx=%d: %v", idx, err)
			}
			if err := c.RebindPorts(ctx); err != nil {
				t.Errorf("RebindPorts: %v", err)
			}
		})

		// src_port 填的是**本端**这条隧道的监听端口：收包侧就靠它认隧道身份
		// （实测 VPP WG 发包的 UDP 源端口 = 接口监听端口）。绑定里没有对端地址。
		if err := c.SetTunnel(ctx, TunnelBinding{
			SWIfIndex: idx, PathID: livePathID, SrcPort: livePort2,
		}); err != nil {
			t.Fatalf("SetTunnel: %v", err)
		}

		paths, err := c.Paths(ctx, ^uint16(0))
		if err != nil {
			t.Fatalf("Paths: %v", err)
		}
		var got *PathInfo
		for i := range paths {
			if paths[i].PathID == livePathID {
				got = &paths[i]
			}
		}
		if got == nil {
			t.Fatalf("刚建的路径没被 dump 回来（共 %d 条）", len(paths))
		}
		if got.HmacID != liveHmacID || !got.Active {
			t.Errorf("路径头部字段不对: %+v", got)
		}
		// 中转链原样回来才算对上数据面的约定：[0]=离终点最近、[n-1]=第一跳。
		if len(got.Relays) != len(relays) {
			t.Fatalf("中转跳数 = %d，期望 %d", len(got.Relays), len(relays))
		}
		for i := range relays {
			if !got.Relays[i].IP.Equal(relays[i].IP) || got.Relays[i].Port != relays[i].Port {
				t.Errorf("relays[%d] = %v:%d，期望 %v:%d",
					i, got.Relays[i].IP, got.Relays[i].Port, relays[i].IP, relays[i].Port)
			}
		}

		tuns, err := c.Tunnels(ctx, ^uint32(0))
		if err != nil {
			t.Fatalf("Tunnels: %v", err)
		}
		var bound *TunnelInfo
		for i := range tuns {
			if tuns[i].SWIfIndex == idx {
				bound = &tuns[i]
			}
		}
		if bound == nil {
			t.Fatalf("刚绑的隧道没被 dump 回来（共 %d 条）", len(tuns))
		}
		if bound.PathID != livePathID || bound.SrcPort != livePort2 {
			t.Errorf("绑定不对: %+v（期望 path=%d src_port=%d）", bound, livePathID, livePort2)
		}
		// is_effective 取决于本机有没有 underlay 启用了 sr-wg4-encap。这里只报状态
		// 不改它：实验室的节点可能正被别的会话跑端到端，关掉挂点会把他们的隧道打断。
		t.Logf("隧道绑定 idx=%d path=%d src_port=%d effective=%v",
			idx, bound.PathID, bound.SrcPort, bound.Effective)

		// CLI 交叉核对：同一张表，第二个读者。串取自 sr_wg_cli.c 的
		// "  [%d] path_id=%u hmac_id=%u relay_count=%u active=%u" 与
		// "      relay[%d]%s: <ip>:<port>"（最后一条带 " (第一跳)"）。
		if out, err := vppctl(ctx, "show", "sr-wg", "paths"); err == nil {
			checks := []string{fmt.Sprintf("path_id=%d hmac_id=%d relay_count=%d active=1",
				livePathID, liveHmacID, len(relays))}
			for i, s := range relays {
				tag := ""
				if i == len(relays)-1 {
					tag = " (第一跳)"
				}
				checks = append(checks, fmt.Sprintf("relay[%d]%s: %v:%d", i, tag, s.IP, s.Port))
			}
			for _, want := range checks {
				if !strings.Contains(out, want) {
					t.Errorf("`show sr-wg paths` 里没有 %q\n%s", want, out)
				}
			}
		} else {
			t.Logf("跳过 CLI 交叉核对（vppctl 不可用）: %v", err)
		}
		if out, err := vppctl(ctx, "show", "sr-wg", "tunnels"); err == nil {
			// 必须落在**同一行**：只看整段输出会读到别的会话留下的绑定。
			line := cliLine(out, fmt.Sprintf("sw_if_index=%d ", idx))
			want := fmt.Sprintf("path_id=%d src_port=%d usable", livePathID, livePort2)
			if line == "" || !strings.Contains(line, want) {
				t.Errorf("`show sr-wg tunnels` 里 idx=%d 那行没有 %q，实际 %q\n%s",
					idx, want, line, out)
			}
			t.Logf("CLI 眼里的绑定: %s", strings.TrimSpace(line))
		} else {
			t.Logf("跳过 CLI 交叉核对（vppctl 不可用）: %v", err)
		}

		// encap 挂点的启停只在**自己刚建的接口**上试：真正的 underlay 是共用状态，
		// 动它会影响同一节点上别的会话。这条验的是消息编解码与 handler 走到，
		// 传 wg0 而不是 underlay 在数据面上本就是错的挂点，不留残留。
		if err := c.EnableEncap(ctx, idx, true); err != nil {
			t.Errorf("EnableEncap(on): %v", err)
		}
		if err := c.EnableEncap(ctx, idx, false); err != nil {
			t.Errorf("EnableEncap(off): %v", err)
		}
	})

	t.Run("path_replace_and_binding_cascade", func(t *testing.T) {
		// 这一段钉的是 agent 的 reconcile 算法能不能这么写，全部是真机测出来的：
		//   a) 同 path_id 再 AddPath 是**原地替换**中转链，不报错 —— 所以换中转
		//      不需要先删路径（先删反而会把隧道绑定一起带走，见 d）；
		//   b) 替换中转链之后隧道绑定**还在、且不需要跟着改**：绑定只认本端源端口，
		//      终点段又是从报文现取的，所以"改对端地址"在数据面只剩 WG peer 一处
		//      —— A 方案之前那条"改路径必须 path+tunnel 成对下发"的不变量已经不成立；
		//   c) encap 挂点开关幂等，agent 每次启动都可以无条件调用；
		//   d) DelPath 一条被隧道引用的路径会**级联解绑**且 retval=0 —— 静默。
		key := mustRand(t, pKeyLen)
		if err := c.SetPKey(ctx, liveHmacID, key); err != nil {
			t.Fatalf("SetPKey: %v", err)
		}
		idx, err := c.CreateInterface(ctx, InterfaceParams{
			UserInstance: liveInstance,
			ListenPort:   livePort,
			PrivateKeys:  mustRand(t, wgKeyLen),
			SrcIP:        src,
		})
		if err != nil {
			t.Fatalf("CreateInterface: %v", err)
		}
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = c.ClearTunnel(cctx, idx)
			_ = c.DelPath(cctx, livePathID)
			_ = c.DelPKey(cctx, liveHmacID)
			if err := c.DeleteInterface(cctx, idx); err != nil {
				t.Errorf("清理 WG 接口 idx=%d: %v", idx, err)
			}
			if err := c.RebindPorts(cctx); err != nil {
				t.Errorf("RebindPorts: %v", err)
			}
		})

		add := func(relays ...Segment) {
			if err := c.AddPath(ctx, PathParams{PathID: livePathID, HmacID: liveHmacID,
				Active: true, Relays: relays}); err != nil {
				t.Fatalf("AddPath(%d 跳中转): %v", len(relays), err)
			}
		}
		pathRelays := func() []Segment {
			ps, err := c.Paths(ctx, livePathID)
			if err != nil || len(ps) != 1 {
				t.Fatalf("Paths(%d) = %v, %v", livePathID, ps, err)
			}
			return ps[0].Relays
		}
		bound := func() (*TunnelInfo, bool) {
			ts, err := c.Tunnels(ctx, idx)
			if err != nil {
				t.Fatalf("Tunnels: %v", err)
			}
			if len(ts) == 0 {
				return nil, false
			}
			if len(ts) != 1 {
				t.Fatalf("我这个接口上应该只有一条绑定，实际 %d 条", len(ts))
			}
			return &ts[0], true
		}

		// 一跳起步，随后换成两跳：hop2 离终点更近，hop1 是第一跳（顺序约定）。
		hop1 := Segment{IP: net.ParseIP("192.0.2.10").To4(), Port: 51821}
		hop2 := Segment{IP: net.ParseIP("192.0.2.11").To4(), Port: 51822}
		add(hop1)
		if err := c.SetTunnel(ctx, TunnelBinding{
			SWIfIndex: idx, PathID: livePathID, SrcPort: livePort,
		}); err != nil {
			t.Fatalf("SetTunnel: %v", err)
		}
		if b, ok := bound(); !ok || b.PathID != livePathID || b.SrcPort != livePort {
			t.Fatalf("绑定初值不对: %+v %v", b, ok)
		}

		// a) 原地替换中转链
		add(hop2, hop1)
		if got := pathRelays(); len(got) != 2 ||
			!got[0].IP.Equal(hop2.IP) || got[0].Port != hop2.Port ||
			!got[1].IP.Equal(hop1.IP) || got[1].Port != hop1.Port {
			t.Errorf("同 path_id 再 AddPath 没替换中转链：dump 回来 %+v，期望 [%v %v]",
				got, hop2, hop1)
		}
		// b) 绑定原样还在，且没有任何"对端地址"字段要跟着改。
		if b, ok := bound(); !ok {
			t.Error("替换中转链之后绑定消失了：DelPath 才该解绑，AddPath 不该")
		} else if b.PathID != livePathID || b.SrcPort != livePort {
			t.Errorf("换中转链把绑定也改了: %+v", b)
		} else {
			t.Logf("实测：换中转链后绑定仍是 path=%d src_port=%d —— 改对端地址只剩 WG peer 一处",
				b.PathID, b.SrcPort)
		}
		// 同接口重复 SetTunnel 必须是原地更新（agent 每次下发都无条件调它，
		// 不能靠"先解绑再绑"，那中间有一条窗口会把包旁路成无 SR 的普通 WG）。
		// 换本端端口时旧键会被摘掉（sr_wg4_encap.c 的 old_port 分支），但那一步
		// 只在数据面生效、配置面 dump 不出来，这里不声称实测到了。
		if err := c.SetTunnel(ctx, TunnelBinding{
			SWIfIndex: idx, PathID: livePathID, SrcPort: livePort2,
		}); err != nil {
			t.Fatalf("SetTunnel(换本端端口): %v", err)
		}
		if b, ok := bound(); !ok || b.SrcPort != livePort2 {
			t.Errorf("同接口重复 SetTunnel 没原地更新: %+v %v（期望 src_port=%d）",
				b, ok, livePort2)
		}

		// c) encap 幂等：无条件重复调用不能报错，也不能出现 FEATURE_ALREADY_* 那种
		//  retval —— agent 每次启动都调一次，报一次错就等于每次都要人工判读。
		for i := 0; i < 2; i++ {
			if err := c.EnableEncap(ctx, idx, true); err != nil {
				t.Errorf("EnableEncap(on) 第 %d 次: %v", i+1, err)
			}
		}
		for i := 0; i < 2; i++ {
			if err := c.EnableEncap(ctx, idx, false); err != nil {
				t.Errorf("EnableEncap(off) 第 %d 次: %v", i+1, err)
			}
		}
		if err := c.EnableEncap(ctx, idx, true); err != nil {
			t.Fatalf("EnableEncap 收尾: %v", err)
		}

		// d) 级联解绑，且 retval 是 0
		if err := c.DelPath(ctx, livePathID); err != nil {
			t.Fatalf("DelPath: %v", err)
		}
		if _, ok := bound(); ok {
			t.Error("删掉被引用的路径之后，隧道绑定居然还在 —— 数据面应当级联清掉")
		}
		t.Log("实测：DelPath 会连带清掉引用它的隧道绑定，且不报错")
	})

	// 每隧道一个监听端口（定案 2：UDP 端口既分功能也分隧道）。这里钉的是 agent 的
	// claimListenPort 与 ApplyPeer 直接依赖的三件真机行为：
	//   a) AddPort 幂等、每次都把端口挂回 sr-wg 节点；Ports 原样读回两种 kind；
	//      DelPort 也幂等（不在集合里删除不是错误）；
	//   b) 同一个外层 IP 上的**两条**隧道（本端监听端口不同）各有各的绑定，互不
	//      覆盖 —— 隧道身份就是本端源端口（A 方案），配置面一接口一槽；
	//   c) 两张接口填了**同一个** src_port 不报错：点对点隧道不做重复防护（操作员
	//      2026-11-03 明确免除），本条只钉住"回 0、两条都还在 dump 里"这个事实。
	t.Run("per_tunnel_ports_and_bind_identity", func(t *testing.T) {
		const (
			instA  = uint32(9003)
			instB  = uint32(9004)
			portA  = uint16(51995)
			portB  = uint16(51994)
			pathA  = uint16(9011)
			pathB  = uint16(9012)
			pathC  = uint16(9013) // 撞端口那一次用的 path_id
			probe  = uint16(51993)
			hmacID = uint16(9011)
		)
		// a) 端口集合。
		for i := 0; i < 2; i++ {
			if err := c.AddPort(ctx, PortKindVPN, probe); err != nil {
				t.Fatalf("AddPort(%d) 第 %d 次: %v", probe, i+1, err)
			}
		}
		portsOf := func(kind uint8) []uint16 {
			ps, err := c.Ports(ctx)
			if err != nil {
				t.Fatalf("Ports: %v", err)
			}
			var out []uint16
			for _, p := range ps {
				if p.Kind == kind {
					out = append(out, p.Port)
				}
			}
			return out
		}
		contains := func(list []uint16, v uint16) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		}
		if got := portsOf(PortKindVPN); !contains(got, probe) {
			t.Errorf("AddPort 之后 vpn 集合里没有 %d: %v", probe, got)
		}
		if out, err := vppctl(ctx, "show", "sr-wg", "ports"); err == nil {
			if !strings.Contains(out, strconv.Itoa(int(probe))) {
				t.Errorf("`show sr-wg ports` 里没有 %d:\n%s", probe, out)
			}
		} else {
			t.Logf("跳过 CLI 交叉核对（vppctl 不可用）: %v", err)
		}
		if err := c.DelPort(ctx, PortKindVPN, probe); err != nil {
			t.Fatalf("DelPort: %v", err)
		}
		if err := c.DelPort(ctx, PortKindVPN, probe); err != nil {
			t.Errorf("重复 DelPort 应当幂等: %v", err)
		}
		if got := portsOf(PortKindVPN); contains(got, probe) {
			t.Errorf("DelPort 之后 %d 还在集合里: %v", probe, got)
		}

		// b)/c) 两条隧道打在同一个外层 IP 上，靠各自的本端监听端口分身份。
		if err := c.SetPKey(ctx, hmacID, mustRand(t, pKeyLen)); err != nil {
			t.Fatalf("SetPKey: %v", err)
		}
		mkPath := func(pathID uint16, hop string) {
			if err := c.AddPath(ctx, PathParams{PathID: pathID, HmacID: hmacID, Active: true,
				Relays: []Segment{{IP: net.ParseIP(hop).To4(), Port: 51821}}}); err != nil {
				t.Fatalf("AddPath(%d): %v", pathID, err)
			}
		}
		mkIface := func(inst uint32, port uint16) uint32 {
			idx, err := c.CreateInterface(ctx, InterfaceParams{
				UserInstance: inst, ListenPort: port,
				PrivateKeys: mustRand(t, wgKeyLen), SrcIP: src})
			if err != nil {
				t.Fatalf("CreateInterface(%d): %v", inst, err)
			}
			t.Cleanup(func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.DeleteInterface(cctx, idx); err != nil {
					t.Errorf("清理接口 idx=%d: %v", idx, err)
				}
				// 建/删接口都会动 UDP 挂点，收尾把集合重新声明一遍。
				if err := c.RebindPorts(cctx); err != nil {
					t.Errorf("RebindPorts: %v", err)
				}
			})
			return idx
		}
		mkPath(pathA, "192.0.2.21")
		mkPath(pathB, "192.0.2.22")
		mkPath(pathC, "192.0.2.23")
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, id := range []uint16{pathA, pathB, pathC} {
				if err := c.DelPath(cctx, id); err != nil {
					t.Errorf("清理路径 %d: %v", id, err)
				}
			}
			if err := c.DelPKey(cctx, hmacID); err != nil {
				t.Errorf("清理 P-Key %d: %v", hmacID, err)
			}
		})
		idxA, idxB := mkIface(instA, portA), mkIface(instB, portB)

		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idxA, PathID: pathA, SrcPort: portA}); err != nil {
			t.Fatalf("SetTunnel(A): %v", err)
		}
		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idxB, PathID: pathB, SrcPort: portB}); err != nil {
			t.Fatalf("第二条隧道（同 IP、不同本端端口）配不上: %v", err)
		}
		tuns, err := c.Tunnels(ctx, ^uint32(0))
		if err != nil {
			t.Fatalf("Tunnels: %v", err)
		}
		bindOf := func(idx uint32) *TunnelInfo {
			for i := range tuns {
				if tuns[i].SWIfIndex == idx {
					return &tuns[i]
				}
			}
			return nil
		}
		// 只比本会话自己那两条：这台节点和别的会话共用，全表断言会读到别人的现场。
		if b := bindOf(idxA); b == nil || b.PathID != pathA || b.SrcPort != portA {
			t.Errorf("第一条绑定被第二条改了: %+v（期望 path=%d src_port=%d）", b, pathA, portA)
		}
		if b := bindOf(idxB); b == nil || b.PathID != pathB || b.SrcPort != portB {
			t.Errorf("第二条绑定不对: %+v（期望 path=%d src_port=%d）", b, pathB, portB)
		}

		// c) 两张接口填同一个 src_port：不报错（点对点隧道不做重复防护，操作员明确
		//    免除）。这里的实测口径只是"两条绑定都在"；数据面这个键只会指向后写的那
		//    一条（sr_wg_bind_set_port 用 hash_set 直接覆盖），先写的那条从此在收包侧
		//    命中不到 —— 配置面看不出来。保证不撞端口是 agent 的事：每个隧道的本端
		//    端口由 claimListenPort 独占分配。
		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idxB, PathID: pathC, SrcPort: portA}); err != nil {
			var apiErr APIError
			if errors.As(err, &apiErr) && apiErr.Retval == -116 {
				t.Errorf("数据面又返回了重复端口保护（-116）：这套语义已按 A 方案删掉，" +
					"点对点隧道不该有这条拦截")
			}
			t.Fatalf("同 src_port 重复下发应当成功，got: %v", err)
		}
		tuns, err = c.Tunnels(ctx, ^uint32(0))
		if err != nil {
			t.Fatalf("Tunnels(复核): %v", err)
		}
		if b := bindOf(idxA); b == nil || b.PathID != pathA || b.SrcPort != portA {
			t.Errorf("重复 src_port 把第一条接口自己的绑定改了: %+v", b)
		}
		if b := bindOf(idxB); b == nil || b.PathID != pathC || b.SrcPort != portA {
			t.Errorf("重复 src_port 之后第二条接口没原地更新: %+v（期望 path=%d src_port=%d）",
				b, pathC, portA)
		}
		t.Log("实测：同 src_port 跨接口重复 = 回 0 且两条绑定都在，端口键归后写那条（免除防护）")
	})

	// WG peer 的去重键**不含接口**：同一把公钥装到第二张接口上，VPP 回 -116。
	// 这条划定了上层能自愈的边界 —— "这把公钥已经有人装了"可能是别的接口上的
	// 现成 peer（操作员手配的 wg0、同机第二个 agent），删它等于拆别人的隧道。
	t.Run("peer_dedup_spans_interfaces", func(t *testing.T) {
		pub := mustRand(t, wgKeyLen)
		mk := func(inst uint32, port uint16) uint32 {
			idx, err := c.CreateInterface(ctx, InterfaceParams{
				UserInstance: inst, ListenPort: port,
				PrivateKeys: mustRand(t, wgKeyLen), SrcIP: src})
			if err != nil {
				t.Fatalf("CreateInterface(%d): %v", inst, err)
			}
			t.Cleanup(func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.DeleteInterface(cctx, idx); err != nil {
					t.Errorf("清理接口 idx=%d 失败: %v（手工 vppctl wireguard delete）", idx, err)
				}
				if err := c.RebindPorts(cctx); err != nil {
					t.Errorf("RebindPorts: %v", err)
				}
			})
			return idx
		}
		a := mk(liveInstance, livePort)
		b := mk(liveInstance2, livePort2)

		if _, err := c.AddPeer(ctx, PeerParams{
			PublicKey: pub, EndpointIP: net.ParseIP("192.0.2.77"), Port: 51820,
			Keepalive: 25, SWIfIndex: a,
			AllowedIPs: []Prefix{{IP: net.IPv4(10, 200, 1, 1), Len: 32}}}); err != nil {
			t.Fatalf("接口 %d 装 peer: %v", a, err)
		}
		_, err := c.AddPeer(ctx, PeerParams{
			PublicKey: pub, EndpointIP: net.ParseIP("192.0.2.78"), Port: 51820,
			Keepalive: 25, SWIfIndex: b,
			AllowedIPs: []Prefix{{IP: net.IPv4(10, 200, 1, 2), Len: 32}}})
		var dup APIError
		if !errors.As(err, &dup) || dup.Retval != -116 {
			t.Fatalf("跨接口同公钥应当 -116，got: %v", err)
		}
		t.Logf("实测：接口 %d 上已持有这把公钥时，接口 %d 装它回 %d（去重键不含接口）",
			a, b, dup.Retval)

		peers, err := c.Peers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, p := range peers {
			if bytes.Equal(p.PublicKey, pub) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("这把公钥在数据面里有 %d 份，期望 1 份（第二次被拒了就不该有第二条）", n)
		}
	})

	t.Run("interface_mtu", func(t *testing.T) {
		idx, err := c.CreateInterface(ctx, InterfaceParams{
			UserInstance: liveInstance, ListenPort: livePort,
			PrivateKeys: mustRand(t, wgKeyLen), SrcIP: src})
		if err != nil {
			t.Fatalf("CreateInterface: %v", err)
		}
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.DeleteInterface(cctx, idx); err != nil {
				t.Errorf("清理接口 idx=%d 失败: %v（手工 vppctl wireguard delete）", idx, err)
			}
			if err := c.RebindPorts(cctx); err != nil {
				t.Errorf("RebindPorts: %v", err)
			}
		})

		// CLI 那一份读法留着，不是历史包袱：它是**对面那个独立实现**眼里的值。
		// API 这一侧的布局是照着 VPP 生成的头文件抄的，只有拿 CLI 来对才知道抄对了。
		readBack := func(t *testing.T) string {
			t.Helper()
			out := showInterfaces(t)
			m := mtuColumnOnIndexLine(out, idx)
			if m == "" {
				t.Fatalf("show interface 里没有 idx=%d 的 MTU 列：\n%s", idx, out)
			}
			return m
		}

		// 实测起点：手建的 wireguard 接口**没有 MTU 记录**（四个槽全 0）。
		// 这就是"agent 什么都不写"时数据面的样子，也是 DefaultTunnelMTU 存在的理由。
		before := readBack(t)
		t.Logf("新建 WG 接口 idx=%d 的 MTU 记录（未写之前）= %s", idx, before)

		if err := c.SetInterfaceMTU(ctx, idx, 1360); err != nil {
			t.Fatalf("SetInterfaceMTU(1360): %v", err)
		}
		if got := readBack(t); got != "1360/0/0/0" {
			t.Errorf("写 1360 之后 MTU 列应是 1360/0/0/0（IP4 回落到 L3），实际 %s", got)
		}

		// ==================== core 的 sw_interface_dump（#17 落地） ====================
		//
		// 这一条是整个读回能力的存在理由，而它必须和 CLI 对，不能只跟自己一致：
		// 偏移是照着 VPP 生成的 interface.api_types.h 抄的（sizeof/offsetof 用容器里
		// 的 gcc 实测），CLI 走的是 VPP 内部结构 —— 两个完全独立的实现读出同一组
		// 名字和数值，才算证明这份布局抄对了。自洽的解析器什么也说明不了：偏移错了
		// 一样能稳定读出一个错值。
		all, err := c.DumpInterfaces(ctx)
		if err != nil {
			t.Fatalf("DumpInterfaces: %v", err)
		}
		if len(all) < 2 {
			t.Fatalf("接口表只有 %d 条，不像一张真的表（至少该有 local0 和刚建的 wg）", len(all))
		}
		cliOut := showInterfaces(t)
		si, ok := FindInterface(all, idx)
		if !ok {
			t.Fatalf("接口表里读不到刚建的隧道 idx=%d（表: %s）", idx, dumpSummary(all))
		}
		if want := interfaceNameOnIndex(cliOut, idx); si.Name != want {
			t.Errorf("接口名 API=%q 而 CLI=%q —— 偏移或枚举宽度抄错了通常读出一串零", si.Name, want)
		}
		if si.L3MTU != 1360 {
			t.Errorf("API 读回的 L3 MTU = %d，CLI 那张表刚说是 1360/0/0/0", si.L3MTU)
		}
		// 整表逐条对：任何一条名字为空都是偏移错位，比单看那一条更严。
		for _, i := range all {
			if i.Name == "" {
				t.Errorf("idx=%d 的接口名读成空串：线布局对不上", i.SWIfIndex)
			}
			if m := mtuColumnOnIndexLine(cliOut, i.SWIfIndex); m != "" &&
				!strings.HasPrefix(m, fmt.Sprintf("%d/", i.L3MTU)) {
				t.Errorf("idx=%d %s：API 记 %d，CLI 是 %s", i.SWIfIndex, i.Name, i.L3MTU, m)
			}
		}
		t.Logf("接口表 %d 条：%s", len(all), dumpSummary(all))

		// ==================== 接口清单：地址（#105 落地） ====================
		//
		// 地址那一侧同样要和 CLI 对，而不是只跟自己一致：ip_address_details 里的
		// vl_api_address_with_prefix_t 是 18 字节（af + 16 + 前缀长度），按错一行
		// 就会读出一条**看起来合法**的假地址。清单里写着一个不存在的地址，比读不
		// 出来更糟 —— 控制器会拿它去和反射器看到的外层源比对，从而挑错出口。
		addrOut, err := vppctl(ctx, "show", "interface", "address")
		if err != nil {
			t.Fatalf("vppctl show interface address: %v", err)
		}
		inv, err := c.Inventory(ctx)
		if err != nil {
			t.Fatalf("Inventory: %v", err)
		}
		withAddr := 0
		for _, i := range inv {
			for _, p := range i.IPv4 {
				s := fmt.Sprintf("%s/%d", p.IP, p.Len)
				if !strings.Contains(addrOut, s) {
					t.Errorf("接口 %s(%d) 的地址 %s 在 CLI 的 show interface address 里找不到：\n%s",
						i.Name, i.SWIfIndex, s, addrOut)
				}
				withAddr++
			}
		}
		// CLI 每条地址是一行 "  L3 a.b.c.d/len"。条数不一致就是"读少/读多"，
		// 两种都会让控制器以为这台设备的地址集与事实不同。
		if cliAddrs := strings.Count(addrOut, "L3 "); withAddr != cliAddrs {
			t.Errorf("清单里 %d 条地址，CLI 有 %d 条（L3 行）", withAddr, cliAddrs)
		}
		var invLines []string
		for _, i := range inv {
			invLines = append(invLines, fmt.Sprintf("%d:%s/mtu%d%v", i.SWIfIndex, i.Name, i.L3MTU, i.IPv4))
		}
		t.Logf("接口清单 %d 张、地址 %d 条：%s", len(inv), withAddr, strings.Join(invLines, " "))

		// VPP **自己不校验**这个数。客户端把 0 拦在发出之前（单测锁住了那条），所以
		// 这里只能走 CLI 去看 VPP 的真实态度：实测 `packet 0` 照单收下，并把四个槽位
		// 一起清零 —— 那是"取消 MTU 记录"，不是"设成 0"。客户端那层拦截因此不是
		// 多余的防御。实验用刚建的这张接口，跑完就删，不留痕迹。
		name := si.Name
		out, err := vppctl(ctx, "set", "interface", "mtu", "packet", "0", name)
		if err != nil || out != "" {
			t.Logf("实测：VPP 的 CLI 拒了 MTU=0（out=%q err=%v）—— 上游补了校验，客户端那层可以退休", out, err)
		} else if got := readBack(t); got != "0/0/0/0" {
			t.Errorf("实测：CLI 写 0 之后 MTU 列 = %s，与\"四槽一起清零\"的既有观测不一致，请复核", got)
		} else {
			t.Logf("实测复现：%s 的 MTU=0 被照单收下并清零四个槽位（VPP 不校验，必须在客户端拦）", name)
			// 清零之后 API 这一侧也必须读出 0 —— 否则 DumpInterfaces 只是在读缓存，
			// 而 agent 的 verifyLanded 靠的就是"读回的是不是当下真记录"。
			again, err := c.DumpInterfaces(ctx)
			if err != nil {
				t.Fatalf("清零后再 DumpInterfaces: %v", err)
			}
			if si2, ok := FindInterface(again, idx); !ok {
				t.Errorf("清零之后接口表里连 %d 都读不到了", idx)
			} else if si2.L3MTU != 0 {
				t.Errorf("CLI 已经显示 0/0/0/0，API 却读回 %d：这一侧读的不是当下记录", si2.L3MTU)
			}
		}

		// 接口号写错时会不会静默？这条必须有答案，因为 underlay 那个 sw_if_index
		// 仍是操作员给的（本层没有 FIB dump，算不出默认出口），而"启用成功了但一个
		// 包都没动"是本项目的老病。用一个绝不可能存在的号试，只看 VPP 回什么。
		if err := c.SetInterfaceMTU(ctx, 0xFFFF, 1400); err != nil {
			t.Logf("实测：不存在的接口号 65535 被拒绝（%v）—— 写错出口号会当场报错，不会静默", err)
		} else {
			t.Error("实测：VPP 收下了不存在接口号的改 MTU 请求（retval=0）—— " +
				"agent 必须在写之前确认这个号来自真实的 dump，不能只信 retval")
		}

		// 出口接口（host-eth0）的 VIF 记录：这台机器上它是 af-packet 建的，实测默认
		// 9000，而容器网卡只有 1500 —— 这个不一致就是 af-packet 发送侧卡死的根因。
		// 这里写成 1500 再两个实现各读一遍；真正的同步与断言由 test/scripts/test-mtu.sh 做。
		if hif := os.Getenv("SRWG_UNDERLAY_SW_IF_INDEX"); hif != "" {
			n, err := strconv.ParseUint(hif, 10, 32)
			if err != nil {
				t.Fatalf("SRWG_UNDERLAY_SW_IF_INDEX 不是数字: %v", err)
			}
			hifIdx := uint32(n)
			if err := c.SetInterfaceMTU(ctx, hifIdx, 1500); err != nil {
				t.Errorf("给 underlay %d 写 1500: %v", hifIdx, err)
			} else if got := mtuColumnOnIndexLine(showInterfaces(t), hifIdx); got != "1500/0/0/0" {
				t.Errorf("underlay MTU 列应是 1500/0/0/0，实际 %s", got)
			}
			// API 这一侧同读一次，并顺带把这张口该具备的状态读出来记进日志：
			// agent 挑不到出口（没有 FIB dump），但它已经能对操作员给的号说"这张口
			// 在表里、不是隧道、记录是 1500、链路是 up"了。
			if e, ok := FindInterface(mustDump(t, c, ctx), hifIdx); !ok {
				t.Errorf("接口表里读不到 underlay %d —— agent 的 verifyLanded 会把这条配置判成错", hifIdx)
			} else {
				if e.L3MTU != 1500 {
					t.Errorf("underlay %s API 读回 MTU=%d，期望 1500", e.Name, e.L3MTU)
				}
				if want := interfaceNameOnIndex(showInterfaces(t), hifIdx); e.Name != want {
					t.Errorf("underlay 接口名 API=%q 而 CLI=%q", e.Name, want)
				}
				t.Logf("underlay %d = %s：MTU %d，admin=%v link=%v",
					hifIdx, e.Name, e.L3MTU, e.AdminUp, e.LinkUp)
			}
		} else {
			t.Log("未设 SRWG_UNDERLAY_SW_IF_INDEX，跳过 underlay 接口那条（由 test-mtu.sh 覆盖）")
		}
	})

	t.Run("mss_clamp", func(t *testing.T) {
		// MSS 钳制是另一个插件（mss_clamp）的消息，不是本插件的。这一条要证明三件事：
		// 线布局抄对了（和 vppctl 读同一张表）、方向真的是 RX、以及**读回能看见方向**
		// —— 最后一条是 agent 拒绝"配成 TX"那个判断的前提。
		idx, err := c.CreateInterface(ctx, InterfaceParams{
			UserInstance: liveInstance, ListenPort: livePort,
			PrivateKeys: mustRand(t, wgKeyLen), SrcIP: src})
		if err != nil {
			t.Fatalf("CreateInterface: %v", err)
		}
		name := interfaceNameOnIndex(showInterfaces(t), idx)
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// 先撤钳制再删接口：mss_clamp 插件在接口删除时**不**清自己的表（实测
			// 留下 `DELETED (4)` 一条），而 disable 会真的把记录抹掉。不撤就删，
			// 这条测试每跑一次就在 VPP 里留一条孤儿。
			if _, err := vppctl(cctx, "set", "interface", "tcp-mss-clamp", name,
				"ip4", "disable", "ip6", "disable"); err != nil {
				t.Errorf("撤钳制: %v", err)
			}
			if err := c.DeleteInterface(cctx, idx); err != nil {
				t.Errorf("清理接口 idx=%d 失败: %v（手工 vppctl wireguard delete）", idx, err)
			}
			if err := c.RebindPorts(cctx); err != nil {
				t.Errorf("RebindPorts: %v", err)
			}
		})
		// 新建的口没有明细：返回 ok=false 而不是一条全 0 的记录。
		if info, ok, err := c.MSSClamp(ctx, idx); err != nil || ok {
			t.Errorf("新接口不该有 MSS 明细: ok=%v err=%v info=%+v", ok, err, info)
		}

		if err := c.SetMSSClamp(ctx, idx, 1320); err != nil {
			t.Fatalf("SetMSSClamp(1320): %v", err)
		}
		info, ok, err := c.MSSClamp(ctx, idx)
		if err != nil || !ok {
			t.Fatalf("读回 MSS: ok=%v err=%v", ok, err)
		}
		if info.IPv4MSS != 1320 || info.IPv4Dir != MSSClampDirRX || info.IPv6Dir != MSSClampDirNone {
			t.Errorf("读回 = %+v，期望 ipv4 1320 RX / ipv6 不动", info)
		}
		out, err := vppctl(ctx, "show", "interface", "tcp-mss-clamp", name)
		if err != nil {
			t.Fatalf("vppctl show interface tcp-mss-clamp %s: %v", name, err)
		}
		if !strings.Contains(out, "ip4: 1320 [RX]") {
			t.Errorf("CLI 眼里的钳制不是 ip4: 1320 [RX]：\n%s", out)
		}

		// 用 CLI 把它改成 TX：真机上这个状态**不报错也完全不生效**。API 必须原样读出
		// TX，agent 的 verifyLanded 正是靠"方向含 RX"把这种状态判红。
		if _, err := vppctl(ctx, "set", "interface", "tcp-mss-clamp", name,
			"ip4", "tx", "ip4-mss", "1320", "ip6", "disable"); err != nil {
			t.Fatalf("CLI 改 TX: %v", err)
		}
		info, ok, err = c.MSSClamp(ctx, idx)
		if err != nil || !ok {
			t.Fatalf("改 TX 后读回: ok=%v err=%v", ok, err)
		}
		if info.IPv4Dir&MSSClampDirRX != 0 || info.IPv4Dir&MSSClampDirTX == 0 {
			t.Errorf("CLI 已改成 TX，API 读回方向 = %d（期望只含 TX）—— 读回看不见方向的话，"+
				"agent 就没法拒绝那张不生效的配置", info.IPv4Dir)
		}
		t.Logf("%s：API 与 CLI 都读到 ip4 mss=1320；方向 TX 可读回（真机上 TX 不改任何包）", name)
	})

	t.Logf("被丢弃的帧计数 Drops=%d（异步通知会计进来，不该持续增长）", c.Drops())
}

// showInterfaces 取 `vppctl show interface` 全文，并在读不到时终结调用方。
func showInterfaces(t *testing.T) string {
	t.Helper()
	out, err := vppctl(context.Background(), "show", "interface")
	if err != nil {
		t.Fatalf("vppctl show interface: %v", err)
	}
	return out
}

// mustDump 用 API 读回 core 的接口表；读不到直接终结调用方（读回失败没有下半段可测）。
func mustDump(t *testing.T, c *Client, ctx context.Context) []SwInterface {
	t.Helper()
	ifs, err := c.DumpInterfaces(ctx)
	if err != nil {
		t.Fatalf("DumpInterfaces: %v", err)
	}
	return ifs
}

// dumpSummary 把接口表压成一行，只用于日志与报错里那句"表里到底有什么"。
func dumpSummary(ifs []SwInterface) string {
	parts := make([]string, 0, len(ifs))
	for _, i := range ifs {
		parts = append(parts, fmt.Sprintf("%d:%s/%d", i.SWIfIndex, i.Name, i.L3MTU))
	}
	return strings.Join(parts, " ")
}

// mtuColumnOnIndexLine 从 `vppctl show interface` 的输出里取指定 sw_if_index 那行的
// MTU 列（形如 1360/0/0/0），找不到返回 ""。
//
// 按"第 2 列是这个号"来定位而不是按接口名：API 只回给我 sw_if_index，而名字是操作员
// 起的、别的会话也可能占同一个名。这张表的分页格式不规整（后面的列会折行成计数器
// 行），所以不数第几列，直接找那唯一一个 "a/b/c/d" 形状的字段。
func mtuColumnOnIndexLine(out string, swIfIndex uint32) string {
	want := strconv.FormatUint(uint64(swIfIndex), 10)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != want {
			continue
		}
		for _, f := range fields {
			if isMTUColumn(f) {
				return f
			}
		}
	}
	return ""
}

// isMTUColumn 认 "a/b/c/d" 这种四段全数字的字段，即 show interface 的 MTU 列。
func isMTUColumn(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 10, 32); err != nil {
			return false
		}
	}
	return true
}

// interfaceNameOnIndex 取同一张表里那个号对应的接口名（第一列）。
// 只有 CLI 需要名字，而写 MTU 的实验走 CLI 是因为客户端会先把非法值拦在发出之前。
func interfaceNameOnIndex(out string, swIfIndex uint32) string {
	want := strconv.FormatUint(uint64(swIfIndex), 10)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == want {
			return fields[0]
		}
	}
	return ""
}

// mustRand 取 n 个随机字节，失败直接终结测试。
func mustRand(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("随机数: %v", err)
	}
	return b
}

// cliLine 返回 CLI 输出里第一处包含 needle 的整行，找不到返回 ""。
// 逐行比对而不是全文搜索，是因为节点上可能留着别的会话配的同名对象。
func cliLine(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// vppctl 跑一条只读命令，用来把 API 写下去的东西和 CLI 眼里的同一张表对齐。
// 没有 vppctl（不在 VPP 容器里）时返回错误，调用方自行跳过。
func vppctl(ctx context.Context, args ...string) (string, error) {
	if _, err := exec.LookPath("vppctl"); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "vppctl", args...)
	out, err := cmd.Output()
	return string(out), err
}
