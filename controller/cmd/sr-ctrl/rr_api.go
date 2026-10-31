package main

// The RR list's HTTP surface, tenant-side (2026-11-01 定案: an RR belongs to one
// tenant, so its tenant is the only one who manages it).
//
// Reads do not live here. A tenant sees its list inside GET /api/overlay
// (overlayView.RRLibrary), together with the RR groups that name those rows --
// filtered by ownership in one place, so "the page showed it" and "the device will
// be told about it" cannot drift apart.
//
// Nothing here publishes anything. A row is an input to the 4th artifact, so an
// edit changes what the NEXT publish renders for every device of this tenant, and
// a box that is running now keeps the addresses it already has until that publish
// lands.

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// rrWriteRequest is the body for create and edit. The whole row is sent both
// times: a page that shows a row lets you change any part of it, and a PATCH-like
// "only what changed" contract would mean a field the page forgot to send keeps a
// value nobody is looking at.
//
// There is no 归属 field: the row belongs to the session's tenant, full stop, and
// accepting one from the body would be a second, forgeable source for the one
// thing that decides whose devices dial this reflector.
type rrWriteRequest struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
}

type rrIDRequest struct {
	ID string `json:"id"`
}

// rrChangeNote is what every write answers with, because the two facts in it are
// exactly the ones a tenant is going to ask about after adding a reflector: who
// is affected, and when it takes effect.
const rrChangeNote = "改动只影响下一次发布：设备要在「配置 › 基础配置」发布以后才会换成新的 RR 列表。" +
	"控制器不会去连这些地址，所以这里没有任何连通性结论可给。"

// rrResponseLocked writes the tenant's whole list back after every mutation. The
// list comes back rather than just the touched row because a delete guard and a
// renamed id are exactly the things one edit changes for the other rows.
func (s *server) rrResponseLocked(w http.ResponseWriter, tenantID string, extra map[string]any) {
	out := map[string]any{
		"ok":       true,
		"rrs":      s.rrRowsLocked(tenantID),
		"max_rrs":  rrMaxPerTenant,
		"note":     rrChangeNote,
		"now_unix": time.Now().Unix(),
	}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}

