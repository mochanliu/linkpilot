package main

// The overlay HTTP surface, tenant-side only.
//
// Same gate and same discipline as the underlay layer: requireTenant before
// anything is read or written, one mutation under s.mu, then a whole-state save.
//
// There is no publish step here, on purpose. A 导通组 and the two reflector
// slots are inputs to the device config's rendered artifacts, and publishing the
// device config is the single act that changes what a box runs (O6-b puts
// route.json into that bundle). Saving either therefore changes what the NEXT
// publish renders and leaves the currently installed bundle alone -- which is
// exactly the draft/publish split the basic config already uses, and why
// "删除一个组" cannot silently cut anyone's routes.

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// overlayGroupRequest is the body for create and edit. RT is absent: the
// controller owns it (定案 6), and accepting one here would let two tenants -- or
// one tenant twice -- hand-write the same value and collide.
type overlayGroupRequest struct {
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name"`
	Members []OverlayMember `json:"members"`
}

// overlayDeviceRRRequest is one device's row of RR 1 / RR 2, the way the 导通策略
// table edits it: a column of one device, not a list to replace. An empty id
// clears the slot, and clearing both means the device goes back to "no rrs key".
type overlayDeviceRRRequest struct {
	NodeID  uint16 `json:"node_id"`
	Primary string `json:"primary"`
	Backup  string `json:"backup"`
}

func (s *server) handleAdminOverlay(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	s.mu.Lock()
	v := s.overlayViewLocked(id.TenantID, time.Now().Unix())
	s.mu.Unlock()
	writeJSON(w, v)
}

