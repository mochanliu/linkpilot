package underlay

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"sdwan/controller/internal/wire"
)

// NodeView is what the controller knows about a device at this moment: who it
// is, whether it is currently reachable, which interfaces it has, and where the
// reflector saw it. It is passed in rather than stored here because it is
// runtime state (it changes with every report), not configuration.
type NodeView struct {
	ID   uint16
	Name string

	// Stale marks a device that has not reported within the freshness window.
	Stale bool
	// Symmetric is the NAT verdict. Two symmetric devices can never reach each
	// other: each one's mapping is only valid toward the observer that created
	// it, and a relay does not rewrite the outer source either.
	Symmetric bool
	// Observed is the outer IPv4 the reflector saw this device send from, empty
	// when unknown. It is an ADDRESS only: a NAT mapping is per destination port,
	// so the port half of an observation says nothing about the tunnel's own port
	// and is never used as an endpoint.
	Observed string
	// ObservedStale marks an Observed value past its freshness window. An
	// observation is a claim about a NAT binding that was true a minute ago; a
	// stale one is still shown, but it cannot be used to build a tunnel without
	// someone re-observing or typing an address.
	ObservedStale bool
	// TransitPort is the port the controller assigned this device (one per device,
	// reused by every tunnel it relays).
	TransitPort uint16
	// Ifaces maps physical port name -> link MTU in bytes. It is nil until the
	// agent starts reporting an inventory; len()==0 therefore means "unknown"
	// and disables the interface-name check rather than rejecting everything.
	Ifaces map[string]uint16 `json:"ifaces,omitempty"`
	// DefaultIface is the physical port the device says tunnels leave through.
	// Auto batches need it because "choose a port" is not a question a batch can
	// ask the operator per pair.
	DefaultIface string `json:"default_iface,omitempty"`
}

func (v NodeView) ifaceMTU(name string) (uint16, bool) {
	if len(v.Ifaces) == 0 {
		return 0, false
	}
	mtu, ok := v.Ifaces[name]
	return mtu, ok && mtu > 0
}

// EndpointSpec is the user-facing half of one tunnel end.
type EndpointSpec struct {
	NodeID uint16
	Iface  string
	Addr   string // optional, "ip" or "ip:port"; empty = use the observed address
	Port   uint16 // 0 = allocate from the pool
}

// CreateRequest is one manual tunnel. The same fields back the topology editor,
// so both entry points validate identically.
type CreateRequest struct {
	A, B    EndpointSpec
	Path    *TunnelPath // nil = direct
	BatchID string
	Roles   Roles
	Now     int64
}

// Create validates, allocates and stores one tunnel. It runs on a clone, so a
// failure leaves no allocated ports or overlay addresses behind.
func (s *State) Create(req CreateRequest, views map[uint16]NodeView) (Tunnel, error) {
	c := s.Clone()
	t, err := c.create(req, views)
	if err != nil {
		return Tunnel{}, err
	}
	c.bump()
	*s = *c
	return t, nil
}

