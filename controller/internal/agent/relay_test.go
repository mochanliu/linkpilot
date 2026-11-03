package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"sdwan/controller/internal/wire"
)

// 中转链的顺序是路径本身：3→5 与 5→3 是两条不同的路。这里把它锁在**最靠近数据面
// 的那一层** —— 真机上的 `sr-wg path add P via H2 via H1` 就是这个顺序（离终点近的
// 写在前面），而中转一旦写反，包会先绕到另一个跳上去，控制台看上去仍然是"这条隧道
// 经 3 和 5"。
//
// 终点（对端外层 ip:port）**不在路径里**：encap 封装前从即将发出的那个包现取
// （2026-11-03 定案的 A 方案）。所以期望列表里只有中转跳，直连隧道是一条空的链。
func TestRelayChainGoesToPathRelaysDestinationFirst(t *testing.T) {
	const peerNode = uint16(3)
	cases := []struct {
		name   string
		relays []string // 按行走顺序，和控制器下发的一致
		want   []string // 路径里的中转跳，离终点近的在前
	}{
		{"直连", nil, nil},
		{"一跳", []string{"10.10.9.1:15001"},
			[]string{"10.10.9.1:15001"}},
		{"两跳", []string{"10.10.9.1:15001", "10.10.9.2:15002"},
			[]string{"10.10.9.2:15002", "10.10.9.1:15001"}},
		{"三跳", []string{"10.10.9.1:15001", "10.10.9.2:15002", "10.10.9.3:15003"},
			[]string{"10.10.9.3:15003", "10.10.9.2:15002", "10.10.9.1:15001"}},
		// 同两台设备、相反的顺序：中转链必须跟着反过来。如果这一条也"通过"，
		// 说明中间某处按 node_id 排了序。
		{"两跳换序", []string{"10.10.9.2:15002", "10.10.9.1:15001"},
			[]string{"10.10.9.1:15001", "10.10.9.2:15002"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVPP()
			pki := newTestPKI(t)
			spec := tunnelSpec(t, pki, "t1", peerNode, 1001, 51820,
				"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
			for _, e := range tc.relays {
				spec.Relays = append(spec.Relays, wire.RelayHop{Endpoint: e})
			}
			b := testBundle(spec)
			ts, _ := newTunnelSync(t, f, pki, b)
			if err := ts.Sync(context.Background(), b.Version); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			got := relaysOf(t, f, wire.UnderlayPathIDBase+peerNode)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("中转链 = %v，期望 %v（终点不在这张表里，中转按离终点近的在前）", got, tc.want)
			}
			for _, s := range got {
				if s == "10.10.1.3:51820" {
					t.Fatalf("对端地址 %s 被塞进了路径表：终点段必须由报文自己提供，"+
						"存在表里就是一个会漂移、还参与 HMAC 的值", s)
				}
			}
		})
	}
}

func relaysOf(t *testing.T, f *fakeVPP, pathID uint16) []string {
	t.Helper()
	p, ok := f.paths[pathID]
	if !ok {
		t.Fatalf("path_id %d 没下发过：全部调用 %v", pathID, f.calls)
	}
	out := make([]string, 0, len(p.Relays))
	for _, s := range p.Relays {
		out = append(out, fmt.Sprintf("%s:%d", s.IP, s.Port))
	}
	return out
}

// 隧道 MTU 按**最多段数**算，不是按这条隧道当前的跳数算：段列表由控制器随时重组，
// 而 MTU 是接口级的、一条隧道只有一个值。所以加跳不该改 MTU，改了就意味着接口要
// 重建、而且缩短路径时永远不会回来。
func TestRelayHopsDoNotChangeTunnelMTU(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	chains := [][]RelayHop{
		nil,
		{{IP: net.IPv4(10, 10, 9, 1), Port: 15001}},
		{{IP: net.IPv4(10, 10, 9, 1), Port: 15001}, {IP: net.IPv4(10, 10, 9, 2), Port: 15002}},
		{{IP: net.IPv4(10, 10, 9, 1), Port: 15001}, {IP: net.IPv4(10, 10, 9, 2), Port: 15002},
			{IP: net.IPv4(10, 10, 9, 3), Port: 15003}},
	}
	for _, relays := range chains {
		cfg := testConfig(t, f, pki)
		cfg.UnderlayMTU = 1500
		cfg.Relays = relays
		l, err := NewLink(cfg)
		if err != nil {
			t.Fatalf("%d 跳: %v", len(relays), err)
		}
		if l.cfg.TunnelMTU != wire.DefaultTunnelMTU {
			t.Fatalf("%d 跳的隧道 MTU = %d，应仍是默认值 %d：预算按最多段数算，与这条隧道几跳无关",
				len(relays), l.cfg.TunnelMTU, wire.DefaultTunnelMTU)
		}
	}
}

