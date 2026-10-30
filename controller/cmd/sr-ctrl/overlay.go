package main

// The overlay layer: 导通组 (route-target groups) plus the tenant-level escape
// switch. This file is the model and the arithmetic only -- the HTTP surface is
// overlay_api.go, and nothing here renders a bundle: turning these groups into
// the route.json a device installs is O6-b.
//
// The model is deliberately NOT "a VPN carries a list of RTs". A group is the
// unit the tenant edits (docs/overlay-design-draft.md §2.2): pick devices, give
// each one a VPN, and the controller allocates one RT for the whole group. The
// per-VPN import/export lists are *derived* at read time from "which groups is
// this (device, VPN) a member of". That is what makes removing a device from a
// group correct by construction -- the stale RT disappears because nothing
// references it any more, not because cleanup code remembered to take it back
// out of N devices.
//
// The RT itself is stored (it must be: it has to stay stable across edits and be
// unique inside the tenant), but it is never a field the tenant types. See
// 定案 6 in the draft: route targets are internal to this system.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"sdwan/controller/internal/route"
)

const (
	// overlayRTASN is the private ASN every allocated RT carries. Only the
	// value after the colon has to be unique inside a tenant, so one ASN for
	// everyone keeps the 排障 story simple.
	overlayRTASN = 65000
	// overlayRTFirst starts the counter above the range lab hand-configured RTs
	// live in (65000:56 and 65000:57 in test/scripts/test-boot.sh). Hand-written
	// values in that neighbourhood are the reason allocateRT also skips anything
	// already held.
	overlayRTFirst    = 1000
	overlayMaxGroups  = 256
	overlayMaxMembers = 64
)

// OverlayMember is one row of a 导通组: a device and one of its VPNs.
type OverlayMember struct {
	NodeID uint16 `json:"node_id"`
	VPNID  uint32 `json:"vpn_id"`
}

// OverlayGroup is one 互通域. Every member's VPN carries Group.RT in both its
// import and its export set, which is what makes the domain symmetric without a
// from/to pair of rules.
//
// RT is assigned at create and never changes: two live domains sharing one RT
// would import each other's routes, and re-minting one on edit would leave the
// routes already announced under the old value.
type OverlayGroup struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	RT      string          `json:"rt"`
	Members []OverlayMember `json:"members"`

	CreatedAt int64 `json:"created_at_unix"`
	UpdatedAt int64 `json:"updated_at_unix"`
}

