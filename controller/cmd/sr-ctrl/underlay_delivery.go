package main

// Tunnel delivery: the controller renders ONE device's whole tunnel view and the
// device pulls it over its own certificate.
//
// The shape is the same as the customer-side config channel (config_push.go) and
// for the same reason: the boxes are behind NAT, so the controller never dials
// them. The heartbeat advertises a version, the device fetches the snapshot when
// it differs from what it runs.
//
// What is DIFFERENT from the config channel: there is no draft/publish step for
// tunnels. Tunnels are created, edited and deleted as live objects, and each
// mutation bumps underlay.State.Gen, so the version a device sees is "the
// controller's current intention", not "the last frozen publish". That is a
// deliberate split: the customer-side config is a staged document somebody
// reviews, while a tunnel is a link that already exists in the tenant's topology
// and whose ports and overlay addresses are allocated on creation.
//
// Nothing here invents facts. A tunnel the controller cannot describe completely
// (peer without certificate or key binding, relay without an address) is listed
// in Skipped with the reason, and the rest of the device's tunnels still go out:
// one unusable peer must not strand every other link on the box.

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

// errUnderlayNotConfigured means the tenant has no published global
// configuration, so no device has a transit port and no tunnel can exist. The
// endpoint answers 409 (the device is known, there is just nothing to install).
var errUnderlayNotConfigured = fmt.Errorf("租户还没有发布全局配置：没有可下发的隧道配置")

// underlayVersionLocked is what a heartbeat advertises. 0 means "nothing to
// install", which the agent must not read as "install generation 0".
//
// Must be called with s.mu held.
func (s *server) underlayVersionLocked(tenant string) int {
	uc := s.underlayConfigLocked(tenant, false)
	if uc == nil || uc.State == nil || !uc.State.Configured() {
		return 0
	}
	return uc.State.Gen
}

// underlayBundleLocked renders one device's snapshot.
//
// Must be called with s.mu held.
func (s *server) underlayBundleLocked(tenant string, nodeID uint16) (wire.UnderlayBundle, error) {
	uc := s.underlayConfigLocked(tenant, false)
	if uc == nil || uc.State == nil || !uc.State.Configured() {
		return wire.UnderlayBundle{}, errUnderlayNotConfigured
	}
	st := uc.State
	now := time.Now().Unix()
	views := s.nodeViewsLocked(tenant, now)

	out := wire.UnderlayBundle{
		TenantID: tenant, NodeID: nodeID,
		Version: st.Gen, GeneratedAt: now,
		OverlayTable: wire.OverlayRouteTable,
		TransitPort:  st.TransitPorts[nodeID],
		Telemetry:    st.Settings.TelemetryPolicy(),
		PKeys:        s.tenantKeyRing(tenant),
		Tunnels:      []wire.TunnelSpec{},
		Skipped:      []wire.TunnelSkip{},
	}
	for _, t := range st.SortedTunnels() {
		var self, peer underlay.Endpoint
		switch nodeID {
		case t.A.NodeID:
			self, peer = t.A, t.B
		case t.B.NodeID:
			self, peer = t.B, t.A
		default:
			continue
		}
		spec, err := s.tunnelSpecLocked(tenant, st, t, self, peer, views)
		if err != nil {
			out.Skipped = append(out.Skipped, wire.TunnelSkip{
				TunnelID: t.ID, PeerNodeID: peer.NodeID, Reason: err.Error(),
			})
			continue
		}
		out.Tunnels = append(out.Tunnels, spec)
	}
	return out, nil
}

