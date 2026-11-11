package main

// The underlay HTTP surface, tenant-side only.
//
// Every endpoint here is a tenant's own network: the operator role sees device
// inventory and nothing else, and that is enforced here, before anything is
// rendered, not in the page. All handlers run under s.mu, mutate through
// internal/underlay (which clones before it commits) and save the whole state
// atomically afterwards, exactly like the basic-config draft does.
//
// The draft/publish split is the same as the device config's: PUT saves a draft,
// publish is the act that changes what devices are told to run. Nothing here
// auto-publishes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"sdwan/controller/internal/underlay"
)

// requireTenant is the gate on the whole underlay layer.
func (s *server) requireTenant(w http.ResponseWriter, r *http.Request) (consoleIdentity, bool) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return id, false
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return id, false
	}
	return id, true
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cfgMaxBody)).Decode(dst); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeUnderlayErr maps the allocator's errors to status codes: a missing
// template/tunnel is 404, an existing reference or an exhausted pool makes the
// request impossible against what exists (409), everything else is a bad
// request.
func writeUnderlayErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, underlay.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, underlay.ErrPathInUse),
		errors.Is(err, underlay.ErrPairTaken),
		errors.Is(err, underlay.ErrPortExhausted),
		errors.Is(err, underlay.ErrOverlayExhausted):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// ------------------------------------------------------------- the document

func (s *server) handleAdminUnderlay(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	s.mu.Lock()
	v := s.underlayViewLocked(id.TenantID, time.Now().Unix())
	s.mu.Unlock()
	writeJSON(w, v)
}

// ------------------------------------------------------- global configuration