// OverlayConfig is one tenant's overlay layer. Keyed by tenant id in State.
type OverlayConfig struct {
	TenantID string `json:"tenant_id"`

	// EscapeAllColors is the 逃生开关, tenant-level by 定案 (draft §11 拍板 #3).
	// It reaches sr-fib's own `escape_all_colors` field; O6-b renders it.
	EscapeAllColors bool           `json:"escape_all_colors"`
	Groups          []OverlayGroup `json:"groups"`

	// NextRT is the counter behind the allocated value; NextGroup mints ids.
	// Neither ever rewinds, so an RT is burned for good when its group is deleted
	// -- reusing a value a device may still hold routes under is worse than a gap.
	NextRT    int `json:"next_rt"`
	NextGroup int `json:"next_group"`

	UpdatedAt int64  `json:"updated_at_unix"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// OverlayVRF is the derived, per-device route-target view of one VPN. The JSON
// field names match cmd/sr-route's own vrfConfig on purpose: O6-b renders this
// into route.json without a translation layer in between, and a name that
// drifted would then fail loudly at the device instead of quietly here.
type OverlayVRF struct {
	ID        uint8    `json:"id"`
	ExportRTs []string `json:"export_rts"`
	ImportRTs []string `json:"import_rts"`
}

func (s *server) overlayConfigLocked(tenantID string, create bool) *OverlayConfig {
	if s.state.Overlays == nil {
		if !create {
			return nil
		}
		s.state.Overlays = map[string]*OverlayConfig{}
	}
	oc, ok := s.state.Overlays[tenantID]
	if !ok {
		if !create {
			return nil
		}
		oc = &OverlayConfig{TenantID: tenantID, NextRT: overlayRTFirst, Groups: []OverlayGroup{}}
		s.state.Overlays[tenantID] = oc
	}
	oc.ensure()
	return oc
}

// ensure makes a record loaded from an older state file usable: a missing
// counter would otherwise allocate "65000:0", and a nil group list is only
// distinct from an empty one in JSON.
func (o *OverlayConfig) ensure() {
	if o.Groups == nil {
		o.Groups = []OverlayGroup{}
	}
	if o.NextRT < overlayRTFirst {
		o.NextRT = overlayRTFirst
	}
	if o.NextGroup < 0 {
		o.NextGroup = 0
	}
	for i := range o.Groups {
		o.Groups[i].Name = strings.TrimSpace(o.Groups[i].Name)
		if o.Groups[i].Members == nil {
			o.Groups[i].Members = []OverlayMember{}
		}
	}
}

func (o *OverlayConfig) group(id string) *OverlayGroup {
	for i := range o.Groups {
		if o.Groups[i].ID == id {
			return &o.Groups[i]
		}
	}
	return nil
}

// usedRTs is what this tenant's groups already hold. The allocator consults it so
// two groups can never be given the same value even if the counter was edited by
// hand or a record was merged in from elsewhere.
func (o *OverlayConfig) usedRTs() map[string]bool {
	used := make(map[string]bool, len(o.Groups))
	for _, g := range o.Groups {
		if g.RT != "" {
			used[g.RT] = true
		}
	}
	return used
}

// allocateRT mints the next canonical "ASN:value" and burns counters until it
// finds one nobody holds. CanonicalRT is the wire's own gate: an RT that is not
// canonical would be rejected by sr-route after publish, which is the worst
// possible time to discover it.
func (o *OverlayConfig) allocateRT() (string, error) {
	used := o.usedRTs()
	for i := 0; i < 100000; i++ {
		n := o.NextRT
		o.NextRT++
		rt, err := route.CanonicalRT(fmt.Sprintf("%d:%d", overlayRTASN, n))
		if err != nil {
			return "", err
		}
		if used[rt] {
			continue
		}
		return rt, nil
	}
	return "", errors.New("导通组编号已用尽")
}

// nextGroupID mints an id nobody holds. Ids are stable strings the page and the
// audit trail quote, so they must not be reused either.
func (o *OverlayConfig) nextGroupID() string {
	for {
		o.NextGroup++
		id := fmt.Sprintf("og%d", o.NextGroup)
		if o.group(id) == nil {
			return id
		}
	}
}

// memberGroupCount is how many groups hold this (device, VPN) -- which, because
// one group contributes exactly one distinct RT, is also the size of the RT set
// that VPN derives. `exclude` is the group being edited so it does not count
// itself twice.
func (o *OverlayConfig) memberGroupCount(nodeID uint16, vpnID uint32, exclude string) int {
	n := 0
	for _, g := range o.Groups {
		if g.ID == exclude {
			continue
		}
		for _, m := range g.Members {
			if m.NodeID == nodeID && m.VPNID == vpnID {
				n++
				break
			}
		}
	}
	return n
}

// normalizeGroup trims and sorts so a save is stable: the state file and the
// console must not show a reordered member list as a change.
func normalizeGroup(g *OverlayGroup) {
	g.Name = strings.TrimSpace(g.Name)
	members := append([]OverlayMember(nil), g.Members...)
	sort.Slice(members, func(i, j int) bool {
		if members[i].NodeID != members[j].NodeID {
			return members[i].NodeID < members[j].NodeID
		}
		return members[i].VPNID < members[j].VPNID
	})
	g.Members = members
}

// overlayVRFsLocked derives one device's per-VPN route targets. Only VPNs that
// are actually members appear: sr-route's VRF table is a full replacement, and a
// VPN with no RTs cannot announce anything either way (route.SkipNoExportRTs), so
// listing only the members is the same table with less noise.
func (s *server) overlayVRFsLocked(tenantID string, nodeID uint16) []OverlayVRF {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil || len(oc.Groups) == 0 {
		return nil
	}
	byVPN := map[uint32]*OverlayVRF{}
	for _, g := range oc.Groups {
		if g.RT == "" {
			continue
		}
		for _, m := range g.Members {
			if m.NodeID != nodeID {
				continue
			}
			// A VPN above the wire's VRF id width cannot be expressed; the checks
			// say so, and the derivation refuses to silently truncate it.
			if m.VPNID > uint32(route.MaxVRFID) {
				continue
			}
			v, ok := byVPN[m.VPNID]
			if !ok {
				v = &OverlayVRF{ID: uint8(m.VPNID)}
				byVPN[m.VPNID] = v
			}
			if !containsString(v.ExportRTs, g.RT) {
				v.ExportRTs = append(v.ExportRTs, g.RT)
			}
			if !containsString(v.ImportRTs, g.RT) {
				v.ImportRTs = append(v.ImportRTs, g.RT)
			}
		}
	}
	ids := make([]uint32, 0, len(byVPN))
	for id := range byVPN {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]OverlayVRF, 0, len(ids))
	for _, id := range ids {
		v := byVPN[id]
		sort.Strings(v.ExportRTs)
		sort.Strings(v.ImportRTs)
		if v.ExportRTs == nil {
			v.ExportRTs = []string{}
		}
		if v.ImportRTs == nil {
			v.ImportRTs = []string{}
		}
		out = append(out, *v)
	}
	return out
}

func containsString(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

// overlayGroupChecksLocked validates one candidate group against what exists.
// It runs on create and on edit, before anything is saved: a member whose VPN
// does not exist on that box would render into a route.json the device rejects,
// and that failure is far harder to read there than here.
//
// `exclude` is the id being edited, so a group does not collide with itself.
func (s *server) overlayGroupChecksLocked(tenantID string, g OverlayGroup, exclude string) []ConfigCheck {
	var out []ConfigCheck
	add := func(sev, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: sev, Text: fmt.Sprintf(format, args...)})
	}

	if g.Name == "" {
		add("error", "导通组要有名字：发布记录和设备上的注释都要能指出这是哪一组。")
	}
	if len(g.Members) < 2 {
		add("error", "导通组至少要有两台设备（当前 %d）：只有一台成员时没有任何东西被导通。", len(g.Members))
	}
	if len(g.Members) > overlayMaxMembers {
		add("error", "导通组最多 %d 行成员，当前 %d。", overlayMaxMembers, len(g.Members))
	}

	seen := map[OverlayMember]bool{}
	for _, m := range g.Members {
		if seen[m] {
			add("error", "成员重复：设备 %d 的 VPN %d 出现了两次。", m.NodeID, m.VPNID)
			continue
		}
		seen[m] = true

		if m.VPNID > uint32(route.MaxVRFID) {
			add("error", "设备 %d 的 VPN %d 超出范围（0–%d）。", m.NodeID, m.VPNID, route.MaxVRFID)
			continue
		}
		if m.VPNID == vpnReservedOverlay {
			add("error", "VPN %d 是 overlay 保留表（隧道与对外下一跳用的那张），不能进导通组。", m.VPNID)
			continue
		}
		if s.state.Nodes[key(tenantID, m.NodeID)] == nil {
			add("error", "设备 %d 不属于这个租户，或者还没入网。", m.NodeID)
			continue
		}
		cfg := s.state.Configs[configKey(tenantID, m.NodeID)]
		if !configHasVPN(cfg, m.VPNID) {
			add("error", "设备 %d 的基础配置里没有 VPN %d：先在「配置 › 基础配置」加这个 VPN。",
				m.NodeID, m.VPNID)
			continue
		}
		// One group adds one RT to this VPN's set; the wire caps the set.
		if n := s.overlayGroupCountLocked(tenantID, m.NodeID, m.VPNID, exclude) + 1; n > route.MaxRTsPerRoute {
			add("error", "设备 %d 的 VPN %d 会同时在 %d 个导通组里，超过 %d：一条路由最多带 %d 个 RT，"+
				"再多 RR 会直接拒收这条 announce。", m.NodeID, m.VPNID, n,
				route.MaxRTsPerRoute, route.MaxRTsPerRoute)
		}
	}
	return out
}

// overlayGroupCountLocked is memberGroupCount for the tenant's stored record.
func (s *server) overlayGroupCountLocked(tenantID string, nodeID uint16, vpnID uint32, exclude string) int {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil {
		return 0
	}
	return oc.memberGroupCount(nodeID, vpnID, exclude)
}

// configHasVPN reports whether the device's basic config defines this VPN. A
// device with no draft at all still has VPN 0 (defaultBasicConfig), and that is
// the only VPN reachable without a config row -- so the nil case is written down
// rather than left to a zero-value accident.
func configHasVPN(c *DeviceConfig, vpnID uint32) bool {
	if c == nil {
		return vpnID == 0
	}
	for _, v := range c.Draft.VPNs {
		if v.ID == vpnID {
			return true
		}
	}
	return false
}

// overlayTunnelChecksLocked warns about members of a group that have no tunnel
// between them. It is a WARNING, not an error: tunnels are built by hand in a
// different page, and "配导通 → 再建隧道" is a perfectly normal order of work.
// Blocking the save would force the tenant to build tunnels for pairs that may
// never need to talk.
//
// Why it is worth saying at all: the reflector only reflects along bidirectional
// tunnel adjacency and does not look at RTs at all (draft §8.4, measured in
// O5-c), so a group whose members are not pairwise tunnelled simply never
// converges -- and the routes go missing with no error anywhere.
func (s *server) overlayTunnelChecksLocked(tenantID string, g OverlayGroup) []ConfigCheck {
	nodes := map[uint16]bool{}
	for _, m := range g.Members {
		nodes[m.NodeID] = true
	}
	pairs := map[[2]uint16]bool{}
	// A tenant with no underlay record has no tunnels either, so the pair set stays
	// empty and every pair is named -- which is the truth, not a missing check.
	if uc := s.underlayConfigLocked(tenantID, false); uc != nil && uc.State != nil {
		for _, t := range uc.State.Tunnels {
			if t == nil {
				continue
			}
			pairs[pairKey(t.A.NodeID, t.B.NodeID)] = true
		}
	}

	ids := make([]uint16, 0, len(nodes))
	for n := range nodes {
		ids = append(ids, n)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var out []ConfigCheck
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			if !pairs[pairKey(a, b)] {
				out = append(out, ConfigCheck{
					Severity: "warning",
					Text: fmt.Sprintf("设备 %d 与 %d 之间没有隧道：这一对成员的路由不会导通"+
						"（反射器只按双向隧道邻接反射，不看 RT）。在「Underlay 配置 › 隧道配置」补一条。", a, b),
				})
			}
		}
	}
	return out
}

func pairKey(a, b uint16) [2]uint16 {
	if a > b {
		a, b = b, a
	}
	return [2]uint16{a, b}
}

// overlayVPNRow is one VPN of one device, shown with everything the page needs
// to both offer it as a member and explain what the groups currently do to it.
// It is the picker and the audit row in one place, so the page cannot disagree
// with the controller about which VPNs can be put in a group.
type overlayVPNRow struct {
	ID         uint32   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Selectable bool     `json:"selectable"`
	Note       string   `json:"note,omitempty"`
	ExportRTs  []string `json:"export_rts"`
	ImportRTs  []string `json:"import_rts"`
	Groups     []string `json:"groups"`
}

// overlayNodeRow is one enrolled device and its VPNs.
type overlayNodeRow struct {
	NodeID uint16          `json:"node_id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	VPNs   []overlayVPNRow `json:"vrfs"`
}

