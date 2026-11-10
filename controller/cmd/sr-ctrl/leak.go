package main

// The 路由泄露 layer (O6-e, docs/o6e-leak-plan.md §3/§8): per-tenant
// prefix-list library plus one rule per (device, direction, VPN).
//
// Two modules share one library but not one rule set: CPE-SDWAN (customer →
// SD-WAN) and SDWAN-CPE (SD-WAN → customer) are separate directions with
// separate protocol vocabularies, and a device may have a row in one direction
// without one in the other.
//
// Nothing here reaches a device on save. Like the 导通组 this is draft
// material: a publish renders it into the bundle's FRR and srfrr artifacts, and
// the draft/publish split is what makes "edit a list" not silently change what a
// box is running.

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"sdwan/controller/internal/route"
	"sdwan/controller/internal/wire"
)

const (
	leakToCPE   = "to_cpe"   // SDWAN-CPE
	leakToSDWAN = "to_sdwan" // CPE-SDWAN

	leakProtoBGP       = "bgp"
	leakProtoOSPF      = "ospf"
	leakProtoStatic    = "static"
	leakProtoConnected = "connected"

	leakMaxPrefixLists = 64
	leakMaxEntries     = 256
	leakMaxNameLen     = 32

	// leakDefaultListName is the seeded list a CPE-SDWAN row points at until the
	// tenant writes a narrower one. 0.0.0.0/0 permits everything, which is the
	// documented default of that direction (§2.1).
	leakDefaultListName = "ALL"
)

// kernelReservedVPN reports whether vpn is a kernel table the box already uses
// for something else. sr-frr injects a VPN N prefix into kernel table N, and
// 253/254/255 are default/main/local — a leak row on them would write into the
// kernel's own tables.
func kernelReservedVPN(vpn uint32) bool { return vpn >= 253 }

// PrefixEntry is one line of a prefix-list. FRR semantics: a prefix with no
// ge/le matches exactly; ge/le widen it to a length range. Zero means "not
// written", so /0 prefixes are still expressible through Prefix.
type PrefixEntry struct {
	Prefix string `json:"prefix"`
	GE     int    `json:"ge,omitempty"`
	LE     int    `json:"le,omitempty"`
}

// PrefixList is one list in a tenant's own library. It is never shared across
// tenants (2026-11-02 拍板): a shared object would belong in the operator view,
// and no such object exists yet.
type PrefixList struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Entries   []PrefixEntry `json:"entries"`
	CreatedAt int64         `json:"created_at_unix,omitempty"`
	UpdatedAt int64         `json:"updated_at_unix,omitempty"`
}

// LeakConfig is one tenant's library. The rules themselves live on DeviceConfig
// (LeakSet) because they are per-device, and keeping them out of BasicConfig is
// what stops a 基础配置 save from wiping them.
type LeakConfig struct {
	TenantID       string       `json:"tenant_id"`
	PrefixLists    []PrefixList `json:"prefix_lists"`
	NextPrefixList int          `json:"next_prefix_list"`
	UpdatedAt      int64        `json:"updated_at_unix,omitempty"`
	UpdatedBy      string       `json:"updated_by,omitempty"`
}

// LeakRule is one (device, direction, VPN) row. PrefixList holds a list ID (not
// a name): renaming a list must not silently break the rules that use it.
type LeakRule struct {
	VPN        uint32   `json:"vpn"`
	Protocols  []string `json:"protocols"`
	PrefixList string   `json:"prefix_list,omitempty"`
}

// LeakSet is the two directions of one device.
type LeakSet struct {
	ToCPE   []LeakRule `json:"to_cpe"`
	ToSDWAN []LeakRule `json:"to_sdwan"`
}

func (s *server) leakConfigLocked(tenantID string, create bool) *LeakConfig {
	if s.state.LeakConfigs == nil {
		if !create {
			return nil
		}
		s.state.LeakConfigs = map[string]*LeakConfig{}
	}
	lc, ok := s.state.LeakConfigs[tenantID]
	if !ok {
		if !create {
			return nil
		}
		lc = &LeakConfig{TenantID: tenantID, PrefixLists: []PrefixList{}}
		s.state.LeakConfigs[tenantID] = lc
	}
	lc.ensure()
	return lc
}

