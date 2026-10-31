package main

// The route-reflector library: every tenant's own list of the boxes that reflect
// its routes. A row is added, edited and deleted from that tenant's overlay page
// (2026-11-01 定案: an RR belongs to exactly one tenant, so the tenant manages it;
// the operator view no longer carries this page).
//
// What an address means here: "host:port", resolved and dialled by the device,
// never by the controller. The controller has no route to the RR and does not
// claim one -- it does not probe the entry (拍板 E4), because a green light the
// controller cannot earn is worse than the plain "没探过" the tunnel page already
// uses.
//
// Only a device an RR group names dials reflectors, and then only that group's
// two. A device no group names gets no rrs at all: it keeps the list its own
// route.json already carries, so adding a row here never starts a session on a
// box nobody placed (2026-11-01 拍板).
//
// The rows are kept in one document with the id counter, and an id is never
// handed back after a delete: a group's selection quotes ids, so re-minting rr3
// would silently re-point a device at a different box.

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"sdwan/controller/internal/wire"
)

const (
	rrNameMaxRunes     = 80
	rrLocationMaxRunes = 120
	rrAddrMaxRunes     = 253
	// rrMaxPerTenant bounds one tenant's list. It is not an arbitrary table
	// limit: every device that no RR group names dials the whole list, so the
	// list length *is* the default session count per device. Eight is far above
	// the two a tenant needs for redundancy and low enough that one typo cannot
	// turn every box into a dial loop.
	rrMaxPerTenant = 8
)

// RRItem is one reflector in the library.
//
// TenantID is the row's one and only 归属: a reflector belongs to the tenant
// whose devices dial it. There is no shared tier (2026-10-31 定案), and since
// 2026-11-01 the owning tenant is also the row's only editor -- which is why an
// empty 归属 is not "nobody's" but a row nobody can manage.
//
// The JSON key stays "owner" even though the field is a required tenant id.
// Renaming it would drop every binding already on disk and turn those rows into
// orphans nobody can pick.
type RRItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
	TenantID string `json:"owner,omitempty"`

	CreatedAt int64 `json:"created_at_unix"`
	UpdatedAt int64 `json:"updated_at_unix,omitempty"`
}

// RRLibrary is the whole RR table. One document, not a per-tenant map, because
// the order the rows were typed in is the order the tenant's page should show,
// and because the id counter lives next to the rows it mints.
type RRLibrary struct {
	Items     []RRItem `json:"items"`
	NextID    int      `json:"next_id"`
	UpdatedAt int64    `json:"updated_at_unix"`
	UpdatedBy string   `json:"updated_by,omitempty"`
}

func (l *RRLibrary) ensure() {
	if l.Items == nil {
		l.Items = []RRItem{}
	}
	if l.NextID < 0 {
		l.NextID = 0
	}
	for i := range l.Items {
		l.Items[i].Name = strings.TrimSpace(l.Items[i].Name)
		l.Items[i].Addr = strings.TrimSpace(l.Items[i].Addr)
		l.Items[i].Location = strings.TrimSpace(l.Items[i].Location)
		l.Items[i].TenantID = strings.TrimSpace(l.Items[i].TenantID)
	}
}

func (l *RRLibrary) get(id string) *RRItem {
	if id == "" {
		return nil
	}
	for i := range l.Items {
		if l.Items[i].ID == id {
			return &l.Items[i]
		}
	}
	return nil
}

// nextID mints "rr1", "rr2" ... and never rewinds, for the same reason
// OverlayConfig.nextGroupID does: an id is quoted by the tenant's selection and
// by the delete guard, so reusing one after a delete would re-point that tenant.
func (l *RRLibrary) nextID() string {
	for {
		l.NextID++
		id := fmt.Sprintf("rr%d", l.NextID)
		if l.get(id) == nil {
			return id
		}
	}
}