// handleAdminOverlaySettings saves the global block of the overlay page. It is one
// small document rather than one endpoint per field so the page can save what it
// shows in one write; 导通策略 and the per-device RR 1 / RR 2 columns have their
// own endpoints because they carry their own checks, and the RR list itself is
// edited through /api/rrs.
//
// A field that is absent is left alone.
func (s *server) handleAdminOverlaySettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var req struct {
		EscapeAllColors *bool `json:"escape_all_colors"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.EscapeAllColors == nil {
		http.Error(w, "escape_all_colors 没给：这次保存没有要改的东西", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	oc := s.overlayConfigLocked(id.TenantID, true)
	oc.EscapeAllColors = *req.EscapeAllColors
	oc.UpdatedAt = time.Now().Unix()
	oc.UpdatedBy = id.Username
	s.save()
	log.Printf("overlay: tenant=%s escape_all_colors=%t by %s",
		id.TenantID, oc.EscapeAllColors, id.Username)
	writeJSON(w, map[string]any{
		"ok":      true,
		"overlay": s.overlayViewLocked(id.TenantID, time.Now().Unix()),
		"settings": map[string]any{
			"escape_all_colors": oc.EscapeAllColors,
		},
	})
}

// handleAdminOverlayDeviceRR writes one device's RR 1 / RR 2. It replaces both
// slots at once because the page edits them side by side and "which one is RR 1"
// is part of the answer -- a per-slot PATCH would let a page leave a row where
// both columns point at the same row.
//
// Clearing both slots deletes the entry rather than storing two empty strings:
// "no entry" is exactly the state that renders no rrs key, and keeping a row of
// empties would make the map grow one entry per device that ever had a choice.
func (s *server) handleAdminOverlayDeviceRR(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var req overlayDeviceRRRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		http.Error(w, "node_id required", http.StatusBadRequest)
		return
	}
	b := RRBind{
		Primary: strings.TrimSpace(req.Primary),
		Backup:  strings.TrimSpace(req.Backup),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if msg := s.rrBindErrorLocked(id.TenantID, req.NodeID, b); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	oc := s.overlayConfigLocked(id.TenantID, true)
	now := time.Now().Unix()
	if b.Primary == "" && b.Backup == "" {
		delete(oc.RRBinds, strconv.Itoa(int(req.NodeID)))
	} else {
		oc.RRBinds[strconv.Itoa(int(req.NodeID))] = b
	}
	oc.UpdatedAt = now
	oc.UpdatedBy = id.Username
	s.save()
	log.Printf("overlay: tenant=%s device %d rrs=%q/%q by %s",
		id.TenantID, req.NodeID, b.Primary, b.Backup, id.Username)
	writeJSON(w, map[string]any{
		"ok":      true,
		"node_id": req.NodeID,
		"bind":    b,
		"overlay": s.overlayViewLocked(id.TenantID, now),
	})
}

func (s *server) handleAdminOverlayGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createOverlayGroup(w, r, id)
	case http.MethodPut:
		s.updateOverlayGroup(w, r, id)
	case http.MethodDelete:
		s.deleteOverlayGroup(w, r, id)
	default:
		methodNotAllowed(w, "POST, PUT, DELETE")
	}
}

// createOverlayGroup allocates the RT and stores the group. The checks run
// against the candidate, and any error blocks the save -- a group whose members
// do not exist would render a route.json the device refuses, and the device has
// no way to say which group caused it.
func (s *server) createOverlayGroup(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req overlayGroupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	oc := s.overlayConfigLocked(id.TenantID, true)
	if len(oc.Groups) >= overlayMaxGroups {
		http.Error(w, "导通策略数量已达上限", http.StatusConflict)
		return
	}
	g := OverlayGroup{
		Name:    req.Name,
		Members: req.Members,
	}
	normalizeGroup(&g)
	checks := s.overlayGroupChecksLocked(id.TenantID, g, "")
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": msg, "checks": checks})
		return
	}
	rt, err := oc.allocateRT()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	g.ID = oc.nextGroupID()
	g.RT = rt
	g.CreatedAt = now
	g.UpdatedAt = now
	oc.Groups = append(oc.Groups, g)
	oc.UpdatedAt = now
	oc.UpdatedBy = id.Username
	s.save()
	log.Printf("overlay: tenant=%s created group %s rt=%s members=%d by %s",
		id.TenantID, g.ID, g.RT, len(g.Members), id.Username)
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"ok":      true,
		"group":   g,
		"overlay": s.overlayViewLocked(id.TenantID, now),
	})
}

// updateOverlayGroup replaces the name and the member set. The RT is carried
// over from the stored group and never reallocated: the routes already announced
// under the old value stay importable by the members that remain, and a device
// that leaves the group loses the RT because nothing references it any more, not
// because this handler cleaned anything up.
func (s *server) updateOverlayGroup(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req overlayGroupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	groupID := strings.TrimSpace(req.ID)
	if groupID == "" {
		groupID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if groupID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	oc := s.overlayConfigLocked(id.TenantID, false)
	if oc == nil {
		http.Error(w, "导通策略不存在", http.StatusNotFound)
		return
	}
	existing := oc.group(groupID)
	if existing == nil {
		http.Error(w, "导通策略不存在", http.StatusNotFound)
		return
	}
	g := OverlayGroup{ID: existing.ID, Name: req.Name, RT: existing.RT, Members: req.Members}
	normalizeGroup(&g)
	checks := s.overlayGroupChecksLocked(id.TenantID, g, groupID)
	if msg := hasBlockingCheck(checks); msg != "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": msg, "checks": checks})
		return
	}
	g.CreatedAt = existing.CreatedAt
	g.UpdatedAt = now
	*existing = g
	oc.UpdatedAt = now
	oc.UpdatedBy = id.Username
	s.save()
	log.Printf("overlay: tenant=%s updated group %s rt=%s members=%d by %s",
		id.TenantID, g.ID, g.RT, len(g.Members), id.Username)
	writeJSON(w, map[string]any{
		"ok":      true,
		"group":   g,
		"overlay": s.overlayViewLocked(id.TenantID, now),
	})
}

func (s *server) deleteOverlayGroup(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	groupID := strings.TrimSpace(r.URL.Query().Get("id"))
	if groupID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	oc := s.overlayConfigLocked(id.TenantID, false)
	if oc == nil {
		http.Error(w, "导通策略不存在", http.StatusNotFound)
		return
	}
	idx := -1
	for i := range oc.Groups {
		if oc.Groups[i].ID == groupID {
			idx = i
			break
		}
	}
	if idx < 0 {
		http.Error(w, "导通策略不存在", http.StatusNotFound)
		return
	}
	removed := oc.Groups[idx]
	oc.Groups = append(oc.Groups[:idx], oc.Groups[idx+1:]...)
	if oc.Groups == nil {
		oc.Groups = []OverlayGroup{}
	}
	oc.UpdatedAt = now
	oc.UpdatedBy = id.Username
	s.save()
	// The RT is not returned to the pool: see OverlayConfig.NextRT.
	log.Printf("overlay: tenant=%s deleted group %s rt=%s by %s",
		id.TenantID, removed.ID, removed.RT, id.Username)
	writeJSON(w, map[string]any{
		"ok":      true,
		"deleted": removed.ID,
		"overlay": s.overlayViewLocked(id.TenantID, now),
	})
}