func (s *State) create(req CreateRequest, views map[uint16]NodeView) (Tunnel, error) {
	if !s.Configured() {
		return Tunnel{}, fmt.Errorf("还没有发布全局配置：先保存并发布“全局配置”，中转端口随它一起下发")
	}
	if req.A.NodeID == req.B.NodeID {
		return Tunnel{}, fmt.Errorf("隧道两端不能是同一台设备")
	}
	if id, ok := s.pairTunnel(req.A.NodeID, req.B.NodeID); ok {
		return Tunnel{}, fmt.Errorf("%w %s：设备 %d 和 %d 之间只能有一条隧道——每台设备只有一把 WireGuard 静态密钥，VPP 的 peer 表按公钥跨接口去重，第二条会被数据面以 -116 拒掉；先删掉现有隧道再建",
			ErrPairTaken, id, req.A.NodeID, req.B.NodeID)
	}
	va, err := viewOf(views, "本端", req.A.NodeID)
	if err != nil {
		return Tunnel{}, err
	}
	vb, err := viewOf(views, "对端", req.B.NodeID)
	if err != nil {
		return Tunnel{}, err
	}
	if va.Symmetric && vb.Symmetric {
		return Tunnel{}, fmt.Errorf("两端都是对称 NAT，这条隧道不可能建立（中转也不改外层源）")
	}
	ifaceA, err := endpointIface("本端", va, req.A.Iface)
	if err != nil {
		return Tunnel{}, err
	}
	ifaceB, err := endpointIface("对端", vb, req.B.Iface)
	if err != nil {
		return Tunnel{}, err
	}
	addrA, err := endpointAddr("本端", va, req.A.Addr)
	if err != nil {
		return Tunnel{}, err
	}
	addrB, err := endpointAddr("对端", vb, req.B.Addr)
	if err != nil {
		return Tunnel{}, err
	}

	relayNode, _, err := s.resolve(req.Path)
	if err != nil {
		return Tunnel{}, err
	}
	if req.Path != nil {
		if relayNode == req.A.NodeID || relayNode == req.B.NodeID {
			return Tunnel{}, fmt.Errorf("中转设备 %d 同时是隧道的一端", relayNode)
		}
		rv, err := viewOf(views, "中转设备", relayNode)
		if err != nil {
			return Tunnel{}, err
		}
		if rv.Stale {
			return Tunnel{}, fmt.Errorf("中转设备 %d 当前离线", relayNode)
		}
		if _, err := s.ensureTransitPort(relayNode); err != nil {
			return Tunnel{}, err
		}
	}

	// Transit first: allocPort must know it so the two never collide.
	portA, err := s.allocPort(req.A.NodeID, req.A.Port)
	if err != nil {
		return Tunnel{}, fmt.Errorf("本端端口: %w", err)
	}
	portB, err := s.allocPort(req.B.NodeID, req.B.Port)
	if err != nil {
		return Tunnel{}, fmt.Errorf("对端端口: %w", err)
	}
	reservedOverlay := map[netip.Addr]bool{}
	ovA, err := s.allocOverlay(reservedOverlay)
	if err != nil {
		return Tunnel{}, err
	}
	if a, err := netip.ParseAddr(ovA); err == nil {
		reservedOverlay[a] = true
	}
	ovB, err := s.allocOverlay(reservedOverlay)
	if err != nil {
		return Tunnel{}, err
	}
	mtu, err := tunnelMTU(va, vb, ifaceA, ifaceB)
	if err != nil {
		return Tunnel{}, err
	}

	t := &Tunnel{
		ID:        s.nextID("t"),
		A:         Endpoint{NodeID: req.A.NodeID, Iface: ifaceA, Addr: addrA, Port: portA, Overlay: ovA},
		B:         Endpoint{NodeID: req.B.NodeID, Iface: ifaceB, Addr: addrB, Port: portB, Overlay: ovB},
		Path:      clonePath(req.Path),
		MTU:       mtu,
		Roles:     req.Roles,
		BatchID:   req.BatchID,
		CreatedAt: req.Now,
		UpdatedAt: req.Now,
	}
	s.Tunnels[t.ID] = t
	out := *t
	return out, nil
}

// UpdateRequest changes a tunnel in place. Zero values mean "keep": an empty
// iface, a nil address, a zero port and a nil path all leave the current value
// alone, while ClearPath is the explicit way to go back to a direct tunnel.
type UpdateRequest struct {
	IfaceA, IfaceB string
	AddrA, AddrB   *string
	PortA, PortB   uint16
	Path           *TunnelPath
	ClearPath      bool
	Now            int64
}