// ensure repairs an older or hand-edited record and keeps the library
// non-empty: a tenant whose lists were all deleted gets ALL back on the next
// read, so the CPE-SDWAN default is always selectable (§3.1).
func (l *LeakConfig) ensure() {
	if l.PrefixLists == nil {
		l.PrefixLists = []PrefixList{}
	}
	if l.NextPrefixList < 0 {
		l.NextPrefixList = 0
	}
	for i := range l.PrefixLists {
		l.PrefixLists[i].Name = strings.TrimSpace(l.PrefixLists[i].Name)
		if l.PrefixLists[i].Entries == nil {
			l.PrefixLists[i].Entries = []PrefixEntry{}
		}
		normalizePrefixEntries(l.PrefixLists[i].Entries)
	}
	if len(l.PrefixLists) == 0 {
		l.PrefixLists = append(l.PrefixLists, PrefixList{
			ID:      l.nextListID(),
			Name:    leakDefaultListName,
			Entries: []PrefixEntry{{Prefix: "0.0.0.0/0"}},
		})
	}
}

func (l *LeakConfig) list(id string) *PrefixList {
	for i := range l.PrefixLists {
		if l.PrefixLists[i].ID == id {
			return &l.PrefixLists[i]
		}
	}
	return nil
}

// nextListID mints an id nobody holds. Ids are quoted by rules and logs, so they
// are never reused even after the list that held one is deleted.
func (l *LeakConfig) nextListID() string {
	for {
		l.NextPrefixList++
		id := fmt.Sprintf("pl%d", l.NextPrefixList)
		if l.list(id) == nil {
			return id
		}
	}
}

// normalizePrefixEntries trims and sorts so the state file and the page cannot
// show a reordered list as a change. Duplicates are kept here: the save gate
// refuses them, and silently collapsing one would hide the mistake.
func normalizePrefixEntries(es []PrefixEntry) {
	for i := range es {
		es[i].Prefix = strings.TrimSpace(es[i].Prefix)
	}
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Prefix != es[j].Prefix {
			return es[i].Prefix < es[j].Prefix
		}
		if es[i].GE != es[j].GE {
			return es[i].GE < es[j].GE
		}
		return es[i].LE < es[j].LE
	})
}

func normalizeLeakRule(r *LeakRule) {
	r.PrefixList = strings.TrimSpace(r.PrefixList)
	seen := map[string]bool{}
	out := make([]string, 0, len(r.Protocols))
	for _, p := range r.Protocols {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	r.Protocols = out
}

// normalizeLeakSet dedupes by VPN (last row wins), sorts by VPN and makes empty
// slices non-nil, so "no rule" and "a rule with nothing in it" cannot be
// confused on disk.
func normalizeLeakSet(set *LeakSet) {
	set.ToCPE = normalizeLeakRules(set.ToCPE)
	set.ToSDWAN = normalizeLeakRules(set.ToSDWAN)
}

func normalizeLeakRules(rows []LeakRule) []LeakRule {
	byVPN := map[uint32]LeakRule{}
	for _, r := range rows {
		normalizeLeakRule(&r)
		byVPN[r.VPN] = r
	}
	out := make([]LeakRule, 0, len(byVPN))
	for _, r := range byVPN {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VPN < out[j].VPN })
	return out
}

// rules returns the slice behind one direction. The pointer is used by the
// write paths; readers get copies through leakSetCopy.
func (set *LeakSet) rules(direction string) *[]LeakRule {
	if direction == leakToCPE {
		return &set.ToCPE
	}
	return &set.ToSDWAN
}

func (set LeakSet) rule(direction string, vpn uint32) (LeakRule, bool) {
	for _, r := range *set.rules(direction) {
		if r.VPN == vpn {
			return r, true
		}
	}
	return LeakRule{}, false
}

// put stores one rule, replacing any row for the same VPN. Empty protocol
// lists are dropped rather than stored: an empty row means "this VPN leaks
// nothing", which is exactly the absence of a rule.
func (set *LeakSet) put(direction string, r LeakRule) {
	normalizeLeakRule(&r)
	rows := set.rules(direction)
	out := make([]LeakRule, 0, len(*rows)+1)
	replaced := false
	for _, old := range *rows {
		if old.VPN != r.VPN {
			out = append(out, old)
			continue
		}
		replaced = true
		if len(r.Protocols) > 0 {
			out = append(out, r)
		}
	}
	if !replaced && len(r.Protocols) > 0 {
		out = append(out, r)
	}
	*rows = out
	normalizeLeakSet(set)
}

