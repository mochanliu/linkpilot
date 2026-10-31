package main

// The route-reflector library: one operator-owned list of the boxes that reflect
// routes, plus the two slots in each tenant's overlay config that pick from it.
//
// Why the operator owns the list (定案, docs/o6c-overlay-page-plan.md §4 E2/E3/E4):
// an RR is shared infrastructure the operator deploys, the same way it hands out
// tenant seats. A tenant may not invent an address -- it would then push that
// address into every one of its devices' route.json, and a reflector that does
// not exist means every session on that box fails its hello with no explanation.
// So the tenant only ever *selects*.
//
// What an address means here: "host:port", resolved and dialled by the device,
// never by the controller. The controller has no route to the RR and does not
// claim one -- it does not probe the entry (拍板 E4), because a green light the
// controller cannot earn is worse than the plain "没探过" the tunnel page already
// uses.
//
// The selection is stored as library *ids*, not addresses. Deleting or renaming
// an RR's address must not silently re-point a tenant at a different box, and a
// tenant pointing at an id that no longer exists is a contradiction the page can
// show and the renderer can refuse -- whereas a stale address looks perfectly
// plausible right up to the moment the device cannot dial it.

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
	rrMaxItems         = 256
)

// RRItem is one reflector in the library.
//
// TenantID is the row's one and only 归属: a reflector belongs to the tenant
// whose devices dial it. There is no shared tier (2026-10-31 定案) -- a row the
// operator adds is always added *for* somebody. A reflector every tenant may
// pick from is one nobody owns: its capacity, its address and its retirement
// become everybody's problem and no one's decision.
//
// The JSON key stays "owner" even though the field is now a required tenant id.
// Renaming it would drop every binding already on disk and turn those rows into
// orphans nobody can pick -- a silent change to a customer's setting is exactly
// what this file refuses to do.
type RRItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
	TenantID string `json:"owner,omitempty"`

	CreatedAt int64 `json:"created_at_unix"`
	UpdatedAt int64 `json:"updated_at_unix,omitempty"`
}

// RRLibrary is the operator's whole RR table. One document, not a map keyed by
// id, because the order the operator maintains it in is the order the tenant's
// picker should show (a list of "RR-A 上海" / "RR-B 法兰克福" typed by hand reads
// better in the order it was typed), and because the id counter lives next to
// the rows it mints.
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

// rrOwnerLabelLocked names a row's 归属 for the operator's table. A row whose
// tenant has been deleted -- or a row restored from before the binding rule --
// stays in the library and says so: dropping it would delete the operator's row
// as a side effect of deleting a customer, and there is no longer anywhere to
// "fall back" to, so the label has to be the thing that gets it fixed.
func (s *server) rrOwnerLabelLocked(it RRItem) string {
	if it.TenantID == "" {
		return "未绑定租户（谁都选不到这一条）"
	}
	if t := s.state.Tenants[it.TenantID]; t != nil {
		return t.Name
	}
	return it.TenantID + "（该租户已不存在，谁都选不到这一条）"
}