// 换中转=换路，而"路"是 Link 生命周期内固定的输入：判定必须认得出**顺序**变了，
// 否则 reconcile 会跳过重建，AddPath 永远停在旧的第一跳上（症状是"改了路径但流量
// 还走老路"，计数器一切正常）。
func TestSameLinkConfigSeesRelayChainShape(t *testing.T) {
	h1 := RelayHop{IP: net.IPv4(10, 10, 9, 1), Port: 15001}
	h2 := RelayHop{IP: net.IPv4(10, 10, 9, 2), Port: 15002}
	two := LinkConfig{Instance: 1001, ListenPort: 51820, EncapIfIndex: 5,
		PathIDBase: 1000, SrcIP: net.IPv4(10, 10, 1, 2), TunnelMTU: 1360,
		Relays: []RelayHop{h1, h2}}

	// 同一条链再读一次不该被判成"变了"，否则每轮 reconcile 都会拆了重建接口。
	same := two
	same.Relays = []RelayHop{h1, h2}
	if !sameLinkConfig(two, same) {
		t.Error("中转链没变却被判成要重建（每轮都会拆了重建接口）")
	}
	diff := map[string][]RelayHop{
		"加了第二跳":  {h1},
		"去掉一跳":   {h1, h2, RelayHop{IP: net.IPv4(10, 10, 9, 3), Port: 15003}},
		"顺序反了":   {h2, h1},
		"同一跳换端口": {h1, RelayHop{IP: net.IPv4(10, 10, 9, 2), Port: 15003}},
		"改成直连":   nil,
	}
	for name, r := range diff {
		c := two
		c.Relays = r
		if sameLinkConfig(two, c) {
			t.Errorf("%s：中转链变了却被判成没变，数据面会停在旧路径上", name)
		}
	}
	// 空链与"没这个字段"是同一个意思：控制器把隧道改成直连时必须认出来。
	if !relaysEqual(nil, []RelayHop{}) {
		t.Error("nil 与空链应当等价")
	}
}

// 控制器给的地址与端口是一体的。半截中转（只有地址或只有端口）不能进段列表：
// VPP 照样接受，包发往 0 端口或 0.0.0.0，而回执显示"已收敛"。
func TestLinkConfigRejectsHalfRelayFromController(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	for _, bad := range []string{"10.10.9.1", "10.10.9.1:notaport", "", "10.10.9.1:70000"} {
		spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
			"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
		spec.Relays = []wire.RelayHop{{NodeID: 9, Endpoint: bad}}
		b := testBundle(spec)
		ts, _ := newTunnelSync(t, f, pki, b)
		err := ts.Sync(context.Background(), b.Version)
		st := ts.State()
		if err == nil && st.OK {
			t.Fatalf("坏中转 %q 竟然装上了: %+v", bad, st)
		}
		msg := st.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if !strings.Contains(msg, "第 1 跳") {
			t.Fatalf("坏中转 %q 的错误没说是第几跳，页面上看不出来是哪一段配错: %q", bad, msg)
		}
	}
}

// 第几跳要说清楚：一条三跳路径报错时，"中转地址不合法"等于没说。
func TestLinkConfigNamesTheFailingHop(t *testing.T) {
	f := newFakeVPP()
	pki := newTestPKI(t)
	spec := tunnelSpec(t, pki, "t1", 3, 1001, 51820,
		"10.250.0.1/32", "10.250.0.3/32", "10.10.1.3:51820")
	spec.Relays = []wire.RelayHop{
		{NodeID: 9, Endpoint: "10.10.9.1:15001"},
		{NodeID: 10, Endpoint: "10.10.9.2:15002"},
		{NodeID: 11, Endpoint: "bogus"},
	}
	b := testBundle(spec)
	ts, _ := newTunnelSync(t, f, pki, b)
	if err := ts.Sync(context.Background(), b.Version); err != nil {
		t.Fatal(err)
	}
	if st := ts.State(); st.OK || !strings.Contains(st.Error, "第 3 跳") {
		t.Fatalf("错误里没定位到第 3 跳（一条三跳路径只说\"中转不合法\"等于没说）: %+v", st)
	}
	// 失败的那条隧道不该留下半装好的接口：AddPath 只在段列表完整时才发生。
	if len(f.paths) != 0 {
		t.Fatalf("第 3 跳坏了却下发了路径: %v", f.calls)
	}
}
