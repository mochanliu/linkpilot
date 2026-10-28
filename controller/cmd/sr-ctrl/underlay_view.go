package main

// What the console reads: the underlay document (全局配置 + 隧道配置) and the
// topology. Both are READ MODELS. They carry names, ages and resolved paths so
// the page never has to look anything up -- a node id means nothing to a
// customer, and the page cannot resolve a template id -- and nothing in this
// file changes state.
//
// The one thing these views deliberately do NOT carry is a per-EDGE tunnel
// state. Devices do report the tunnel channel (wire.TunnelState, shown per node)
// and their own tunnel table, but telemetry is still keyed by the dataplane's
// sw_if_index and the controller does not yet join a telemetry row to a
// configured tunnel, so every edge says "unknown" and the document says why. An
// invented "down" would be read as a measurement.

import (
	"sort"

	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

// underlayStateNote explains the fixed "unknown" edge state. It is a sentence,
// not a code, because it is the honest answer to "why is nothing green yet".
const underlayStateNote = "每条隧道的运行状态仍是「未知」：设备会回报它自己建了几条隧道（见设备表的「隧道下发」列），但遥测行按数据面接口索引记账，控制器还没把每一行对到某条隧道上。"

type underlayEndpointView struct {
	NodeID uint16 `json:"node_id"`
	Serial string `json:"serial"`
	Iface  string `json:"iface"`
	// Addr is the outer address this end is dialed at (ip or ip:port, as stored).
	Addr string `json:"addr"`
	// MatchesObservation says whether Addr is still the address the reflector sees.
	// It is a comparison, not a provenance claim: a tunnel built from an older
	// observation also differs, which is exactly when the page should point at it.
	MatchesObservation bool   `json:"matches_observation"`
	Port               uint16 `json:"port"`
	Overlay            string `json:"overlay"`
}

type underlayResolvedPathView struct {
	TemplateID   string `json:"template_id,omitempty"`
	TemplateName string `json:"template_name,omitempty"`
	NodeID       uint16 `json:"node_id"`
	Serial       string `json:"serial"`
	Addr         string `json:"addr,omitempty"`
}

type underlayTunnelView struct {
	ID string               `json:"id"`
	A  underlayEndpointView `json:"a"`
	B  underlayEndpointView `json:"b"`
	// Path is the relay hop; nil means the tunnel is direct. PathError is a
	// reference that no longer resolves -- shown, not silently drawn as direct.
	Path      *underlayResolvedPathView `json:"path,omitempty"`
	PathError string                    `json:"path_error,omitempty"`

	// MTU 0 means no device on this tunnel has reported a link MTU; delivery then
	// uses wire.DefaultTunnelMTU, and MTUIsDefault says so instead of pretending
	// the number was computed.
	MTU          uint16 `json:"mtu"`
	MTUIsDefault bool   `json:"mtu_is_default"`

	Roles     underlay.Roles `json:"roles,omitempty"`
	BatchID   string         `json:"batch_id,omitempty"`
	CreatedAt int64          `json:"created_at_unix"`
	UpdatedAt int64          `json:"updated_at_unix"`

	State string `json:"state"` // always "unknown"; see underlayStateNote
}

type underlayNodeView struct {
	NodeID   uint16 `json:"node_id"`
	Serial   string `json:"serial"`
	AgentVer string `json:"agent_version"`
	Status   string `json:"status"` // online | stale | pending
	LastSeen int64  `json:"last_seen_unix"`

	// Observed is the address half of the reflector's observation (the only half
	// a tunnel endpoint may use); Mapping is the raw "ip:port" as reported.
	Observed       string `json:"observed_addr"`
	Mapping        string `json:"udp_mapping"`
	ObservedAgeSec int64  `json:"observed_age_sec"`
	ObservedStale  bool   `json:"observed_stale"`
	Symmetric      bool   `json:"symmetric_nat"`

	// Ifaces is [] when the agent never reported an inventory; inventory_reported
	// separates "no data" from "dataplane has no interfaces".
	InventoryReported bool             `json:"inventory_reported"`
	InventoryAgeSec   int64            `json:"inventory_age_sec"`
	Ifaces            []wire.IfaceInfo `json:"ifaces"`
	DefaultIface      string           `json:"default_iface"`

	TransitPort uint16   `json:"transit_port"`
	Ports       []uint16 `json:"ports"`
	TunnelCount int      `json:"tunnel_count"`
	RelayFor    int      `json:"relay_for"`

	// TunnelDelivery is the device's OWN account of the underlay channel
	// (wire.TunnelState), persisted with the node. nil means it never reported
	// (an agent without -vpp-api); capable:false means the channel exists and is
	// switched off. TunnelCount above is what the CONTROLLER configured for this
	// node, so the page shows both: "configured" is not "running".
	TunnelDelivery *wire.TunnelState `json:"tunnel_delivery,omitempty"`
}

type underlayTemplateView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	NodeID    uint16   `json:"node_id"`
	Serial    string   `json:"serial"`
	Addr      string   `json:"addr,omitempty"`
	Users     []string `json:"users"`
	CreatedAt int64    `json:"created_at_unix"`
}