func (set *LeakSet) drop(direction string, vpn uint32) bool {
	rows := set.rules(direction)
	out := make([]LeakRule, 0, len(*rows))
	removed := false
	for _, r := range *rows {
		if r.VPN == vpn {
			removed = true
			continue
		}
		out = append(out, r)
	}
	*rows = out
	return removed
}

func leakSetCopy(in LeakSet) LeakSet {
	out := LeakSet{
		ToCPE:   append([]LeakRule(nil), in.ToCPE...),
		ToSDWAN: append([]LeakRule(nil), in.ToSDWAN...),
	}
	if out.ToCPE == nil {
		out.ToCPE = []LeakRule{}
	}
	if out.ToSDWAN == nil {
		out.ToSDWAN = []LeakRule{}
	}
	for i := range out.ToCPE {
		out.ToCPE[i].Protocols = append([]string(nil), out.ToCPE[i].Protocols...)
	}
	for i := range out.ToSDWAN {
		out.ToSDWAN[i].Protocols = append([]string(nil), out.ToSDWAN[i].Protocols...)
	}
	return out
}

// leakProtocols is the vocabulary of one direction. static/connected exist only
// in CPE-SDWAN: SDWAN→CPE can only originate through BGP or OSPF.
func leakProtocols(direction string) []string {
	if direction == leakToCPE {
		return []string{leakProtoBGP, leakProtoOSPF}
	}
	return []string{leakProtoBGP, leakProtoOSPF, leakProtoStatic, leakProtoConnected}
}

func leakDirectionLabel(direction string) string {
	if direction == leakToCPE {
		return "SDWAN-CPE"
	}
	return "CPE-SDWAN"
}

// srfrrArtifact is bundle 第 5 段: the keys of /etc/sr-frr.json the controller
// owns (docs/o6e-leak-plan.md §4). Both lists are always non-nil so removing the
// last row produces an explicit "leak nothing" document rather than dropping the
// segment and leaving the box's previous document in place.
type srfrrArtifact struct {
	ExportVRFs  []uint32          `json:"export_vrfs"`
	ImportRules []srfrrImportRule `json:"import_rules"`
}

// srfrrImportRule is one CPE-SDWAN VPN with its whitelist already expanded.
// The box filters by these entries; it never reads the tenant's library.
type srfrrImportRule struct {
	VPN      uint32        `json:"vpn"`
	Sources  []string      `json:"sources"`
	Prefixes []PrefixEntry `json:"prefixes"`
}

// leakRenderLocked expands the tenant's library into the two shapes a publish
// needs: SDWAN-CPE rows for renderFRR and the srfrr segment. A list that
// vanished expands to nothing; the publish gate has already refused that case,
// so this only keeps a page preview deterministic.
func (s *server) leakRenderLocked(tenantID string, set LeakSet) ([]leakRenderRule, srfrrArtifact) {
	lc := s.leakConfigLocked(tenantID, true)
	expand := func(id string) []PrefixEntry {
		if id == "" {
			return nil
		}
		pl := lc.list(id)
		if pl == nil {
			return nil
		}
		out := make([]PrefixEntry, len(pl.Entries))
		copy(out, pl.Entries)
		return out
	}

	art := srfrrArtifact{ExportVRFs: []uint32{}, ImportRules: []srfrrImportRule{}}
	var rules []leakRenderRule
	for _, r := range set.ToCPE {
		if len(r.Protocols) == 0 {
			continue
		}
		rules = append(rules, leakRenderRule{
			VPN: r.VPN, Protocols: r.Protocols,
			Entries: expand(r.PrefixList), HasList: r.PrefixList != "",
		})
		art.ExportVRFs = append(art.ExportVRFs, r.VPN)
	}
	for _, r := range set.ToSDWAN {
		if len(r.Protocols) == 0 {
			continue
		}
		prefixes := expand(r.PrefixList)
		if prefixes == nil {
			prefixes = []PrefixEntry{}
		}
		art.ImportRules = append(art.ImportRules, srfrrImportRule{
			VPN: r.VPN, Sources: r.Protocols, Prefixes: prefixes,
		})
	}
	// normalizeLeakSet already sorts, but the artifact is compared byte for byte
	// and map-fed callers exist; sorting here keeps two renders identical.
	sort.Slice(rules, func(i, j int) bool { return rules[i].VPN < rules[j].VPN })
	sort.Slice(art.ExportVRFs, func(i, j int) bool { return art.ExportVRFs[i] < art.ExportVRFs[j] })
	sort.Slice(art.ImportRules, func(i, j int) bool { return art.ImportRules[i].VPN < art.ImportRules[j].VPN })
	return rules, art
}

