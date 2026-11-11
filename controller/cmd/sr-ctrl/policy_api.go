package main

// 静态策略页的读写面：GET/PUT /api/policy。租户专属 —— 一张策略表说的是
// "哪个客户的哪类流走哪条线"，运营角色没有理由看到。
//
// 形状和「基础配置」一致：按设备读、按设备写，页面自己管设备选择。不像泄露那两页
// 做成租户级的总表，是因为一行策略的入口口是从**这台设备**的客户口派生的：脱离
// 设备就没有一行是完整的。
//
// 保存只写草稿，不发布。发布走 /api/config/publish 那条既有路径（设备下一次
// 心跳自己拉），所以这一页没有第二个发布实现。

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

// queryNodeID reads ?node=N. Every per-device endpoint accepts the same handle,
// so a page that already knows which device it is editing never has to re-derive
// it from the path.
func queryNodeID(w http.ResponseWriter, r *http.Request) (uint16, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get("node")), 10, 16)
	if err != nil || n == 0 {
		http.Error(w, "node must be 1..65535", http.StatusBadRequest)
		return 0, false
	}
	return uint16(n), true
}

// policyRowView 是页面上的一行：存下来的匹配条件 + 派生出来的入口口 + 一句人话。
type policyRowView struct {
	ID      int      `json:"id"`
	VPN     uint32   `json:"vpn"`
	Color   uint8    `json:"color"`
	Src     string   `json:"src,omitempty"`
	Dst     string   `json:"dst,omitempty"`
	Proto   uint8    `json:"proto"`
	SrcPort string   `json:"src_port,omitempty"`
	DstPort string   `json:"dst_port,omitempty"`
	Attach  []string `json:"attach"`
	Match   string   `json:"match"`

	// 动态行专属。静态行这些键是零值，页面按 Mode 分支渲染。
	Mode          string     `json:"mode,omitempty"`
	Peer          uint16     `json:"peer,omitempty"`
	PeerName      string     `json:"peer_name,omitempty"`
	Colors        []uint8    `json:"colors,omitempty"`
	SLA           *PolicySLA `json:"sla,omitempty"`
	Preempt       bool       `json:"preempt,omitempty"`
	SelectorTable uint32     `json:"selector_table,omitempty"`
	// CurrentColor 是盒子上的选路组件**现在**给这一行选的颜色（-1 = 还没有
	// 上报/这一行是静态的）。它来自监控回报，不是草稿的一部分。
	CurrentColor int8 `json:"current_color,omitempty"`
}

// policyPeerRow 是动态行的对端下拉框：这台设备到每台对端各有哪些颜色。
type policyPeerRow struct {
	NodeID uint16  `json:"node_id"`
	Name   string  `json:"name,omitempty"`
	Colors []uint8 `json:"colors"`
}

// policyVPNRow 是 VPN 选择框的一项。IfaceCount 为 0 的 VPN 在页面上要标出来：
// 选它一定是一个"策略没有入口"的错误。
type policyVPNRow struct {
	ID         uint32   `json:"id"`
	Name       string   `json:"name,omitempty"`
	IfaceCount int      `json:"iface_count"`
	Ifaces     []string `json:"ifaces"`
}

// policyColorRow 是一种颜色的现状：这台设备上有几条隧道配了这个颜色。
// 隧道是全局的（不按 VPN 分），所以这个数字对这台设备的每个 VPN 都一样。
type policyColorRow struct {
	Value   uint8 `json:"value"`
	Tunnels int   `json:"tunnels"`
}

type policyFallbackRow struct {
	VPN   uint32 `json:"vpn"`
	Color uint8  `json:"color"`
}

type policyProtoOption struct {
	Value uint8  `json:"value"`
	Label string `json:"label"`
	Ports bool   `json:"ports"`
}

