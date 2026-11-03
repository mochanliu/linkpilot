// The device-facing render of a tunnel: what one end is told to install.
//
// The rule these tests hold is that the bundle speaks the dataplane's language,
// not the pool's. Overlay addresses are stored as bare host addresses because
// that is what a pool hands out and what a page shows; a WG interface is
// configured with PREFIXES, and netip.ParsePrefix rejects an address with no
// mask. The first lab run of a controller-managed tunnel failed exactly there:
// the agent reported "本机 overlay \"10.250.0.1\": no '/'" and the console showed
// a tunnel that never existed in VPP.
package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/wire"
)

// giveIdentity fills in the two facts a peer spec needs and that the fixture
// does not have: a certificate and the eight key bindings. Neither is verified
// here -- the render only refuses to describe a peer without them.
//
// Every slot gets its own public key because that is what the model promises:
// the n-th tunnel between two devices is dialled with key n, and two slots
// holding the same key would put both tunnels on one peer in the dataplane.
func giveIdentity(t *testing.T, s *server, tenant string, ids ...uint16) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		rec := s.state.Nodes[key(tenant, id)]
		if rec == nil {
			t.Fatalf("node %s/%d is not in the registry", tenant, id)
		}
		rec.CertPEM = "-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"
		rec.Bindings = make([]*keybinding.Binding, keybinding.WGKeySlots)
		for slot := range rec.Bindings {
			pub := make([]byte, keybinding.X25519PubLen)
			pub[0] = byte(slot + 1)
			rec.Bindings[slot] = &keybinding.Binding{
				Slot:      uint8(slot),
				X25519Pub: base64.StdEncoding.EncodeToString(pub),
				KeyGen:    1,
			}
		}
	}
}

func bundleOf(t *testing.T, s *server, tenant string, node uint16) wire.UnderlayBundle {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.underlayBundleLocked(tenant, node)
	if err != nil {
		t.Fatalf("bundle for node %d: %v", node, err)
	}
	return b
}