func configVPNOf(c *DeviceConfig, vpnID uint32) *ConfigVPN {
	if c == nil {
		return nil
	}
	for i := range c.Draft.VPNs {
		if c.Draft.VPNs[i].ID == vpnID {
			return &c.Draft.VPNs[i]
		}
	}
	return nil
}

// prefixListChecks validates one candidate list. `selfID` is excluded from the
// name-uniqueness rule so an edit does not collide with itself.
func prefixListChecks(l PrefixList, others []PrefixList, selfID string) []ConfigCheck {
	var out []ConfigCheck
	add := func(severity, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: severity, Text: fmt.Sprintf(format, args...)})
	}

	name := strings.TrimSpace(l.Name)
	if name == "" {
		add("error", "prefix-list 要有名字：规则引用时看的就是它。")
	} else if len(name) > leakMaxNameLen {
		add("error", "prefix-list 名字最多 %d 个字符。", leakMaxNameLen)
	} else if !validDescription(name) {
		add("error", "prefix-list 名字不能包含控制字符。")
	}
	for _, other := range others {
		if other.ID != selfID && strings.EqualFold(strings.TrimSpace(other.Name), name) && name != "" {
			add("error", "已经有一个叫「%s」的 prefix-list：名字在租户内要能区分。", name)
			break
		}
	}
	if len(l.Entries) > leakMaxEntries {
		add("error", "prefix-list 条目最多 %d 条，当前 %d。", leakMaxEntries, len(l.Entries))
	}
	seen := map[string]bool{}
	for _, e := range l.Entries {
		p, err := netip.ParsePrefix(e.Prefix)
		if err != nil || !p.Addr().Is4() || p.Bits() > 32 {
			add("error", "条目「%s」不是合法 IPv4 前缀（应为 a.b.c.d/len）。", e.Prefix)
			continue
		}
		if p.Masked() != p {
			add("error", "条目 %s 不是规范网段，应写成 %s。", e.Prefix, p.Masked())
		}
		bits := p.Bits()
		if e.GE != 0 && (e.GE < bits || e.GE > 32) {
			add("error", "条目 %s 的 ge 需在 %d–32 之间。", e.Prefix, bits)
		}
		if e.LE != 0 && (e.LE < bits || e.LE > 32) {
			add("error", "条目 %s 的 le 需在 %d–32 之间。", e.Prefix, bits)
		}
		if e.GE != 0 && e.LE != 0 && e.LE < e.GE {
			add("error", "条目 %s 的 le 不能小于 ge（ge=%d, le=%d）。", e.Prefix, e.GE, e.LE)
		}
		key := fmt.Sprintf("%s|%d|%d", e.Prefix, e.GE, e.LE)
		if seen[key] {
			add("error", "条目 %s（ge=%d le=%d）重复。", e.Prefix, e.GE, e.LE)
		}
		seen[key] = true
	}
	return out
}

