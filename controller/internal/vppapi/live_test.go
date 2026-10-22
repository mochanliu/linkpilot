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
// 它证明的是这个包最要紧的五件事，全部用真 VPP 而不是假端：
//  1. 握手、消息尺寸与 CRC 校验对得上真实注册表（假端只能锁住我们自己抄的格式）；
//  2. 定案 3：agent 自己生成的 X25519 私钥能原样装进 VPP —— dump 回来的私钥
//     逐字节等于装进去的那把，公钥等于本地用 crypto/ecdh 推算的那把；
//  3. 观测表为空时返回空切片而不是报错（VPP 的 dump 对空结果一帧都不发）；
//  4. SR 路径/隧道绑定/encap 挂点这一整套能配进真数据面，且 API 读回与
//     `vppctl show sr-wg` 读到的是同一张表、同一段序（[0]=终点、[n-1]=首跳）；
//  5. peer 与路径的**重复下发语义**（agent 重启与 NAT 重绑全靠它）：同公钥
//     重复 add peer 回 -116 且**跨接口也回 -116**（去重键不含接口，换 endpoint
//     也救不回来）、0 条 allowed_ip 回 -7、删不存在的 peer 回 -6、
//     同 path_id 再 AddPath 是原地替换、但绑定的 endpoint 不会跟着段走、
//     DelPath 会静默级联解掉引用它的绑定。
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
		segments := []Segment{
			{IP: net.IPv4(192, 0, 2, 1), Port: 51820}, // [0] = 终点，打 VPN 口
			{IP: net.IPv4(192, 0, 2, 2), Port: 51821}, // [n-1] = 首跳，打中转口
		}
		if err := c.AddPath(ctx, PathParams{
			PathID: livePathID, HmacID: liveHmacID, Active: true, Segments: segments,
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
			Segments: segments,
		}); err == nil {
			t.Error("引用不存在的 hmac_id 却建成了路径 —— 数据面回 NO_SUCH_ENTRY 才算对")
			_ = c.DelPath(ctx, livePathID+1)
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

		if err := c.SetTunnel(ctx, TunnelBinding{
			SWIfIndex: idx, PathID: livePathID, EndpointIP: net.IPv4(192, 0, 2, 1),
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
		// 段序原样回来才算对上数据面的约定：[0]=终点、[n-1]=首跳。
		// 抄反了不会报错，只会让隧道"配上了但不通"。
		if len(got.Segments) != len(segments) {
			t.Fatalf("段数 = %d，期望 %d", len(got.Segments), len(segments))
		}
		for i := range segments {
			if !got.Segments[i].IP.Equal(segments[i].IP) || got.Segments[i].Port != segments[i].Port {
				t.Errorf("segments[%d] = %v:%d，期望 %v:%d",
					i, got.Segments[i].IP, got.Segments[i].Port, segments[i].IP, segments[i].Port)
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
		if bound.PathID != livePathID || !bound.EndpointIP.Equal(segments[0].IP) {
			t.Errorf("绑定不对: %+v", bound)
		}
		// is_effective 取决于本机有没有 underlay 启用了 sr-wg4-encap。这里只报状态
		// 不改它：实验室的节点可能正被别的会话跑端到端，关掉挂点会把他们的隧道打断。
		t.Logf("隧道绑定 idx=%d path=%d endpoint=%v effective=%v",
			idx, bound.PathID, bound.EndpointIP, bound.Effective)

		// CLI 交叉核对：同一张表，第二个读者。串取自 sr_wg_cli.c 的
		// "  [%d] path_id=%u hmac_id=%u seg_count=%u active=%u" 与
		// "      seg[%d]: <ip>:<port>"。
		if out, err := vppctl(ctx, "show", "sr-wg", "paths"); err == nil {
			checks := []string{fmt.Sprintf("path_id=%d hmac_id=%d seg_count=%d active=1",
				livePathID, liveHmacID, len(segments))}
			for i, s := range segments {
				checks = append(checks, fmt.Sprintf("seg[%d]: %v:%d", i, s.IP, s.Port))
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
			want := fmt.Sprintf("path_id=%d endpoint=%v", livePathID, segments[0].IP)
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
		//   a) 同 path_id 再 AddPath 是**原地替换**段列表，不报错 —— 所以换地址
		//      不需要先删路径（先删反而会把隧道绑定一起带走，见 d）；
		//   b) 替换段之后，隧道绑定**还在**，但它记的 endpoint 仍是旧的 —— 这正
		//      是"隧道起来了但不通"的形态，所以改路径必须 path 与 tunnel 成对重设；
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

		add := func(ip string) {
			if err := c.AddPath(ctx, PathParams{PathID: livePathID, HmacID: liveHmacID, Active: true,
				Segments: []Segment{{IP: net.ParseIP(ip).To4(), Port: 51820}}}); err != nil {
				t.Fatalf("AddPath(%s): %v", ip, err)
			}
		}
		pathSeg := func() net.IP {
			ps, err := c.Paths(ctx, livePathID)
			if err != nil || len(ps) == 0 {
				t.Fatalf("Paths(%d) = %v, %v", livePathID, ps, err)
			}
			if len(ps) != 1 {
				t.Fatalf("按 path_id 过滤应该只剩 1 条，实际 %d 条", len(ps))
			}
			return ps[0].Segments[0].IP
		}
		boundEP := func() (net.IP, bool) {
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
			return ts[0].EndpointIP, true
		}

		old := net.ParseIP("192.0.2.10")
		fresh := net.ParseIP("192.0.2.11")
		add(old.String())
		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idx, PathID: livePathID, EndpointIP: old.To4()}); err != nil {
			t.Fatalf("SetTunnel: %v", err)
		}
		if got, ok := boundEP(); !ok || !got.Equal(old.To4()) {
			t.Fatalf("绑定初值不对: %v %v", got, ok)
		}

		// a) 原地替换
		add(fresh.String())
		if got := pathSeg(); !got.Equal(fresh.To4()) {
			t.Errorf("同 path_id 再 AddPath 没替换段：dump 回来 %v，期望 %v", got, fresh)
		}
		// b) 绑定还在，但 endpoint 停在旧值 —— agent 必须成对重设
		if got, ok := boundEP(); !ok {
			t.Error("替换段之后绑定消失了：DelPath 才该解绑，AddPath 不该")
		} else if !got.Equal(old.To4()) {
			t.Errorf("绑定 endpoint 被替换成了 %v（数据面不会自己跟着段走，agent 得重设）", got)
		} else {
			t.Logf("实测：替换段后绑定仍指向旧 endpoint=%v，新段=%v —— 改地址必须 path+tunnel 成对下发", got, fresh)
		}
		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idx, PathID: livePathID, EndpointIP: fresh.To4()}); err != nil {
			t.Fatalf("SetTunnel(新): %v", err)
		}
		if got, _ := boundEP(); !got.Equal(fresh.To4()) {
			t.Errorf("重设之后绑定 = %v，期望 %v", got, fresh)
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
		if _, ok := boundEP(); ok {
			t.Error("删掉被引用的路径之后，隧道绑定居然还在 —— 数据面应当级联清掉")
		}
		t.Log("实测：DelPath 会连带清掉引用它的隧道绑定，且不报错")
	})

	// 每隧道一个监听端口（定案 2：UDP 端口既分功能也分隧道）。这里钉的是 agent 的
	// claimListenPort 与 ApplyPeer 直接依赖的四件真机行为：
	//   a) AddPort 幂等、每次都把端口挂回 sr-wg 节点；Ports 原样读回两种 kind；
	//      DelPort 也幂等（不在集合里删除不是错误）；
	//   b) 同一个外层 IP 上的**两条**隧道（端口不同）各有各的绑定，互不覆盖 ——
	//      改之前绑定只按 IP 建键，第二条会把第一条拽进自己的路径；
	//   c) 别的接口来抢同一个 (endpoint, 端口) 身份时回 -116，而不是静默抢走；
	//   d) 被拒的那次一个字都没改（前一条绑定仍是原样）。
	t.Run("per_tunnel_ports_and_bind_identity", func(t *testing.T) {
		const (
			instA   = uint32(9003)
			instB   = uint32(9004)
			portA   = uint16(51995)
			portB   = uint16(51994)
			pathA   = uint16(9011)
			pathB   = uint16(9012)
			probe   = uint16(51993)
			shared  = "192.0.2.20"
			hmacID  = uint16(9011)
			bindIP1 = 9013 // 抢身份那一次用的 path_id
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

		// b)/c)/d) 两条隧道打在同一个外层 IP 上。
		if err := c.SetPKey(ctx, hmacID, mustRand(t, pKeyLen)); err != nil {
			t.Fatalf("SetPKey: %v", err)
		}
		ep := net.ParseIP(shared).To4()
		mkPath := func(pathID uint16) {
			if err := c.AddPath(ctx, PathParams{PathID: pathID, HmacID: hmacID, Active: true,
				Segments: []Segment{{IP: ep, Port: portA}}}); err != nil {
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
		mkPath(pathA)
		mkPath(pathB)
		mkPath(bindIP1)
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, id := range []uint16{pathA, pathB, bindIP1} {
				if err := c.DelPath(cctx, id); err != nil {
					t.Errorf("清理路径 %d: %v", id, err)
				}
			}
			if err := c.DelPKey(cctx, hmacID); err != nil {
				t.Errorf("清理 P-Key %d: %v", hmacID, err)
			}
		})
		idxA, idxB := mkIface(instA, portA), mkIface(instB, portB)

		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idxA, PathID: pathA, EndpointIP: ep, DstPort: portA}); err != nil {
			t.Fatalf("SetTunnel(A): %v", err)
		}
		if err := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idxB, PathID: pathB, EndpointIP: ep, DstPort: portB}); err != nil {
			t.Fatalf("第二条隧道（同 IP、不同端口）配不上: %v", err)
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
		if b := bindOf(idxA); b == nil || b.PathID != pathA || b.DstPort != portA || !b.EndpointIP.Equal(ep) {
			t.Errorf("第一条绑定被第二条改了: %+v（期望 path=%d port=%d）", b, pathA, portA)
		}
		if b := bindOf(idxB); b == nil || b.PathID != pathB || b.DstPort != portB || !b.EndpointIP.Equal(ep) {
			t.Errorf("第二条绑定不对: %+v（期望 path=%d port=%d）", b, pathB, portB)
		}

		// c) 抢身份：把 (shared, portA) 再绑一次到接口 B 上，必须被拒。
		steal := c.SetTunnel(ctx, TunnelBinding{SWIfIndex: idxB, PathID: bindIP1, EndpointIP: ep, DstPort: portA})
		var apiErr APIError
		if steal == nil || !errors.As(steal, &apiErr) || apiErr.Retval != -116 {
			t.Errorf("抢同一个 (endpoint, 端口) 身份应当回 -116，got: %v", steal)
		} else {
			t.Logf("实测：抢身份被拒（%v）", steal)
		}
		// d) 被拒之后 A 的绑定、以及 B 自己的绑定都还是原样。
		tuns, err = c.Tunnels(ctx, ^uint32(0))
		if err != nil {
			t.Fatalf("Tunnels(复核): %v", err)
		}
		if b := bindOf(idxA); b == nil || b.PathID != pathA || b.DstPort != portA {
			t.Errorf("被拒的那次动了一条绑定: %+v", b)
		}
		if b := bindOf(idxB); b == nil || b.PathID != pathB || b.DstPort != portB {
			t.Errorf("被拒的那次把 B 自己的绑定也改了: %+v", b)
		}
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

	t.Logf("被丢弃的帧计数 Drops=%d（异步通知会计进来，不该持续增长）", c.Drops())
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
