package underlay

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"sdwan/controller/internal/keybinding"
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
	// Color is the tunnel's SD-WAN color; see Tunnel.Color.
	Color uint8
	// Weight 是分担权重（0 = 默认 1）；Dynamic 见 Tunnel.Dynamic。值域由
	// uint8 天然封顶（VPP fib_path.weight 是 u8），JSON 解码层挡超界。
	Weight  uint8
	Dynamic bool
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
	if err := checkColor(req.Color); err != nil {
		return Tunnel{}, err
	}
	slot, err := s.allocPairSlot(req.A.NodeID, req.B.NodeID)
	if err != nil {
		return Tunnel{}, err
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

	hops, err := s.resolve(req.Path)
	if err != nil {
		return Tunnel{}, err
	}
	if err := s.checkHops(hops, req.A.NodeID, req.B.NodeID, views); err != nil {
		return Tunnel{}, err
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
		Slot:      slot,
		Color:     req.Color,
		Weight:    req.Weight,
		Dynamic:   req.Dynamic,
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
	// Color is a pointer because 0 is a real color: "keep the current color" and
	// "recolor this tunnel to 0" cannot both be written as a zero value.
	Color *uint8
	// Weight / Dynamic 是指针：weight 0 是合法值（= 回到默认 1），dynamic
	// false 是合法值（= 关掉动态）——"保持不变"只能用 nil 表达。控制台表单
	// 总是提交这两个字段的当前值，所以 nil 只出现在没有带它们的调用方。
	Weight  *uint8
	Dynamic *bool
	Now     int64
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

	if req.Color != nil {
		if err := checkColor(*req.Color); err != nil {
			return Tunnel{}, err
		}
		t.Color = *req.Color
	}
	if req.Weight != nil {
		t.Weight = *req.Weight
	}
	if req.Dynamic != nil {
		t.Dynamic = *req.Dynamic
	}
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
	hops, err := c.resolve(t.Path)
	if err != nil {
		return Tunnel{}, err
	}
	if err := c.checkHops(hops, t.A.NodeID, t.B.NodeID, views); err != nil {
		return Tunnel{}, err
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

// pairTunnel returns the tunnel already connecting two devices, if any.
//
// It is NO LONGER a create gate: a pair may hold up to keybinding.WGKeySlots
// tunnels (one per key slot), which is what UCMP needs. What is left of it is
// the Fan contract — an auto batch builds one tunnel per pair and must not
// duplicate an existing one on a re-run — so Fan skips peers that already have
// a tunnel while manual Create allocates the next free slot.
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

// allocPairSlot picks the key slot for a NEW tunnel between two devices: the
// lowest slot not already used by the pair's live tunnels. Both ends keep
// WGKeySlots attested keys and the pair's n-th tunnel uses key n on BOTH ends,
// so the two ends never have to exchange a keyring to agree
// (docs/nat-tunnel-ladder-plan.md §10). Derived at creation, never typed.
//
// Slot 0 is where a device's pre-slot key already lives, so the first tunnel of
// every pair lands there and old single-tunnel pairs cost no re-handshake.
// Deleting a tunnel frees its slot; the next create reuses the lowest free one,
// which keeps slots dense without any renumbering of live tunnels. The cap is
// WGKeySlots itself: out of slots means ErrPairFull.
func (s *State) allocPairSlot(a, b uint16) (uint8, error) {
	used := map[uint8]bool{}
	for _, t := range s.Tunnels {
		if (t.A.NodeID == a && t.B.NodeID == b) || (t.A.NodeID == b && t.B.NodeID == a) {
			used[t.Slot] = true
		}
	}
	for slot := uint8(0); slot < keybinding.WGKeySlots; slot++ {
		if !used[slot] {
			return slot, nil
		}
	}
	return 0, fmt.Errorf("%w：设备 %d 和 %d 之间已有 %d 条隧道，每台设备只有 %d 把 WireGuard 密钥（槽 0..%d）"+
		"；要先删一条才能再建", ErrPairFull, a, b, keybinding.WGKeySlots, keybinding.WGKeySlots, keybinding.WGKeySlots-1)
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

// checkHops validates a resolved relay chain against the tunnel it belongs to and
// against the devices as they are right now. A hop may not be one of the two ends
// (the box would be asked to relay to itself); each hop must be enrolled and
// online; and each hop must already hold a transit port, which is why the port
// allocation happens here and not as a surprise at delivery time.
//
// It is the same rule for create and update: a tunnel edited onto an offline
// relay is no more installable than one created onto it, and saving it would only
// move the failure to the device.
//
// It mutates the receiver (ensureTransitPort), so callers must pass a clone.
func (s *State) checkHops(hops []PathHop, a, b uint16, views map[uint16]NodeView) error {
	for i, h := range hops {
		if h.NodeID == a || h.NodeID == b {
			return fmt.Errorf("路径第 %d 跳的中转设备 %d 同时是隧道的一端", i+1, h.NodeID)
		}
		v, ok := views[h.NodeID]
		if !ok {
			return fmt.Errorf("路径第 %d 跳的中转设备 %d 不存在或未入网", i+1, h.NodeID)
		}
		if v.Stale {
			return fmt.Errorf("路径第 %d 跳的中转设备 %d 当前离线", i+1, h.NodeID)
		}
		if _, err := s.ensureTransitPort(h.NodeID); err != nil {
			return err
		}
	}
	return nil
}

func clonePath(p *TunnelPath) *TunnelPath {
	if p == nil {
		return nil
	}
	return &TunnelPath{TemplateID: p.TemplateID, Hops: cloneHops(p.Hops)}
}

// checkColor refuses a color whose VPP preference would either wrap (PrefBase is
// 10 and preference is a u8 with no saturation) or land on a table-number segment
// nobody verified. 0..7 is the range the design committed to.
func checkColor(c uint8) error {
	if c > MaxTunnelColor {
		return fmt.Errorf("颜色 %d 超出 0..%d", c, MaxTunnelColor)
	}
	return nil
}