// leakRuleChecksLocked is the gate from §8. It runs on save, on batch preview
// and again through publishChecksLocked, because a device's base config or the
// tenant's 导通组 can change after a row was written.
func (s *server) leakRuleChecksLocked(tenantID string, nodeID uint16, direction string, r LeakRule) []ConfigCheck {
	var out []ConfigCheck
	add := func(severity, format string, args ...any) {
		out = append(out, ConfigCheck{Severity: severity, Text: fmt.Sprintf(format, args...)})
	}
	label := leakDirectionLabel(direction)

	if direction != leakToCPE && direction != leakToSDWAN {
		add("error", "未知的泄露方向「%s」。", direction)
		return out
	}
	dc := s.state.Configs[configKey(tenantID, nodeID)]
	if dc == nil || dc.Version == 0 {
		add("error", "设备 %d 还没有保存过基础配置草稿（v0）：先去「配置 › 基础配置」保存一次。", nodeID)
		return out
	}
	if r.VPN > uint32(route.MaxVRFID) {
		add("error", "VPN %d 超出泄露规则范围（0–%d）：路由平面的 VRF id 只有 8 位。", r.VPN, route.MaxVRFID)
		return out
	}
	if r.VPN == vpnReservedOverlay {
		add("error", "VPN %d 是 overlay 保留表（隧道与对外下一跳用的那张），不能泄露。", r.VPN)
		return out
	}
	if kernelReservedVPN(r.VPN) {
		add("error", "VPN %d 是内核保留表（253 default / 254 main / 255 local）：盒内把 VPN N 落进内核表 N，泄露出去会覆盖内核自己的表。", r.VPN)
		return out
	}
	if !configHasVPN(dc, r.VPN) {
		add("error", "设备 %d 的基础配置里没有 VPN %d：先在「配置 › 基础配置」加这个 VPN。", nodeID, r.VPN)
		return out
	}
	if s.overlayGroupCountLocked(tenantID, nodeID, r.VPN, "") == 0 {
		add("error", "设备 %d 的 VPN %d 不在任何导通策略里：没有 RT，泄露出去的前缀也进不了 overlay。"+
			"先在「导通策略配置」把它加进至少一个组。", nodeID, r.VPN)
	}
	if len(r.Protocols) == 0 {
		add("error", "%s 规则至少选一个来源；一个都不选等于没有这条规则。", label)
	}
	vc := configVPNOf(dc, r.VPN)
	for _, p := range r.Protocols {
		switch p {
		case leakProtoBGP:
			if vc == nil || !vc.BGP.On {
				add("error", "设备 %d 的 VPN %d 没有启用 BGP：先在「基础配置」打开这个实例。", nodeID, r.VPN)
			}
		case leakProtoOSPF:
			if vc == nil || !vc.OSPF.On {
				add("error", "设备 %d 的 VPN %d 没有启用 OSPF：先在「基础配置」打开这个实例。", nodeID, r.VPN)
			}
		case leakProtoStatic, leakProtoConnected:
			if direction == leakToCPE {
				add("error", "%s 只能把 BGP / OSPF 路由放进 SD-WAN；static / connected 只属于 CPE-SDWAN 方向。", label)
			}
		default:
			add("error", "未知来源「%s」：%s 的取值是 %s。", p, label, strings.Join(leakProtocols(direction), " / "))
		}
	}
	lc := s.leakConfigLocked(tenantID, true)
	if r.PrefixList != "" {
		pl := lc.list(r.PrefixList)
		if pl == nil {
			add("error", "prefix-list「%s」不存在（可能已经被删除）：换一个列表。", r.PrefixList)
		} else if len(pl.Entries) == 0 && direction == leakToSDWAN {
			add("error", "prefix-list「%s」没有条目：CPE-SDWAN 会一条都上报不了，先给它加条目。", pl.Name)
		}
	} else if direction == leakToSDWAN {
		add("error", "CPE-SDWAN 必须选一个 prefix-list（默认 ALL 就是 0.0.0.0/0）。")
	}
	return out
}

// leakSetChecksLocked validates every stored rule. `set` is passed explicitly so
// the batch endpoint can check the candidate set before it is written.
func (s *server) leakSetChecksLocked(tenantID string, nodeID uint16, set LeakSet) []ConfigCheck {
	var out []ConfigCheck
	for _, r := range set.ToCPE {
		out = append(out, s.leakRuleChecksLocked(tenantID, nodeID, leakToCPE, r)...)
	}
	for _, r := range set.ToSDWAN {
		out = append(out, s.leakRuleChecksLocked(tenantID, nodeID, leakToSDWAN, r)...)
	}
	return out
}

// publishChecksLocked is the one gate every publish path uses: base config,
// overlay membership, the stored leak rules and the stored static policy.
// publishChecksForLeakLocked is the batch variant that checks a candidate leak
// set before any write.
func (s *server) publishChecksLocked(tenantID string, nodeID uint16, dc *DeviceConfig) []ConfigCheck {
	return s.publishChecksForLeakLocked(tenantID, nodeID, dc, dc.Leak)
}

func (s *server) publishChecksForLeakLocked(tenantID string, nodeID uint16, dc *DeviceConfig, set LeakSet) []ConfigCheck {
	checks := validateBasicConfig(&dc.Draft)
	checks = append(checks, s.overlayNodeChecksLocked(tenantID, nodeID)...)
	checks = append(checks, s.leakSetChecksLocked(tenantID, nodeID, set)...)
	// 策略排最后：它的错误要建立在"VPN 与客户口已经合法"这个前提上，排在前面
	// 只会让一条根因变成一串连带报错。
	checks = append(checks, s.policyChecksLocked(tenantID, nodeID, dc, dc.Policy)...)
	return checks
}