// visibleTo is the rows one tenant may pick: only the rows bound to it. An
// unbound row is visible to nobody -- before this rule existed an empty owner
// meant "everyone", so carrying that reading forward would quietly hand out rows
// the operator never gave to anybody.
func (l *RRLibrary) visibleTo(tenantID string) []RRItem {
	out := []RRItem{}
	if tenantID == "" {
		return out
	}
	for _, it := range l.Items {
		if it.TenantID == tenantID {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rrLibraryLocked returns the operator's table, creating it on first use. A
// tenant-facing read may pass create=false: an empty library is what it is.
func (s *server) rrLibraryLocked(create bool) *RRLibrary {
	if s.state.RRs == nil {
		if !create {
			return &RRLibrary{Items: []RRItem{}}
		}
		s.state.RRs = &RRLibrary{Items: []RRItem{}}
	}
	s.state.RRs.ensure()
	return s.state.RRs
}

// rrGroupRef is one RR group that names a row, as much of it as the delete guard
// and the page's explanation need.
type rrGroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// rrGroupRefsLocked lists the RR groups of one tenant that name this row. It is
// the delete guard's only input now that the tenant-level pair is gone: a group
// still pointing at a deleted row would render that group's devices a route.json
// with no rrs at all, and the device's existing hand-written list would look like
// it still matched.
func (s *server) rrGroupRefsLocked(tenantID, id string) []rrGroupRef {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil || id == "" {
		return nil
	}
	out := []rrGroupRef{}
	for _, g := range oc.RRGroups {
		if g.RRPrimary == id || g.RRBackup == id {
			out = append(out, rrGroupRef{ID: g.ID, Name: g.Name})
		}
	}
	return out
}

// rrsForDeviceLocked is the per-device answer the 第 4 段 uses: only a device
// named by an RR group gets reflectors -- that group's two slots. A device no
// group names gets nil, i.e. the rrs key is omitted, and the box keeps whatever
// list its own route.json already carries. This is deliberately not "dial every
// RR of the tenant": an ungrouped device has no RR neighbours at all, so adding a
// row to the library never quietly lights up sessions on boxes nobody placed.
func (s *server) rrsForDeviceLocked(tenantID string, nodeID uint16) []wire.RRAddress {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil {
		return nil
	}
	g := oc.rrGroupOf(nodeID)
	if g == nil {
		return nil
	}
	return s.rrAddrsLocked(tenantID, g.RRPrimary, g.RRBackup)
}

// rrAddrsLocked resolves an RR group's two slots against the library. A slot
// that no longer resolves is skipped (the read-time checks say why), and an empty
// result means "omit the key" -- never "send an empty list", because sr-route
// refuses to start with no rrs at all (cmd/sr-route/main.go:91).
func (s *server) rrAddrsLocked(tenantID, primary, backup string) []wire.RRAddress {
	lib := s.rrLibraryLocked(false)
	out := []wire.RRAddress{}
	for _, id := range []string{primary, backup} {
		it := lib.get(id)
		if it == nil || it.TenantID != tenantID {
			continue
		}
		out = append(out, wire.RRAddress{ID: it.ID, Addr: it.Addr})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// overlayRRListChecksLocked is the read-time check of the tenant's RR list. The
// list is the page's own input now, so the checks are about the list as a whole:
//
//   - no rows is an info, not an error: the controller then sends no rrs key at
//     all, and every box enrolled before this page keeps the list the operator
//     hand-wrote into route.json;
//   - two rows with the same host:port is an error: the device would dial one
//     reflector twice and hear every prefix twice. The API refuses it on save, so
//     reaching this line means the state file was edited or restored.
func (s *server) overlayRRListChecksLocked(tenantID string) []ConfigCheck {
	rows := s.rrLibraryLocked(false).visibleTo(tenantID)
	if len(rows) == 0 {
		return []ConfigCheck{{Severity: "info",
			Text: "还没有 RR：控制器不会下发 rrs 这个键，设备继续用它盘上自己那一份；要控制器接管，就在这里添加。"}}
	}
	seen := map[string]string{}
	var out []ConfigCheck
	for _, it := range rows {
		if first, ok := seen[it.Addr]; ok {
			out = append(out, ConfigCheck{Severity: "error", Text: fmt.Sprintf(
				"%s 和 %s 的地址都是 %s：两条行会让设备把同一个反射器拨两遍，删掉一条。",
				first, it.ID, it.Addr)})
			continue
		}
		seen[it.Addr] = it.ID
	}
	return out
}

// rrGroupsChecksLocked validates a whole candidate group list. It is the same
// question asked once for the document the page saves: the page replaces the
// list, so "this device is in two groups" only exists as a property of the list.
//
// Every message carries the group it is about, because the page shows the flat
// result and a sentence about "RR 1" would otherwise be indistinguishable from
// the tenant-level pair above it.
func (s *server) rrGroupsChecksLocked(tenantID string, groups []RRGroup) []ConfigCheck {
	var out []ConfigCheck
	add := func(g RRGroup, format string, args ...any) {
		out = append(out, ConfigCheck{
			Severity: "error",
			Text:     fmt.Sprintf("分组「%s」（%s）：", g.Name, g.ID) + fmt.Sprintf(format, args...),
		})
	}
	owner := map[uint16]string{}
	for _, g := range groups {
		if g.Name == "" {
			add(g, "要有名字：页面和设备上的注释都要能指出这是哪一组。")
		}
		if len(g.DeviceIDs) == 0 {
			add(g, "至少要把一台设备放进这一组；一台也没有的组等于不存在，直接删掉它。")
		}
		for _, slot := range []struct{ name, id string }{{"RR 1", g.RRPrimary}, {"RR 2", g.RRBackup}} {
			if msg := s.rrSelectionErrorLocked(tenantID, slot.id); msg != "" {
				add(g, "%s：%s", slot.name, msg)
			}
		}
		if g.RRPrimary == "" && g.RRBackup == "" {
			add(g, "至少选一台 RR：组里的设备靠它反射，留空只会让这些设备拿不到控制器下发的 rrs。")
		} else if g.RRPrimary != "" && g.RRPrimary == g.RRBackup {
			add(g, "RR 1 和 RR 2 不能选同一条。")
		}
		seen := map[uint16]bool{}
		for _, nodeID := range g.DeviceIDs {
			if seen[nodeID] {
				add(g, "设备 %d 出现了两次。", nodeID)
				continue
			}
			seen[nodeID] = true
			if s.state.Nodes[key(tenantID, nodeID)] == nil {
				add(g, "设备 %d 不属于这个租户，或者还没入网。", nodeID)
				continue
			}
			if other, ok := owner[nodeID]; ok {
				add(g, "设备 %d 已经在分组 %s 里：一台设备只能有一组 RR。", nodeID, other)
				continue
			}
			owner[nodeID] = g.ID
		}
	}
	return out
}

// overlayRRGroupChecksLocked is the read-time form: stored groups are re-checked
// on every read, so a list restored from an older state file (foreign RR, device
// unenrolled, device in two groups) is named even when nobody is about to save.
func (s *server) overlayRRGroupChecksLocked(tenantID string) []ConfigCheck {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil || len(oc.RRGroups) == 0 {
		return nil
	}
	return s.rrGroupsChecksLocked(tenantID, oc.RRGroups)
}

// rrRow is one row of a tenant's RR list as GET /api/overlay returns it: what the
// page edits, plus the RR groups that name it -- so the delete button can explain
// itself before anyone clicks it. No 归属 field (the list is already this
// tenant's) and no reference count from other tenants, which is not this
// tenant's business.
type rrRow struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Addr      string       `json:"addr"`
	Location  string       `json:"location"`
	Groups    []rrGroupRef `json:"groups"`
	CreatedAt int64        `json:"created_at_unix"`
	UpdatedAt int64        `json:"updated_at_unix,omitempty"`
}

func (s *server) rrRowsLocked(tenantID string) []rrRow {
	out := []rrRow{}
	for _, it := range s.rrLibraryLocked(false).visibleTo(tenantID) {
		out = append(out, s.rrRowOfLocked(it))
	}
	return out
}

func (s *server) rrRowOfLocked(it RRItem) rrRow {
	groups := s.rrGroupRefsLocked(it.TenantID, it.ID)
	if groups == nil {
		groups = []rrGroupRef{}
	}
	return rrRow{
		ID: it.ID, Name: it.Name, Addr: it.Addr, Location: it.Location,
		Groups: groups, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
	}
}

// validRRAddr accepts what the device can dial: a hostname or an IP literal with
// an explicit port. The port is required rather than defaulted because sr-route
// splits host:port to derive the TLS server name when no tls_server_name is set
// (cmd/sr-route/main.go:128-134), so an address without one would only fail at
// the device -- and "the box cannot reach its reflector" is the last place
// anyone wants to discover a typo.
func validRRAddr(addr string) error {
	if len(addr) > rrAddrMaxRunes {
		return errors.New("RR 地址过长")
	}
	if strings.ContainsAny(addr, " \t\r\n") {
		return errors.New("RR 地址不能含空格")
	}
	// A scheme would be dialled literally and fail; a path means somebody pasted
	// a URL into a host:port field.
	if strings.ContainsAny(addr, "/@") {
		return errors.New("RR 地址只写「主机:端口」，不要带协议或路径")
	}
	// A bare IPv6 has colons of its own and SplitHostPort cannot tell where the
	// port starts, so it has to arrive bracketed -- which is also the exact shape
	// the device dials, so nothing is translated afterwards.
	if strings.Count(addr, ":") > 1 && !strings.HasPrefix(addr, "[") {
		return errors.New(`IPv6 要写成方括号形式，例如 "[2001:db8::1]:20200"`)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New(`RR 地址要写成「主机:端口」，例如 "rr.example.com:20200" 或 "192.0.2.10:20200"`)
	}
	if host == "" {
		return errors.New("RR 地址要有主机部分")
	}
	if net.ParseIP(host) == nil {
		if err := validRRHost(host); err != nil {
			return err
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("RR 地址的端口要是 1-65535")
	}
	return nil
}

// validRRHost is the hostname half of the check. Go's resolver accepts shapes that
// no DNS zone can hold, and every one of them fails only when the device dials.
func validRRHost(host string) error {
	if len(host) > 253 {
		return errors.New("RR 地址的主机名过长")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return errors.New("RR 地址的主机名每一段都要是 1-63 个字符")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("RR 地址的主机名每段不能以 - 开头或结尾")
		}
		for _, c := range []byte(strings.ToLower(label)) {
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
				return errors.New("RR 地址的主机名只能用字母、数字、- 和 _")
			}
		}
	}
	return nil
}

func validRRName(name string) error {
	r := []rune(name)
	if len(r) < 1 || len(r) > rrNameMaxRunes {
		return fmt.Errorf("RR 名字需要 1-%d 个字符", rrNameMaxRunes)
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f {
			return errors.New("RR 名字不能含控制字符")
		}
	}
	return nil
}

// rrSelectionErrorLocked is the save-time half of the visibility rule. It says
// only that the id is not in this tenant's list: whether it exists, and whose it
// is when it does, would tell a curious customer who else buys service here.
func (s *server) rrSelectionErrorLocked(tenantID, id string) string {
	if id == "" {
		return ""
	}
	it := s.rrLibraryLocked(false).get(id)
	if it == nil || it.TenantID != tenantID {
		return fmt.Sprintf("%q 不在本租户的 RR 列表里：选一条已经加在本租户的 RR", id)
	}
	return ""
}

func validRRLocation(loc string) error {
	r := []rune(loc)
	if len(r) > rrLocationMaxRunes {
		return fmt.Errorf("RR 位置最长 %d 个字符", rrLocationMaxRunes)
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f {
			return errors.New("RR 位置不能含控制字符")
		}
	}
	return nil
}

// normalizeRRs repairs the library itself. Two kinds of row are dropped:
//
//   - no id or no addr: one cannot be selected, the other cannot be dialled, and
//     there is nothing to invent -- a half-empty RR would be advertised to a whole
//     tenant;
//   - no 归属, or a 归属 that names a tenant which no longer exists: since
//     2026-11-01 the owning tenant is the row's only editor, so such a row is
//     unreachable from every page and could never be deleted through the console.
//
// Nothing else is repaired behind the tenant's back: a duplicate address stays and
// overlayRRListChecksLocked names it, because dropping one of two indistinguishable
// rows is still picking a row for the customer.
func normalizeRRs(st *State) bool {
	changed := false
	// Creating the container is not a repair worth a rewrite: a state file from
	// before this page simply has no RR table, and that is the same world as an
	// empty one.
	if st.RRs == nil {
		st.RRs = &RRLibrary{Items: []RRItem{}}
	}
	lib := st.RRs
	if lib.Items == nil {
		lib.Items = []RRItem{}
	}
	if lib.NextID < 0 {
		lib.NextID = 0
		changed = true
	}
	kept := make([]RRItem, 0, len(lib.Items))
	seen := map[string]bool{}
	for _, it := range lib.Items {
		if it.ID == "" || it.Addr == "" || seen[it.ID] {
			changed = true
			continue
		}
		if it.TenantID == "" || st.Tenants[it.TenantID] == nil {
			changed = true
			continue
		}
		seen[it.ID] = true
		kept = append(kept, it)
	}
	if len(kept) != len(lib.Items) {
		lib.Items = kept
	}
	lib.ensure()
	// The counter has to stay above every id left in the table, or the next add
	// re-mints an id somebody may still be pointing at.
	for _, it := range lib.Items {
		var n int
		if _, err := fmt.Sscanf(it.ID, "rr%d", &n); err == nil && n > lib.NextID {
			lib.NextID = n
			changed = true
		}
	}
	return changed
}