func (s *State) Update(id string, req UpdateRequest, views map[uint16]NodeView) (Tunnel, error) {
	c := s.Clone()
	t, ok := c.Tunnels[id]
	if !ok {
		return Tunnel{}, ErrNotFound
	}
	if _, err := viewOf(views, "本端", t.A.NodeID); err != nil {
		return Tunnel{}, err
	}
	if _, err := viewOf(views, "对端", t.B.NodeID); err != nil {
		return Tunnel{}, err
	}
	// Take it out of the table first: its own ports must not count as "taken"
	// while we re-validate them. The clone is thrown away on any error, so
	// nothing has to be put back.
	delete(c.Tunnels, id)

	if req.IfaceA != "" {
		iface, err := endpointIface("本端", views[t.A.NodeID], req.IfaceA)
		if err != nil {
			return Tunnel{}, err
		}
		t.A.Iface = iface
	}
	if req.IfaceB != "" {
		iface, err := endpointIface("对端", views[t.B.NodeID], req.IfaceB)
		if err != nil {
			return Tunnel{}, err
		}
		t.B.Iface = iface
	}
	if req.AddrA != nil {
		addr, err := endpointAddr("本端", views[t.A.NodeID], *req.AddrA)
		if err != nil {
			return Tunnel{}, err
		}
		t.A.Addr = addr
	}
	if req.AddrB != nil {
		addr, err := endpointAddr("对端", views[t.B.NodeID], *req.AddrB)
		if err != nil {
			return Tunnel{}, err
		}
		t.B.Addr = addr
	}
	if req.PortA != 0 {
		port, err := c.allocPort(t.A.NodeID, req.PortA)
		if err != nil {
			return Tunnel{}, fmt.Errorf("本端端口: %w", err)
		}
		t.A.Port = port
	}
	if req.PortB != 0 {
		port, err := c.allocPort(t.B.NodeID, req.PortB)
		if err != nil {
			return Tunnel{}, fmt.Errorf("对端端口: %w", err)
		}
		t.B.Port = port
	}

	if req.ClearPath {
		t.Path = nil
	} else if req.Path != nil {
		t.Path = clonePath(req.Path)
	}
	relay, _, err := c.resolve(t.Path)
	if err != nil {
		return Tunnel{}, err
	}
	if t.Path != nil {
		if relay == t.A.NodeID || relay == t.B.NodeID {
			return Tunnel{}, fmt.Errorf("中转设备 %d 同时是隧道的一端", relay)
		}
		if _, err := c.ensureTransitPort(relay); err != nil {
			return Tunnel{}, err
		}
	}
	mtu, err := tunnelMTU(views[t.A.NodeID], views[t.B.NodeID], t.A.Iface, t.B.Iface)
	if err != nil {
		return Tunnel{}, err
	}
	t.MTU = mtu
	t.UpdatedAt = req.Now
	c.Tunnels[id] = t
	c.bump()
	*s = *c
	return *t, nil
}

// Delete removes a tunnel and puts both overlay /32s back at the back of the
// queue. The transit port is NOT released: the device keeps listening on it,
// and the same port serves every other tunnel relayed through it.
func (s *State) Delete(id string) error {
	c := s.Clone()
	t, ok := c.Tunnels[id]
	if !ok {
		return ErrNotFound
	}
	delete(c.Tunnels, id)
	c.freeOverlay(t.A.Overlay)
	c.freeOverlay(t.B.Overlay)
	c.bump()
	*s = *c
	return nil
}

// pairTunnel returns the tunnel already connecting two devices, so a second
// one can be refused before any port or overlay address is allocated.
//
// This is a hard limit of the delivery shape, not a policy choice: the
// controller holds ONE attested WireGuard static key per device, and VPP's peer
// table dedupes public keys across interfaces -- installing the same peer on a
// second interface fails with -116. Per-tunnel keys would lift it; until then
// the device pair is the unit this dataplane can carry.
func (s *State) pairTunnel(a, b uint16) (string, bool) {
	ids := make([]string, 0, len(s.Tunnels))
	for id := range s.Tunnels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := s.Tunnels[id]
		if (t.A.NodeID == a && t.B.NodeID == b) || (t.A.NodeID == b && t.B.NodeID == a) {
			return id, true
		}
	}
	return "", false
}

