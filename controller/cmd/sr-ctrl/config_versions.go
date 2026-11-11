// Version history, diff and rollback for the 基础配置 layer (config.go's
// DeviceConfig.History / PublishedBundle.Source).
//
// The shape mirrors what the box already does with its store: versions are
// identified by number, "what is running" and "what is saved" stay separate
// facts, and a rollback is a RESTORE OF THE DRAFT plus a normal publish --
// not a rerun of old bytes. Re-rendering is deliberate: the underlay and
// overlay layers may have moved since vN, and the rendered artifacts of a
// rollback must describe today's world, not the world when vN was published.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// configVersionRow is one row of GET /api/config/versions.
type configVersionRow struct {
	Version    int    `json:"version"`
	PublishedAt int64 `json:"published_at_unix"`
	PublishedBy string `json:"published_by,omitempty"`
	HasSource  bool   `json:"has_source"`
	IsRunning  bool   `json:"is_running"`
	IsDraft    bool   `json:"is_draft_base"`
}

type configVersionsView struct {
	NodeID      uint16             `json:"node_id"`
	DraftVersion int               `json:"draft_version"`
	PublishedVersion int           `json:"published_version"`
	Versions    []configVersionRow `json:"versions"`
}

// findPublishedVersion looks a version up in History first, then Published --
// the newest bundle is only in Published, the rest only in History.
func findPublishedVersion(dc *DeviceConfig, version int) *PublishedBundle {
	if dc == nil {
		return nil
	}
	if dc.Published != nil && dc.Published.Version == version {
		return dc.Published
	}
	for _, h := range dc.History {
		if h.Version == version {
			return h
		}
	}
	return nil
}

func (s *server) handleAdminConfigVersions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	nodeID, err := parseNodeParam(w, r)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Nodes[key(id.TenantID, nodeID)]
	if n == nil || n.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(id.TenantID, nodeID)]
	v := configVersionsView{NodeID: nodeID, Versions: []configVersionRow{}}
	if dc == nil {
		writeJSON(w, v)
		return
	}
	v.DraftVersion = dc.Version
	if dc.Published != nil {
		v.PublishedVersion = dc.Published.Version
	}
	// Newest first: the running version leads, then history descending.
	if dc.Published != nil {
		p := dc.Published
		v.Versions = append(v.Versions, configVersionRow{
			Version: p.Version, PublishedAt: p.PublishedAt, PublishedBy: p.PublishedBy,
			HasSource: p.Source != nil, IsRunning: n.Config != nil && n.Config.Version == p.Version,
		})
	}
	for i := len(dc.History) - 1; i >= 0; i-- {
		h := dc.History[i]
		v.Versions = append(v.Versions, configVersionRow{
			Version: h.Version, PublishedAt: h.PublishedAt, PublishedBy: h.PublishedBy,
			HasSource: h.Source != nil, IsRunning: false,
		})
	}
	writeJSON(w, v)
}

// configDiffSegment is one artifact segment's diff. Diff is a []string so the
// page can render each line as its own element; empty means "unchanged", and
// the page hides the segment rather than printing an empty block.
type configDiffSegment struct {
	Seg  string   `json:"seg"`
	Lines []string `json:"lines"`
}

type configDiffView struct {
	NodeID uint16              `json:"node_id"`
	A      string              `json:"a"`
	B      string              `json:"b"`
	Segs   []configDiffSegment `json:"segs"`
}

