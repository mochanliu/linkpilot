package vppapi

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"net"
	"os"
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
const (
	liveInstance = 9001
	livePort     = 51999
)

// TestLiveAgainstVPP 是真机验证，默认跳过：只有显式给出 socket 才跑。
//
// 在容器里跑（macOS 上没有 VPP）：
//
//	SRWG_VPP_SOCK=/run/vpp/api.sock SRWG_UNDERLAY_IP=10.10.1.2 \
//	  go test ./internal/vppapi -run TestLive -v
//
// 它证明的是这个包最要紧的三件事，全部用真 VPP 而不是假端：
//  1. 握手、消息尺寸与 CRC 校验对得上真实注册表（假端只能锁住我们自己抄的格式）；
//  2. 定案 3：agent 自己生成的 X25519 私钥能原样装进 VPP —— dump 回来的私钥
//     逐字节等于装进去的那把，公钥等于本地用 crypto/ecdh 推算的那把；
//  3. 观测表为空时返回空切片而不是报错（VPP 的 dump 对空结果一帧都不发）。
//
// 用 51999 端口、hmac_id 99 和一个独立 WG 接口，跑完自己删干净，
// 不碰实验室约定的 51820/51821。
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

	t.Logf("被丢弃的帧计数 Drops=%d（异步通知会计进来，不该持续增长）", c.Drops())
}
