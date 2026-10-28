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

type underlayPathSpec struct {
	TemplateID string `json:"template_id,omitempty"`
	NodeID     uint16 `json:"node_id,omitempty"`
	Addr       string `json:"addr,omitempty"`
	// SaveAs names a NEW template for the inline hop ("存为模板"). It is only
	// valid together with node_id and without template_id.
	SaveAs string `json:"save_as,omitempty"`
}

type underlayTunnelRequest struct {
	// ID is the tunnel to update/delete. Endpoint node_ids are ignored on update:
	// a tunnel's two devices are what it IS, not a field to retarget.
	ID   string            `json:"id,omitempty"`
	A    underlaySpec      `json:"a"`
	B    underlaySpec      `json:"b"`
	Path *underlayPathSpec `json:"path,omitempty"`
	// ClearPath turns a relayed tunnel into a direct one; a nil path with
	// ClearPath=false keeps the current path.
	ClearPath bool `json:"clear_path,omitempty"`
}

func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
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
	if ps.TemplateID != "" {
		if ps.NodeID != 0 || strings.TrimSpace(ps.Addr) != "" || strings.TrimSpace(ps.SaveAs) != "" {
			return nil, "", fmt.Errorf("路径模板不能和 inline 中转设备/地址/存为模板同时填写")
		}
		return &underlay.TunnelPath{TemplateID: ps.TemplateID}, "", nil
	}
	if ps.NodeID == 0 {
		return nil, "", fmt.Errorf("路径需要指定一个中转设备或引用一个路径模板")
	}
	addr := strings.TrimSpace(ps.Addr)
	if name := strings.TrimSpace(ps.SaveAs); name != "" {
		tpl, err := st.CreatePath(name, ps.NodeID, addr, now)
		if err != nil {
			return nil, "", err
		}
		return &underlay.TunnelPath{TemplateID: tpl.ID}, tpl.ID, nil
	}
	return &underlay.TunnelPath{NodeID: ps.NodeID, Addr: addr}, "", nil
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
		if req.A.NodeID == 0 || req.B.NodeID == 0 {
			http.Error(w, "两端都需要指定设备", http.StatusBadRequest)
			return
		}
		path, created, err := s.tunnelPathLocked(st, req.Path, now)
		if err != nil {
			writeUnderlayErr(w, err)
			return
		}
		tun, err := st.Create(underlay.CreateRequest{
			A:    underlay.EndpointSpec{NodeID: req.A.NodeID, Iface: req.A.Iface, Addr: strPtr(req.A.Addr), Port: req.A.Port},
			B:    underlay.EndpointSpec{NodeID: req.B.NodeID, Iface: req.B.Iface, Addr: strPtr(req.B.Addr), Port: req.B.Port},
			Path: path,
			Now:  now,
		}, views)
		if err != nil {
			if created != "" {
				_ = st.DeletePath(created)
			}
			writeUnderlayErr(w, err)
			return
		}
		s.save()
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
	writeJSON(w, map[string]any{
		"ok":       true,
		"tunnel":   buildTunnelView(st, &tun, serialMap(views), views),
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
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	NodeID uint16 `json:"node_id"`
	Addr   string `json:"addr,omitempty"`
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
	if _, ok := views[req.NodeID]; !ok {
		http.Error(w, "中转设备不存在或未入网", http.StatusBadRequest)
		return
	}
	uc := s.underlayConfigLocked(id.TenantID, true)
	tpl, err := uc.State.CreatePath(req.Name, req.NodeID, req.Addr, now)
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
	if _, ok := views[req.NodeID]; !ok {
		http.Error(w, "中转设备不存在或未入网", http.StatusBadRequest)
		return
	}
	uc := s.underlayConfigLocked(id.TenantID, true)
	if err := uc.State.UpdatePath(pathID, req.Name, req.NodeID, req.Addr); err != nil {
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