type underlayView struct {
	NowUnix int64 `json:"now_unix"`

	// Configured is "global configuration has been published". Nothing can be
	// created before that, and the page shows the gate rather than an error after
	// the fact.
	Configured bool              `json:"configured"`
	Settings   underlay.Settings `json:"settings"` // the draft
	DraftAt    int64             `json:"draft_at_unix,omitempty"`
	DraftBy    string            `json:"draft_by,omitempty"`
	// Published is present only after a publish; Settings may differ from it, in
	// which case Changes lists how.
	Published   *underlay.Settings `json:"published,omitempty"`
	PublishedAt int64              `json:"published_at_unix,omitempty"`
	PublishedBy string             `json:"published_by,omitempty"`
	Changes     []string           `json:"changes"`
	Checks      []ConfigCheck      `json:"checks"`
	Usage       underlay.Usage     `json:"usage"`

	DefaultTunnelMTU uint16 `json:"default_tunnel_mtu"`
	StateNote        string `json:"state_note"`

	Nodes   []underlayNodeView     `json:"nodes"`
	Tunnels []underlayTunnelView   `json:"tunnels"`
	Paths   []underlayTemplateView `json:"paths"`
}

// underlayTopology is the graph page's document. Edges are bundled per device
// pair: several tunnels between the same two devices are one line that can be
// expanded, not three lines on top of each other.
type underlayTopology struct {
	NowUnix    int64                `json:"now_unix"`
	Configured bool                 `json:"configured"`
	StateNote  string               `json:"state_note"`
	Nodes      []underlayNodeView   `json:"nodes"`
	Bundles    []underlayTopoBundle `json:"bundles"`
}

type underlayTopoBundle struct {
	A       uint16               `json:"a"`
	B       uint16               `json:"b"`
	SerialA string               `json:"serial_a"`
	SerialB string               `json:"serial_b"`
	Count   int                  `json:"count"`
	State   string               `json:"state"`
	Tunnels []underlayTunnelView `json:"tunnels"`
}

// --------------------------------------------------------------- builders

// underlayViewLocked renders the underlay document. Must be called with s.mu
// held.
func (s *server) underlayViewLocked(tenant string, now int64) underlayView {
	uc := s.underlayConfigLocked(tenant, false)
	st := s.effectiveUnderlayLocked(tenant)
	draft := underlay.DefaultSettings()
	out := underlayView{
		NowUnix:          now,
		Configured:       st.Configured(),
		Settings:         draft,
		DefaultTunnelMTU: wire.DefaultTunnelMTU,
		StateNote:        underlayStateNote,
		Changes:          []string{},
		Checks:           []ConfigCheck{},
		Nodes:            []underlayNodeView{},
		Tunnels:          []underlayTunnelView{},
		Paths:            []underlayTemplateView{},
	}
	if uc != nil {
		draft = uc.Draft
		out.Settings = draft
		out.DraftAt, out.DraftBy = uc.DraftAt, uc.DraftBy
		out.PublishedBy = uc.PublishedBy
	}
	if st.Configured() {
		p := st.Settings
		out.Published = &p
		out.PublishedAt = p.UpdatedAt
		out.Changes = settingsDiff(p, draft)
	}
	out.Checks = s.underlaySettingsChecksLocked(tenant, draft)
	out.Usage = st.Usage()

	views := s.nodeViewsLocked(tenant, now)
	serials := map[uint16]string{}
	relayFor := map[uint16]int{}
	counts := map[uint16]int{}
	for _, t := range st.SortedTunnels() {
		counts[t.A.NodeID]++
		counts[t.B.NodeID]++
		if rp, has, err := st.PathFor(t.Path); has && err == nil {
			relayFor[rp.NodeID]++
		}
	}

	for _, n := range s.tenantNodesLocked(tenant) {
		v := views[n.NodeID]
		serials[n.NodeID] = v.Name
		row := underlayNodeView{
			NodeID: n.NodeID, Serial: n.Serial, AgentVer: n.AgentVer,
			Status: nodeStatus(n), LastSeen: n.LastSeen,
			Observed: v.Observed, Mapping: n.UDPMapping,
			ObservedStale: v.ObservedStale, Symmetric: n.Symmetric,
			DefaultIface: v.DefaultIface, TransitPort: v.TransitPort,
			Ports: st.PortsOnNode(n.NodeID), TunnelCount: counts[n.NodeID],
			RelayFor: relayFor[n.NodeID], Ifaces: []wire.IfaceInfo{},
			TunnelDelivery: n.Tunnel,
		}
		if n.MappingAt > 0 && now > n.MappingAt {
			row.ObservedAgeSec = now - n.MappingAt
		}
		if n.Ifaces != nil {
			row.InventoryReported = true
			row.Ifaces = append(row.Ifaces, (*n.Ifaces)...)
			if n.InventoryAt > 0 && now > n.InventoryAt {
				row.InventoryAgeSec = now - n.InventoryAt
			}
		}
		out.Nodes = append(out.Nodes, row)
	}

	for _, t := range st.SortedTunnels() {
		out.Tunnels = append(out.Tunnels, buildTunnelView(st, t, serials, views))
	}
	for _, p := range st.SortedPaths() {
		out.Paths = append(out.Paths, buildTemplateView(st, p, serials))
	}
	return out
}