// handleTenantRRs: GET lists this tenant's rows, POST adds one, PUT edits one.
func (s *server) handleTenantRRs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.rrResponseLocked(w, id.TenantID, nil)
		return
	case http.MethodPost, http.MethodPut:
	default:
		methodNotAllowed(w, "GET, POST, PUT")
		return
	}

	var req rrWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	addr := strings.TrimSpace(req.Addr)
	location := strings.TrimSpace(req.Location)
	if err := validRRName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validRRAddr(addr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validRRLocation(location); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lib := s.rrLibraryLocked(true)
	now := time.Now().Unix()
	rows := lib.visibleTo(id.TenantID)

	// One address, one session. Two rows with the same host:port would make the
	// device dial the same reflector twice and hear every prefix twice, which
	// looks like a second path and is not one. Checked on both create and edit,
	// against the rows that will remain.
	addrTaken := func(exceptID string) bool {
		for _, it := range rows {
			if it.ID != exceptID && it.Addr == addr {
				return true
			}
		}
		return false
	}

	if r.Method == http.MethodPost {
		if strings.TrimSpace(req.ID) != "" {
			http.Error(w, "新建 RR 不应带 id", http.StatusBadRequest)
			return
		}
		if len(rows) >= rrMaxPerTenant {
			// The list is also the default per-device fan-out, so the cap is not a
			// table limit -- it is a session count on every device of this tenant.
			http.Error(w, fmt.Sprintf("本租户已有 %d 条 RR，达到了上限：没被反射器分组点名的设备会拨每一条，"+
				"再多只会加重每台设备的会话数", rrMaxPerTenant), http.StatusConflict)
			return
		}
		if addrTaken("") {
			http.Error(w, fmt.Sprintf("本租户已经有一条 RR 用的是 %s：一台盒子两个身份会把同一份路由反射两遍", addr),
				http.StatusBadRequest)
			return
		}
		it := RRItem{ID: lib.nextID(), Name: name, Addr: addr, Location: location,
			TenantID: id.TenantID, CreatedAt: now}
		lib.Items = append(lib.Items, it)
		s.rrTouchLocked(lib, id.Username, now)
		s.save()
		log.Printf("rrlib: tenant=%s created %s addr=%s by %s", id.TenantID, it.ID, it.Addr, id.Username)
		s.rrResponseLocked(w, id.TenantID, map[string]any{"rr": s.rrRowOfLocked(it)})
		return
	}

	targetID := strings.TrimSpace(req.ID)
	it := lib.get(targetID)
	if it == nil || it.TenantID != id.TenantID {
		// Same answer as a row that never existed: whether another tenant has a row
		// with this id is not something this tenant may learn by probing.
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	if addrTaken(targetID) {
		http.Error(w, fmt.Sprintf("本租户已经有一条 RR 用的是 %s：一台盒子两个身份会把同一份路由反射两遍", addr),
			http.StatusBadRequest)
		return
	}
	before := *it
	it.Name, it.Addr, it.Location = name, addr, location
	it.UpdatedAt = now
	// The address is what the device dials, so a change here is the one edit that
	// is worth a log line even when nothing else moved.
	addrChanged := before.Addr != it.Addr
	s.rrTouchLocked(lib, id.Username, now)
	s.save()
	log.Printf("rrlib: tenant=%s updated %s addr=%s addrChanged=%t by %s",
		id.TenantID, it.ID, it.Addr, addrChanged, id.Username)
	s.rrResponseLocked(w, id.TenantID, map[string]any{
		"rr":           s.rrRowOfLocked(*it),
		"addr_changed": addrChanged,
	})
}

// handleTenantRRDelete refuses while one of this tenant's RR groups still names
// the row (拍板 E5). The alternative -- letting the delete through and silently
// dropping the group's choice -- would leave that group's devices rendering no
// rrs at all, and "my box has no reflector" is a far worse surprise than a delete
// button that explains itself.
func (s *server) handleTenantRRDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req rrIDRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	targetID := strings.TrimSpace(req.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	lib := s.rrLibraryLocked(false)
	it := lib.get(targetID)
	if it == nil || it.TenantID != id.TenantID {
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	if refs := s.rrGroupRefsLocked(id.TenantID, targetID); len(refs) > 0 {
		names := make([]string, 0, len(refs))
		for _, g := range refs {
			names = append(names, g.Name)
		}
		http.Error(w, fmt.Sprintf("这条 RR 还被 %d 个反射器分组选着（%s）：先把这些分组改成别的 RR，再来删",
			len(refs), strings.Join(names, "、")), http.StatusConflict)
		return
	}
	idx := indexOfRRLocked(lib, targetID)
	// Copy before the splice: lib.Items is one array, so shifting the tail left
	// moves a *different* row onto this index, and a pointer taken before the delete
	// would name the wrong RR in the log.
	removed := lib.Items[idx]
	// The id is not handed back to the counter: lib.NextID keeps climbing, so a
	// row that appears later is rr<n+1> and never a resurrected rr<n>. That matters
	// because an RR group's slot is a plain string that outlives nothing else.
	lib.Items = append(lib.Items[:idx], lib.Items[idx+1:]...)
	s.rrTouchLocked(lib, id.Username, time.Now().Unix())
	s.save()
	log.Printf("rrlib: tenant=%s deleted %s addr=%s by %s", id.TenantID, removed.ID, removed.Addr, id.Username)
	s.rrResponseLocked(w, id.TenantID, map[string]any{"deleted": targetID})
}

func indexOfRRLocked(lib *RRLibrary, id string) int {
	for i := range lib.Items {
		if lib.Items[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *server) rrTouchLocked(lib *RRLibrary, by string, now int64) {
	lib.UpdatedAt = now
	lib.UpdatedBy = by
}
