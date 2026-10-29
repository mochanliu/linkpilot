package main

// The site endpoints: who may write a location, and what the map is allowed to
// assume about a device that has none.
//
// Tenant-only, like the rest of the underlay layer -- the operator role manages
//归属 and seat counts, not where a customer's building is. Coordinates are
// entered by hand, deliberately: nothing here measures a position, and a device
// that has never been placed is reported as 未定位 rather than drawn somewhere.

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type siteWriteRequest struct {
	ID   string   `json:"id,omitempty"`
	Name string   `json:"name"`
	Lon  *float64 `json:"lon"`
	Lat  *float64 `json:"lat"`
}

type siteIDRequest struct {
	ID string `json:"id"`
}

type nodeSiteRequest struct {
	NodeID uint16 `json:"node_id"`
	// SiteID empty clears the device's location: "unknown" is a state the page
	// can show, not an error to reject.
	SiteID string `json:"site_id"`
}

// sitesResponse is the shape every write returns. The full list comes back
// because a device count per site is exactly what changes when a device is
// placed or a site is edited, and the page should not need a second round trip
// to stop showing a stale number.
func (s *server) sitesResponseLocked(w http.ResponseWriter, tenant string, extra map[string]any) {
	out := map[string]any{
		"ok":                true,
		"sites":             s.tenantSitesLocked(tenant),
		"unplaced_nodes":    s.unplacedNodesLocked(tenant),
		"unplaced_preauths": s.unplacedPreAuthsLocked(tenant),
	}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}

// handleAdminUnderlaySites: GET lists, POST creates, PUT edits.
func (s *server) handleAdminUnderlaySites(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, map[string]any{
			"sites":             s.tenantSitesLocked(id.TenantID),
			"unplaced_nodes":    s.unplacedNodesLocked(id.TenantID),
			"unplaced_preauths": s.unplacedPreAuthsLocked(id.TenantID),
			"now_unix":          time.Now().Unix(),
		})
		return
	case http.MethodPost, http.MethodPut:
	default:
		methodNotAllowed(w, "GET, POST, PUT")
		return
	}

	var req siteWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validSiteName(name) {
		http.Error(w, "站点名需要 1-80 个字符，且不能含控制字符", http.StatusBadRequest)
		return
	}
	// Both halves must arrive. A missing coordinate is rejected rather than read
	// as 0, because (0,0) is a real ocean position that would look plausible.
	if req.Lon == nil || req.Lat == nil {
		http.Error(w, "必须同时提供经度与纬度", http.StatusBadRequest)
		return
	}
	if err := validSiteCoords(*req.Lon, *req.Lat); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()

	if r.Method == http.MethodPost {
		if strings.TrimSpace(req.ID) != "" {
			http.Error(w, "新建站点不应带 id", http.StatusBadRequest)
			return
		}
		si := &Site{ID: newSiteID(), TenantID: id.TenantID, Name: name,
			Lon: *req.Lon, Lat: *req.Lat, CreatedAt: now}
		for s.state.Sites[si.ID] != nil {
			si.ID = newSiteID()
		}
		s.state.Sites[si.ID] = si
		s.save()
		log.Printf("console: tenant %s created site %s (%s)", id.TenantID, si.ID, si.Name)
		s.sitesResponseLocked(w, id.TenantID, map[string]any{"site": siteViewOf(si, 0)})
		return
	}

	si := s.siteLocked(id.TenantID, strings.TrimSpace(req.ID))
	if si == nil {
		http.Error(w, "站点不存在", http.StatusNotFound)
		return
	}
	si.Name, si.Lon, si.Lat = name, *req.Lon, *req.Lat
	si.UpdatedAt = now
	s.save()
	log.Printf("console: tenant %s updated site %s (%s)", id.TenantID, si.ID, si.Name)
	s.sitesResponseLocked(w, id.TenantID, map[string]any{"site": s.siteViewLocked(si)})
}

