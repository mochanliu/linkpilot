package main

// A Site is one physical location a tenant's devices sit in. It exists because a
// customer's branch box is not alone: a site usually holds an active CPE, its
// standby, sometimes a relay-only box, all at the same coordinates. Storing the
// position per device would mean typing the same numbers N times and nothing
// would stop those copies from drifting apart, so the position belongs to the
// site and a device only references it.
//
// This is CONSOLE-ONLY material. The dataplane routes on addresses and ports and
// has no use for a coordinate, so a site never enters /v1/underlay/bundle and is
// never pushed to a device. Its whole job is to let the topology be drawn on a
// map instead of a circle.

import (
	"errors"
	"math"
	"sort"
)

const siteNameMaxRunes = 80

// Site is one persisted row. Lon/Lat are degrees, WGS84: the basemap is drawn
// with the same Natural Earth I forward projection the console applies to them,
// so a coordinate stored here lands where the customer actually is.
type Site struct {
	ID        string  `json:"id"`
	TenantID  string  `json:"tenant_id"`
	Name      string  `json:"name"`
	Lon       float64 `json:"lon"`
	Lat       float64 `json:"lat"`
	CreatedAt int64   `json:"created_at_unix"`
	UpdatedAt int64   `json:"updated_at_unix,omitempty"`
}

// underlaySiteView is a site plus how many of the tenant's devices stand in it.
// The count is derived, never stored: it is exactly the thing that goes stale
// when a device is enrolled or deleted somewhere else.
type underlaySiteView struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Lon       float64 `json:"lon"`
	Lat       float64 `json:"lat"`
	Devices   int     `json:"devices"`
	CreatedAt int64   `json:"created_at_unix"`
	UpdatedAt int64   `json:"updated_at_unix,omitempty"`
	// PreAuths is how many 预授权 records stand here with no device yet. It is a
	// separate number, not added into Devices, because the map only draws boxes
	// that exist: a site page that said "3 台设备" over 2 dots would be the kind of
	// contradiction this console already refuses to render.
	PreAuths int `json:"preauths"`
}

func validSiteName(name string) bool {
	r := []rune(name)
	if len(r) < 1 || len(r) > siteNameMaxRunes {
		return false
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// validSiteCoords rejects anything the map could not draw. NaN and +-Inf are the
// reason this is not just a range check: both pass a naive `lon < -180 || lon >
// 180` test and produce a point at nowhere that SVG silently drops.
func validSiteCoords(lon, lat float64) error {
	if math.IsNaN(lon) || math.IsNaN(lat) || math.IsInf(lon, 0) || math.IsInf(lat, 0) {
		return errors.New("lon/lat must be a finite number")
	}
	if lon < -180 || lon > 180 {
		return errors.New("lon must be within [-180, 180]")
	}
	if lat < -90 || lat > 90 {
		return errors.New("lat must be within [-90, 90]")
	}
	return nil
}

// siteLocked returns the tenant's site with that id, or nil. A site id from
// another tenant is indistinguishable from one that never existed -- the caller
// answers 404, not 403, so this endpoint cannot be used to probe who owns what.
func (s *server) siteLocked(tenant, id string) *Site {
	si := s.state.Sites[id]
	if si == nil || si.TenantID != tenant {
		return nil
	}
	return si
}

// tenantSitesLocked lists the tenant's sites with device counts, sorted by name
// so the page's dropdown has a stable order.
func (s *server) tenantSitesLocked(tenant string) []underlaySiteView {
	counts := map[string]int{}
	for _, n := range s.state.Nodes {
		if n != nil && n.TenantID == tenant && n.SiteID != "" {
			counts[n.SiteID]++
		}
	}
	// An unclaimed record still occupies the building the admin chose for it; a
	// claimed one lives on the node now, so it must not be counted twice.
	paCounts := map[string]int{}
	for _, p := range s.state.PreAuths {
		if p != nil && p.TenantID == tenant && p.SiteID != "" && !p.claimed() {
			paCounts[p.SiteID]++
		}
	}
	out := []underlaySiteView{}
	for _, si := range s.state.Sites {
		if si == nil || si.TenantID != tenant {
			continue
		}
		out = append(out, underlaySiteView{
			ID: si.ID, Name: si.Name, Lon: si.Lon, Lat: si.Lat,
			Devices: counts[si.ID], PreAuths: paCounts[si.ID],
			CreatedAt: si.CreatedAt, UpdatedAt: si.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// normalizeSites drops rows that can no longer mean anything: a site whose
// tenant is gone, and a device pointing at a site that does not exist or belongs
// to somebody else. It runs at load, so a state file hand-edited or restored
// from an older tenant list cannot leave a device placed at another customer's
// coordinates.
func normalizeSites(st *State) bool {
	if st.Sites == nil {
		st.Sites = map[string]*Site{}
	}
	changed := false
	for id, si := range st.Sites {
		if si == nil || id == "" || si.ID == "" {
			delete(st.Sites, id)
			changed = true
			continue
		}
		if _, ok := st.Tenants[si.TenantID]; !ok {
			delete(st.Sites, id)
			changed = true
			continue
		}
		if si.ID != id {
			// Keyed by id; a mismatch means the file was edited. Trust the key,
			// because every reference (node.site_id) was read as this key.
			si.ID = id
			changed = true
		}
	}
	for _, n := range st.Nodes {
		if n == nil || n.SiteID == "" {
			continue
		}
		si := st.Sites[n.SiteID]
		if si == nil || si.TenantID != n.TenantID {
			n.SiteID = ""
			changed = true
		}
	}
	return changed
}
