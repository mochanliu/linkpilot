package main

// HTTP surface of the 路由泄露 layer. Tenant-only, same discipline as the
// underlay and overlay handlers: one identity check, one mutation under s.mu,
// then a whole-state save.
//
// Publishing has no private path here. Every "发布" entry on the two pages --
// the device row, the prefix-list row and the batch form -- ends in
// POST /api/config/publish' handler body (publishDeviceLocked), which freezes
// the same bundle and moves the same version number as the 基础配置 page.

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

type leakRowRequest struct {
	NodeID     uint16   `json:"node_id"`
	Direction  string   `json:"direction"`
	VPN        uint32   `json:"vpn"`
	Protocols  []string `json:"protocols"`
	PrefixList string   `json:"prefix_list"`
}

type leakBatchRequest struct {
	Direction  string   `json:"direction"`
	NodeIDs    []uint16 `json:"node_ids"`
	VPNs       []uint32 `json:"vpns"`
	Protocols  []string `json:"protocols"`
	PrefixList string   `json:"prefix_list"`
	Apply      bool     `json:"apply"`
}

type leakBatchRow struct {
	NodeID uint16        `json:"node_id"`
	VPN    uint32        `json:"vpn"`
	Keep   bool          `json:"keep"`
	Reason string        `json:"reason,omitempty"`
	Rule   *LeakRule     `json:"rule,omitempty"`
	Checks []ConfigCheck `json:"checks,omitempty"`
}