// overlayView is what GET /api/overlay returns. The per-device expansion is
// included because the page must be able to show "this group wrote these lines"
// without inventing its own arithmetic -- the derivation lives here, once.
type overlayView struct {
	TenantID        string           `json:"tenant_id"`
	NowUnix         int64            `json:"now_unix"`
	EscapeAllColors bool             `json:"escape_all_colors"`
	Groups          []OverlayGroup   `json:"groups"`
	Checks          []ConfigCheck    `json:"checks"`
	Nodes           []overlayNodeRow `json:"nodes"`
	MaxMembers      int              `json:"max_members"`
	MaxGroups       int              `json:"max_groups"`
	// MaxRTsPerVPN is the number the create gate above stops at; the page shows it
	// instead of letting a tenth group fail with a sentence it cannot explain.
	MaxRTsPerVPN int `json:"max_rts_per_vpn"`
	// RTNote states who owns the RT values, because the page must not make them
	// editable and a reader is going to ask why not.
	RTNote string `json:"rt_note"`
}

func (s *server) overlayViewLocked(tenantID string, now int64) overlayView {
	oc := s.overlayConfigLocked(tenantID, true)
	view := overlayView{
		TenantID:        tenantID,
		NowUnix:         now,
		EscapeAllColors: oc.EscapeAllColors,
		Groups:          append([]OverlayGroup(nil), oc.Groups...),
		MaxMembers:      overlayMaxMembers,
		MaxGroups:       overlayMaxGroups,
		MaxRTsPerVPN:    route.MaxRTsPerRoute,
		RTNote: fmt.Sprintf("RT 由控制器分配并在租户内唯一（%d:1000 起），"+
			"页面上不可编辑；设备重启后靠本地那一份配置工作，不依赖控制器在线。", overlayRTASN),
	}
	if view.Groups == nil {
		view.Groups = []OverlayGroup{}
	}
	for i := range view.Groups {
		if view.Groups[i].Members == nil {
			view.Groups[i].Members = []OverlayMember{}
		}
	}

	// Every stored group is re-checked on read: state can be edited outside the
	// page (an older file, a device that has since been removed), and a problem
	// that exists is reported whether or not anyone is about to save.
	for _, g := range oc.Groups {
		view.Checks = append(view.Checks, s.overlayGroupChecksLocked(tenantID, g, g.ID)...)
		view.Checks = append(view.Checks, s.overlayTunnelChecksLocked(tenantID, g)...)
	}
	if view.Checks == nil {
		view.Checks = []ConfigCheck{}
	}

	views := s.nodeViewsLocked(tenantID, now)
	for _, n := range s.tenantNodesLocked(tenantID) {
		row := overlayNodeRow{NodeID: n.NodeID, Status: nodeStatus(n), VPNs: []overlayVPNRow{}}
		if v, ok := views[n.NodeID]; ok {
			row.Name = v.Name
		}
		derived := map[uint32]OverlayVRF{}
		for _, d := range s.overlayVRFsLocked(tenantID, n.NodeID) {
			derived[uint32(d.ID)] = d
		}
		cfg := s.state.Configs[configKey(tenantID, n.NodeID)]
		vpns := []ConfigVPN{}
		if cfg != nil {
			vpns = append(vpns, cfg.Draft.VPNs...)
		} else {
			vpns = append(vpns, defaultBasicConfig().VPNs...)
		}
		sort.Slice(vpns, func(i, j int) bool { return vpns[i].ID < vpns[j].ID })
		for _, vp := range vpns {
			r := overlayVPNRow{
				ID: vp.ID, Name: vp.Name,
				ExportRTs: []string{}, ImportRTs: []string{}, Groups: []string{},
			}
			switch {
			case vp.ID > uint32(route.MaxVRFID):
				r.Note = fmt.Sprintf("VPN 号超过 %d，路由平面的 VRF id 是 8 位，这张表导通不了。", route.MaxVRFID)
			case vp.ID == vpnReservedOverlay:
				r.Note = "overlay 保留表（隧道与对外下一跳），不参与导通。"
			default:
				r.Selectable = true
			}
			for _, g := range oc.Groups {
				for _, m := range g.Members {
					if m.NodeID == n.NodeID && m.VPNID == vp.ID {
						r.Groups = append(r.Groups, g.ID)
					}
				}
			}
			if d, ok := derived[vp.ID]; ok {
				r.ExportRTs = append(r.ExportRTs, d.ExportRTs...)
				r.ImportRTs = append(r.ImportRTs, d.ImportRTs...)
			}
			row.VPNs = append(row.VPNs, r)
		}
		view.Nodes = append(view.Nodes, row)
	}
	if view.Nodes == nil {
		view.Nodes = []overlayNodeRow{}
	}
	return view
}
