package vppapi

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
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
// 它证明的是这个包最要紧的四件事，全部用真 VPP 而不是假端：
//  1. 握手、消息尺寸与 CRC 校验对得上真实注册表（假端只能锁住我们自己抄的格式）；
//  2. 定案 3：agent 自己生成的 X25519 私钥能原样装进 VPP —— dump 回来的私钥
//     逐字节等于装进去的那把，公钥等于本地用 crypto/ecdh 推算的那把；
//  3. 观测表为空时返回空切片而不是报错（VPP 的 dump 对空结果一帧都不发）；
//  4. SR 路径/隧道绑定/encap 挂点这一整套能配进真数据面，且 API 读回与
//     `vppctl show sr-wg` 读到的是同一张表、同一段序（[0]=终点、[n-1]=首跳）。
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
