package main

// The overlay layer: 导通组 (route-target groups), the tenant's two reflector
// slots and the tenant-level escape switch. This file is the model and the
// arithmetic; the HTTP surface is overlay_api.go, and the bundle's 4th artifact
// (route.json's tunnels+vrfs, plus rrs once the tenant has picked) is rendered
// here by overlayRouteArtifactLocked.
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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"sdwan/controller/internal/route"
	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
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

	// RRPrimary and RRBackup are the tenant's two 反射器 slots. They hold ids of
	// rows in the operator's library (State.RRs), never addresses: the operator
	// owns what a reflector's address is, the tenant only picks. Empty means "the
	// controller says nothing about rrs", which is what every box enrolled before
	// this page relied on -- it keeps the list the operator hand-wrote into
	// route.json. The two names carry no priority: the route client keeps a
	// session up to every entry it is handed, so two rows are two equal active
	// reflectors (docs/o6c-overlay-page-plan.md 拍板 E0).
	RRPrimary string `json:"rr_primary,omitempty"`
	RRBackup  string `json:"rr_backup,omitempty"`

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
	o.RRPrimary = strings.TrimSpace(o.RRPrimary)
	o.RRBackup = strings.TrimSpace(o.RRBackup)
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

	// The two reflector slots and the rows this tenant may pick from them. The
	// library is filtered here, not in the page: a tenant must never be able to
	// name (or even learn the existence of) another tenant's exclusive RR.
	// The field names are 主/备-shaped because that is what is already on disk;
	// every string the page shows calls them RR 1 and RR 2 (拍板 E0).
	RRPrimary string      `json:"rr_primary"`
	RRBackup  string      `json:"rr_backup"`
	RRLibrary []rrPickRow `json:"rr_library"`
	// RRNote says what the two slots do and do not do on the device, because two
	// labelled boxes promise a failover order this plane does not implement: the
	// route client keeps a session up to every entry it is handed.
	RRNote string `json:"rr_note"`
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
		RRPrimary: oc.RRPrimary,
		RRBackup:  oc.RRBackup,
		RRLibrary: s.rrPickRowsLocked(tenantID),
		RRNote: "这两格（RR 1、RR 2）只是这一页上的两个格子，设备不排序：路由客户端对拿到的每一条 RR 都保持会话，" +
			"两条的宣告并进同一张路由表（同一条前缀在两台上是同一份路由的两份拷贝），所以两条就是两条等价活动反射器，" +
			"掉一条表不缩。控制器不会去连库里这些地址，所以这里没有绿灯可给——" +
			"能不能连上，只有设备自己的路由状态知道。发布后设备就地换：先把新 RR 的会话建起来，" +
			"等新 RR 真的收全了本机的前缀，才摘掉被它顶替的那条，路由进程不重启；谁还没接管、谁还替它守着，" +
			"看设备的路由状态。只填一条时换地址必然有一段没有反射器的窗口——没有冗余是这一格本身的形态，不是故障。",
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
	view.Checks = append(view.Checks, s.overlayRRChecksLocked(tenantID)...)
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

// overlayRouteTunnel is one row of the tunnel table sr-route installs and
// announces: the peer device and that peer's /32 on the tunnel. The field names
// are sr-route's own (cmd/sr-route/main.go:54-57), which is also the shape of
// PUT /tunnels -- so one render serves the installed file and the hot update, and
// neither side needs a second encoding.
type overlayRouteTunnel struct {
	Peer    uint16 `json:"peer_device_id"`
	NextHop string `json:"next_hop"`
}

