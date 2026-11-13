package main

// 服务面（NAT 出口 / DHCP / DNS）的读写面：GET/PUT /api/services。租户专属，
// 与策略页同一形状：按设备读、按设备写、整份替换，保存只写草稿，发布走
// /api/config/publish 既有路径（设备下一次心跳自己拉）。
//
// 为什么是按设备而不是租户级总表：每一行的挂点都是**这台设备**的客户口
// （VPP 名 + 它的地址/掩码/TAP），脱离设备就没有一行是完整的。

import (
	"net/http"
	"time"

	"sdwan/controller/internal/wire"
)

type servicesView struct {
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

	Services ServicesSet `json:"services"`

	// Ifaces 是可选挂点：客户口（VPP 名 + 地址/掩码 + VPN），页面用它生成
	// 下拉框并标注这个口能不能当 DHCP/DNS/NAT 的 inside。
	Ifaces   []svcIfaceRow     `json:"ifaces"`
	Checks   []ConfigCheck     `json:"checks"`
	Artifact string            `json:"artifact"`
	Notes    map[string]string `json:"notes"`
}

type svcIfaceRow struct {
	VPP       string `json:"vpp"`
	LCP       string `json:"lcp"`
	IP        string `json:"ip"`
	PrefixLen uint8  `json:"prefix_len"`
	VPN       uint32 `json:"vpn"`
}

func (s *server) servicesViewLocked(tenantID string, nodeID uint16, now int64) (servicesView, bool) {
	n := s.state.Nodes[key(tenantID, nodeID)]
	if n == nil || n.TenantID != tenantID {
		return servicesView{}, false
	}
	dc := s.state.Configs[configKey(tenantID, nodeID)]
	view := servicesView{
		TenantID: tenantID,
		NowUnix:  now,
		NodeID:   nodeID,
		Name:     n.Serial,
		Status:   nodeStatus(n),
		Device:   n.Config,
		Ifaces:   []svcIfaceRow{},
		Checks:   []ConfigCheck{},
		Notes: map[string]string{
			"nat": "一行 = 一条 NAT 出口：客户口(VPN 内)的流量经出口口出去，源地址翻译成" +
				"地址池里的地址。池是显式配置的——要用出口口自己的地址就把那个地址写进池里；" +
				"池里的地址必须能收到回程流量，否则翻译出去就回不来。",
			"dhcp": "一行 = 一台 DHCP server，绑定客户口自己的地址。可分配池必须落在客户口" +
				"子网内且不能包含口本身的地址；网关与 DNS 留空 = 用客户口地址（DNS 即本机转发器）。",
			"dns": "一行 = 一个 DNS 转发器，监听客户口地址的 53/udp，按序转发到上游。" +
				"上游留空 = 只回 SERVFAIL。",
			"nat_pending": "盒子上 VPP 26.06 自带的 NAT44 插件有数据面崩溃缺陷（2026-11-11 实测：" +
				"转发路径第一个向量即整机卡死）。发布后 DHCP/DNS 照常下发，NAT 行暂不下发，" +
				"等后端落定（自研插件或升级 VPP）。",
			"publish": "保存只写草稿；设备要等「发布到设备」之后才会拿到，发布走基础配置同一条路径。",
		},
	}

	cfg := defaultBasicConfig()
	var svc ServicesSet
	if dc != nil {
		cfg = dc.Draft
		svc = dc.Services
		view.Version = dc.Version
		view.HasDraft = dc.Version > 0
		if dc.Published != nil {
			view.PublishedVersion = dc.Published.Version
			view.PublishedAt = dc.Published.PublishedAt
			art := s.artifactsLocked(tenantID, nodeID, &dc.Draft, dc.Leak, dc.Policy)
			view.Pending = dc.Published.Version != dc.Version || dc.Published.Artifacts != art
		}
	}
	normalizeBasicConfig(&cfg)
	normalizeServices(&svc)
	view.Services = svc
	view.Artifact = renderServicesArtifact(&cfg, svc)

	for _, it := range cfg.Ifaces {
		view.Ifaces = append(view.Ifaces, svcIfaceRow{
			VPP: it.VPP, LCP: it.LCP, IP: it.IP, PrefixLen: it.PrefixLen, VPN: it.VPN,
		})
	}

	if view.HasDraft {
		view.Checks = servicesChecks(&cfg, &svc)
		if view.Checks == nil {
			view.Checks = []ConfigCheck{}
		}
	} else {
		view.Checks = []ConfigCheck{{
			Severity: "warning",
			Text:     "这台设备还没有保存过基础配置草稿（v0）：服务行还没有客户口可以挂。先去「基础配置」保存一次。",
		}}
	}
	return view, true
}

type servicesSaveRequest struct {
	NodeID   uint16      `json:"node_id"`
	Services ServicesSet `json:"services"`
}

func (s *server) handleAdminServices(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getServices(w, r, id)
	case http.MethodPut:
		s.putServices(w, r, id)
	default:
		methodNotAllowed(w, "GET, PUT")
	}
}

func (s *server) getServices(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	nodeID, ok := queryNodeID(w, r)
	if !ok {
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	view, found := s.servicesViewLocked(id.TenantID, nodeID, now)
	s.mu.Unlock()
	if !found {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	writeJSON(w, view)
}

func (s *server) putServices(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req servicesSaveRequest
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

	set := req.Services
	normalizeServices(&set)
	checks := servicesChecks(&dc.Draft, &set)
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": msg, "checks": checks})
		return
	}

	changed := !servicesEqual(dc.Services, set)
	if changed {
		dc.Services = set
		dc.Version++
		dc.UpdatedAt = now
		dc.UpdatedBy = id.Username
		s.save()
	}
	view, _ := s.servicesViewLocked(id.TenantID, req.NodeID, now)
	writeJSON(w, map[string]any{"ok": true, "changed": changed, "services": view})
}

// servicesEqual 比归一化之后的值：两张表逐行 == 就够了（行里没有指针）。
func servicesEqual(a, b ServicesSet) bool {
	normalizeServices(&a)
	normalizeServices(&b)
	if len(a.NAT) != len(b.NAT) || len(a.DHCP) != len(b.DHCP) || len(a.DNS) != len(b.DNS) {
		return false
	}
	for i := range a.NAT {
		x, y := a.NAT[i], b.NAT[i]
		if x.On != y.On || x.VPN != y.VPN || x.Inside != y.Inside || x.Outside != y.Outside ||
			x.MSSClamp != y.MSSClamp || !strSliceEqual(x.Pool, y.Pool) {
			return false
		}
	}
	for i := range a.DHCP {
		if a.DHCP[i] != b.DHCP[i] {
			return false
		}
	}
	for i := range a.DNS {
		x, y := a.DNS[i], b.DNS[i]
		if x.VPN != y.VPN || x.VPP != y.VPP || !strSliceEqual(x.Upstream, y.Upstream) {
			return false
		}
	}
	return true
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