// tunnelSpecLocked renders the view ONE end needs. Everything it cannot know is
// an error, not a zero: a peer dialed at port 0 or an empty certificate would
// install a peer that never completes a handshake, and the device would have no
// way to tell "not configured yet" from "configured wrong".
//
// Must be called with s.mu held.
func (s *server) tunnelSpecLocked(tenant string, st *underlay.State, t *underlay.Tunnel,
	self, peer underlay.Endpoint, views map[uint16]underlay.NodeView) (wire.TunnelSpec, error) {

	prec := s.state.Nodes[key(tenant, peer.NodeID)]
	if prec == nil || prec.TenantID != tenant {
		return wire.TunnelSpec{}, fmt.Errorf("对端设备 %d 不在本租户下", peer.NodeID)
	}
	if strings.TrimSpace(prec.CertPEM) == "" {
		return wire.TunnelSpec{}, fmt.Errorf("对端设备 %d 没有证书，设备无法自己验证它", peer.NodeID)
	}
	// The tunnel's key slot, not the peer's whole keyring: this end installs
	// exactly the one attestation that covers the key its peer will hold for this
	// tunnel. A slot the peer has never reported is the same "nothing to install"
	// hole a missing key always was. The slot number stays out of the text -- this
	// reason is rendered to the tenant, and the skip record already carries both
	// TunnelID and PeerNodeID, so the slot is one lookup away when we debug it.
	peerBinding := bindingAt(prec.Bindings, t.Slot)
	if peerBinding == nil {
		return wire.TunnelSpec{}, fmt.Errorf("对端设备 %d 尚未上报该隧道的密钥", peer.NodeID)
	}

	instance, err := tunnelInstance(t.ID)
	if err != nil {
		return wire.TunnelSpec{}, err
	}
	peerEp, err := dialEndpoint("对端", peer)
	if err != nil {
		return wire.TunnelSpec{}, err
	}

	spec := wire.TunnelSpec{
		TunnelID: t.ID, PeerNodeID: peer.NodeID, PeerSerial: prec.Serial,
		Instance: instance, PathIDBase: wire.UnderlayPathIDBase,
		Iface:        self.Iface,
		ListenPort:   self.Port,
		PeerEndpoint: peerEp,
		PeerCertPEM:  prec.CertPEM,
		Slot:         t.Slot,
		PeerBinding:  peerBinding,
		MTU:          t.MTU,
	}
	// The pool hands out bare addresses and the console renders them bare; the
	// device contract is a /32 prefix, because the dataplane puts it ON the tunnel
	// interface and routes the peer's /32 to it.
	if spec.LocalOverlay, err = overlayPrefix("本端", self); err != nil {
		return wire.TunnelSpec{}, err
	}
	if spec.PeerOverlay, err = overlayPrefix("对端", peer); err != nil {
		return wire.TunnelSpec{}, err
	}
	// 颜色跟着隧道段一起下发（G-2 的唯一契约改动）。设备侧的 sr-fib 隧道表要
	// {next_hop, interface, color} 三个键，前两个设备自己拿得到，color 只有
	// 控制器知道 —— 不发，主备关系在这台盒子上就永远立不起来。指针不是值：
	// 颜色 0 是合法颜色，"老控制器没发过 color"必须能和它区分开。
	color := t.Color
	spec.Color = &color
	// weight / dynamic 跟着隧道段一起下发（weight 的唯一契约改动）。设备侧的
	// sr-fib 隧道表要 weight 才能把 UCMP 写进 VPP 路径；dynamic 只归 agent 的
	// 档位函数用，sr-fib 拿到的是算好的有效值。0 = 默认 1。
	spec.Weight = t.Weight
	spec.Dynamic = t.Dynamic
	// fec_ratio 跟着隧道段一起下发（插件 1.2.0 的 sr_wg_fec_set）。
	spec.FecRatio = t.FecRatio
	// UnderlayMTU is THIS box's egress link MTU; the controller writes only what
	// the device itself reported (each side configures its own port). The map is
	// empty until the agent has reported an inventory, and then the field stays 0
	// ("unknown"), which makes the device keep VPP's own MTU record untouched.
	if mtu, ok := views[self.NodeID].Ifaces[self.Iface]; ok && mtu > 0 {
		spec.UnderlayMTU = mtu
	}
	// The encapsulation source is the egress port's own address, and the only one
	// the controller can prove is on that port is the address the reflector saw
	// (nodeViewsLocked only names a default interface when exactly one interface
	// owns it). Otherwise the device picks from its own inventory.
	if v := views[self.NodeID]; !v.ObservedStale && v.Observed != "" && v.DefaultIface == self.Iface {
		spec.SrcIP = v.Observed
	}

	if t.Path != nil {
		relays, err := relayHops(st, t, views, self.NodeID)
		if err != nil {
			return wire.TunnelSpec{}, err
		}
		spec.Relays = relays
	}
	return spec, nil
}

