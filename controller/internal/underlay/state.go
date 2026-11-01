package underlay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"sdwan/controller/internal/wire"
)

var (
	ErrOverlayExhausted = errors.New("overlay 地址池已耗尽")
	ErrPortExhausted    = errors.New("端口池没有空闲端口")
	ErrPathInUse        = errors.New("路径模板仍被隧道引用")
	ErrNotFound         = errors.New("隧道不存在")
	// ErrPairTaken means this device pair already has a tunnel. It is a conflict
	// with existing state, not a malformed request: one static WG key per device
	// makes a second tunnel on the same pair uninstallable (VPP dedupes peers by
	// pubkey across interfaces, -116).
	ErrPairTaken = errors.New("设备对之间已经有隧道")
)

// Endpoint is one end of a tunnel.
type Endpoint struct {
	NodeID uint16 `json:"node_id"`
	// Iface is the physical (WAN) port this tunnel leaves through. It is a name,
	// not an index: indices are renumbered by VPP restarts, names are not.
	Iface string `json:"iface"`
	// Addr is an explicitly configured outer address. Empty means "use what the
	// reflector observed", which is the normal case.
	Addr string `json:"addr,omitempty"`
	// Port is this end's own listening UDP port, unique on this device.
	Port uint16 `json:"port"`
	// Overlay is this end's /32 on the tunnel.
	Overlay string `json:"overlay"`
}

// MaxTunnelColor is the highest color a tunnel may carry. See Tunnel.Color: the
// eight segments 颜色*1000+10000..+255 are the ones that were checked against the
// table numbers already in use.
const MaxTunnelColor uint8 = 7

// MaxRelayHops is the deepest relay chain a path may carry.
//
// The dataplane can carry more: a VPP path message holds wire.MaxSRSegments (8)
// segments and one of them must be the tunnel destination, so 7 relays is the
// physical ceiling. 3 is the number we accept because it is the depth that has
// actually been made to work on a real box; a longer chain would be an untested
// claim about the data plane, and a path that is wrong only under load is the
// worst kind of wrong. Asserted against the dataplane limit in TestRelayDepthFitsSegments.
const MaxRelayHops = 3

// PathHop is one relay device on a path. Hops are ordered the way a person reads
// a route -- hops[0] is the FIRST box the packet touches -- because that is the
// order the console shows and the order the operator means. The dataplane wants
// the reverse (segment 0 is the destination); the agent flips it, see
// internal/agent/dataplane.go's segment construction.
//
// Addr is an explicit outer address for this hop. Empty means "use what the
// reflector observed", exactly like Endpoint.Addr.
type PathHop struct {
	NodeID uint16 `json:"node_id"`
	Addr   string `json:"addr,omitempty"`
}

// TunnelPath names the relay chain of a tunnel. Empty (nil on the tunnel) means
// the tunnel is direct, which is the zero-hop SR path.
//
// Two forms: TemplateID points at a reusable PathTemplate (editing the template
// changes every tunnel that uses it), or Hops is set inline. Exactly one form is
// allowed; see resolve.
type TunnelPath struct {
	TemplateID string    `json:"template_id,omitempty"`
	Hops       []PathHop `json:"hops,omitempty"`
}