func viewOf(views map[uint16]NodeView, what string, node uint16) (NodeView, error) {
	v, ok := views[node]
	if !ok || node == 0 {
		return NodeView{}, fmt.Errorf("%s设备 %d 不存在或未入网", what, node)
	}
	return v, nil
}

func endpointIface(what string, v NodeView, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(v.DefaultIface)
	}
	if name == "" {
		return "", fmt.Errorf("%s需要选择物理端口（设备也没有上报可用的端口）", what)
	}
	if len(v.Ifaces) > 0 {
		if _, ok := v.Ifaces[name]; !ok {
			return "", fmt.Errorf("%s设备上没有接口 %q", what, name)
		}
	}
	return name, nil
}

// endpointAddr resolves the outer address a peer is dialed at. An explicit
// value wins; otherwise the reflector's observation is used. Both are "ip" or
// "ip:port" -- a bare ip is paired with the tunnel's own port at delivery time,
// because the observed port belongs to whatever NAT binding produced it.
func endpointAddr(what string, v NodeView, explicit string) (string, error) {
	if a := strings.TrimSpace(explicit); a != "" {
		if err := validateOuterAddr(a); err != nil {
			return "", fmt.Errorf("%s地址 %q: %w", what, a, err)
		}
		return a, nil
	}
	if a := strings.TrimSpace(v.Observed); a != "" {
		if v.ObservedStale {
			return "", fmt.Errorf("%s观测到的外层地址 %s 已过期（超过保鲜期没有新观测）；等设备重新上报或手填地址", what, a)
		}
		return a, nil
	}
	return "", fmt.Errorf("%s还没有可用的外层地址（没有观测到，也没有手填）", what)
}

func validateOuterAddr(s string) error {
	if _, err := netip.ParseAddrPort(s); err == nil {
		return nil
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return nil
	}
	return fmt.Errorf("需要 ip 或 ip:port")
}

// tunnelMTU is the overlay MTU this tunnel may carry, derived from the egress
// links the devices reported -- never from what the operator typed.
//
// It uses the dataplane's WORST-CASE segment count, not this tunnel's current
// path, because that is exactly what the agent refuses to exceed
// (internal/agent/dataplane.go: NewLink). The MTU is one interface-level number
// while the segment list is something the controller rewrites at will, so a
// budget computed from today's hop count would hand out 1440 on a 1500-byte
// underlay and the agent would reject the tunnel -- the mismatch the lab e2e hit
// on 2026-10-28, where every controller-managed tunnel failed to install.
//
// When no side has reported its link MTU the result is 0 (unknown): delivery
// then falls back to wire.DefaultTunnelMTU and labels it as the default. A
// device that has not reported an MTU also gets no UnderlayMTU, so it does not
// check the fallback either.
func tunnelMTU(a, b NodeView, ifaceA, ifaceB string) (uint16, error) {
	ma, oka := a.ifaceMTU(ifaceA)
	mb, okb := b.ifaceMTU(ifaceB)
	if !oka && !okb {
		return 0, nil
	}
	underlay := ma
	if !oka || mb < ma {
		underlay = mb
	}
	budget := wire.TunnelMTUBudget(underlay, wire.MaxSRSegments)
	if budget < wire.MinTunnelMTU {
		return 0, fmt.Errorf("underlay MTU %d 扣掉最多 %d 段 SR 头后只剩 %d，低于最小 %d",
			underlay, wire.MaxSRSegments, budget, wire.MinTunnelMTU)
	}
	if budget > wire.MaxTunnelMTU {
		return wire.MaxTunnelMTU, nil
	}
	return budget, nil
}

func clonePath(p *TunnelPath) *TunnelPath {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