// relayHops names the relay chain as ONE box must dial it: every hop's outer
// address plus its transit port, in the order THAT box walks them. A hop that
// cannot be named is an error -- the alternative, dropping it or quietly building
// a direct tunnel, would move the traffic onto a path nobody selected.
//
// The stored chain has one direction: hops[0] is the first box a packet leaving A
// touches (underlay.PathHop), because that is the way the operator read it on the
// page. A relayed tunnel is a road with two ends, though -- the return traffic
// walks the same boxes in the opposite order -- so the B end is handed the mirror
// image of the same chain. Sending both ends the stored order (what this used to
// do) makes the far end pop its first segment onto the NEAR end's first relay:
// on a flat L2 lab that still delivers, on any chained topology the packet is
// handed to a box that has no route to where it wants to go, and nothing on the
// page says so.
//
// Per hop, not per tunnel: an address fallback (explicit value vs the reflector's
// observation) and a transit-port lookup are each a hop's own facts, and one hop
// without them must not borrow another hop's. Endpoints are resolved in stored
// order and only the finished list is flipped, so a skip reason's "第 N 跳" always
// points at the row the operator sees on the page, whichever end is being served.
//
// Deliberately one chain, not two: an asymmetric return path (back through
// different relays) is not expressible, and inventing a second chain per tunnel
// would be a data-model change nobody asked for. The tenant topology is a road;
// if a return path must differ, that is a second tunnel's job.
func relayHops(st *underlay.State, t *underlay.Tunnel, views map[uint16]underlay.NodeView,
	from uint16) ([]wire.RelayHop, error) {

	rp, has, err := st.PathFor(t.Path)
	if err != nil {
		return nil, fmt.Errorf("中转路径无法解析：%v", err)
	}
	if !has {
		return nil, fmt.Errorf("隧道 %s 有路径却没有可解析的中转跳", t.ID)
	}
	out := make([]wire.RelayHop, 0, len(rp.Hops))
	for i, h := range rp.Hops {
		ep, err := relayHopEndpoint(st, views, i, h.NodeID, h.Addr)
		if err != nil {
			return nil, err
		}
		out = append(out, wire.RelayHop{NodeID: h.NodeID, Endpoint: ep})
	}
	switch from {
	case t.A.NodeID:
		// A 端就是存储顺序，原样下发。
	case t.B.NodeID:
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	default:
		// 只有两端能拿到一条隧道的链；别的地方要它，说明调用点把隧道弄错了，
		// 与其发一份没人验证过的顺序，不如让它显式失败。
		return nil, fmt.Errorf("隧道 %s 的中转链只能发给它自己的两端，这次要的是设备 %d", t.ID, from)
	}
	return out, nil
}