// UnmarshalJSON accepts the one-hop record shape (top-level node_id/addr) that
// every state file written before multi-hop carries, and normalizes it into a
// one-element Hops list. Nothing writes that shape any more; a record that
// carries both is refused rather than guessed at, because "which hop did the
// operator mean" has no safe answer.
func (p *TunnelPath) UnmarshalJSON(data []byte) error {
	var raw struct {
		TemplateID string    `json:"template_id,omitempty"`
		Hops       []PathHop `json:"hops,omitempty"`
		NodeID     uint16    `json:"node_id,omitempty"`
		Addr       string    `json:"addr,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.TemplateID, p.Hops = raw.TemplateID, raw.Hops
	if raw.NodeID != 0 {
		if len(p.Hops) > 0 {
			return fmt.Errorf("路径同时写了 hops 和单跳字段，无法判断以哪个为准")
		}
		p.Hops = []PathHop{{NodeID: raw.NodeID, Addr: raw.Addr}}
	}
	return nil
}

// Roles are display labels only. A hub is a hub for the batch that created this
// tunnel, not a property of the device: the same device can be a hub on one
// edge and a spoke on another, and there is deliberately no node-level role.
type Roles struct {
	A string `json:"a,omitempty"`
	B string `json:"b,omitempty"`
}

// Tunnel is one WireGuard tunnel between two devices.
//
// ID is controller-internal: the user never types or sees it as an identity,
// but the API needs a stable handle for edit/delete.
//
// One tunnel per device pair: Create refuses a second one (see pairTunnel) --
// with one WireGuard static key per device, VPP rejects the duplicate peer on a
// second interface.
type Tunnel struct {
	ID   string      `json:"id"`
	A    Endpoint    `json:"a"`
	B    Endpoint    `json:"b"`
	Path *TunnelPath `json:"path,omitempty"`

	// Color is the tunnel's SD-WAN color. It is the only thing the data plane
	// learns from this field: VPP's preference for the FIB paths that go through
	// this tunnel is PrefBase+Color, so color 0 wins while it is alive and the
	// next-lowest live color takes over on its own (docs/overlay-design-draft.md §4).
	//
	// 0..7, never more: the color is also the high digit of the VRF table number
	// (颜色*1000+10000+主表号), and only those eight segments were checked against
	// the tables in use.
	Color uint8 `json:"color,omitempty"`

	// MTU is derived, never configured by the user: the smaller of the underlay
	// MTUs the devices reported (interface inventory), minus the overhead of the
	// dataplane's WORST-CASE segment list -- not of this path's hop count, because
	// the interface has one MTU while the segment list can be rewritten at any
	// moment, and the agent enforces exactly this budget. 0 means "not known yet":
	// no device on this tunnel reported an interface MTU, so delivery falls back to
	// wire.DefaultTunnelMTU and says so instead of pretending.
	MTU uint16 `json:"mtu,omitempty"`

	Roles     Roles  `json:"roles,omitempty"`
	BatchID   string `json:"batch_id,omitempty"`
	CreatedAt int64  `json:"created_at_unix"`
	UpdatedAt int64  `json:"updated_at_unix"`
}

// PathTemplate is a reusable relay chain: several tunnels can share one template,
// which is the point -- editing the template once moves all of them. The hops are
// ordered exactly like TunnelPath's.
type PathTemplate struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hops      []PathHop `json:"hops"`
	CreatedAt int64     `json:"created_at_unix"`
}

// UnmarshalJSON is the same migration as TunnelPath's: an old template stored one
// device at the top level.
func (t *PathTemplate) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Hops      []PathHop `json:"hops"`
		NodeID    uint16    `json:"node_id"`
		Addr      string    `json:"addr,omitempty"`
		CreatedAt int64     `json:"created_at_unix"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.ID, t.Name, t.Hops, t.CreatedAt = raw.ID, raw.Name, raw.Hops, raw.CreatedAt
	if raw.NodeID != 0 {
		if len(t.Hops) > 0 {
			return fmt.Errorf("路径模板 %s 同时写了 hops 和单跳字段，无法判断以哪个为准", raw.ID)
		}
		t.Hops = []PathHop{{NodeID: raw.NodeID, Addr: raw.Addr}}
	}
	return nil
}

// State is one tenant's whole underlay configuration. It is persisted as part
// of the controller state, so every field must survive a restart.
type State struct {
	Settings Settings `json:"settings"`

	// OverlayNext is the cursor of the never-used overlay addresses (an offset
	// into Settings.OverlayHosts). OverlayFreed holds addresses that came back
	// and are handed out AFTER every never-used one -- a released /32 must go
	// through the whole pool before it can be reused, because peers still route
	// to it for a while. This is the same order a literal FIFO would produce,
	// without storing 65k strings.
	OverlayNext  uint64   `json:"overlay_next"`
	OverlayFreed []string `json:"overlay_freed,omitempty"`

	// TransitPorts is the transit port assigned to a device (one per device,
	// however many tunnels it relays). Assignment happens when the device first
	// appears in a path, not at enrollment.
	TransitPorts map[uint16]uint16 `json:"transit_ports,omitempty"`

	Paths   map[string]*PathTemplate `json:"paths,omitempty"`
	Tunnels map[string]*Tunnel       `json:"tunnels,omitempty"`

	// Seq makes IDs monotonic so a deleted ID is never handed out again.
	Seq int `json:"seq,omitempty"`

	// Gen is the delivery generation: every mutation that changes what a device
	// would install bumps it, and the heartbeat advertises it as the tunnel
	// version. It is stored rather than derived from timestamps because a
	// device compares it for equality (and because two mutations inside one
	// second must still be two versions).
	Gen int `json:"gen,omitempty"`
}

// New returns an empty tenant state with defaults applied.
func New(settings Settings) *State {
	s := &State{Settings: settings.Normalize()}
	s.ensure()
	return s
}

// ensure repairs a state decoded from JSON: nil maps are the normal result of
// omitempty, and a zero settings block means an older/blank record.
func (s *State) ensure() {
	s.Settings = s.Settings.Normalize()
	if s.TransitPorts == nil {
		s.TransitPorts = map[uint16]uint16{}
	}
	if s.Paths == nil {
		s.Paths = map[string]*PathTemplate{}
	}
	if s.Tunnels == nil {
		s.Tunnels = map[string]*Tunnel{}
	}
}

// Clone is a deep copy. Every mutating public method runs on a clone and swaps
// it in on success, so a rejected request can never leave half-allocated ports
// or overlay addresses behind.
func (s *State) Clone() *State {
	c := &State{
		Settings:     s.Settings,
		OverlayNext:  s.OverlayNext,
		OverlayFreed: append([]string(nil), s.OverlayFreed...),
		TransitPorts: make(map[uint16]uint16, len(s.TransitPorts)),
		Paths:        make(map[string]*PathTemplate, len(s.Paths)),
		Tunnels:      make(map[string]*Tunnel, len(s.Tunnels)),
		Seq:          s.Seq,
		Gen:          s.Gen,
	}
	for k, v := range s.TransitPorts {
		c.TransitPorts[k] = v
	}
	for k, v := range s.Paths {
		p := *v
		p.Hops = cloneHops(v.Hops)
		c.Paths[k] = &p
	}
	for k, v := range s.Tunnels {
		t := *v
		if v.Path != nil {
			p := *v.Path
			p.Hops = cloneHops(v.Path.Hops)
			t.Path = &p
		}
		c.Tunnels[k] = &t
	}
	return c
}

// bump advances the delivery generation. Called by every mutation whose result
// a device would install differently -- see State.Gen.
func (s *State) bump() { s.Gen++ }

func (s *State) nextID(prefix string) string {
	s.Seq++
	return fmt.Sprintf("%s%d", prefix, s.Seq)
}

// ---------------------------------------------------------------- port pools

func (s *State) avoided(port uint16) bool {
	for _, r := range s.Settings.VPNAvoid {
		if r.Contains(port) {
			return true
		}
	}
	return false
}

// portsOn is the set of VPN ports this device already listens on. Uniqueness is
// per device; two devices may use the same number.
func (s *State) portsOn(node uint16) map[uint16]bool {
	used := map[uint16]bool{}
	for _, t := range s.Tunnels {
		if t.A.NodeID == node && t.A.Port != 0 {
			used[t.A.Port] = true
		}
		if t.B.NodeID == node && t.B.Port != 0 {
			used[t.B.Port] = true
		}
	}
	return used
}

// allocPort hands out a listening port for one device. want != 0 is an explicit
// override and is checked, not just recorded: a port that is outside the pool,
// excluded, used by this device, or equal to its transit port would leave the
// dataplane with one port and two meanings.
func (s *State) allocPort(node, want uint16) (uint16, error) {
	used := s.portsOn(node)
	transit := s.TransitPorts[node]
	if want != 0 {
		switch {
		case !s.Settings.VPNPorts.Contains(want):
			return 0, fmt.Errorf("端口 %d 不在 VPN 端口池 %s 内", want, s.Settings.VPNPorts)
		case s.avoided(want):
			return 0, fmt.Errorf("端口 %d 在排除段里", want)
		case used[want]:
			return 0, fmt.Errorf("端口 %d 已被这台设备的另一条隧道占用", want)
		case want == transit:
			return 0, fmt.Errorf("端口 %d 是这台设备的中转端口", want)
		}
		return want, nil
	}
	for p := int(s.Settings.VPNPorts.Min); p <= int(s.Settings.VPNPorts.Max); p++ {
		port := uint16(p)
		if s.avoided(port) || used[port] || port == transit {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("%w（VPN %s）", ErrPortExhausted, s.Settings.VPNPorts)
}

// ensureTransitPort assigns the device's single transit port. Every device
// prefers the same value (the bottom of the transit pool); only a device whose
// port was already taken elsewhere needs the next one, and that decision is
// recorded here so it does not move again.
func (s *State) ensureTransitPort(node uint16) (uint16, error) {
	if p := s.TransitPorts[node]; p != 0 {
		return p, nil
	}
	for p := int(s.Settings.TransitPorts.Min); p <= int(s.Settings.TransitPorts.Max); p++ {
		port := uint16(p)
		if s.portsOn(node)[port] {
			continue
		}
		s.TransitPorts[node] = port
		return port, nil
	}
	return 0, fmt.Errorf("%w（中转 %s）", ErrPortExhausted, s.Settings.TransitPorts)
}

// SetTransitPort records the port a device actually bound. The agent reports it
// after a rebind, and peers must use what it really holds, not what was asked.
func (s *State) SetTransitPort(node, port uint16) error {
	if port == 0 {
		if _, had := s.TransitPorts[node]; had {
			delete(s.TransitPorts, node)
			s.bump()
		}
		return nil
	}
	if !s.Settings.TransitPorts.Contains(port) {
		return fmt.Errorf("中转端口 %d 不在中转端口池 %s 内", port, s.Settings.TransitPorts)
	}
	if s.portsOn(node)[port] {
		return fmt.Errorf("中转端口 %d 与这台设备的一条隧道端口相同", port)
	}
	if s.TransitPorts[node] != port {
		s.TransitPorts[node] = port
		s.bump()
	}
	return nil
}

// ------------------------------------------------------------ overlay pool

func (s *State) overlayUsed() (map[netip.Addr]bool, error) {
	used := map[netip.Addr]bool{}
	for _, t := range s.Tunnels {
		for _, raw := range []string{t.A.Overlay, t.B.Overlay} {
			if raw == "" {
				continue
			}
			a, err := netip.ParseAddr(raw)
			if err != nil {
				return nil, fmt.Errorf("隧道 %s 的 overlay 地址 %q 损坏: %w", t.ID, raw, err)
			}
			used[a] = true
		}
	}
	return used, nil
}

// allocOverlay takes the next address: never-used ones in pool order first, and
// only after those run out, the ones released earliest (the back of the queue).
//
// reserved holds addresses handed out earlier in the same request but not yet
// stored on a tunnel -- the two ends of a new tunnel are allocated before the
// tunnel exists, and without this the second call would happily return the
// first end's address when it comes off the released queue (the cursor does not
// move in that case).
func (s *State) allocOverlay(reserved map[netip.Addr]bool) (string, error) {
	hosts, err := s.Settings.OverlayHosts()
	if err != nil {
		return "", err
	}
	used, err := s.overlayUsed()
	if err != nil {
		return "", err
	}
	for a := range reserved {
		used[a] = true
	}
	n := uint64(len(hosts))
	if n == 0 {
		return "", ErrOverlayExhausted
	}
	queued := make(map[netip.Addr]bool, len(s.OverlayFreed))
	for _, raw := range s.OverlayFreed {
		if a, err := netip.ParseAddr(raw); err == nil {
			queued[a] = true
		}
	}
	for i := uint64(0); i < n; i++ {
		idx := (s.OverlayNext + i) % n
		a := hosts[idx]
		if used[a] || queued[a] {
			continue
		}
		s.OverlayNext = (idx + 1) % n
		return a.String(), nil
	}
	for len(s.OverlayFreed) > 0 {
		raw := s.OverlayFreed[0]
		s.OverlayFreed = s.OverlayFreed[1:]
		a, err := netip.ParseAddr(raw)
		if err != nil || used[a] {
			continue
		}
		return a.String(), nil
	}
	return "", ErrOverlayExhausted
}

// freeOverlay puts an address at the back of the queue.
func (s *State) freeOverlay(addr string) {
	if addr == "" {
		return
	}
	s.OverlayFreed = append(s.OverlayFreed, addr)
}

// ---------------------------------------------------------------- settings

// Configured reports whether the tenant has published a global configuration.
// Nothing can be created before that: publishing is what hands every device its
// transit port and tells it to listen, so a tunnel pushed earlier would point at
// a relay port nobody is bound to.
func (s *State) Configured() bool { return s.Settings.UpdatedAt != 0 }

// PublishSettings applies a new global configuration and gives every enrolled
// device its single transit port. nodes is the tenant's current device list:
// the port is allocated per device (not per tunnel), so it is assigned once here
// and reused by every tunnel that later relays through that device.
func (s *State) PublishSettings(next Settings, nodes []uint16, now int64) error {
	if now <= 0 {
		return fmt.Errorf("发布全局配置需要时间戳")
	}
	c := s.Clone()
	next = next.Normalize()
	next.UpdatedAt = now
	if err := c.SetSettings(next); err != nil {
		return err
	}
	seen := map[uint16]bool{}
	for _, n := range nodes {
		if n == 0 || seen[n] {
			continue
		}
		seen[n] = true
		if _, err := c.ensureTransitPort(n); err != nil {
			return err
		}
	}
	*s = *c
	return nil
}

// EnsureDevice gives one device the transit port the published configuration
// owes it. Publishing only reaches the devices enrolled at that moment, and
// customers do not enroll every device at once: without this, a device that
// arrives later carries transit_port 0 forever (nothing can relay through it)
// even though the tenant's global configuration is in force.
//
// Before publishing it is a no-op -- there is no configuration to attach a port
// to yet. Afterwards it is idempotent, and a failed allocation leaves the state
// untouched so a retry can still succeed.
func (s *State) EnsureDevice(node uint16) (port uint16, assigned bool, err error) {
	if node == 0 {
		return 0, false, fmt.Errorf("设备号不能为 0")
	}
	if !s.Configured() {
		return 0, false, nil
	}
	if p := s.TransitPorts[node]; p != 0 {
		return p, false, nil
	}
	c := s.Clone()
	p, err := c.ensureTransitPort(node)
	if err != nil {
		return 0, false, err
	}
	c.bump()
	*s = *c
	return p, true, nil
}

// SetSettings validates and applies a new global configuration. Existing
// allocations are never renumbered: a range change only affects what is handed
// out from now on. A change that would orphan a live tunnel is refused instead
// of silently stranding it.
func (s *State) SetSettings(next Settings) error {
	next = next.Normalize()
	if err := next.Validate(); err != nil {
		return err
	}
	prefix, err := next.overlayPrefix()
	if err != nil {
		return err
	}
	for _, t := range s.Tunnels {
		for _, ep := range []Endpoint{t.A, t.B} {
			if !next.VPNPorts.Contains(ep.Port) {
				return fmt.Errorf("隧道 %s 的端口 %d 会落到新的 VPN 端口池 %s 之外；先改隧道再改池",
					t.ID, ep.Port, next.VPNPorts)
			}
			a, err := netip.ParseAddr(ep.Overlay)
			if err != nil || !prefix.Contains(a) {
				return fmt.Errorf("隧道 %s 的 overlay 地址 %s 会落到新的地址池 %s 之外；先删隧道再改池",
					t.ID, ep.Overlay, prefix)
			}
		}
	}
	for node, port := range s.TransitPorts {
		if !next.TransitPorts.Contains(port) {
			return fmt.Errorf("设备 %d 的中转端口 %d 会落到新的中转端口池 %s 之外", node, port, next.TransitPorts)
		}
	}
	// Released addresses outside the new pool can never be handed out again.
	kept := s.OverlayFreed[:0]
	for _, raw := range s.OverlayFreed {
		if a, err := netip.ParseAddr(raw); err == nil && prefix.Contains(a) {
			kept = append(kept, raw)
		}
	}
	s.OverlayFreed = kept
	hosts, err := next.OverlayHosts()
	if err != nil {
		return err
	}
	if len(hosts) > 0 {
		s.OverlayNext = s.OverlayNext % uint64(len(hosts))
	}
	s.Settings = next
	s.bump()
	return nil
}

// ------------------------------------------------------------------ paths

// CreatePath adds a reusable relay chain.
func (s *State) CreatePath(name string, hops []PathHop, now int64) (PathTemplate, error) {
	c := s.Clone()
	p, err := c.createPath(name, hops, now)
	if err != nil {
		return PathTemplate{}, err
	}
	c.bump()
	*s = *c
	return p, nil
}

func (s *State) createPath(name string, hops []PathHop, now int64) (PathTemplate, error) {
	if err := validateHops(hops); err != nil {
		return PathTemplate{}, err
	}
	name = trimName(name)
	if name == "" {
		name = defaultPathName(hops)
	}
	p := PathTemplate{ID: s.nextID("p"), Name: name, Hops: cloneHops(hops), CreatedAt: now}
	s.Paths[p.ID] = &p
	return p, nil
}

// defaultPathName names a template the operator did not bother to name. The hops
// are listed in traversal order so the name reads like the route.
func defaultPathName(hops []PathHop) string {
	names := make([]string, 0, len(hops))
	for _, h := range hops {
		names = append(names, strconv.FormatUint(uint64(h.NodeID), 10))
	}
	return "经设备 " + strings.Join(names, "→") + " 中转"
}

// UpdatePath moves every tunnel that references the template at once -- that is
// what sharing a template means. The caller is expected to show the fan-out
// (PathUsers) before confirming.
func (s *State) UpdatePath(id, name string, hops []PathHop) error {
	c := s.Clone()
	p, ok := c.Paths[id]
	if !ok {
		return ErrNotFound
	}
	if err := validateHops(hops); err != nil {
		return err
	}
	for _, t := range c.Tunnels {
		if t.Path == nil || t.Path.TemplateID != id {
			continue
		}
		for _, h := range hops {
			if h.NodeID == t.A.NodeID || h.NodeID == t.B.NodeID {
				return fmt.Errorf("隧道 %s 的一端就是设备 %d，不能把它同时当作中转", t.ID, h.NodeID)
			}
		}
	}
	p.Hops = cloneHops(hops)
	if n := trimName(name); n != "" {
		p.Name = n
	}
	c.bump()
	*s = *c
	return nil
}

// DeletePath refuses while any tunnel still points at it: silently falling back
// to a direct path would turn "delete a template" into "drop everyone's relay".
func (s *State) DeletePath(id string) error {
	if _, ok := s.Paths[id]; !ok {
		return ErrNotFound
	}
	if users := s.PathUsers(id); len(users) > 0 {
		return fmt.Errorf("%w（%d 条隧道）", ErrPathInUse, len(users))
	}
	delete(s.Paths, id)
	s.bump()
	return nil
}

// PathUsers lists the tunnels a template is applied to.
func (s *State) PathUsers(id string) []string {
	out := []string{}
	for _, t := range s.Tunnels {
		if t.Path != nil && t.Path.TemplateID == id {
			out = append(out, t.ID)
		}
	}
	sort.Strings(out)
	return out
}

// resolve turns a tunnel's path into the ordered relay chain it must traverse.
// The ports are not returned: they belong to the devices and are looked up at
// delivery time, so a device that had to rebind does not leave stale ports on
// every tunnel.
//
// Every caller gets the whole list or an error -- a resolve that returned only
// the first hop would silently shorten a 3-hop path on every tunnel that uses it.
func (s *State) resolve(p *TunnelPath) ([]PathHop, error) {
	if p == nil {
		return nil, nil
	}
	if p.TemplateID != "" {
		if len(p.Hops) > 0 {
			return nil, fmt.Errorf("路径只能二选一：模板或内联中转设备")
		}
		tpl, ok := s.Paths[p.TemplateID]
		if !ok {
			return nil, fmt.Errorf("路径模板 %s 不存在", p.TemplateID)
		}
		if err := validateHops(tpl.Hops); err != nil {
			return nil, fmt.Errorf("路径模板 %s: %w", p.TemplateID, err)
		}
		return cloneHops(tpl.Hops), nil
	}
	if err := validateHops(p.Hops); err != nil {
		return nil, err
	}
	return cloneHops(p.Hops), nil
}

// validateHops checks a hop list on its own terms: non-empty, within the depth
// the data plane has been made to carry, no device twice, no placeholder. Whether
// each hop exists, is online and is not one of the tunnel's own ends is a question
// about the devices, and lives in checkHops.
func validateHops(hops []PathHop) error {
	if len(hops) == 0 {
		return fmt.Errorf("路径需要至少一个中转设备")
	}
	if len(hops) > MaxRelayHops {
		return fmt.Errorf("中转最多 %d 跳，这条路径有 %d 跳：一段路径最多 %d 个段，其中一段必须留给隧道终点",
			MaxRelayHops, len(hops), wire.MaxSRSegments)
	}
	seen := make(map[uint16]bool, len(hops))
	for i, h := range hops {
		if h.NodeID == 0 {
			return fmt.Errorf("路径第 %d 跳没有指定中转设备", i+1)
		}
		if seen[h.NodeID] {
			return fmt.Errorf("设备 %d 在路径里出现了两次：中转链不能绕回同一台设备", h.NodeID)
		}
		seen[h.NodeID] = true
	}
	return nil
}

func cloneHops(hops []PathHop) []PathHop {
	if len(hops) == 0 {
		return nil
	}
	out := make([]PathHop, len(hops))
	copy(out, hops)
	return out
}

func trimName(s string) string { return strings.Join(strings.Fields(s), " ") }