func TestBundleRendersOverlayAsPrefixes(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	if rec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("create tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2)

	for _, node := range []uint16{1, 2} {
		b := bundleOf(t, s, "t", node)
		if len(b.Tunnels) != 1 {
			t.Fatalf("node %d: %d tunnel(s), skipped=%+v", node, len(b.Tunnels), b.Skipped)
		}
		spec := b.Tunnels[0]
		for _, pair := range [][2]string{{"本机 overlay", spec.LocalOverlay}, {"对端 overlay", spec.PeerOverlay}} {
			p, err := netip.ParsePrefix(pair[1])
			if err != nil {
				t.Fatalf("node %d: %s %q 不是前缀（设备会拒绝安装）: %v", node, pair[0], pair[1], err)
			}
			if !p.Addr().Is4() || p.Bits() != 32 {
				t.Fatalf("node %d: %s 应是 IPv4 /32，实际 %s", node, pair[0], pair[1])
			}
			if strings.Contains(pair[1], " ") {
				t.Fatalf("node %d: %s %q 带了空白", node, pair[0], pair[1])
			}
		}
		if spec.LocalOverlay == spec.PeerOverlay {
			t.Fatalf("node %d: 两端 overlay 相同 (%s)", node, spec.LocalOverlay)
		}
		// The peer must be dialled at the port the controller allocated for it,
		// and the source address stays a bare IP (VPP takes it separately).
		if _, err := netip.ParseAddrPort(spec.PeerEndpoint); err != nil {
			t.Fatalf("node %d: 对端拨号地址 %q: %v", node, spec.PeerEndpoint, err)
		}
	}
}

// A tunnel whose peer cannot be described is skipped with the reason, and the
// other end's bundle says the same -- it is the controller's honest answer, not
// a half-installed link.
func TestBundleSkipsAPeerWithNoBinding(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	if rec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("create tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2)
	// The peer has a certificate but the ONE key this tunnel dials with is
	// missing: the other seven slots are still there, so this also proves the
	// render looks the binding up BY SLOT instead of grabbing whatever exists.
	// A peer with no public key for this tunnel is literally un-installable.
	s.mu.Lock()
	s.state.Nodes[key("t", 2)].Bindings[0] = nil
	s.mu.Unlock()

	b := bundleOf(t, s, "t", 1)
	if len(b.Tunnels) != 0 {
		t.Fatalf("对端没有担保时不该下发隧道规格: %+v", b.Tunnels)
	}
	if len(b.Skipped) != 1 || !strings.Contains(b.Skipped[0].Reason, "公钥担保") {
		t.Fatalf("node 1 应看到对端缺担保的跳过项，实际 %+v", b.Skipped)
	}
}

// 规格里两处槽号必须同号：本端拿 Slot 去取自己第 n 把私钥，PeerBinding 说的是
// 对端担保的第 m 把。n != m 装上去就是"这条隧道用的是别条隧道的密钥"，
// agent 那一侧会整条拒绝（见 internal/agent/tunnel.go 的 applySpec）。
//
// 这一段的期望值是"两边都 0"：本增量只把槽号打通，同设备对仍然只放开一条隧道，
// 而槽 0 就是升级前磁盘上那把 —— 存量隧道的公钥因此一个字节都没变。
func TestBundleSpecCarriesTheTunnelsKeySlot(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	if rec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("create tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2)

	for _, node := range []uint16{1, 2} {
		b := bundleOf(t, s, "t", node)
		if len(b.Tunnels) != 1 {
			t.Fatalf("node %d: %d tunnel(s), skipped=%+v", node, len(b.Tunnels), b.Skipped)
		}
		spec := b.Tunnels[0]
		if spec.Slot != 0 {
			t.Errorf("node %d: 本端槽号 %d，同设备对只放开一条隧道时期望 0", node, spec.Slot)
		}
		if spec.PeerBinding == nil {
			t.Fatalf("node %d: 对端担保为空", node)
		}
		if spec.PeerBinding.Slot != spec.Slot {
			t.Errorf("node %d: 本端用槽 %d，对端担保却是槽 %d", node, spec.Slot, spec.PeerBinding.Slot)
		}
	}
}

// enrollExtra adds a device to tenant "t" the way the fixture adds its three:
// an observation, a serial and one WAN port with an address. Node 4 is needed by
// any test that wants two relay boxes.
func enrollExtra(t *testing.T, s *server, id uint16, ip string) {
	t.Helper()
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := addNode(s, "t", id, nil, ip+":40000")
	n.Serial = fmt.Sprintf("cpe-%d", id)
	n.MappingAt = now
	n.Ifaces = &[]wire.IfaceInfo{{
		Name: "wan0", SWIfIndex: uint32(id), MTU: 1500,
		AdminUp: true, LinkUp: true, Addrs: []string{ip + "/24"},
	}}
	n.InventoryAt = now
}

// A relay chain reaches the box as one ordered list, each hop paired with THAT
// hop's transit port. Two things this locks: the order (the agent reverses it into
// the segment list, so a sorted copy silently changes which box the packet pops
// first) and the per-hop port (a chain that borrowed one port for every hop would
// send the packet to a device that is not listening for it).
func TestBundleRendersEveryHopWithItsOwnTransitPort(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	// Enrolled AFTER the publish: a late device must still get a transit port the
	// moment a path starts using it, or the whole tunnel would be undeliverable.
	enrollExtra(t, s, 4, "198.51.100.14")
	enrollExtra(t, s, 5, "198.51.100.15")

	rec := createTunnel(t, s, ten, tunnelReq{
		A:    underlaySpec{NodeID: 1, Iface: "wan0"},
		B:    underlaySpec{NodeID: 2, Iface: "wan0"},
		Path: via(3, 4, 5),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("three-hop tunnel = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2)

	want := []string{"198.51.100.13:15000", "198.51.100.14:15000", "198.51.100.15:15000"}
	for _, node := range []uint16{1, 2} {
		b := bundleOf(t, s, "t", node)
		if len(b.Tunnels) != 1 {
			t.Fatalf("node %d: %d tunnel(s), skipped=%+v", node, len(b.Tunnels), b.Skipped)
		}
		relays := b.Tunnels[0].Relays
		if len(relays) != 3 {
			t.Fatalf("node %d: 中转链应有 3 跳，实际 %+v", node, relays)
		}
		for i, r := range relays {
			if r.NodeID != uint16(i+3) || r.Endpoint != want[i] {
				t.Fatalf("node %d: 第 %d 跳 = %+v，应是 node %d %s",
					node, i+1, r, i+3, want[i])
			}
		}
	}
}

// A hop that cannot be named fails the WHOLE tunnel, not just that hop: a chain
// delivered with one link missing is a different path than the one the tenant
// chose, and it would look identical in the console.
func TestBundleSkipsTunnelWhenOneHopIsUnnameable(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	enrollExtra(t, s, 4, "198.51.100.14")
	rec := createTunnel(t, s, ten, tunnelReq{
		A:    underlaySpec{NodeID: 1, Iface: "wan0"},
		B:    underlaySpec{NodeID: 2, Iface: "wan0"},
		Path: via(3, 4),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2)
	// 第二跳的观测过期了（第一跳仍然可用）。
	s.mu.Lock()
	s.state.Nodes[key("t", 4)].MappingAt = 0
	s.mu.Unlock()

	b := bundleOf(t, s, "t", 1)
	if len(b.Tunnels) != 0 {
		t.Fatalf("有一跳说不清时不该下发: %+v", b.Tunnels)
	}
	if len(b.Skipped) != 1 || !strings.Contains(b.Skipped[0].Reason, "第 2 跳") ||
		!strings.Contains(b.Skipped[0].Reason, "已过期") {
		t.Fatalf("跳过项要指出是第几跳、为什么，实际 %+v", b.Skipped)
	}
}

// A direct tunnel carries no relay list at all. It must not be an empty-but-
// present field either: the agent reads len(Relays)==0 as "no SR hop", and a
// stale single-hop field left in the JSON would rebuild every direct tunnel.
func TestBundleDirectTunnelHasNoRelays(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	if rec := createTunnel(t, s, ten, manualTunnel(1, 2, "wan0")); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	giveIdentity(t, s, "t", 1, 2, 3)
	b := bundleOf(t, s, "t", 1)
	if len(b.Tunnels) != 1 || len(b.Tunnels[0].Relays) != 0 {
		t.Fatalf("直连隧道带了中转: %+v", b.Tunnels)
	}
	// 同一台设备上的两条隧道：一条经中转、一条直连，各自的段列表不能互相污染。
	if rec := createTunnel(t, s, ten, tunnelReq{
		A: underlaySpec{NodeID: 1, Iface: "wan0"}, B: underlaySpec{NodeID: 3, Iface: "wan0"},
		Path: via(2),
	}); rec.Code != http.StatusCreated {
		t.Fatalf("relayed = %d (%s)", rec.Code, rec.Body.String())
	}
	b = bundleOf(t, s, "t", 1)
	byPeer := map[uint16]wire.TunnelSpec{}
	for _, sp := range b.Tunnels {
		byPeer[sp.PeerNodeID] = sp
	}
	if len(byPeer[2].Relays) != 0 {
		t.Fatalf("直连那条被写进了中转: %+v", byPeer[2].Relays)
	}
	if len(byPeer[3].Relays) != 1 || byPeer[3].Relays[0].NodeID != 2 {
		t.Fatalf("中转那条的链不对: %+v", byPeer[3].Relays)
	}
}

// Editing a shared template's hop ORDER must move the delivered segment list.
// Two chains over the same two boxes are two different routes, and a page that
// shows 4→3 while the box dials 3→4 is worse than no page at all -- the tenant
// has no other way to see which one the traffic actually takes.
func TestBundleFollowsAReorderedTemplate(t *testing.T) {
	s, ten, _ := underlayFixture(t)
	publishDefault(t, s, ten)
	enrollExtra(t, s, 4, "198.51.100.14")
	rec := createTunnel(t, s, ten, tunnelReq{
		A:    underlaySpec{NodeID: 1, Iface: "wan0"},
		B:    underlaySpec{NodeID: 2, Iface: "wan0"},
		Path: &pathReq{Hops: []hopReq{{NodeID: 3}, {NodeID: 4}}, SaveAs: "主路径"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	tun := tunnelFrom(t, rec)
	giveIdentity(t, s, "t", 1, 2)

	flip := mustJSON(t, underlayPathRequest{
		ID:   tun.Path.TemplateID,
		Hops: []underlayHopSpec{{NodeID: 4}, {NodeID: 3}},
	})
	if r := underlayDo(t, s.handleAdminUnderlayPaths, ten, http.MethodPut, "/api/underlay/paths", flip); r.Code != http.StatusOK {
		t.Fatalf("swap hops = %d (%s)", r.Code, r.Body.String())
	}
	relays := bundleOf(t, s, "t", 1).Tunnels[0].Relays
	if len(relays) != 2 || relays[0].NodeID != 4 || relays[1].NodeID != 3 ||
		relays[0].Endpoint != "198.51.100.14:15000" || relays[1].Endpoint != "198.51.100.13:15000" {
		t.Fatalf("模板换序后下发的中转链没跟着换: %+v", relays)
	}
}
