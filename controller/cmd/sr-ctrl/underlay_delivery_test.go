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
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/wire"
)

// giveIdentity fills in the two facts a peer spec needs and that the fixture
// does not have: a certificate and a key binding. Neither is verified here --
// the render only refuses to describe a peer without them.
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
		rec.Binding = &keybinding.Binding{
			X25519Pub: base64.StdEncoding.EncodeToString(make([]byte, keybinding.X25519PubLen)),
			KeyGen:    1,
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
	// The peer has a certificate but never reported its WireGuard key binding:
	// there is literally no public key to install a peer with.
	s.mu.Lock()
	s.state.Nodes[key("t", 2)].Binding = nil
	s.mu.Unlock()

	b := bundleOf(t, s, "t", 1)
	if len(b.Tunnels) != 0 {
		t.Fatalf("对端没有担保时不该下发隧道规格: %+v", b.Tunnels)
	}
	if len(b.Skipped) != 1 || !strings.Contains(b.Skipped[0].Reason, "公钥担保") {
		t.Fatalf("node 1 应看到对端缺担保的跳过项，实际 %+v", b.Skipped)
	}
}