// prefixListRefsLocked lists the devices whose stored rules use one list.
func (s *server) prefixListRefsLocked(tenantID, listID string) []uint16 {
	var out []uint16
	for _, n := range s.tenantNodesLocked(tenantID) {
		dc := s.state.Configs[configKey(tenantID, n.NodeID)]
		if dc == nil {
			continue
		}
		if leakSetReferences(dc.Leak, listID) {
			out = append(out, n.NodeID)
		}
	}
	return out
}

func leakSetReferences(set LeakSet, listID string) bool {
	if listID == "" {
		return false
	}
	for _, rows := range [][]LeakRule{set.ToCPE, set.ToSDWAN} {
		for _, r := range rows {
			if r.PrefixList == listID {
				return true
			}
		}
	}
	return false
}

// leakPrefixListRow is one library row plus the two counts the page needs to
// make "需重新发布" actionable.
type leakPrefixListRow struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Entries            []PrefixEntry `json:"entries"`
	ReferencedBy       int           `json:"referenced_by"`
	ReferencingDevices []uint16      `json:"referencing_devices"`
	PendingDevices     int           `json:"pending_devices"`
}

// leakVPNRow is one VPN of one device as the picker sees it: what the box
// actually has, whether a rule can exist at all, and the current rows.
type leakVPNRow struct {
	ID         uint32    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Selectable bool      `json:"selectable"`
	Note       string    `json:"note,omitempty"`
	InGroup    bool      `json:"in_group"`
	HasBGP     bool      `json:"has_bgp"`
	HasOSPF    bool      `json:"has_ospf"`
	ToCPE      *LeakRule `json:"to_cpe,omitempty"`
	ToSDWAN    *LeakRule `json:"to_sdwan,omitempty"`
}