// rrReferencesLocked lists the tenants whose 主/备 slots -- or whose RR groups --
// name this row. The delete guard is the whole reason this exists: a tenant that
// keeps selecting a deleted RR would render a route.json with no rrs at all, and
// the device's existing hand-written list would look like it still matched.
func (s *server) rrReferencesLocked(id string) []string {
	if id == "" {
		return nil
	}
	var out []string
	for tenantID, oc := range s.state.Overlays {
		if oc == nil || s.state.Tenants[tenantID] == nil {
			continue
		}
		if oc.RRPrimary == id || oc.RRBackup == id {
			out = append(out, tenantID)
			continue
		}
		for _, g := range oc.RRGroups {
			if g.RRPrimary == id || g.RRBackup == id {
				out = append(out, tenantID)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// rrForTenantLocked resolves one tenant's two slots into the addresses the
// device dials. Anything that does not resolve is skipped, and the caller must
// treat an empty result as "omit the key" -- never as an empty list, because
// sr-route refuses to start with "rrs is required" (cmd/sr-route/main.go:91).
// Order is 主 then 备, and that order carries no meaning on the device: the
// client keeps every session it is given up, so two rows are two equal active
// reflectors (docs/o6c-overlay-page-plan.md §4, 拍板 E0).
func (s *server) rrForTenantLocked(tenantID string) []wire.RRAddress {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil {
		return nil
	}
	return s.rrAddrsLocked(tenantID, oc.RRPrimary, oc.RRBackup)
}

// rrsForDeviceLocked is the per-device answer the 第 4 段 uses: a device named by
// an RR group dials that group's reflectors, every other device dials the
// tenant's two slots. Both come back through the same resolver, so "the group
// picked a row another tenant owns" fails exactly like the tenant-level pick.
func (s *server) rrsForDeviceLocked(tenantID string, nodeID uint16) []wire.RRAddress {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil {
		return nil
	}
	if g := oc.rrGroupOf(nodeID); g != nil {
		return s.rrAddrsLocked(tenantID, g.RRPrimary, g.RRBackup)
	}
	return s.rrAddrsLocked(tenantID, oc.RRPrimary, oc.RRBackup)
}

// rrAddrsLocked resolves two slot ids against the library. A slot that no longer
// resolves is skipped (the read-time checks say why), and an empty result means
// "omit the key" -- never "send an empty list", because sr-route refuses to start
// with no rrs at all (cmd/sr-route/main.go:91).
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

// overlayRRChecksLocked is the read-time check of the two slots. A dangling or
// foreign reference cannot be produced through the API (create/edit reject it
// and delete is guarded), so reaching this line means the state file was edited
// or restored -- exactly the case that must be named rather than papered over.
func (s *server) overlayRRChecksLocked(tenantID string) []ConfigCheck {
	oc := s.overlayConfigLocked(tenantID, false)
	if oc == nil {
		return nil
	}
	lib := s.rrLibraryLocked(false)
	var out []ConfigCheck
	add := func(sev, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: sev, Text: fmt.Sprintf(format, args...)})
	}
	// 拍板 E0：两格在设备上等价，所以检查里也叫 RR 1 / RR 2，不叫主/备。
	// （存储与接口的字段名仍是 rr_primary / rr_backup：那是已经落盘的名字，
	// 为了一个措辞改动 state 形状不值。）
	for _, slot := range []struct{ name, id string }{{"RR 1", oc.RRPrimary}, {"RR 2", oc.RRBackup}} {
		if slot.id == "" {
			continue
		}
		it := lib.get(slot.id)
		switch {
		case it == nil:
			add("error", "%s 选了库里已经不存在的条目 %q：这一台设备这一版拿不到 RR，去「Overlay 配置 › 全局」重选。",
				slot.name, slot.id)
		case it.TenantID == "":
			add("error", "%s 选的 %q（%s）还没绑定租户，谁都选不到：让运营方在「资产 › RR 库」把它绑到一个租户。",
				slot.name, slot.id, it.Name)
		case it.TenantID != tenantID:
			add("error", "%s 选的 %q（%s）归属别的租户，本租户用不了：这一版不会把它下发。",
				slot.name, slot.id, it.Name)
		}
	}
	if oc.RRPrimary == "" && oc.RRBackup == "" {
		// Not an error: the device keeps whatever rrs the operator hand-wrote into
		// route.json, which is how every box enrolled before this page works.
		add("info", "还没选 RR：控制器不会下发 rrs 这个键，设备继续用它盘上自己那一份。")
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

// rrPickRow is one library entry as the tenant's picker shows it. No 归属 and no
// reference count: the tenant has already been filtered to what it may pick, and
// how many other customers share a reflector is not this tenant's business.
type rrPickRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
}

// rrLibraryRow is the same entry from the operator's side.
type rrLibraryRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
	TenantID string `json:"owner,omitempty"`
	// OwnerLabel is the 归属 as a sentence, because a bare tenant id is unreadable
	// and the two rows that need explaining -- no binding, or a tenant that no
	// longer exists -- look identical as an empty or stale id.
	OwnerLabel string   `json:"owner_label"`
	UsedBy     []string `json:"used_by"`
	UsedNames  []string `json:"used_names"`
	CreatedAt  int64    `json:"created_at_unix"`
	UpdatedAt  int64    `json:"updated_at_unix,omitempty"`
}

func (s *server) rrPickRowsLocked(tenantID string) []rrPickRow {
	lib := s.rrLibraryLocked(false)
	out := []rrPickRow{}
	for _, it := range lib.visibleTo(tenantID) {
		out = append(out, rrPickRow{ID: it.ID, Name: it.Name, Addr: it.Addr, Location: it.Location})
	}
	return out
}

func (s *server) rrLibraryRowsLocked() []rrLibraryRow {
	lib := s.rrLibraryLocked(false)
	out := []rrLibraryRow{}
	for _, it := range lib.Items {
		refs := s.rrReferencesLocked(it.ID)
		names := make([]string, 0, len(refs))
		for _, t := range refs {
			if tn := s.state.Tenants[t]; tn != nil {
				names = append(names, tn.Name)
			}
		}
		out = append(out, rrLibraryRow{
			ID: it.ID, Name: it.Name, Addr: it.Addr, Location: it.Location,
			TenantID: it.TenantID, OwnerLabel: s.rrOwnerLabelLocked(it),
			UsedBy: refs, UsedNames: names,
			CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
		})
	}
	return out
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
// only who may not pick a row: whether the id exists, and whether it is bound to
// somebody else. Naming that tenant would tell a curious customer who else buys
// service here, so the message stops at "不属于本租户".
func (s *server) rrSelectionErrorLocked(tenantID, id string) string {
	if id == "" {
		return ""
	}
	it := s.rrLibraryLocked(false).get(id)
	switch {
	case it == nil:
		return fmt.Sprintf("%q 不在 RR 库里；库里只有运营方在「资产 › RR 库」添加的那些条目", id)
	case it.TenantID == "":
		return fmt.Sprintf("%q（%s）还没绑定租户，谁都选不了：让运营方在「资产 › RR 库」把它绑到一个租户", id, it.Name)
	case it.TenantID != tenantID:
		return fmt.Sprintf("%q（%s）不属于本租户：每条 RR 只归一个租户所有", id, it.Name)
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

// normalizeRRs repairs the library itself. It deliberately does NOT clear a
// tenant's selection when the id it names is missing: that would make a
// hand-edited or restored state file lose the customer's setting without anyone
// being told, and overlayRRChecksLocked reports the contradiction instead. The
// same rule covers rows with no tenant: they stay, visible to nobody, and the
// operator's table says so.
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
		// A row with no id cannot be selected, and a row with no addr cannot be
		// dialled. Both are dropped rather than repaired -- there is nothing to
		// invent, and a half-empty RR would be advertised to every tenant.
		if it.ID == "" || it.Addr == "" || seen[it.ID] {
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
	// The counter has to stay above every id in the table, or the next add
	// re-mints an id somebody may still be pointing at.
	for _, it := range lib.Items {
		var n int
		if _, err := fmt.Sscanf(it.ID, "rr%d", &n); err == nil && n > lib.NextID {
			lib.NextID = n
			changed = true
		}
	}
	for _, oc := range st.Overlays {
		if oc == nil {
			continue
		}
		p, b := strings.TrimSpace(oc.RRPrimary), strings.TrimSpace(oc.RRBackup)
		if p != oc.RRPrimary || b != oc.RRBackup {
			oc.RRPrimary, oc.RRBackup = p, b
			changed = true
		}
	}
	return changed
}