// handleAdminUnderlaySettings saves the tenant's draft. A draft with blocking
// problems is NOT saved: unlike the device config (where a half-written policy
// is a normal intermediate state), every rule here is a pool overlap or an
// arithmetic constraint, and there is no sensible partial version of "the
// overlay pool overlaps a customer subnet".
func (s *server) handleAdminUnderlaySettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var req underlay.Settings
	if !decodeJSONBody(w, r, &req) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	checks := s.underlaySettingsChecksLocked(id.TenantID, req)
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": msg, "checks": checks,
		})
		return
	}
	req = req.Normalize()
	uc := s.underlayConfigLocked(id.TenantID, true)
	changes := settingsDiff(uc.Draft, req)
	uc.Draft = req
	uc.DraftAt = time.Now().Unix()
	uc.DraftBy = id.Username
	s.save()
	writeJSON(w, map[string]any{
		"ok":       true,
		"changes":  changes,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

// handleAdminUnderlayPublish freezes the draft as the published configuration
// and assigns every enrolled device its transit port. Re-publishing never moves
// a value that was already assigned: SetSettings refuses a change that would
// orphan a live tunnel or a transit port instead of silently stranding it.
func (s *server) handleAdminUnderlayPublish(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	uc := s.underlayConfigLocked(id.TenantID, true)
	now := time.Now().Unix()

	checks := s.underlaySettingsChecksLocked(id.TenantID, uc.Draft)
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": msg, "checks": checks,
		})
		return
	}
	nodes := s.tenantNodesLocked(id.TenantID)
	ids := make([]uint16, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.NodeID)
	}
	if err := uc.State.PublishSettings(uc.Draft, ids, now); err != nil {
		writeUnderlayErr(w, err)
		return
	}
	uc.PublishedBy = id.Username
	s.save()
	s.appendOps("underlay.publish", id.TenantID, 0, id.Username, "info",
		fmt.Sprintf("发布 underlay 全局配置（%d 台设备的中转端口已分配）", len(ids)))
	log.Printf("underlay: tenant=%s published global config by %s (%d device(s), transit ports assigned)",
		id.TenantID, id.Username, len(ids))
	writeJSON(w, map[string]any{
		"ok":       true,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

// ------------------------------------------------------------------ tunnels

// underlaySpec is one tunnel end as the form submits it. Addr nil means "keep"
// on update and "use the observation" on create; an explicit empty string
// clears an override back to the observation.
type underlaySpec struct {
	NodeID uint16  `json:"node_id"`
	Iface  string  `json:"iface"`
	Addr   *string `json:"addr,omitempty"`
	Port   uint16  `json:"port"`
}

// underlayHopSpec is one relay hop as the form submits it, in the order the
// operator read the route. Addr empty means "use what the reflector observed".
type underlayHopSpec struct {
	NodeID uint16 `json:"node_id"`
	Addr   string `json:"addr,omitempty"`
}

type underlayPathSpec struct {
	TemplateID string            `json:"template_id,omitempty"`
	Hops       []underlayHopSpec `json:"hops,omitempty"`
	// SaveAs names a NEW template for the inline hop chain ("存为模板"). It is
	// only valid together with hops and without template_id.
	SaveAs string `json:"save_as,omitempty"`
}

type underlayTunnelRequest struct {
	// ID is the tunnel to update/delete. Endpoint node_ids are ignored on update:
	// a tunnel's two devices are what it IS, not a field to retarget.
	ID   string            `json:"id,omitempty"`
	A    underlaySpec      `json:"a"`
	B    underlaySpec      `json:"b"`
	Path *underlayPathSpec `json:"path,omitempty"`
	// Peers is the batch shape: one local end (A), one path, many far ends. When it
	// is present B is ignored and the request fans out one tunnel per peer, sharing
	// a batch id. Peers that cannot take a tunnel are reported per peer instead of
	// failing the whole request.
	Peers []uint16 `json:"peers,omitempty"`
	// ClearPath turns a relayed tunnel into a direct one; a nil path with
	// ClearPath=false keeps the current path.
	ClearPath bool `json:"clear_path,omitempty"`
	// Color is the tunnel's SD-WAN color. Pointer because 0 is a real color: on
	// update "absent" means keep while "0" means recolor this tunnel to 0.
	Color *uint8 `json:"color,omitempty"`
}

func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func u8Val(p *uint8) uint8 {
	if p == nil {
		return 0
	}
	return *p
}

// tunnelPathLocked validates the path half of a request and, for "存为模板",
// creates the template. created is the id of a template this request made, so
// the caller can delete it again when the tunnel itself is rejected: a failed
// save must not leave a template behind that nobody asked for.
//
// Must be called with s.mu held.
func (s *server) tunnelPathLocked(st *underlay.State, ps *underlayPathSpec, now int64) (path *underlay.TunnelPath, created string, err error) {
	if ps == nil {
		return nil, "", nil
	}
	hops := pathHops(ps.Hops)
	if ps.TemplateID != "" {
		if len(hops) > 0 || strings.TrimSpace(ps.SaveAs) != "" {
			return nil, "", fmt.Errorf("路径模板不能和 inline 中转设备/存为模板同时填写")
		}
		return &underlay.TunnelPath{TemplateID: ps.TemplateID}, "", nil
	}
	if len(hops) == 0 {
		return nil, "", fmt.Errorf("路径需要至少一个中转设备，或引用一个路径模板")
	}
	if name := strings.TrimSpace(ps.SaveAs); name != "" {
		tpl, err := st.CreatePath(name, hops, now)
		if err != nil {
			return nil, "", err
		}
		return &underlay.TunnelPath{TemplateID: tpl.ID}, tpl.ID, nil
	}
	return &underlay.TunnelPath{Hops: hops}, "", nil
}

// pathHops turns form hops into the model's ordered chain. The order is kept as
// submitted: the page is what decides which hop is first, and re-sorting here
// would build a different route than the one the operator read.
func pathHops(specs []underlayHopSpec) []underlay.PathHop {
	out := make([]underlay.PathHop, 0, len(specs))
	for _, h := range specs {
		out = append(out, underlay.PathHop{NodeID: h.NodeID, Addr: strings.TrimSpace(h.Addr)})
	}
	return out
}

// serialMap names devices for a view. NodeView.Name is the serial the agent
// enrolled with (or "设备 N"), which is what the tenant sees everywhere else.
func serialMap(views map[uint16]underlay.NodeView) map[uint16]string {
	out := make(map[uint16]string, len(views))
	for id, v := range views {
		out[id] = v.Name
	}
	return out
}

func (s *server) handleAdminUnderlayTunnels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		s.saveUnderlayTunnel(w, r, id)
	case http.MethodDelete:
		s.deleteUnderlayTunnel(w, r, id)
	default:
		methodNotAllowed(w, "POST, PUT, DELETE")
	}
}