func relayHopEndpoint(st *underlay.State, views map[uint16]underlay.NodeView, idx int, node uint16, explicit string) (string, error) {
	where := fmt.Sprintf("第 %d 跳", idx+1)
	if node == 0 {
		return "", fmt.Errorf("%s的中转设备没有指定", where)
	}
	addr := strings.TrimSpace(explicit)
	if addr == "" {
		v := views[node]
		switch {
		case v.Observed == "":
			return "", fmt.Errorf("%s的中转设备 %d 还没有外层地址（没有观测到，也没有手填）", where, node)
		case v.ObservedStale:
			return "", fmt.Errorf("%s的中转设备 %d 的观测地址 %s 已过期", where, node, v.Observed)
		}
		addr = v.Observed
	}
	port := st.TransitPorts[node]
	if port == 0 {
		return "", fmt.Errorf("%s的中转设备 %d 还没有分配中转端口", where, node)
	}
	if _, err := netip.ParseAddrPort(addr); err == nil {
		return addr, nil
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return "", fmt.Errorf("%s的中转设备 %d 的地址 %q 不合法", where, node, addr)
	}
	return net.JoinHostPort(a.String(), strconv.Itoa(int(port))), nil
}

// dialEndpoint turns a stored endpoint address into "ip:port".
//
// A stored value is either a bare ip (the reflector's observation, whose port
// half belongs to whichever NAT binding produced it and MUST NOT be dialed) or an
// explicit ip:port the operator typed. Only the second form already carries a
// port; the first is paired with the peer's own listen port.
func dialEndpoint(what string, ep underlay.Endpoint) (string, error) {
	addr := strings.TrimSpace(ep.Addr)
	if addr == "" {
		return "", fmt.Errorf("%s设备 %d 还没有可用的外层地址", what, ep.NodeID)
	}
	if _, err := netip.ParseAddrPort(addr); err == nil {
		return addr, nil
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return "", fmt.Errorf("%s设备 %d 的外层地址 %q 不合法", what, ep.NodeID, addr)
	}
	if !a.Is4() {
		return "", fmt.Errorf("%s设备 %d 的外层地址 %q 不是 IPv4", what, ep.NodeID, addr)
	}
	if ep.Port == 0 {
		return "", fmt.Errorf("%s设备 %d 没有端口", what, ep.NodeID)
	}
	return net.JoinHostPort(a.String(), strconv.Itoa(int(ep.Port))), nil
}

// overlayPrefix renders one end's overlay address as the /32 the device needs.
// A pool address that does not parse is not something to paper over: the tunnel
// would be installed with the wrong inner identity and every packet would be
// misrouted, so the tunnel is skipped with the reason instead.
func overlayPrefix(what string, ep underlay.Endpoint) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(ep.Overlay))
	if err != nil {
		return "", fmt.Errorf("%s设备 %d 的 overlay 地址 %q 不合法", what, ep.NodeID, ep.Overlay)
	}
	if !a.Is4() {
		return "", fmt.Errorf("%s设备 %d 的 overlay 地址 %q 不是 IPv4", what, ep.NodeID, ep.Overlay)
	}
	return netip.PrefixFrom(a, 32).String(), nil
}

// tunnelInstance derives the WG user_instance from the tunnel id's never-reused
// numeric suffix (t7 -> 1007). Reusing an instance is an error the dataplane
// reports as -31, and deriving it from the id keeps it stable across restarts and
// unrelated tunnel deletions. It stays at or above wire.UnderlayInstanceBase,
// which is what tells the device these interfaces are its to manage.
func tunnelInstance(id string) (uint32, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "t"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("隧道 ID %q 读不出序号，无法分配 WG 实例号", id)
	}
	if uint64(n)+uint64(wire.UnderlayInstanceBase) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("隧道 ID %q 的序号过大，实例号装不进 uint32", id)
	}
	return wire.UnderlayInstanceBase + uint32(n), nil
}

// handleUnderlayBundle is the device-facing half of tunnel delivery. Identity
// comes from the client certificate, so a device cannot ask for another node's
// tunnels.
func (s *server) handleUnderlayBundle(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Nodes[key(tenant, nodeID)]; !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	b, err := s.underlayBundleLocked(tenant, nodeID)
	if err != nil {
		// 409 and not 404: the node is known, the tenant just has nothing to
		// deliver yet. The agent keys its retry on this distinction.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, b)
}