// handleAdminConfigDiff diffs two selectable snapshots: any published version
// (by number) or the current draft (a=b=draft). Tenant-only: artifacts are
// configuration.
func (s *server) handleAdminConfigDiff(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	nodeID, err := parseNodeParam(w, r)
	if err != nil {
		return
	}
	a := r.URL.Query().Get("a")
	b := r.URL.Query().Get("b")
	if a == "" || b == "" {
		http.Error(w, "a and b required (version number or \"draft\")", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Nodes[key(id.TenantID, nodeID)]
	if n == nil || n.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(id.TenantID, nodeID)]

	artA, err := s.configArtifactsForLocked(id.TenantID, nodeID, dc, a)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	artB, err := s.configArtifactsForLocked(id.TenantID, nodeID, dc, b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	v := configDiffView{NodeID: nodeID, A: a, B: b, Segs: []configDiffSegment{}}
	for _, seg := range []struct {
		name string
		get  func(configArtifacts) string
	}{
		{"frr", func(x configArtifacts) string { return x.FRR }},
		{"vpp", func(x configArtifacts) string { return x.VPP }},
		{"boot", func(x configArtifacts) string { return x.Boot }},
		{"route", func(x configArtifacts) string { return x.Route }},
		{"srfrr", func(x configArtifacts) string { return x.Srfrr }},
		{"vrrp", func(x configArtifacts) string { return x.Vrrp }},
		{"policy", func(x configArtifacts) string { return x.Policy }},
	} {
		d := unifiedDiffStrings(seg.get(artA), seg.get(artB))
		if len(d) == 0 {
			continue
		}
		v.Segs = append(v.Segs, configDiffSegment{Seg: seg.name, Lines: d})
	}
	writeJSON(w, v)
}

// configArtifactsForLocked resolves a diff selector to rendered artifacts.
// "draft" renders the live draft; a number renders the stored bundle of that
// version. Rendering the draft (instead of storing it) keeps one renderer and
// cannot drift from what a publish would send.
func (s *server) configArtifactsForLocked(tenant string, nodeID uint16, dc *DeviceConfig, sel string) (configArtifacts, error) {
	if sel == "draft" {
		if dc == nil || dc.Version == 0 {
			return configArtifacts{}, fmt.Errorf("这台设备还没有草稿")
		}
		cfg := dc.Draft
		normalizeBasicConfig(&cfg)
		return s.artifactsLocked(tenant, nodeID, &cfg, dc.Leak, dc.Policy), nil
	}
	version, err := strconv.Atoi(sel)
	if err != nil || version <= 0 {
		return configArtifacts{}, fmt.Errorf("版本选择器必须是版本号或 \"draft\"，收到 %q", sel)
	}
	b := findPublishedVersion(dc, version)
	if b == nil {
		return configArtifacts{}, fmt.Errorf("版本 v%d 不存在（或已被历史窗口挤出）", version)
	}
	return b.Artifacts, nil
}

type rollbackRequest struct {
	NodeID  uint16 `json:"node_id"`
	Version int    `json:"version"`
}

// handleAdminConfigRollback restores one published version's draft input and
// publishes it as a NEW version. The device therefore treats it like any other
// publish (it pulls by version number), the console keeps one numbering scheme,
// and the ledger records who rolled back to what. Refusals: the version has no
// stored source (pre-history bundles), or the restored draft has a blocking
// check -- a rollback must not bypass the same gate a save+publish would face.
func (s *server) handleAdminConfigRollback(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	var req rollbackRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.NodeID == 0 || req.Version <= 0 {
		http.Error(w, "node_id and version are required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Nodes[key(id.TenantID, req.NodeID)]
	if n == nil || n.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(id.TenantID, req.NodeID)]
	b := findPublishedVersion(dc, req.Version)
	if b == nil {
		http.Error(w, fmt.Sprintf("版本 v%d 不存在（或已被历史窗口挤出）", req.Version), http.StatusNotFound)
		return
	}
	if b.Source == nil {
		http.Error(w, fmt.Sprintf("版本 v%d 发布于版本历史功能之前，没有可恢复的草稿快照，无法回滚（只能看 diff）", req.Version), http.StatusConflict)
		return
	}
	if dc == nil {
		http.Error(w, "这台设备还没有配置", http.StatusConflict)
		return
	}
	// Restore the draft input. The snapshot is a deep copy already (it was
	// cloned at publish time and nothing mutates history), but a defensive
	// round-trip here keeps the invariant local: the draft never aliases
	// history, whatever wrote it.
	src := snapshotConfigSource(&DeviceConfig{
		Draft: b.Source.Draft, Leak: b.Source.Leak, Policy: b.Source.Policy,
	})
	if src == nil {
		http.Error(w, "内部错误：快照无法复制", http.StatusInternalServerError)
		return
	}
	dc.Draft = src.Draft
	dc.Leak = src.Leak
	dc.Policy = src.Policy
	dc.Version++
	dc.UpdatedAt = time.Now().Unix()
	dc.UpdatedBy = id.Username

	checks := s.publishChecksLocked(id.TenantID, req.NodeID, dc)
	for _, c := range checks {
		if c.Severity == "error" {
			http.Error(w, fmt.Sprintf("回滚后的配置还有校验错误：%s（回到草稿了，可以在页面上改完再发布）", c.Text), http.StatusConflict)
			return
		}
	}
	changed := s.publishDeviceLocked(id.TenantID, req.NodeID, dc, checks, id.Username, time.Now().Unix())
	s.save()

	// 台账：回滚是"这台设备被改了"的事实，比一次普通发布更值得排障时先看。
	// （发布本身也会记一条 config.publish —— 两条是同一个动作的两个侧面。）
	msg := fmt.Sprintf("回滚设备配置：以 v%d 的草稿内容重新发布，新版本 v%d", req.Version, dc.Published.Version)
	if !changed {
		msg = fmt.Sprintf("回滚设备配置：以 v%d 的草稿内容重发，内容与当前一致，未提升版本", req.Version)
	}
	s.appendOps("config.rollback", id.TenantID, req.NodeID, id.Username, "warn", msg)
	writeJSON(w, s.buildConfigView(n, dc))
}