type leakPublishResult struct {
	NodeID    uint16 `json:"node_id"`
	Published bool   `json:"published"`
	Version   int    `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

type leakBatchView struct {
	Direction string              `json:"direction"`
	Apply     bool                `json:"apply"`
	Blocked   bool                `json:"blocked"`
	Rows      []leakBatchRow      `json:"rows"`
	Results   []leakPublishResult `json:"results,omitempty"`
	Leak      *leakView           `json:"leak,omitempty"`
}

type leakRepublishRequest struct {
	PrefixListID string `json:"prefix_list_id"`
	Apply        bool   `json:"apply"`
}

type leakRepublishRow struct {
	NodeID           uint16        `json:"node_id"`
	PublishedVersion int           `json:"published_version,omitempty"`
	WillPublish      bool          `json:"will_publish"`
	Reason           string        `json:"reason,omitempty"`
	Checks           []ConfigCheck `json:"checks,omitempty"`
}

type leakRepublishView struct {
	PrefixListID   string              `json:"prefix_list_id"`
	PrefixListName string              `json:"prefix_list_name"`
	Apply          bool                `json:"apply"`
	Blocked        bool                `json:"blocked"`
	Rows           []leakRepublishRow  `json:"rows"`
	Results        []leakPublishResult `json:"results,omitempty"`
}

type prefixListRequest struct {
	ID      string        `json:"id,omitempty"`
	Name    string        `json:"name"`
	Entries []PrefixEntry `json:"entries"`
}

func (s *server) handleAdminLeak(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	s.mu.Lock()
	v := s.leakViewLocked(id.TenantID, time.Now().Unix())
	s.mu.Unlock()
	writeJSON(w, v)
}

// handleAdminLeakRows saves one (device, direction, VPN) row. An empty protocol
// set deletes the row: "this VPN leaks nothing" is the absence of a rule, and
// storing an empty one would show a row that renders nothing.
func (s *server) handleAdminLeakRows(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var req leakRowRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		http.Error(w, "node_id required", http.StatusBadRequest)
		return
	}
	if req.Direction != leakToCPE && req.Direction != leakToSDWAN {
		http.Error(w, "direction 只能是 to_cpe 或 to_sdwan", http.StatusBadRequest)
		return
	}
	rule := LeakRule{VPN: req.VPN, Protocols: req.Protocols, PrefixList: req.PrefixList}
	normalizeLeakRule(&rule)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Nodes[key(id.TenantID, req.NodeID)] == nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(id.TenantID, req.NodeID)]
	if dc == nil || dc.Version == 0 {
		http.Error(w, "这台设备还没有保存过基础配置草稿（v0）：先去「基础配置」保存一次", http.StatusConflict)
		return
	}

	now := time.Now().Unix()
	if len(rule.Protocols) == 0 {
		removed := dc.Leak.drop(req.Direction, req.VPN)
		if removed {
			dc.Version++
			dc.UpdatedAt = now
			dc.UpdatedBy = id.Username
			s.save()
			log.Printf("leak: tenant=%s node=%d %s vpn=%d row removed by %s",
				id.TenantID, req.NodeID, req.Direction, req.VPN, id.Username)
		}
		writeJSON(w, map[string]any{
			"ok":        true,
			"node_id":   req.NodeID,
			"direction": req.Direction,
			"removed":   removed,
			"leak":      s.leakViewLocked(id.TenantID, now),
		})
		return
	}

	checks := s.leakRuleChecksLocked(id.TenantID, req.NodeID, req.Direction, rule)
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": msg, "checks": checks})
		return
	}
	dc.Leak.put(req.Direction, rule)
	normalizeLeakSet(&dc.Leak)
	dc.Version++
	dc.UpdatedAt = now
	dc.UpdatedBy = id.Username
	s.save()
	log.Printf("leak: tenant=%s node=%d %s vpn=%d protocols=%s list=%q by %s",
		id.TenantID, req.NodeID, req.Direction, req.VPN, strings.Join(rule.Protocols, ","),
		rule.PrefixList, id.Username)
	writeJSON(w, map[string]any{
		"ok":        true,
		"node_id":   req.NodeID,
		"direction": req.Direction,
		"rule":      rule,
		"leak":      s.leakViewLocked(id.TenantID, now),
	})
}

// handleAdminLeakBatch is the one-shot template form (§7). ?apply=false (the
// default) only previews; apply refuses the whole batch when any device would
// be left out, so a batch can never half-land.
func (s *server) handleAdminLeakBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req leakBatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Direction != leakToCPE && req.Direction != leakToSDWAN {
		http.Error(w, "direction 只能是 to_cpe 或 to_sdwan", http.StatusBadRequest)
		return
	}
	if len(req.NodeIDs) == 0 || len(req.VPNs) == 0 {
		http.Error(w, "node_ids 和 vpns 都不能为空", http.StatusBadRequest)
		return
	}
	protocols := normalizeProtocolList(req.Protocols)
	if len(protocols) == 0 {
		http.Error(w, "至少选一个来源", http.StatusBadRequest)
		return
	}
	if msg := unknownProtocol(protocols, req.Direction); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	prefixList := strings.TrimSpace(req.PrefixList)
	if req.Direction == leakToSDWAN && prefixList == "" {
		http.Error(w, "CPE-SDWAN 必须选一个 prefix-list（默认 ALL 就是 0.0.0.0/0）", http.StatusBadRequest)
		return
	}

	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	lc := s.leakConfigLocked(id.TenantID, true)
	if prefixList != "" && lc.list(prefixList) == nil {
		http.Error(w, "prefix-list 不存在", http.StatusNotFound)
		return
	}
	// A prefix list with no entries would silently leak nothing for CPE-SDWAN;
	// the per-row gate says so too, but naming it once here is clearer.
	if req.Direction == leakToSDWAN && len(lc.list(prefixList).Entries) == 0 {
		http.Error(w, "这个 prefix-list 没有条目：CPE-SDWAN 会一条都上报不了", http.StatusBadRequest)
		return
	}

	nodeIDs := uniqueNodeIDs(req.NodeIDs)
	vpnIDs := uniqueVPNs(req.VPNs)

	view := leakBatchView{Direction: req.Direction, Apply: req.Apply, Rows: []leakBatchRow{}}
	candidates := map[uint16]LeakSet{}
	for _, nodeID := range nodeIDs {
		n := s.state.Nodes[key(id.TenantID, nodeID)]
		if n == nil {
			view.Blocked = true
			view.Rows = append(view.Rows, leakBatchRow{NodeID: nodeID, Reason: "设备不属于这个租户"})
			continue
		}
		dc := s.state.Configs[configKey(id.TenantID, nodeID)]
		if dc == nil || dc.Version == 0 {
			view.Blocked = true
			view.Rows = append(view.Rows, leakBatchRow{NodeID: nodeID,
				Reason: "还没有保存过基础配置草稿（v0）：先去「基础配置」保存一次"})
			continue
		}
		candidates[nodeID] = leakSetCopy(dc.Leak)
		for _, vpn := range vpnIDs {
			row := leakBatchRow{NodeID: nodeID, VPN: vpn}
			vc := configVPNOf(dc, vpn)
			if vc == nil {
				row.Reason = fmt.Sprintf("设备 %d 的基础配置里没有 VPN %d", nodeID, vpn)
				view.Rows = append(view.Rows, row)
				continue
			}
			kept, dropped := filterProtocols(req.Direction, protocols, vc)
			if len(kept) == 0 {
				row.Reason = strings.Join(dropped, "；")
				view.Rows = append(view.Rows, row)
				continue
			}
			rule := LeakRule{VPN: vpn, Protocols: kept, PrefixList: prefixList}
			normalizeLeakRule(&rule)
			row.Keep = true
			row.Rule = &rule
			if len(dropped) > 0 {
				row.Reason = strings.Join(dropped, "；")
			}
			row.Checks = s.leakRuleChecksLocked(id.TenantID, nodeID, req.Direction, rule)
			if hasBlockingCheck(row.Checks) != "" {
				view.Blocked = true
			}
			view.Rows = append(view.Rows, row)
		}
	}

	if !req.Apply {
		writeJSON(w, view)
		return
	}
	if view.Blocked {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "预览里还有会被丢弃或校验不过的行：整批不写，先把上面的原因改掉",
			"batch": view,
		})
		return
	}

	// Apply every kept row to the candidate sets and run the full publish gate on
	// each affected device before writing anything. One device failing means
	// nothing lands.
	applyNodes := map[uint16]bool{}
	for _, row := range view.Rows {
		if !row.Keep || row.Rule == nil {
			continue
		}
		applyNodes[row.NodeID] = true
		set := candidates[row.NodeID]
		set.put(req.Direction, *row.Rule)
		candidates[row.NodeID] = set
	}
	checksByNode := map[uint16][]ConfigCheck{}
	for nodeID := range applyNodes {
		dc := s.state.Configs[configKey(id.TenantID, nodeID)]
		checks := s.publishChecksForLeakLocked(id.TenantID, nodeID, dc, candidates[nodeID])
		if msg := hasBlockingCheck(checks); msg != "" {
			view.Blocked = true
			view.Results = append(view.Results, leakPublishResult{NodeID: nodeID, Error: msg})
		}
		checksByNode[nodeID] = checks
	}
	if view.Blocked {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "有设备的发布检查不通过：整批不写（不会一半生效）",
			"batch": view,
		})
		return
	}

	// Write the drafts, then publish each device through the same path as the
	// device-row button. One save at the end: a crash mid-batch leaves the file
	// exactly as it was, not with half the rows.
	var applied []uint16
	for nodeID := range applyNodes {
		applied = append(applied, nodeID)
		dc := s.state.Configs[configKey(id.TenantID, nodeID)]
		dc.Leak = candidates[nodeID]
		normalizeLeakSet(&dc.Leak)
		dc.Version++
		dc.UpdatedAt = now
		dc.UpdatedBy = id.Username
	}
	sort.Slice(applied, func(i, j int) bool { return applied[i] < applied[j] })
	sort.Slice(view.Rows, func(i, j int) bool {
		if view.Rows[i].NodeID != view.Rows[j].NodeID {
			return view.Rows[i].NodeID < view.Rows[j].NodeID
		}
		return view.Rows[i].VPN < view.Rows[j].VPN
	})
	for _, nodeID := range applied {
		dc := s.state.Configs[configKey(id.TenantID, nodeID)]
		changed := s.publishDeviceLocked(id.TenantID, nodeID, dc, checksByNode[nodeID], id.Username, now)
		view.Results = append(view.Results, leakPublishResult{
			NodeID: nodeID, Published: changed, Version: dc.Published.Version,
		})
	}
	s.save()
	log.Printf("leak: tenant=%s batch %s applied to %d devices by %s",
		id.TenantID, req.Direction, len(applied), id.Username)
	lv := s.leakViewLocked(id.TenantID, now)
	view.Leak = &lv
	writeJSON(w, view)
}

// handleAdminLeakRepublish is the prefix-list row's "重新发布引用它的 N 台
// 设备". It is orchestration only: every device goes through publishDeviceLocked,
// exactly like the device row button.
func (s *server) handleAdminLeakRepublish(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req leakRepublishRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	listID := strings.TrimSpace(req.PrefixListID)
	if listID == "" {
		http.Error(w, "prefix_list_id required", http.StatusBadRequest)
		return
	}

	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	lc := s.leakConfigLocked(id.TenantID, true)
	pl := lc.list(listID)
	if pl == nil {
		http.Error(w, "prefix-list 不存在", http.StatusNotFound)
		return
	}
	devices := s.prefixListRefsLocked(id.TenantID, listID)
	if len(devices) == 0 {
		http.Error(w, "没有设备引用这个 prefix-list", http.StatusBadRequest)
		return
	}

	view := leakRepublishView{
		PrefixListID: listID, PrefixListName: pl.Name,
		Apply: req.Apply, Rows: []leakRepublishRow{},
	}
	checksByNode := map[uint16][]ConfigCheck{}
	for _, nodeID := range devices {
		row := leakRepublishRow{NodeID: nodeID}
		dc := s.state.Configs[configKey(id.TenantID, nodeID)]
		if dc == nil || dc.Version == 0 {
			row.Reason = "还没有保存过基础配置草稿（v0）"
			view.Blocked = true
			view.Rows = append(view.Rows, row)
			continue
		}
		if dc.Published != nil {
			row.PublishedVersion = dc.Published.Version
		}
		checks := s.publishChecksLocked(id.TenantID, nodeID, dc)
		row.Checks = checks
		if msg := hasBlockingCheck(checks); msg != "" {
			row.Reason = msg
			view.Blocked = true
		} else {
			row.WillPublish = true
		}
		checksByNode[nodeID] = checks
		view.Rows = append(view.Rows, row)
	}
	if !req.Apply {
		writeJSON(w, view)
		return
	}
	if view.Blocked {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":     "有设备现在发布不了：整批不发布，先把上面的原因改掉",
			"republish": view,
		})
		return
	}

	for _, row := range view.Rows {
		if !row.WillPublish {
			continue
		}
		dc := s.state.Configs[configKey(id.TenantID, row.NodeID)]
		changed := s.publishDeviceLocked(id.TenantID, row.NodeID, dc, checksByNode[row.NodeID], id.Username, now)
		view.Results = append(view.Results, leakPublishResult{
			NodeID: row.NodeID, Published: changed, Version: dc.Published.Version,
		})
	}
	s.save()
	log.Printf("leak: tenant=%s republished prefix-list %s to %d devices by %s",
		id.TenantID, listID, len(view.Results), id.Username)
	writeJSON(w, view)
}

// handleAdminPrefixLists edits the tenant's library. Reads come with /api/leak;
// these three methods are the only writes.
func (s *server) handleAdminPrefixLists(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createPrefixList(w, r, id)
	case http.MethodPut:
		s.updatePrefixList(w, r, id)
	case http.MethodDelete:
		s.deletePrefixList(w, r, id)
	default:
		methodNotAllowed(w, "POST, PUT, DELETE")
	}
}

func (s *server) createPrefixList(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req prefixListRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	pl := PrefixList{Name: strings.TrimSpace(req.Name), Entries: req.Entries}
	if pl.Entries == nil {
		pl.Entries = []PrefixEntry{}
	}
	normalizePrefixEntries(pl.Entries)

	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	lc := s.leakConfigLocked(id.TenantID, true)
	if len(lc.PrefixLists) >= leakMaxPrefixLists {
		http.Error(w, fmt.Sprintf("prefix-list 数量已达上限 %d", leakMaxPrefixLists), http.StatusConflict)
		return
	}
	if checks := prefixListChecks(pl, lc.PrefixLists, ""); hasBlockingCheck(checks) != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": hasBlockingCheck(checks), "checks": checks,
		})
		return
	}
	pl.ID = lc.nextListID()
	pl.CreatedAt = now
	pl.UpdatedAt = now
	lc.PrefixLists = append(lc.PrefixLists, pl)
	lc.UpdatedAt = now
	lc.UpdatedBy = id.Username
	s.save()
	log.Printf("leak: tenant=%s created prefix-list %s %q entries=%d by %s",
		id.TenantID, pl.ID, pl.Name, len(pl.Entries), id.Username)
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"ok": true, "list": pl, "leak": s.leakViewLocked(id.TenantID, now),
	})
}

func (s *server) updatePrefixList(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req prefixListRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	listID := strings.TrimSpace(req.ID)
	if listID == "" {
		listID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if listID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	lc := s.leakConfigLocked(id.TenantID, true)
	existing := lc.list(listID)
	if existing == nil {
		http.Error(w, "prefix-list 不存在", http.StatusNotFound)
		return
	}
	pl := PrefixList{ID: listID, Name: strings.TrimSpace(req.Name), Entries: req.Entries}
	if pl.Entries == nil {
		pl.Entries = []PrefixEntry{}
	}
	normalizePrefixEntries(pl.Entries)
	if checks := prefixListChecks(pl, lc.PrefixLists, listID); hasBlockingCheck(checks) != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": hasBlockingCheck(checks), "checks": checks,
		})
		return
	}
	pl.CreatedAt = existing.CreatedAt
	pl.UpdatedAt = now
	*existing = pl
	lc.UpdatedAt = now
	lc.UpdatedBy = id.Username
	s.save()
	log.Printf("leak: tenant=%s updated prefix-list %s %q entries=%d by %s",
		id.TenantID, pl.ID, pl.Name, len(pl.Entries), id.Username)
	writeJSON(w, map[string]any{
		"ok": true, "list": pl, "leak": s.leakViewLocked(id.TenantID, now),
	})
}

func (s *server) deletePrefixList(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	listID := strings.TrimSpace(r.URL.Query().Get("id"))
	if listID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	lc := s.leakConfigLocked(id.TenantID, true)
	existing := lc.list(listID)
	if existing == nil {
		http.Error(w, "prefix-list 不存在", http.StatusNotFound)
		return
	}
	if refs := s.prefixListRefsLocked(id.TenantID, listID); len(refs) > 0 {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":   fmt.Sprintf("还有 %d 台设备的泄露规则引用这个列表：先把那些行换掉再删", len(refs)),
			"devices": refs,
		})
		return
	}
	idx := -1
	for i := range lc.PrefixLists {
		if lc.PrefixLists[i].ID == listID {
			idx = i
			break
		}
	}
	lc.PrefixLists = append(lc.PrefixLists[:idx], lc.PrefixLists[idx+1:]...)
	lc.UpdatedAt = now
	lc.UpdatedBy = id.Username
	s.save()
	log.Printf("leak: tenant=%s deleted prefix-list %s %q by %s",
		id.TenantID, existing.ID, existing.Name, id.Username)
	writeJSON(w, map[string]any{
		"ok": true, "deleted": listID, "leak": s.leakViewLocked(id.TenantID, now),
	})
}

func normalizeProtocolList(in []string) []string {
	rule := LeakRule{Protocols: in}
	normalizeLeakRule(&rule)
	return rule.Protocols
}

func unknownProtocol(protocols []string, direction string) string {
	allowed := map[string]bool{}
	for _, p := range leakProtocols(direction) {
		allowed[p] = true
	}
	for _, p := range protocols {
		if !allowed[p] {
			return fmt.Sprintf("%s 不支持来源「%s」：只能是 %s",
				leakDirectionLabel(direction), p, strings.Join(leakProtocols(direction), " / "))
		}
	}
	return ""
}

// filterProtocols keeps what this VPN can actually originate and reports why the
// rest was dropped. static/connected need no FRR instance.
func filterProtocols(direction string, requested []string, vc *ConfigVPN) (kept, dropped []string) {
	for _, p := range requested {
		switch p {
		case leakProtoBGP:
			if vc.BGP.On {
				kept = append(kept, p)
			} else {
				dropped = append(dropped, "这个 VPN 没有启用 BGP 实例，已去掉 bgp")
			}
		case leakProtoOSPF:
			if vc.OSPF.On {
				kept = append(kept, p)
			} else {
				dropped = append(dropped, "这个 VPN 没有启用 OSPF 实例，已去掉 ospf")
			}
		case leakProtoStatic, leakProtoConnected:
			if direction == leakToSDWAN {
				kept = append(kept, p)
			}
		}
	}
	return kept, dropped
}

func uniqueNodeIDs(in []uint16) []uint16 {
	seen := map[uint16]bool{}
	out := make([]uint16, 0, len(in))
	for _, id := range in {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func uniqueVPNs(in []uint32) []uint32 {
	seen := map[uint32]bool{}
	out := make([]uint32, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