func (s *server) saveUnderlayTunnel(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req underlayTunnelRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	uc := s.underlayConfigLocked(id.TenantID, true)
	st := uc.State
	views := s.nodeViewsLocked(id.TenantID, now)

	if r.Method == http.MethodPost {
		if req.A.NodeID == 0 {
			http.Error(w, "本端需要指定设备", http.StatusBadRequest)
			return
		}
		path, created, err := s.tunnelPathLocked(st, req.Path, now)
		if err != nil {
			writeUnderlayErr(w, err)
			return
		}
		if len(req.Peers) > 0 {
			s.saveUnderlayFan(w, id, st, views, req, path, created, now)
			return
		}
		if req.B.NodeID == 0 {
			http.Error(w, "两端都需要指定设备", http.StatusBadRequest)
			return
		}
		tun, err := st.Create(underlay.CreateRequest{
			A:     underlay.EndpointSpec{NodeID: req.A.NodeID, Iface: req.A.Iface, Addr: strPtr(req.A.Addr), Port: req.A.Port},
			B:     underlay.EndpointSpec{NodeID: req.B.NodeID, Iface: req.B.Iface, Addr: strPtr(req.B.Addr), Port: req.B.Port},
			Path:  path,
			Color: u8Val(req.Color),
			Now:   now,
		}, views)
		if err != nil {
			if created != "" {
				_ = st.DeletePath(created)
			}
			writeUnderlayErr(w, err)
			return
		}
		s.save()
		s.appendOps("underlay.change", id.TenantID, 0, id.Username, "info",
			fmt.Sprintf("新建隧道 %s：设备 %d ↔ %d，颜色 %d", tun.ID, tun.A.NodeID, tun.B.NodeID, tun.Color))
		writeJSONStatus(w, http.StatusCreated, map[string]any{
			"ok":       true,
			"tunnel":   buildTunnelView(st, &tun, serialMap(views), views),
			"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
		})
		return
	}

	tunnelID := strings.TrimSpace(req.ID)
	if tunnelID == "" {
		tunnelID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if tunnelID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	upd := underlay.UpdateRequest{
		IfaceA: req.A.Iface, IfaceB: req.B.Iface,
		AddrA: req.A.Addr, AddrB: req.B.Addr,
		PortA: req.A.Port, PortB: req.B.Port,
		ClearPath: req.ClearPath,
		Color:     req.Color,
		Now:       now,
	}
	created := ""
	if !req.ClearPath {
		path, made, err := s.tunnelPathLocked(st, req.Path, now)
		if err != nil {
			writeUnderlayErr(w, err)
			return
		}
		upd.Path, created = path, made
	}
	tun, err := st.Update(tunnelID, upd, views)
	if err != nil {
		if created != "" {
			_ = st.DeletePath(created)
		}
		writeUnderlayErr(w, err)
		return
	}
	s.save()
	s.appendOps("underlay.change", id.TenantID, 0, id.Username, "info",
		fmt.Sprintf("修改隧道 %s（颜色 %d）", tun.ID, tun.Color))
	writeJSON(w, map[string]any{
		"ok":       true,
		"tunnel":   buildTunnelView(st, &tun, serialMap(views), views),
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

// saveUnderlayFan is the batch shape: one local end, one path, many peers.
//
// Every peer is committed or refused on its own (underlay.State.Fan), and the
// answer names both. That is the whole point of the batch: "从 A 经这条路径到
// B C D E" where D already has a tunnel is a success with one line of explanation,
// not a failed request that makes the operator start over.
//
// Must be called with s.mu held.
func (s *server) saveUnderlayFan(w http.ResponseWriter, id consoleIdentity, st *underlay.State,
	views map[uint16]underlay.NodeView, req underlayTunnelRequest, path *underlay.TunnelPath, createdTmpl string, now int64) {
	res, err := st.Fan(underlay.FanRequest{
		Local: underlay.EndpointSpec{NodeID: req.A.NodeID, Iface: req.A.Iface, Addr: strPtr(req.A.Addr), Port: req.A.Port},
		Peers: req.Peers,
		Path:  path,
		Color: u8Val(req.Color),
		Now:   now,
	}, views)
	if err != nil {
		if createdTmpl != "" {
			_ = st.DeletePath(createdTmpl)
		}
		writeUnderlayErr(w, err)
		return
	}
	// A "存为模板" that ended up on no tunnel at all is clutter nobody asked for.
	if len(res.Created) == 0 && createdTmpl != "" {
		_ = st.DeletePath(createdTmpl)
	}
	if len(res.Created) > 0 {
		s.save()
	}
	log.Printf("underlay: tenant=%s fanned tunnels from device %d to %d peer(s): %d created, %d skipped, batch %s by %s",
		id.TenantID, req.A.NodeID, len(req.Peers), len(res.Created), len(res.Skipped), res.BatchID, id.Username)

	serials := serialMap(views)
	tunnels := make([]underlayTunnelView, 0, len(res.Created))
	for i := range res.Created {
		t := res.Created[i]
		tunnels = append(tunnels, buildTunnelView(st, &t, serials, views))
	}
	// 200, not 201: a batch that skipped a peer is still a completed request, and
	// a caller that tests "status == 201" would read a partial success as a failure.
	writeJSON(w, map[string]any{
		"ok":       true,
		"batch_id": res.BatchID,
		"tunnels":  tunnels,
		"skipped":  res.Skipped,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

func (s *server) deleteUnderlayTunnel(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	tunnelID := strings.TrimSpace(r.URL.Query().Get("id"))
	if tunnelID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	uc := s.underlayConfigLocked(id.TenantID, true)
	if err := uc.State.Delete(tunnelID); err != nil {
		writeUnderlayErr(w, err)
		return
	}
	s.save()
	s.appendOps("underlay.change", id.TenantID, 0, id.Username, "info",
		fmt.Sprintf("删除隧道 %s", tunnelID))
	log.Printf("underlay: tenant=%s deleted tunnel %s by %s", id.TenantID, tunnelID, id.Username)
	writeJSON(w, map[string]any{
		"ok":       true,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

// --------------------------------------------------------------- auto batch

type underlayAutoRequest struct {
	Nodes     []uint16 `json:"nodes"`
	Mode      string   `json:"mode"` // full-mesh | hub-spoke
	Hub       uint16   `json:"hub,omitempty"`
	SpokeMode string   `json:"spoke_mode,omitempty"` // none | via-hub | hub-transit-only
}

// handleAdminUnderlayAutoPlan previews a batch. The preview is computed by the
// same allocator the apply uses, on a throwaway clone, so the ports and /32s
// shown are the ones the apply will hand out -- and nothing is committed here.
func (s *server) handleAdminUnderlayAutoPlan(w http.ResponseWriter, r *http.Request) {
	s.runUnderlayAuto(w, r, false)
}

// handleAdminUnderlayAutoApply commits a batch. Pairs that already have a
// tunnel are skipped, never duplicated: a second tunnel between two devices is
// a manual act.
func (s *server) handleAdminUnderlayAutoApply(w http.ResponseWriter, r *http.Request) {
	s.runUnderlayAuto(w, r, true)
}

func (s *server) runUnderlayAuto(w http.ResponseWriter, r *http.Request, apply bool) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var body underlayAutoRequest
	if !decodeJSONBody(w, r, &body) {
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	uc := s.underlayConfigLocked(id.TenantID, true)
	if !uc.State.Configured() {
		// The gate is stated as a conflict with the CURRENT state, not as a bad
		// request: the selection is fine, the prerequisite is missing.
		http.Error(w, "还没有发布全局配置：先保存并发布「全局配置」，中转端口随它一起下发", http.StatusConflict)
		return
	}
	req := underlay.AutoRequest{
		Nodes: body.Nodes, Mode: underlay.Mode(body.Mode), Hub: body.Hub,
		SpokeMode: underlay.SpokeMode(body.SpokeMode), Now: now,
	}
	views := s.nodeViewsLocked(id.TenantID, now)

	var plan underlay.Plan
	var err error
	if apply {
		req.BatchID = uc.State.NewBatchID()
		plan, err = uc.State.AutoApply(req, views)
	} else {
		plan, err = uc.State.AutoPlan(req, views)
	}
	if err != nil {
		writeUnderlayErr(w, err)
		return
	}
	out := map[string]any{
		"ok":   true,
		"plan": s.underlayAutoViewLocked(id.TenantID, req, plan, now, apply),
	}
	if apply {
		s.save()
		log.Printf("underlay: tenant=%s applied auto batch %s mode=%s (%d added, %d skipped) by %s",
			id.TenantID, req.BatchID, req.Mode, len(plan.Add), len(plan.Skip), id.Username)
		out["underlay"] = s.underlayViewLocked(id.TenantID, time.Now().Unix())
	}
	writeJSON(w, out)
}

// -------------------------------------------------------------------- paths

type underlayPathRequest struct {
	ID   string            `json:"id,omitempty"`
	Name string            `json:"name,omitempty"`
	Hops []underlayHopSpec `json:"hops"`
}

// checkPathHops rejects a chain that names a device the tenant does not have. The
// template itself is allowed to hold a device that is offline (it may come back),
// but not one that was never enrolled -- that is a typo, and a typo in a shared
// template moves every tunnel that uses it.
func checkPathHops(views map[uint16]underlay.NodeView, hops []underlay.PathHop) error {
	if len(hops) == 0 {
		return fmt.Errorf("路径需要至少一个中转设备")
	}
	for _, h := range hops {
		if _, ok := views[h.NodeID]; !ok {
			return fmt.Errorf("中转设备 %d 不存在或未入网", h.NodeID)
		}
	}
	return nil
}

func (s *server) handleAdminUnderlayPaths(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createUnderlayPath(w, r, id)
	case http.MethodPut:
		s.updateUnderlayPath(w, r, id)
	case http.MethodDelete:
		s.deleteUnderlayPath(w, r, id)
	default:
		methodNotAllowed(w, "POST, PUT, DELETE")
	}
}

func (s *server) createUnderlayPath(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req underlayPathRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	views := s.nodeViewsLocked(id.TenantID, now)
	hops := pathHops(req.Hops)
	if err := checkPathHops(views, hops); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uc := s.underlayConfigLocked(id.TenantID, true)
	tpl, err := uc.State.CreatePath(req.Name, hops, now)
	if err != nil {
		writeUnderlayErr(w, err)
		return
	}
	s.save()
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"ok":       true,
		"path":     buildTemplateView(uc.State, &tpl, serialMap(views)),
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

func (s *server) updateUnderlayPath(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req underlayPathRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	pathID := strings.TrimSpace(req.ID)
	if pathID == "" {
		pathID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if pathID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	views := s.nodeViewsLocked(id.TenantID, time.Now().Unix())
	hops := pathHops(req.Hops)
	if err := checkPathHops(views, hops); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uc := s.underlayConfigLocked(id.TenantID, true)
	if err := uc.State.UpdatePath(pathID, req.Name, hops); err != nil {
		writeUnderlayErr(w, err)
		return
	}
	s.save()
	// The fan-out is returned WITH the result: editing a shared template moves
	// every tunnel that uses it, and the page must be able to say how many.
	users := uc.State.PathUsers(pathID)
	out := map[string]any{
		"ok":       true,
		"users":    users,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	}
	if tpl := uc.State.Paths[pathID]; tpl != nil {
		out["path"] = buildTemplateView(uc.State, tpl, serialMap(views))
	}
	writeJSON(w, out)
}

func (s *server) deleteUnderlayPath(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	pathID := strings.TrimSpace(r.URL.Query().Get("id"))
	if pathID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	uc := s.underlayConfigLocked(id.TenantID, true)
	users := uc.State.PathUsers(pathID)
	if err := uc.State.DeletePath(pathID); err != nil {
		if errors.Is(err, underlay.ErrPathInUse) {
			// Name the users: "still in use" alone leaves the tenant hunting for
			// a tunnel they may not even know references this template.
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": err.Error(), "users": users,
			})
			return
		}
		writeUnderlayErr(w, err)
		return
	}
	s.save()
	writeJSON(w, map[string]any{
		"ok":       true,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}

// ---------------------------------------------------------------- topology

func (s *server) handleAdminUnderlayTopology(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	s.mu.Lock()
	v := s.underlayTopologyLocked(id.TenantID, time.Now().Unix())
	s.mu.Unlock()
	writeJSON(w, v)
}
