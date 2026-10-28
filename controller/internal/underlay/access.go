package underlay

import (
	"sort"
	"strconv"
)

// Read-side helpers the controller needs. They live here, not in cmd/sr-ctrl,
// because they encode pool semantics: "how much of the pool is in use" must be
// answered by the same code that hands addresses out, or the number on the page
// will eventually disagree with the allocator.

// Repair restores the invariants of a State that came off disk. A JSON decode
// leaves nil maps behind (omitempty) and may carry a zero settings block, so a
// decoded record is not usable until this has run once.
func (s *State) Repair() { s.ensure() }

// NewBatchID labels one auto batch. It shares the id sequence with tunnels and
// templates so a batch label can never collide with a later one after a restart.
func (s *State) NewBatchID() string { return s.nextID("b") }

// Usage is how much of each pool is spoken for. It is derived on read and never
// stored: a persisted counter is one crash away from being wrong forever.
type Usage struct {
	OverlayUsed  int `json:"overlay_used"`
	OverlayTotal int `json:"overlay_total"`
	// VPNPortsMax is the busiest device's endpoint count. Ports are unique per
	// DEVICE, so "the pool is 80% full" is only meaningful per device; a tenant
	// with ten devices can allocate ten times the pool's size in total.
	VPNPortsMax   int `json:"vpn_ports_max"`
	VPNPortsTotal int `json:"vpn_ports_total"`
	// TransitAssigned counts devices that hold a transit port. The transit pool is
	// not consumed per allocation the way the VPN pool is: every device prefers the
	// same first value.
	TransitAssigned int `json:"transit_assigned"`
}

func (s *State) Usage() Usage {
	u := Usage{
		VPNPortsTotal:   s.Settings.VPNPorts.Len(),
		TransitAssigned: len(s.TransitPorts),
	}
	if n, err := s.Settings.OverlaySize(); err == nil {
		u.OverlayTotal = n
	}
	if used, err := s.overlayUsed(); err == nil {
		u.OverlayUsed = len(used)
	}
	perNode := map[uint16]int{}
	for _, t := range s.Tunnels {
		perNode[t.A.NodeID]++
		perNode[t.B.NodeID]++
	}
	for _, n := range perNode {
		if n > u.VPNPortsMax {
			u.VPNPortsMax = n
		}
	}
	return u
}

// PortsOnNode is the set of endpoint ports this device already listens on. The
// console shows it next to the device so a manual override can avoid a
// collision without guessing.
func (s *State) PortsOnNode(node uint16) []uint16 {
	used := s.portsOn(node)
	out := make([]uint16, 0, len(used))
	for p := range used {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ResolvedPath is a tunnel's relay hop with the template already looked up. The
// console and the topology need the hop as a NODE, not as "template p3": the
// page cannot resolve ids, and a template edit must show up on every tunnel that
// uses it on the next read.
type ResolvedPath struct {
	TemplateID   string `json:"template_id,omitempty"`
	TemplateName string `json:"template_name,omitempty"`
	NodeID       uint16 `json:"node_id"`
	Addr         string `json:"addr,omitempty"`
}

// PathFor resolves a tunnel's path against the current template table. has is
// false for a direct tunnel; err means a reference that does not resolve (the
// template is gone), which the page shows instead of drawing the tunnel as
// direct.
func (s *State) PathFor(p *TunnelPath) (rp ResolvedPath, has bool, err error) {
	if p == nil {
		return ResolvedPath{}, false, nil
	}
	node, addr, err := s.resolve(p)
	if err != nil {
		return ResolvedPath{}, true, err
	}
	rp = ResolvedPath{TemplateID: p.TemplateID, NodeID: node, Addr: addr}
	if p.TemplateID != "" {
		if tpl := s.Paths[p.TemplateID]; tpl != nil {
			rp.TemplateName = tpl.Name
		}
	}
	return rp, true, nil
}

// SortedTunnels returns the tunnels oldest first, breaking ties on the numeric
// part of the id. Ids are monotonic but their string form is not: a plain string
// sort puts t10 before t9.
func (s *State) SortedTunnels() []*Tunnel {
	out := make([]*Tunnel, 0, len(s.Tunnels))
	for _, t := range s.Tunnels {
		if t != nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return idNumber(out[i].ID) < idNumber(out[j].ID)
	})
	return out
}

// SortedPaths is SortedTunnels for path templates.
func (s *State) SortedPaths() []*PathTemplate {
	out := make([]*PathTemplate, 0, len(s.Paths))
	for _, p := range s.Paths {
		if p != nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return idNumber(out[i].ID) < idNumber(out[j].ID)
	})
	return out
}

func idNumber(id string) int64 {
	if len(id) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(id[1:], 10, 64)
	return n
}