type policyView struct {
	TenantID string `json:"tenant_id"`
	NowUnix  int64  `json:"now_unix"`

	NodeID           uint16            `json:"node_id"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	HasDraft         bool              `json:"has_draft"`
	Version          int               `json:"version"`
	PublishedVersion int               `json:"published_version,omitempty"`
	PublishedAt      int64             `json:"published_at_unix,omitempty"`
	Pending          bool              `json:"pending"`
	Device           *wire.ConfigState `json:"device_state,omitempty"`

	Rows     []policyRowView     `json:"rows"`
	Fallback []policyFallbackRow `json:"fallback"`
	VPNs     []policyVPNRow      `json:"vpns"`
	Colors   []policyColorRow    `json:"colors"`
	Peers    []policyPeerRow     `json:"peers"`

	Protocols []policyProtoOption `json:"protocols"`
	MaxRows   int                 `json:"max_rows"`
	MaxColor  uint8               `json:"max_color"`

	Checks   []ConfigCheck     `json:"checks"`
	Summary  string            `json:"summary"`
	Artifact string            `json:"artifact"`
	Notes    map[string]string `json:"notes"`
}

func policyProtoOptions() []policyProtoOption {
	return []policyProtoOption{
		{Value: policyProtoAny, Label: "任意", Ports: false},
		{Value: policyProtoTCP, Label: "TCP", Ports: true},
		{Value: policyProtoUDP, Label: "UDP", Ports: true},
		{Value: policyProtoICMP, Label: "ICMP", Ports: false},
	}
}

// policyRowMatchText 把一行说成人话，页面上直接显示 —— 免得用户对着
// "src/dst/proto/sport/dport" 五个空字段猜"这一行到底匹配什么"。
func policyRowMatchText(r PolicyRow) string {
	any := func(s string) bool { return s == "" || s == "any" }
	src, dst := "任意", "任意"
	if !any(r.Src) {
		src = r.Src
	}
	if !any(r.Dst) {
		dst = r.Dst
	}
	out := src + " → " + dst + " · " + policyProtoName(r.Proto)
	if r.Proto == policyProtoTCP || r.Proto == policyProtoUDP {
		sp, dp := "任意", "任意"
		if !any(r.SrcPort) {
			sp = r.SrcPort
		}
		if !any(r.DstPort) {
			dp = r.DstPort
		}
		out += " · 源端口 " + sp + " · 目的端口 " + dp
	}
	if r.Mode == policyModeDynamic {
		out += " · 动态 → 对端 #" + strconv.FormatUint(uint64(r.Peer), 10)
		if len(r.Colors) > 0 {
			out += " · 候选 " + policyColorList(r.Colors)
		}
		if s := r.SLA; s != nil && *s != (PolicySLA{}) {
			var parts []string
			if s.LatencyMS > 0 {
				parts = append(parts, fmt.Sprintf("延迟≤%gms", s.LatencyMS))
			}
			if s.JitterMS > 0 {
				parts = append(parts, fmt.Sprintf("抖动≤%gms", s.JitterMS))
			}
			if s.LossPermille > 0 {
				parts = append(parts, fmt.Sprintf("丢包≤%d‰", s.LossPermille))
			}
			if len(parts) > 0 {
				out += " · " + strings.Join(parts, "，")
			}
		} else {
			out += " · 不设门槛"
		}
	}
	return out
}

// policyViewLocked 拼出这一页要的全部东西。没有草稿（v0）的设备也照样能打开：
// 那是一个空策略集 + 一句"先去保存基础配置"，不是一个 404。
//
// Must be called with s.mu held.
func (s *server) policyViewLocked(tenantID string, nodeID uint16, now int64) (policyView, bool) {
	n := s.state.Nodes[key(tenantID, nodeID)]
	if n == nil || n.TenantID != tenantID {
		return policyView{}, false
	}
	dc := s.state.Configs[configKey(tenantID, nodeID)]
	view := policyView{
		TenantID:  tenantID,
		NowUnix:   now,
		NodeID:    nodeID,
		Name:      n.Serial,
		Status:    nodeStatus(n),
		Device:    n.Config,
		Rows:      []policyRowView{},
		Fallback:  []policyFallbackRow{},
		VPNs:      []policyVPNRow{},
		Colors:    []policyColorRow{},
		Peers:     []policyPeerRow{},
		Protocols: policyProtoOptions(),
		MaxRows:   policyMaxRows,
		MaxColor:  underlay.MaxTunnelColor,
		Checks:    []ConfigCheck{},
		Notes: map[string]string{
			"rows": "一行 = 一类流走哪个颜色。空着的字段表示任意；行号就是匹配顺序，" +
				"前面的行先命中，所以更宽的行要放在上面。",
			"attach": "入口口不用选：一行策略挂到它所属 VPN 的**全部**客户口上。" +
				"这个 VPN 一个客户口都没有时，这一行会被拒。",
			"fallback": "兜底是每 VRF 每颜色一个开关，不是每行一个：勾上 = 这个颜色的表里" +
				"加一条默认路由指向颜色 0（本 VPN 主表），该颜色查不到时退回去而不是丢掉。",
			"color": "颜色决定走哪张 VPP 表（表号 = 颜色×1000 + VPN）。表里的路径由 sr-fib " +
				"按隧道状态自己收敛，所以链路抖动不用改策略。",
			"dynamic": "动态行不钉颜色：盒子盯着到指定对端的每条颜色隧道的探针指标" +
				"（延迟/抖动/丢包），满足门槛的里面挑延迟最低的一条。全部不达标时走" +
				"最接近的一条，不会因为一时抖动断流。默认粘住：当前隧道还达标就一直用，" +
				"更优的不抢；勾上\"抢占\"才永远追最优。\"当前颜色\"是盒子上报的实时选择。",
		},
	}

	cfg := defaultBasicConfig()
	var set PolicySet
	if dc != nil {
		cfg = dc.Draft
		set = policySetCopy(dc.Policy)
		view.Version = dc.Version
		view.HasDraft = dc.Version > 0
		view.PublishedVersion = 0
		if dc.Published != nil {
			view.PublishedVersion = dc.Published.Version
			view.PublishedAt = dc.Published.PublishedAt
			art := s.artifactsLocked(tenantID, nodeID, &dc.Draft, dc.Leak, dc.Policy)
			view.Pending = dc.Published.Version != dc.Version || dc.Published.Artifacts != art
		}
	}
	normalizeBasicConfig(&cfg)
	normalizePolicySet(&set)

	// VPN 选择框从这台设备的草稿来：一个盒子上没有的 VPN 不该能被选中。
	vpns := append([]ConfigVPN(nil), cfg.VPNs...)
	sort.Slice(vpns, func(i, j int) bool { return vpns[i].ID < vpns[j].ID })
	for _, vp := range vpns {
		names := policyAttachIfaces(&cfg, vp.ID)
		view.VPNs = append(view.VPNs, policyVPNRow{
			ID: vp.ID, Name: vp.Name, IfaceCount: len(names), Ifaces: names,
		})
	}

	counts, hasUnderlay := s.nodeTunnelColorsLocked(tenantID, nodeID)
	for c := uint8(0); c <= underlay.MaxTunnelColor; c++ {
		row := policyColorRow{Value: c}
		if hasUnderlay {
			row.Tunnels = counts[c]
		}
		view.Colors = append(view.Colors, row)
	}

	// 对端下拉框：动态行要选"SLA 量到哪台对端"。没有隧道拓扑的设备给空列表，
	// 页面把动态模式禁用掉并说明原因 —— 和 VPN 下拉框同一个形状。
	peerColors, hasPeers := s.nodeTunnelPeersLocked(tenantID, nodeID)
	if hasPeers {
		for peer, colors := range peerColors {
			row := policyPeerRow{NodeID: peer, Colors: colors}
			if n := s.state.Nodes[key(tenantID, peer)]; n != nil {
				row.Name = n.Serial
			}
			view.Peers = append(view.Peers, row)
		}
		sort.Slice(view.Peers, func(i, j int) bool { return view.Peers[i].NodeID < view.Peers[j].NodeID })
	}

	// 当前选路来自监控回报（agent 的 sla.Selector 随遥测一起报）。没有回报就
	// 是"未上报"，CurrentColor 保持 0 且靠 omitempty 缺省。
	var currentColor map[int]wire.PolicySelection
	if t := s.tel[key(tenantID, nodeID)]; t != nil && t.sel != nil {
		currentColor = make(map[int]wire.PolicySelection, len(t.sel))
		for _, sel := range t.sel {
			currentColor[sel.RowID] = sel
		}
	}

	for _, r := range set.Rows {
		row := policyRowView{
			ID: r.ID, VPN: r.VPN, Color: r.Color,
			Src: r.Src, Dst: r.Dst, Proto: r.Proto,
			SrcPort: r.SrcPort, DstPort: r.DstPort,
			Attach: policyAttachIfaces(&cfg, r.VPN),
			Match:  policyRowMatchText(r),
		}
		if r.Mode == policyModeDynamic {
			row.Mode = r.Mode
			row.Peer = r.Peer
			if n := s.state.Nodes[key(tenantID, r.Peer)]; n != nil {
				row.PeerName = n.Serial
			}
			row.Colors = r.Colors
			row.SLA = r.SLA
			row.Preempt = r.Preempt
			if st, err := fib.SelectorTable(r.ID); err == nil {
				row.SelectorTable = st
			}
		}
		if sel, ok := currentColor[r.ID]; ok {
			row.CurrentColor = int8(sel.Color)
			if row.Match != "" {
				row.Match += " · 当前颜色 " + strconv.Itoa(int(sel.Color))
			}
		}
		if r.Preempt {
			row.Match += " · 抢占"
		}
		view.Rows = append(view.Rows, row)
	}
	for _, f := range set.Fallback {
		view.Fallback = append(view.Fallback, policyFallbackRow{VPN: f.VPN, Color: f.Color})
	}

	view.Summary = policyEmptyCheckText(set)
	view.Artifact = renderPolicyArtifact(&cfg, set)
	if view.HasDraft {
		view.Checks = s.policyChecksLocked(tenantID, nodeID, dc, set)
		// 一条都不报的时候也要是 [] 而不是 null：页面把它当列表渲染，null 会让
		// "没有校验结果"和"校验没跑"看起来一样。
		if view.Checks == nil {
			view.Checks = []ConfigCheck{}
		}
	} else {
		view.Checks = []ConfigCheck{{
			Severity: "warning",
			Text:     "这台设备还没有保存过基础配置草稿（v0）：一行策略都还没有入口口可以挂。先去「配置 › 基础配置」保存一次。",
		}}
	}
	return view, true
}

type policySaveRequest struct {
	NodeID   uint16           `json:"node_id"`
	Rows     []PolicyRow      `json:"rows"`
	Fallback []PolicyFallback `json:"fallback"`
}

// handleAdminPolicy 是这一页唯一的写入口。整份替换，不是逐行：行号即匹配顺序，
// 逐行改会把"顺序"这件事拆到多次请求里，中途失败就留下一个谁也没设计过的顺序。
//
// 有 error 的草稿**拒绝保存**（和水印不同）：策略的错误几乎都是"盒子会整段拒绝"
// 或"这一行一定不通"，存进去只会让人以为配好了。警告不妨碍保存。
func (s *server) handleAdminPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getPolicy(w, r, id)
	case http.MethodPut:
		s.putPolicy(w, r, id)
	default:
		methodNotAllowed(w, "GET, PUT")
	}
}

func (s *server) getPolicy(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	nodeID, ok := queryNodeID(w, r)
	if !ok {
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	view, found := s.policyViewLocked(id.TenantID, nodeID, now)
	s.mu.Unlock()
	if !found {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, view)
}

func (s *server) putPolicy(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req policySaveRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		http.Error(w, "node_id must be 1..65535", http.StatusBadRequest)
		return
	}

	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Nodes[key(id.TenantID, req.NodeID)]
	if n == nil || n.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(id.TenantID, req.NodeID)]
	if dc == nil || dc.Version == 0 {
		http.Error(w, "这台设备还没有保存过基础配置草稿（v0）：先去「基础配置」保存一次", http.StatusConflict)
		return
	}

	set := PolicySet{Rows: req.Rows, Fallback: req.Fallback}
	normalizePolicySet(&set)

	checks := s.policyChecksLocked(id.TenantID, req.NodeID, dc, set)
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": msg, "checks": checks})
		return
	}

	// 内容没变就不动版本号：改一行再改回来不该让设备重下发一次。
	changed := !policySetEqual(dc.Policy, set)
	if changed {
		dc.Policy = set
		dc.Version++
		dc.UpdatedAt = now
		dc.UpdatedBy = id.Username
		s.save()
	}
	view, _ := s.policyViewLocked(id.TenantID, req.NodeID, now)
	writeJSON(w, map[string]any{"ok": true, "changed": changed, "policy": view})
}

// policySetEqual 比的是"下发出去会不一样吗"，所以只比归一化之后的字段：
// 行号（顺序）、匹配条件、动态行的对端/候选/SLA、兜底清单。ID 由 normalize
// 按顺序重排，天然包含在内；行的比较用 policyRowEqual（SLA 是指针，== 比不了）。
func policySetEqual(a, b PolicySet) bool {
	x, y := policySetCopy(a), policySetCopy(b)
	normalizePolicySet(&x)
	normalizePolicySet(&y)
	if len(x.Rows) != len(y.Rows) || len(x.Fallback) != len(y.Fallback) {
		return false
	}
	for i := range x.Rows {
		if !policyRowEqual(x.Rows[i], y.Rows[i]) {
			return false
		}
	}
	for i := range x.Fallback {
		if x.Fallback[i] != y.Fallback[i] {
			return false
		}
	}
	return true
}
