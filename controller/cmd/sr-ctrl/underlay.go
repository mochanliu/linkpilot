package main

// The underlay layer, controller side. See the file set:
//
//	underlay.go       one persisted record per tenant (+ device views, settings checks)
//	underlay_view.go  what the console reads: snapshot and topology
//	underlay_api.go   the /api/underlay handlers
//
// The allocator, the pools and the tunnel model live in internal/underlay and
// are never re-implemented here.

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

// UnderlayConfig is one tenant's underlay record.
type UnderlayConfig struct {
	TenantID string `json:"tenant_id"`

	// Draft is the saved-but-unpublished global configuration. It equals the
	// published one until someone edits and saves.
	Draft   underlay.Settings `json:"draft"`
	DraftAt int64             `json:"draft_at_unix,omitempty"`
	DraftBy string            `json:"draft_by,omitempty"`

	// State holds the published settings (State.Settings.UpdatedAt is the publish
	// time; zero means never published) together with everything derived from
	// them: the transit port of each device, the overlay cursor and the released
	// queue, tunnels and path templates.
	State *underlay.State `json:"state"`

	// PublishedBy is who clicked publish. Publishing is the moment every device is
	// told to open its transit port, so the page states who did it rather than
	// leaving an anonymous act in the audit trail.
	PublishedBy string `json:"published_by,omitempty"`
}

// underlayConfigLocked returns the tenant's record, creating an empty one when
// asked to. A read path passes create=false so a tenant that never opened the
// page does not get an empty row written to disk just by looking.
func (s *server) underlayConfigLocked(tenant string, create bool) *UnderlayConfig {
	if uc := s.state.Underlays[tenant]; uc != nil {
		return uc
	}
	if !create {
		return nil
	}
	d := underlay.DefaultSettings()
	uc := &UnderlayConfig{TenantID: tenant, Draft: d, State: underlay.New(d)}
	s.state.Underlays[tenant] = uc
	return uc
}

// effectiveUnderlayLocked is the tenant's underlay state for reading: the stored
// one, or a fresh default-valued one that is NOT stored.
func (s *server) effectiveUnderlayLocked(tenant string) *underlay.State {
	if uc := s.underlayConfigLocked(tenant, false); uc != nil {
		return uc.State
	}
	return underlay.New(underlay.DefaultSettings())
}

// observedAddr is the address half of an observation. The port half is a NAT
// binding that belongs to whatever destination produced it, so it never becomes
// a tunnel endpoint; only the address is carried into NodeView. See NodeView.
func observedAddr(mapping string) string {
	mapping = strings.TrimSpace(mapping)
	if mapping == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(mapping); err == nil {
		return strings.TrimSpace(host)
	}
	return mapping
}

// nodeViewsLocked turns the tenant's persisted device records into the planner's
// view. Only facts the devices reported are used: an interface is selectable when
// the inventory says it has an MTU, the egress port is the one owning the observed
// address, and a tunnel built on an observation older than observationTTL is
// refused by the planner instead of guessed.
//
// Must be called with s.mu held.
func (s *server) nodeViewsLocked(tenant string, now int64) map[uint16]underlay.NodeView {
	st := s.effectiveUnderlayLocked(tenant)
	views := make(map[uint16]underlay.NodeView, len(s.state.Nodes))
	for _, rec := range s.state.Nodes {
		if rec == nil || rec.TenantID != tenant {
			continue
		}
		name := strings.TrimSpace(rec.Serial)
		if name == "" {
			name = fmt.Sprintf("设备 %d", rec.NodeID)
		}
		v := underlay.NodeView{
			ID:          rec.NodeID,
			Name:        name,
			Stale:       rec.Stale,
			Symmetric:   rec.Symmetric,
			Observed:    observedAddr(rec.UDPMapping),
			TransitPort: st.TransitPorts[rec.NodeID],
		}
		// An observation is a claim about a NAT binding; it is only usable while it
		// is fresh. MappingAt is set by the reflector's report, not by the address
		// being re-sent on a heartbeat, because what has to be fresh is the
		// observation, not the echo of an old one.
		if v.Observed != "" && (rec.MappingAt <= 0 || now-rec.MappingAt > int64(observationTTL/time.Second)) {
			v.ObservedStale = true
		}
		if rec.Ifaces != nil {
			v.Ifaces = make(map[string]uint16, len(*rec.Ifaces))
			for _, it := range *rec.Ifaces {
				iname := strings.TrimSpace(it.Name)
				// MTU 0 is VPP's "not known yet" (an af-packet port before the
				// kernel reports it), and a value beyond uint16 cannot be a link
				// MTU. Neither is a number the budget may be computed from.
				if iname == "" || it.MTU == 0 || it.MTU > 65535 {
					continue
				}
				v.Ifaces[iname] = uint16(it.MTU)
			}
			v.DefaultIface = ifaceForAddr(*rec.Ifaces, v.Observed)
		}
		views[rec.NodeID] = v
	}
	return views
}

// ifaceForAddr names the interface that OWNS an address -- equality with the
// configured address, not containment in its subnet. Containment would pick the
// egress port for any observed address in the same subnet, including the NAT
// gateway's, which is a different box.
//
// Two interfaces claiming one address is not a fact the planner can act on, so
// it yields "" (no default) rather than an arbitrary winner.
func ifaceForAddr(ifaces []wire.IfaceInfo, ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !addr.Is4() || addr.IsUnspecified() {
		return ""
	}
	out := ""
	for _, it := range ifaces {
		name := strings.TrimSpace(it.Name)
		if name == "" || it.MTU == 0 || it.MTU > 65535 {
			continue
		}
		for _, raw := range it.Addrs {
			p, err := netip.ParsePrefix(strings.TrimSpace(raw))
			if err != nil || !p.Addr().Is4() || p.Addr() != addr {
				continue
			}
			if out != "" && out != name {
				return ""
			}
			out = name
		}
	}
	return out
}