func buildTemplateView(st *underlay.State, p *underlay.PathTemplate, serials map[uint16]string) underlayTemplateView {
	return underlayTemplateView{
		ID: p.ID, Name: p.Name, NodeID: p.NodeID, Serial: serials[p.NodeID],
		Addr: p.Addr, Users: st.PathUsers(p.ID), CreatedAt: p.CreatedAt,
	}
}

func buildTunnelView(st *underlay.State, t *underlay.Tunnel, serials map[uint16]string, views map[uint16]underlay.NodeView) underlayTunnelView {
	v := underlayTunnelView{
		ID:           t.ID,
		A:            buildEndpointView(views, t.A),
		B:            buildEndpointView(views, t.B),
		MTU:          t.MTU,
		MTUIsDefault: t.MTU == 0,
		Roles:        t.Roles,
		BatchID:      t.BatchID,
		CreatedAt:    t.CreatedAt,
		UpdatedAt:    t.UpdatedAt,
		State:        "unknown",
	}
	if rp, has, err := st.PathFor(t.Path); has {
		if err != nil {
			v.PathError = err.Error()
		} else {
			v.Path = &underlayResolvedPathView{
				TemplateID: rp.TemplateID, TemplateName: rp.TemplateName,
				NodeID: rp.NodeID, Serial: serials[rp.NodeID], Addr: rp.Addr,
			}
		}
	}
	return v
}

func buildEndpointView(views map[uint16]underlay.NodeView, ep underlay.Endpoint) underlayEndpointView {
	v := views[ep.NodeID]
	return underlayEndpointView{
		NodeID: ep.NodeID, Serial: v.Name, Iface: ep.Iface,
		Addr: ep.Addr, Port: ep.Port, Overlay: ep.Overlay,
		MatchesObservation: v.Observed != "" && !v.ObservedStale && observedAddr(ep.Addr) == v.Observed,
	}
}

// underlayTopologyLocked renders the graph document from the same read model,
// so a page and the tunnel list can never disagree about which tunnels exist.
//
// Must be called with s.mu held.
func (s *server) underlayTopologyLocked(tenant string, now int64) underlayTopology {
	v := s.underlayViewLocked(tenant, now)
	out := underlayTopology{
		NowUnix:    now,
		Configured: v.Configured,
		StateNote:  v.StateNote,
		Nodes:      v.Nodes,
		Bundles:    []underlayTopoBundle{},
	}
	serials := make(map[uint16]string, len(v.Nodes))
	for _, n := range v.Nodes {
		serials[n.NodeID] = n.Serial
	}
	// Parallel tunnels between one pair are one edge with N details. The pair is
	// unordered: A->B and B->A are the same line on the page.
	index := map[[2]uint16]int{}
	for _, t := range v.Tunnels {
		a, b := t.A.NodeID, t.B.NodeID
		if a > b {
			a, b = b, a
		}
		k := [2]uint16{a, b}
		i, ok := index[k]
		if !ok {
			out.Bundles = append(out.Bundles, underlayTopoBundle{
				A: a, B: b, SerialA: serials[a], SerialB: serials[b],
				State: "unknown", Tunnels: []underlayTunnelView{},
			})
			i = len(out.Bundles) - 1
			index[k] = i
		}
		out.Bundles[i].Tunnels = append(out.Bundles[i].Tunnels, t)
		out.Bundles[i].Count = len(out.Bundles[i].Tunnels)
	}
	sort.Slice(out.Bundles, func(i, j int) bool {
		if out.Bundles[i].A != out.Bundles[j].A {
			return out.Bundles[i].A < out.Bundles[j].A
		}
		return out.Bundles[i].B < out.Bundles[j].B
	})
	return out
}

