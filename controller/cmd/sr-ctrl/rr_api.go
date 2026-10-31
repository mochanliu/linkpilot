package main

// The RR library's HTTP surface: operator-only writes, and the delete guard that
// makes the library safe to change while tenants are pointing at it.
//
// Tenant-facing reads do not live here. A tenant sees the rows it may pick inside
// GET /api/overlay (overlayView.RRLibrary), filtered by ownership in one place --
// a second endpoint would be a second filter to keep in sync, and the one thing
// this table must never do is leak which rows belong to another customer.
//
// Nothing here publishes anything. A row is an input to the 4th artifact, so an
// edit changes what the NEXT publish renders for every tenant that selected it,
// and the device that is running now keeps the address it already has until that
// publish lands and its route process restarts.

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
type rrWriteRequest struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
	// TenantID is the row's 归属 and is required: there is no shared tier, so a
	// row without a tenant is a row nobody can pick. An orphaned row (its tenant
	// was deleted) has to be re-bound to a live tenant on the next save -- there
	// is no "keep it as it was" value left to send.
	TenantID string `json:"owner"`
}

type rrIDRequest struct {
	ID string `json:"id"`
}

// rrChangeNote is what every write answers with, because the two facts in it are
// exactly the ones an operator is going to ask about after changing an address:
// who is affected, and when it takes effect.
const rrChangeNote = "改动只影响下一次发布：选了这条 RR 的租户，其设备要在配置发布并且盒内路由进程重启以后才会用新地址。" +
	"控制器不会去连库里的地址，所以这里没有任何连通性结论可给。"

// rrResponseLocked writes the whole table back after every mutation. The list
// comes back rather than just the touched row because a delete guard, a renumbered
// id and a reference count are exactly the things one operator's edit changes for
// the other rows.
func (s *server) rrResponseLocked(w http.ResponseWriter, extra map[string]any) {
	out := map[string]any{
		"ok":       true,
		"rrs":      s.rrLibraryRowsLocked(),
		"note":     rrChangeNote,
		"now_unix": time.Now().Unix(),
	}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}

// handleAdminRRs: GET lists the whole library, POST adds a row, PUT edits one.
func (s *server) handleAdminRRs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		// The 归属 picker needs the tenant list and already has it: /api/state
		// carries it on every operator poll, so this answer stays the library alone.
		// (/api/tenants is the create endpoint -- a second shape for the same list
		// would be a second thing to keep honest.)
		writeJSON(w, map[string]any{
			"rrs":       s.rrLibraryRowsLocked(),
			"max_items": rrMaxItems,
			"note":      rrChangeNote,
			"now_unix":  time.Now().Unix(),
		})
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
	owner := strings.TrimSpace(req.TenantID)
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

	if r.Method == http.MethodPost {
		if strings.TrimSpace(req.ID) != "" {
			http.Error(w, "新建 RR 不应带 id", http.StatusBadRequest)
			return
		}
		if len(lib.Items) >= rrMaxItems {
			http.Error(w, fmt.Sprintf("RR 库数量已达上限 %d", rrMaxItems), http.StatusConflict)
			return
		}
		// A row is added for a tenant, always: an empty 归属 is not "shared", it is
		// a missing decision, and an id that names nobody is a typo -- either way
		// the row would be one no tenant can ever pick.
		if owner == "" {
			http.Error(w, "RR 必须归属一个租户：先在租户表里建好那个租户，再回到这一页添加。", http.StatusBadRequest)
			return
		}
		if s.state.Tenants[owner] == nil {
			http.Error(w, "归属租户不存在："+owner, http.StatusBadRequest)
			return
		}
		it := RRItem{ID: lib.nextID(), Name: name, Addr: addr, Location: location,
			TenantID: owner, CreatedAt: now}
		lib.Items = append(lib.Items, it)
		s.rrTouchLocked(lib, id.Username, now)
		s.save()
		log.Printf("rrlib: created %s addr=%s owner=%q by %s", it.ID, it.Addr, it.TenantID, id.Username)
		s.rrResponseLocked(w, map[string]any{"rr": s.rrRowLocked(it)})
		return
	}

	targetID := strings.TrimSpace(req.ID)
	it := lib.get(targetID)
	if it == nil {
		// Same answer as a row that never existed: this endpoint is operator-only
		// anyway, but a 404 here keeps the id space unprobed by anything that gets
		// a session it should not have.
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	if owner == "" {
		http.Error(w, "RR 必须归属一个租户：这一条现在没有租户，请选一个现存租户保存。", http.StatusBadRequest)
		return
	}
	// Unlike create, this also catches the orphan case: an id that no longer names
	// a tenant cannot be saved back, so fixing an orphaned row means binding it.
	if s.state.Tenants[owner] == nil {
		http.Error(w, "归属租户不存在："+owner, http.StatusBadRequest)
		return
	}
	before := *it
	it.Name, it.Addr, it.Location, it.TenantID = name, addr, location, owner
	it.UpdatedAt = now
	// The address is what the device dials, so a change here is the one edit that
	// is worth a log line even when nothing else moved.
	addrChanged := before.Addr != it.Addr
	s.rrTouchLocked(lib, id.Username, now)
	s.save()
	log.Printf("rrlib: updated %s addr=%s owner=%q addrChanged=%t by %s",
		it.ID, it.Addr, it.TenantID, addrChanged, id.Username)
	s.rrResponseLocked(w, map[string]any{
		"rr":           s.rrRowLocked(*it),
		"addr_changed": addrChanged,
	})
}

// handleAdminRRDelete refuses while any tenant still names the row (拍板 E5).
// The alternative -- letting the delete through and silently dropping the
// selection -- would leave that tenant's devices rendering no rrs at all, and
// "my box has no reflector" is a far worse surprise for the customer than a
// delete button that explains itself.
func (s *server) handleAdminRRDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireOperator(w, r)
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
	if it == nil {
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	if refs := s.rrReferencesLocked(targetID); len(refs) > 0 {
		names := make([]string, 0, len(refs))
		for _, t := range refs {
			if tn := s.state.Tenants[t]; tn != nil {
				names = append(names, tn.Name)
			}
		}
		http.Error(w, fmt.Sprintf("这条 RR 还被 %d 个租户选着（%s）：让他们先在「Overlay 配置 › 全局」改选别的，再来删",
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
	// because a tenant's selection is a plain string that outlives nothing else.
	lib.Items = append(lib.Items[:idx], lib.Items[idx+1:]...)
	s.rrTouchLocked(lib, id.Username, time.Now().Unix())
	s.save()
	log.Printf("rrlib: deleted %s addr=%s by %s", removed.ID, removed.Addr, id.Username)
	s.rrResponseLocked(w, map[string]any{"deleted": targetID})
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

func (s *server) rrRowLocked(it RRItem) rrLibraryRow {
	refs := s.rrReferencesLocked(it.ID)
	names := make([]string, 0, len(refs))
	for _, t := range refs {
		if tn := s.state.Tenants[t]; tn != nil {
			names = append(names, tn.Name)
		}
	}
	return rrLibraryRow{
		ID: it.ID, Name: it.Name, Addr: it.Addr, Location: it.Location,
		TenantID: it.TenantID, OwnerLabel: s.rrOwnerLabelLocked(it),
		UsedBy: refs, UsedNames: names,
		CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
	}
}