// overlayRouteDoc is bundle 第 4 段. **Which keys are present is the
// instruction**: sr-apply overlays exactly the keys that appear in it and leaves
// every other key of the box's route.json alone. `vrfs` is therefore always
// there (no 导通组 means no RTs, and that is the truth), while `tunnels` is
// omitted when the tenant has never published a global underlay config -- an
// empty list from a controller that knows nothing about this box's tunnels would
// withdraw all of them.
//
// `rrs` follows the tunnels rule, not the vrfs one, and for the same reason:
// sr-route refuses to *start* on an empty rrs list (cmd/sr-route/main.go:91), so
// a tenant that has not picked a reflector must send no key at all and leave the
// list the operator hand-wrote in place. omitempty is what makes "not selected"
// and "selected, empty" different on the wire -- and only the second one would
// be a lie.
type overlayRouteDoc struct {
	Tunnels []overlayRouteTunnel `json:"tunnels,omitempty"`
	VRFs    []OverlayVRF         `json:"vrfs"`
	RRs     []wire.RRAddress     `json:"rrs,omitempty"`
}

// overlayRouteArtifactLocked renders one device's 第 4 段. Empty arrays are
// rendered as arrays, never as null: sr-route treats an absent tunnels key as a
// withdraw, and a device reading "null" cannot tell that apart from a bug.
func (s *server) overlayRouteArtifactLocked(tenantID string, nodeID uint16) string {
	vrfs := s.overlayVRFsLocked(tenantID, nodeID)
	if vrfs == nil {
		vrfs = []OverlayVRF{}
	}
	doc := overlayRouteDoc{VRFs: vrfs}
	if uc := s.underlayConfigLocked(tenantID, false); uc != nil && uc.State != nil && uc.State.Configured() {
		tunnels := []overlayRouteTunnel{}
		for _, t := range uc.State.SortedTunnels() {
			peer, ok := overlayPeerOf(t, nodeID)
			if !ok {
				continue
			}
			tunnels = append(tunnels, overlayRouteTunnel{Peer: peer.NodeID, NextHop: peer.Overlay})
		}
		doc.Tunnels = tunnels
	}
	// A tenant that has not picked, or one whose pick no longer resolves, renders
	// no key at all -- overlayRRChecksLocked says why out loud, and the device
	// keeps the rrs the operator wrote by hand until something real arrives.
	if rrs := s.rrForTenantLocked(tenantID); len(rrs) > 0 {
		doc.RRs = rrs
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// Unreachable: both fields are plain JSON types. Returning "" rather than
		// panicking keeps a render failure a *missing* artifact, which sr-apply
		// reports as "第 4 段为空" instead of installing half a file.
		return ""
	}
	return string(out) + "\n"
}

// overlayPeerOf returns the far end of a tunnel as seen from nodeID.
func overlayPeerOf(t *underlay.Tunnel, nodeID uint16) (underlay.Endpoint, bool) {
	if t == nil {
		return underlay.Endpoint{}, false
	}
	switch nodeID {
	case t.A.NodeID:
		return t.B, true
	case t.B.NodeID:
		return t.A, true
	}
	return underlay.Endpoint{}, false
}

// overlayNodeChecksLocked is the publish-time half of gate ①. Saving a group is
// checked against the whole tenant, but a device is published one at a time and
// the base config can change afterwards -- deleting a VPN from this box while a
// group still points at it must stop the publish, because sr-route would be
// handed a VRF it cannot create and the failure would surface on the box.
func (s *server) overlayNodeChecksLocked(tenantID string, nodeID uint16) []ConfigCheck {
	dc := s.state.Configs[configKey(tenantID, nodeID)]
	var out []ConfigCheck
	for _, v := range s.overlayVRFsLocked(tenantID, nodeID) {
		if configHasVPN(dc, uint32(v.ID)) {
			continue
		}
		var names []string
		if oc := s.overlayConfigLocked(tenantID, false); oc != nil {
			for _, g := range oc.Groups {
				for _, m := range g.Members {
					if m.NodeID == nodeID && m.VPNID == uint32(v.ID) {
						names = append(names, g.ID)
					}
				}
			}
		}
		out = append(out, ConfigCheck{
			Severity: "error",
			Text: fmt.Sprintf("这台设备的 VPN %d 要带 RT %s，但它的基础配置里没有 VPN %d："+
				"先在「配置 › 基础配置」加这个 VPN，或者把它从导通组 %s 里摘掉。",
				v.ID, strings.Join(v.ExportRTs, "/"), v.ID, strings.Join(names, "、")),
		})
	}
	return out
}