// ------------------------------------------------------------ auto batch

type underlaySkipView struct {
	A       uint16 `json:"a"`
	B       uint16 `json:"b"`
	SerialA string `json:"serial_a"`
	SerialB string `json:"serial_b"`
	Reason  string `json:"reason"`
}

type underlayAutoNodeCount struct {
	NodeID  uint16 `json:"node_id"`
	Serial  string `json:"serial"`
	Tunnels int    `json:"tunnels"`
}

// underlayAutoView is the confirmation document for a batch. It is built from
// the same allocation path that the apply runs, so the ports and /32s shown are
// the ones the apply will hand out.
type underlayAutoView struct {
	Mode      string `json:"mode"`
	Hub       uint16 `json:"hub,omitempty"`
	SpokeMode string `json:"spoke_mode,omitempty"`
	Pairs     int    `json:"pairs"`

	Add  []underlayTunnelView `json:"add"`
	Skip []underlaySkipView   `json:"skip"`

	// Usage is the CURRENT usage; OverlayAfter and PerNode say what the batch
	// would make it. Nothing here is applied unless the apply endpoint is called.
	Usage        underlay.Usage          `json:"usage"`
	OverlayAfter int                     `json:"overlay_after"`
	PerNode      []underlayAutoNodeCount `json:"per_node"`
}

// underlayAutoViewLocked renders a batch for confirmation (applied=false) or as
// the result of an apply (applied=true). The difference is only counting: after
// an apply the tunnels are already in the state, so adding plan.Add again would
// report double the tunnels and double the overlay consumption.
//
// Must be called with s.mu held.
func (s *server) underlayAutoViewLocked(tenant string, req underlay.AutoRequest, plan underlay.Plan, now int64, applied bool) underlayAutoView {
	st := s.effectiveUnderlayLocked(tenant)
	views := s.nodeViewsLocked(tenant, now)
	serials := serialMap(views)
	counts := map[uint16]int{}
	for _, t := range st.Tunnels {
		counts[t.A.NodeID]++
		counts[t.B.NodeID]++
	}

	out := underlayAutoView{
		Mode: string(req.Mode), Hub: req.Hub, SpokeMode: string(req.SpokeMode),
		Pairs:   plan.Pairs(),
		Add:     []underlayTunnelView{},
		Skip:    []underlaySkipView{},
		Usage:   st.Usage(),
		PerNode: []underlayAutoNodeCount{},
	}
	for _, t := range plan.Add {
		if !applied {
			counts[t.A.NodeID]++
			counts[t.B.NodeID]++
		}
		tv := buildTunnelView(st, &t, serials, views)
		// A relay template created by this batch is not in the stored state yet on
		// a preview, so the lookup above fails; the plan carries it for exactly
		// this reason.
		if tv.Path == nil && t.Path != nil && t.Path.TemplateID != "" {
			for _, tpl := range plan.Templates {
				if tpl.ID == t.Path.TemplateID {
					tv.Path = &underlayResolvedPathView{
						TemplateID: tpl.ID, TemplateName: tpl.Name,
						NodeID: tpl.NodeID, Serial: serials[tpl.NodeID], Addr: tpl.Addr,
					}
					tv.PathError = ""
					break
				}
			}
		}
		out.Add = append(out.Add, tv)
	}
	for _, sk := range plan.Skip {
		out.Skip = append(out.Skip, underlaySkipView{
			A: sk.A, B: sk.B, SerialA: serials[sk.A], SerialB: serials[sk.B], Reason: sk.Reason,
		})
	}
	out.OverlayAfter = out.Usage.OverlayUsed
	if !applied {
		out.OverlayAfter += 2 * len(plan.Add)
	}
	for _, n := range s.tenantNodesLocked(tenant) {
		out.PerNode = append(out.PerNode, underlayAutoNodeCount{
			NodeID: n.NodeID, Serial: serials[n.NodeID], Tunnels: counts[n.NodeID],
		})
	}
	return out
}