// leakNodeRow is one device: the row the page filters and expands.
type leakNodeRow struct {
	NodeID           uint16            `json:"node_id"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	HasDraft         bool              `json:"has_draft"`
	Version          int               `json:"version"`
	PublishedVersion int               `json:"published_version,omitempty"`
	Pending          bool              `json:"pending"`
	Device           *wire.ConfigState `json:"device_state,omitempty"`
	VPNs             []leakVPNRow      `json:"vpns"`
	Checks           []ConfigCheck     `json:"checks"`
}

type leakView struct {
	TenantID       string              `json:"tenant_id"`
	NowUnix        int64               `json:"now_unix"`
	PrefixLists    []leakPrefixListRow `json:"prefix_lists"`
	Nodes          []leakNodeRow       `json:"nodes"`
	Checks         []ConfigCheck       `json:"checks"`
	MaxEntries     int                 `json:"max_entries"`
	MaxPrefixLists int                 `json:"max_prefix_lists"`
	Notes          map[string]string   `json:"notes"`
}

// leakViewLocked builds everything the two pages show. It derives the VPN
// picker from the device's own draft, so a VPN the box does not have can never
// be selected, and it never fabricates reachability: an unconfigured device is
// shown as v0 with no rules rather than with defaults it does not run.
func (s *server) leakViewLocked(tenantID string, now int64) leakView {
	lc := s.leakConfigLocked(tenantID, true)
	view := leakView{
		TenantID:       tenantID,
		NowUnix:        now,
		MaxEntries:     leakMaxEntries,
		MaxPrefixLists: leakMaxPrefixLists,
		Checks:         []ConfigCheck{},
		Notes: map[string]string{
			"library": "prefix-list 只属于这个租户，两个泄露方向共用同一份；只做 permit，不做 deny。",
			"to_cpe": "SDWAN-CPE：把 overlay 收到的前缀注入这台设备的内核表（K 路由），" +
				"再由该 VPN 的 BGP/OSPF 分发给客户。不挂列表 = 全放过。",
			"to_sdwan": "CPE-SDWAN：只把该 VPN 选中的来源、命中白名单的前缀上报进 SD-WAN；" +
				"默认列表 ALL（0.0.0.0/0）。",
		},
	}
	for _, pl := range lc.PrefixLists {
		view.Checks = append(view.Checks, prefixListChecks(pl, lc.PrefixLists, pl.ID)...)
	}

	names := map[uint16]string{}
	for _, v := range s.nodeViewsLocked(tenantID, now) {
		names[v.ID] = v.Name
	}

	refs := map[string][]uint16{}
	pendingRefs := map[string]int{}

	view.Nodes = []leakNodeRow{}
	for _, n := range s.tenantNodesLocked(tenantID) {
		row := leakNodeRow{
			NodeID: n.NodeID,
			Name:   names[n.NodeID],
			Status: nodeStatus(n),
			Device: n.Config,
			VPNs:   []leakVPNRow{},
			Checks: []ConfigCheck{},
		}
		dc := s.state.Configs[configKey(tenantID, n.NodeID)]
		var set LeakSet
		if dc != nil {
			row.Version = dc.Version
			row.HasDraft = dc.Version > 0
			set = leakSetCopy(dc.Leak)
			if dc.Published != nil {
				row.PublishedVersion = dc.Published.Version
				art := s.artifactsLocked(tenantID, n.NodeID, &dc.Draft, dc.Leak, dc.Policy)
				row.Pending = dc.Published.Version != dc.Version || dc.Published.Artifacts != art
			}
		}
		if row.HasDraft {
			row.Checks = append(row.Checks, s.leakSetChecksLocked(tenantID, n.NodeID, set)...)
		}
		if row.Pending {
			seen := map[string]bool{}
			for _, rows := range [][]LeakRule{set.ToCPE, set.ToSDWAN} {
				for _, r := range rows {
					if r.PrefixList == "" || seen[r.PrefixList] {
						continue
					}
					seen[r.PrefixList] = true
					pendingRefs[r.PrefixList]++
				}
			}
		}
		for _, rows := range [][]LeakRule{set.ToCPE, set.ToSDWAN} {
			for _, r := range rows {
				if r.PrefixList != "" {
					refs[r.PrefixList] = append(refs[r.PrefixList], n.NodeID)
				}
			}
		}

		vpns := []ConfigVPN{}
		if row.HasDraft {
			vpns = append(vpns, dc.Draft.VPNs...)
		} else {
			vpns = append(vpns, defaultBasicConfig().VPNs...)
		}
		sort.Slice(vpns, func(i, j int) bool { return vpns[i].ID < vpns[j].ID })
		for _, vp := range vpns {
			vrow := leakVPNRow{
				ID: vp.ID, Name: vp.Name,
				HasBGP: vp.BGP.On, HasOSPF: vp.OSPF.On,
			}
			vrow.InGroup = s.overlayGroupCountLocked(tenantID, n.NodeID, vp.ID, "") > 0
			switch {
			case vp.ID > uint32(route.MaxVRFID):
				vrow.Note = fmt.Sprintf("VPN 号超过 %d：路由平面的 VRF id 是 8 位，泄露只支持 0–%d。",
					route.MaxVRFID, route.MaxVRFID)
			case vp.ID == vpnReservedOverlay:
				vrow.Note = "overlay 保留表（隧道与对外下一跳），不参与泄露。"
			case kernelReservedVPN(vp.ID):
				vrow.Note = fmt.Sprintf("VPN %d 是内核保留表（253 default / 254 main / 255 local），不参与泄露。", vp.ID)
			case !row.HasDraft:
				vrow.Note = "这台设备还没有保存过基础配置草稿（v0）。"
			case !vrow.InGroup:
				vrow.Note = "不在任何导通策略里：先把它加进至少一个组，前缀才有 RT 可走。"
			default:
				vrow.Selectable = true
			}
			if r, ok := set.rule(leakToCPE, vp.ID); ok {
				cp := r
				vrow.ToCPE = &cp
			}
			if r, ok := set.rule(leakToSDWAN, vp.ID); ok {
				cp := r
				vrow.ToSDWAN = &cp
			}
			row.VPNs = append(row.VPNs, vrow)
		}
		view.Nodes = append(view.Nodes, row)
	}

	for _, pl := range lc.PrefixLists {
		devices := refs[pl.ID]
		if devices == nil {
			devices = []uint16{}
		}
		sort.Slice(devices, func(i, j int) bool { return devices[i] < devices[j] })
		view.PrefixLists = append(view.PrefixLists, leakPrefixListRow{
			ID:                 pl.ID,
			Name:               pl.Name,
			Entries:            append([]PrefixEntry(nil), pl.Entries...),
			ReferencedBy:       len(devices),
			ReferencingDevices: devices,
			PendingDevices:     pendingRefs[pl.ID],
		})
	}
	return view
}