// handleAdminUnderlaySiteDelete refuses while devices still stand in the site.
// Deleting a location out from under a placed device would silently turn a
// mapped box into an unplaced one, and the page would show fewer devices on the
// map with no explanation. The tenant has to move the devices first.
func (s *server) handleAdminUnderlaySiteDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req siteIDRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	siteID := strings.TrimSpace(req.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.siteLocked(id.TenantID, siteID)
	if si == nil {
		http.Error(w, "站点不存在", http.StatusNotFound)
		return
	}
	if used := s.siteDevicesLocked(siteID); used > 0 {
		http.Error(w, fmt.Sprintf("该站点还有 %d 台设备，先把它们移到别的站点或清掉位置", used),
			http.StatusConflict)
		return
	}
	if held := s.sitePreAuthsLocked(siteID); held > 0 {
		http.Error(w, fmt.Sprintf("该站点还有 %d 台预授权设备（还没到货，但位置已经定了），先撤销或改到别的站点", held),
			http.StatusConflict)
		return
	}
	delete(s.state.Sites, si.ID)
	s.save()
	log.Printf("console: tenant %s deleted site %s (%s)", id.TenantID, si.ID, si.Name)
	s.sitesResponseLocked(w, id.TenantID, nil)
}

// handleAdminUnderlayNodeSite places one device in a site (or clears it).
func (s *server) handleAdminUnderlayNodeSite(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var req nodeSiteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	siteID := strings.TrimSpace(req.SiteID)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodeByIDLocked(id.TenantID, req.NodeID)
	if n == nil {
		http.Error(w, "设备不存在", http.StatusNotFound)
		return
	}
	if siteID != "" {
		si := s.siteLocked(id.TenantID, siteID)
		if si == nil {
			// Same answer as a site that never existed: this endpoint must not
			// reveal which tenant owns which site.
			http.Error(w, "站点不存在", http.StatusNotFound)
			return
		}
		siteID = si.ID
	}
	n.SiteID = siteID
	s.save()
	s.sitesResponseLocked(w, id.TenantID, map[string]any{
		"node_id": n.NodeID, "site_id": n.SiteID,
	})
}

// ------------------------------------------------------------- small helpers

// siteViewOf lets a freshly created row be rendered without a tenant lookup.
func siteViewOf(si *Site, devices int) underlaySiteView {
	return underlaySiteView{
		ID: si.ID, Name: si.Name, Lon: si.Lon, Lat: si.Lat,
		Devices: devices, CreatedAt: si.CreatedAt, UpdatedAt: si.UpdatedAt,
	}
}

func (s *server) siteViewLocked(si *Site) underlaySiteView {
	return underlaySiteView{
		ID: si.ID, Name: si.Name, Lon: si.Lon, Lat: si.Lat,
		Devices: s.siteDevicesLocked(si.ID), PreAuths: s.sitePreAuthsLocked(si.ID),
		CreatedAt: si.CreatedAt, UpdatedAt: si.UpdatedAt,
	}
}

// sitePreAuthsLocked counts the on-paper devices standing in a site. They are
// deliberately not added into Devices -- the map draws boxes that exist, and a
// site row saying "3 台设备" over two dots is a contradiction this console already
// refuses to render. They do, however, hold the location: deleting a site out
// from under one would silently turn a planned placement into 未定位.
func (s *server) sitePreAuthsLocked(siteID string) int {
	if siteID == "" {
		return 0
	}
	n := 0
	for _, p := range s.state.PreAuths {
		if p != nil && p.SiteID == siteID && !p.claimed() {
			n++
		}
	}
	return n
}

func (s *server) siteDevicesLocked(siteID string) int {
	if siteID == "" {
		return 0
	}
	n := 0
	for _, rec := range s.state.Nodes {
		if rec != nil && rec.SiteID == siteID {
			n++
		}
	}
	return n
}

// unplacedNodesLocked counts the tenant's devices with no usable site -- either
// none named, or one that no longer belongs to this tenant.
func (s *server) unplacedNodesLocked(tenant string) int {
	n := 0
	for _, rec := range s.state.Nodes {
		if rec == nil || rec.TenantID != tenant {
			continue
		}
		if s.siteLocked(tenant, rec.SiteID) == nil {
			n++
		}
	}
	return n
}

func (s *server) nodeByIDLocked(tenant string, nodeID uint16) *NodeRecord {
	for _, rec := range s.state.Nodes {
		if rec != nil && rec.TenantID == tenant && rec.NodeID == nodeID {
			return rec
		}
	}
	return nil
}

// unplacedPreAuthsLocked counts the tenant's on-paper devices with no location
// the map could use. Claimed records are excluded: their placement moved onto the
// node, so counting them here would double-report one box.
func (s *server) unplacedPreAuthsLocked(tenant string) int {
	n := 0
	for _, p := range s.state.PreAuths {
		if p == nil || p.TenantID != tenant || p.claimed() {
			continue
		}
		if s.siteLocked(tenant, p.SiteID) == nil {
			n++
		}
	}
	return n
}