// underlaySettingsChecksLocked validates a candidate global configuration the
// same way the page previews it. It runs the package's own rules first, then the
// cross-layer rule only this file can know about: the overlay pool is installed
// in its own table, so it may not overlap a customer subnet or a static route of
// the same tenant -- a next hop resolving to the wrong table is not something a
// device can detect at apply time.
//
// Must be called with s.mu held.
func (s *server) underlaySettingsChecksLocked(tenant string, set underlay.Settings) []ConfigCheck {
	var out []ConfigCheck
	add := func(severity, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: severity, Text: fmt.Sprintf(format, args...)})
	}

	set = set.Normalize()
	if err := set.Validate(); err != nil {
		add("error", "%v", err)
		return out
	}
	pool, err := netip.ParsePrefix(strings.TrimSpace(set.OverlayCIDR))
	if err != nil {
		add("error", "overlay 地址池 %q 不是合法 CIDR。", set.OverlayCIDR)
		return out
	}
	pool = pool.Masked()

	for _, n := range s.tenantNodesLocked(tenant) {
		dc := s.state.Configs[configKey(tenant, n.NodeID)]
		if dc == nil {
			continue
		}
		label := nodeLabel(n)
		for _, it := range dc.Draft.Ifaces {
			p, ok := ifacePrefix(it)
			if !ok || !pool.Overlaps(p) {
				continue
			}
			add("error", "overlay 地址池 %s 与 %s 的客户口 %s（VPN %d）网段 %s 重叠：overlay 在独立表里，撞段会让下一跳解析走错表。",
				pool, label, it.VPP, it.VPN, p)
		}
		for _, r := range dc.Draft.Statics {
			p, err := netip.ParsePrefix(strings.TrimSpace(r.Prefix))
			if err != nil || !p.Masked().Overlaps(pool) {
				continue
			}
			add("error", "overlay 地址池 %s 与 %s 的静态路由 %s 重叠：两个表里同一段地址会互相遮蔽。",
				pool, label, p.Masked())
		}
	}

	// A nearly spent pool is not an error -- exhaustion is reported per pair by
	// the allocator -- but it is the one number worth showing before a tenant
	// plans a bigger mesh.
	usage := s.effectiveUnderlayLocked(tenant).Usage()
	if usage.OverlayTotal > 0 && usage.OverlayUsed*10 >= usage.OverlayTotal*9 {
		add("warning", "overlay 地址池已分配 %d/%d（%d%%），剩余地址只够 %d 条新隧道。",
			usage.OverlayUsed, usage.OverlayTotal,
			usage.OverlayUsed*100/usage.OverlayTotal, (usage.OverlayTotal-usage.OverlayUsed)/2)
	}
	if len(out) == 0 {
		add("info", "没有发现冲突：端口池、overlay 地址池与基础配置的网段、静态路由都不重叠。")
	}
	return out
}

// tenantNodesLocked lists the tenant's devices in node-id order. Order matters
// wherever the result becomes a decision (port assignment on publish, checks):
// map iteration order would otherwise make two runs differ.
//
// Must be called with s.mu held.
func (s *server) tenantNodesLocked(tenant string) []*NodeRecord {
	out := make([]*NodeRecord, 0, len(s.state.Nodes))
	for _, n := range s.state.Nodes {
		if n != nil && n.TenantID == tenant {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func nodeLabel(n *NodeRecord) string {
	if n == nil {
		return "设备"
	}
	if s := strings.TrimSpace(n.Serial); s != "" {
		return fmt.Sprintf("设备 %d（%s）", n.NodeID, s)
	}
	return fmt.Sprintf("设备 %d", n.NodeID)
}

// hasBlockingCheck returns the first error, or "" when nothing blocks a save or
// a publish. Warnings are shown but never gate: "this neighbor is not directly
// connected" is advice, "this pool overlaps a customer subnet" is not.
func hasBlockingCheck(checks []ConfigCheck) string {
	for _, c := range checks {
		if c.Severity == "error" {
			return c.Text
		}
	}
	return ""
}

// settingsDiff is the field-by-field change list the confirmation step shows.
// It compares normalized values, so re-saving the same form reports no change.
func settingsDiff(prev, next underlay.Settings) []string {
	prev, next = prev.Normalize(), next.Normalize()
	var out []string
	line := func(what, a, b string) {
		if a != b {
			out = append(out, fmt.Sprintf("%s：%s → %s", what, a, b))
		}
	}
	line("VPN 端口池", prev.VPNPorts.String(), next.VPNPorts.String())
	line("中转端口池", prev.TransitPorts.String(), next.TransitPorts.String())
	line("VPN 端口排除段", formatRanges(prev.VPNAvoid), formatRanges(next.VPNAvoid))
	line("overlay 地址池", prev.OverlayCIDR, next.OverlayCIDR)
	line("探针间隔", fmt.Sprintf("%dms", prev.ProbeIntervalMS), fmt.Sprintf("%dms", next.ProbeIntervalMS))
	line("BFD 最小发送间隔", fmt.Sprintf("%dms", prev.BFDMinTxMS), fmt.Sprintf("%dms", next.BFDMinTxMS))
	line("BFD 最小接收间隔", fmt.Sprintf("%dms", prev.BFDMinRxMS), fmt.Sprintf("%dms", next.BFDMinRxMS))
	line("BFD 检测倍数", fmt.Sprint(prev.BFDDetectMult), fmt.Sprint(next.BFDDetectMult))
	return out
}

func formatRanges(rs []underlay.PortRange) string {
	if len(rs) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ", ")
}
