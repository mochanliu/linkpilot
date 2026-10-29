package main

// 预授权 -- a device the tenant registers on paper before it exists.
//
// The problem this solves is ordering, not authority: a box arrives on site next
// month, but the customer wants its hostname, interfaces, static routes and BGP
// neighbours decided now. Until the box enrolls there is no NodeRecord (one is
// only ever created inside handleEnroll) and therefore no node_id, so a
// pre-authorization cannot be a device row in disguise -- it is its own record,
// and the config draft hangs off that record until the enrollment hands it to
// the real node.
//
// What it changes about seats (his D2): creating one takes a seat out of the
// grant immediately, so MaxUses covers 已入网 + 未认领预授权 and an unclaimed
// record cannot be ignored by a plain enrollment.
//
// What it does NOT change: publishing. A pre-authorization has no device to
// install anything, so the draft can only be saved -- the explicit publish gate,
// the frozen bundle and the heartbeat pull stay exactly as they were, and D4
// keeps a binding from auto-publishing what it just transferred.

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	preAuthNameMaxRunes = 64
	preAuthSerialMax    = 64
)

// PreAuth is one on-paper device. Name is the index the field engineer types at
// enrollment, so it has to be unique and stable within the tenant -- which is
// also why a claimed record stops being editable: it is now the record of what
// was typed on site.
type PreAuth struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	ClaimID   string `json:"claim_id"`
	Name      string `json:"name"`
	SiteID    string `json:"site_id,omitempty"`
	CreatedAt int64  `json:"created_at_unix"`
	CreatedBy string `json:"created_by,omitempty"`
	UpdatedAt int64  `json:"updated_at_unix,omitempty"`

	// Serial is what the admin believes the box's serial number is. Optional:
	// the label is often unreadable until the box is out of its carton. When it
	// IS filled, enrollment refuses a device that reports a different one --
	// otherwise a mistyped index would quietly move a config onto the wrong box.
	Serial string `json:"serial,omitempty"`

	// Bound* are written by the enrollment that claimed this record and never
	// cleared, so the row stays the audit trail of the hand-over.
	BoundNodeID uint16 `json:"bound_node_id,omitempty"`
	BoundSerial string `json:"bound_serial,omitempty"`
	BoundAt     int64  `json:"bound_at_unix,omitempty"`
}

func (p *PreAuth) claimed() bool { return p != nil && p.BoundNodeID != 0 }

// newPreAuthID is a pre-authorization's handle. Same rule as a grant id: random
// so ids cannot be enumerated, and carrying no authority of its own -- the
// tenant's session is what authorizes these calls.
func newPreAuthID() string {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("console: entropy unavailable: %v", err)
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return "pa_" + string(out)
}

// preAuthConfigKey keys the draft map. The prefix differs from a node key so a
// pre-authorization id can never collide with a node number.
func preAuthConfigKey(tenantID, preAuthID string) string {
	return tenantID + "|" + preAuthID
}

// seatsTakenLocked is the seat ledger of a grant: activated devices plus the
// 预授权 records still holding a seat.
func seatsTakenLocked(c *Claim) int { return len(c.Serials) + len(c.Reserved) }

func (s *server) preAuthLocked(tenant, id string) *PreAuth {
	p := s.state.PreAuths[id]
	if p == nil || p.TenantID != tenant {
		return nil
	}
	return p
}

// preAuthByNameLocked resolves the index a field engineer types. The match is
// case-insensitive because this is typed on a console at a site, and a name that
// differs only in case is not a second device.
func (s *server) preAuthByNameLocked(tenant, name string) *PreAuth {
	want := strings.TrimSpace(name)
	if want == "" {
		return nil
	}
	for _, p := range s.state.PreAuths {
		if p != nil && p.TenantID == tenant && strings.EqualFold(p.Name, want) {
			return p
		}
	}
	return nil
}

// nameTakenLocked is the uniqueness rule behind "the index identifies exactly
// one record". Claimed records count: a second 预授权 reusing the name of a box
// that already claimed one would be ambiguous at the site.
func (s *server) nameTakenLocked(tenant, name, exceptID string) *PreAuth {
	for _, p := range s.state.PreAuths {
		if p == nil || p.TenantID != tenant || p.ID == exceptID {
			continue
		}
		if strings.EqualFold(p.Name, name) {
			return p
		}
	}
	return nil
}

// serialTakenLocked stops two unclaimed records from expecting the same physical
// box, which would make "the serial matches" meaningless.
func (s *server) serialTakenLocked(tenant, serial, exceptID string) *PreAuth {
	for _, p := range s.state.PreAuths {
		if p == nil || p.TenantID != tenant || p.ID == exceptID || p.Serial == "" || p.claimed() {
			continue
		}
		if strings.EqualFold(p.Serial, serial) {
			return p
		}
	}
	return nil
}

// unclaimedPreAuthNamesLocked lists the indexes an engineer could type right
// now. It belongs in the refusal because "index not found" with no list means a
// phone call to the office from a rack.
func (s *server) unclaimedPreAuthNamesLocked(tenant string) string {
	names := []string{}
	for _, p := range s.state.PreAuths {
		if p != nil && p.TenantID == tenant && !p.claimed() {
			names = append(names, "「"+p.Name+"」")
		}
	}
	if len(names) == 0 {
		return "无"
	}
	sort.Strings(names)
	if len(names) > 12 {
		return strings.Join(names[:12], "、") + fmt.Sprintf(" 等 %d 个", len(names))
	}
	return strings.Join(names, "、")
}

func (s *server) tenantPreAuthsLocked(tenant string) []*PreAuth {
	out := []*PreAuth{}
	for _, p := range s.state.PreAuths {
		if p != nil && p.TenantID == tenant {
			out = append(out, p)
		}
	}
	// Unclaimed first, newest first inside each group: the list a tenant works
	// from is the one still waiting for a box.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.claimed() != b.claimed() {
			return !a.claimed()
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		return a.ID < b.ID
	})
	return out
}

// ------------------------------------------------------------------ views

type consolePreAuth struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Serial      string `json:"serial,omitempty"`
	ClaimID     string `json:"claim_id"`
	ClaimName   string `json:"claim_name,omitempty"`
	SiteID      string `json:"site_id,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	CreatedAt   int64  `json:"created_at_unix"`
	CreatedBy   string `json:"created_by,omitempty"`
	UpdatedAt   int64  `json:"updated_at_unix,omitempty"`
	BoundNodeID uint16 `json:"bound_node_id,omitempty"`
	BoundSerial string `json:"bound_serial,omitempty"`
	BoundAt     int64  `json:"bound_at_unix,omitempty"`

	// The draft is what "预授权" is for, so its version travels with the row:
	// a record with v0 is one nobody has configured yet, and the page must not
	// render that as "已配置".
	ConfigVersion   int    `json:"config_version"`
	ConfigUpdatedAt int64  `json:"config_updated_at_unix,omitempty"`
	ConfigUpdatedBy string `json:"config_updated_by,omitempty"`

	// ClaimRevoked is the difference between "waiting for the box" and "can
	// never be claimed": a grant that was revoked has no live code, so the
	// index on site will be refused. Showing it beats letting the customer
	// discover it at the rack.
	ClaimRevoked bool `json:"claim_revoked,omitempty"`
}

func (s *server) preAuthViewLocked(p *PreAuth) consolePreAuth {
	v := consolePreAuth{
		ID: p.ID, TenantID: p.TenantID, Name: p.Name, Serial: p.Serial,
		ClaimID: p.ClaimID, SiteID: p.SiteID,
		CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.UpdatedAt,
		BoundNodeID: p.BoundNodeID, BoundSerial: p.BoundSerial, BoundAt: p.BoundAt,
	}
	if si := s.siteLocked(p.TenantID, p.SiteID); si != nil {
		v.SiteName = si.Name
	}
	if c := s.state.Claims[p.ClaimID]; c != nil && c.TenantID == p.TenantID {
		v.ClaimName = c.Name
		v.ClaimRevoked = c.Revoked
	}
	if !p.claimed() {
		if dc := s.state.PreAuthConfigs[preAuthConfigKey(p.TenantID, p.ID)]; dc != nil {
			v.ConfigVersion = dc.Version
			v.ConfigUpdatedAt = dc.UpdatedAt
			v.ConfigUpdatedBy = dc.UpdatedBy
		}
	}
	return v
}

func (s *server) preAuthListLocked(tenant string) []consolePreAuth {
	out := []consolePreAuth{}
	for _, p := range s.tenantPreAuthsLocked(tenant) {
		out = append(out, s.preAuthViewLocked(p))
	}
	return out
}

// preAuthsResponse is what every write returns: the whole list, because the
// seat count on the grant and the per-site numbers both move when a record is
// created or revoked, and the page should not need a second round trip to stop
// showing a stale count.
func (s *server) preAuthsResponseLocked(w http.ResponseWriter, tenant string, extra map[string]any) {
	out := map[string]any{
		"ok":       true,
		"preauths": s.preAuthListLocked(tenant),
		"claims":   s.tenantClaimViewsLocked(tenant),
	}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}

// ------------------------------------------------------------------ validation

// validPreAuthName rejects what a field engineer could not type reliably: empty,
// absurdly long, or containing control characters. Spaces are allowed (the agent
// flag takes one quoted argument), but leading/trailing ones are trimmed away so
// "CPE-01 " and "CPE-01" cannot be two records.
func validPreAuthName(name string) bool {
	r := []rune(name)
	if len(r) < 1 || len(r) > preAuthNameMaxRunes {
		return false
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func validPreAuthSerial(serial string) bool {
	r := []rune(serial)
	if len(r) < 1 || len(r) > preAuthSerialMax {
		return false
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f || c == ' ' {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------ handlers

type createPreAuthRequest struct {
	ClaimID string `json:"claim_id"`
	Name    string `json:"name"`
	Serial  string `json:"serial"`
	SiteID  string `json:"site_id"`
}

type updatePreAuthRequest struct {
	ID     string  `json:"id"`
	Name   *string `json:"name"`
	Serial *string `json:"serial"`
	SiteID *string `json:"site_id"`
}

type preAuthIDRequest struct {
	ID string `json:"id"`
}

// handleAdminPreAuth: GET lists, POST creates, PUT edits. Tenant-only -- the
// operator role hands out grants and never names a customer's devices.
func (s *server) handleAdminPreAuth(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, map[string]any{
			"preauths": s.preAuthListLocked(id.TenantID),
			"now_unix": time.Now().Unix(),
		})
		return
	case http.MethodPost, http.MethodPut:
	default:
		methodNotAllowed(w, "GET, POST, PUT")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost {
		var req createPreAuthRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		s.createPreAuthLocked(w, id, &req)
		return
	}
	var req updatePreAuthRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.updatePreAuthLocked(w, id, &req)
}

func (s *server) createPreAuthLocked(w http.ResponseWriter, id consoleIdentity, req *createPreAuthRequest) {
	claim := s.state.Claims[strings.TrimSpace(req.ClaimID)]
	if claim == nil || claim.TenantID != id.TenantID {
		// Same answer as a grant that never existed: this endpoint must not
		// reveal which tenant owns which grant.
		http.Error(w, "授权不存在", http.StatusNotFound)
		return
	}
	if claim.Revoked {
		http.Error(w, "该授权已吊销，预授权无法认领", http.StatusConflict)
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validPreAuthName(name) {
		http.Error(w, fmt.Sprintf("设备名称需要 1-%d 个字符，且不能含控制字符", preAuthNameMaxRunes),
			http.StatusBadRequest)
		return
	}
	if taken := s.nameTakenLocked(id.TenantID, name, ""); taken != nil {
		if taken.claimed() {
			http.Error(w, fmt.Sprintf("名称「%s」已被 node %d 认领，不能重复使用", taken.Name, taken.BoundNodeID),
				http.StatusConflict)
		} else {
			http.Error(w, fmt.Sprintf("名称「%s」已经预授权过（%s）", taken.Name, taken.ID), http.StatusConflict)
		}
		return
	}
	serial := strings.TrimSpace(req.Serial)
	if serial != "" {
		if !validPreAuthSerial(serial) {
			http.Error(w, "序列号需要 1-64 个字符，且不能含空格或控制字符", http.StatusBadRequest)
			return
		}
		if other := s.serialTakenLocked(id.TenantID, serial, ""); other != nil {
			http.Error(w, fmt.Sprintf("序列号 %s 已经登记在预授权「%s」上", serial, other.Name), http.StatusConflict)
			return
		}
	}
	siteID := strings.TrimSpace(req.SiteID)
	if siteID != "" && s.siteLocked(id.TenantID, siteID) == nil {
		http.Error(w, "站点不存在", http.StatusNotFound)
		return
	}
	// D2: the seat is taken now, not at enrollment.
	if claim.MaxUses > 0 && seatsTakenLocked(claim) >= claim.MaxUses {
		http.Error(w, fmt.Sprintf("授权「%s」名额已满：已入网 %d 台、预授权未认领 %d 个，共 %d。先撤销一条预授权或提高名额",
			claim.Name, len(claim.Serials), len(claim.Reserved), claim.MaxUses), http.StatusConflict)
		return
	}

	now := time.Now().Unix()
	p := &PreAuth{ID: newPreAuthID(), TenantID: id.TenantID, ClaimID: claim.ID,
		Name: name, Serial: serial, SiteID: siteID, CreatedAt: now, CreatedBy: id.Username}
	for s.state.PreAuths[p.ID] != nil {
		p.ID = newPreAuthID()
	}
	s.state.PreAuths[p.ID] = p
	claim.Reserved = append(claim.Reserved, p.ID)
	s.save()
	log.Printf("console: tenant %s 预授权 %s (%s) on grant %s，名额 %d/%d（其中预授权 %d）",
		id.TenantID, p.ID, p.Name, claim.ID, len(claim.Serials), claim.MaxUses, len(claim.Reserved))
	s.preAuthsResponseLocked(w, id.TenantID, map[string]any{"preauth": s.preAuthViewLocked(p)})
}

func (s *server) updatePreAuthLocked(w http.ResponseWriter, id consoleIdentity, req *updatePreAuthRequest) {
	p := s.preAuthLocked(id.TenantID, strings.TrimSpace(req.ID))
	if p == nil {
		http.Error(w, "预授权记录不存在", http.StatusNotFound)
		return
	}
	if p.claimed() {
		http.Error(w, fmt.Sprintf("这条预授权已被 node %d 认领，名称与登记序列号是现场认领的凭证，不再可改；设备配置请去基础配置页",
			p.BoundNodeID), http.StatusConflict)
		return
	}
	name := p.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if !validPreAuthName(name) {
			http.Error(w, fmt.Sprintf("设备名称需要 1-%d 个字符，且不能含控制字符", preAuthNameMaxRunes), http.StatusBadRequest)
			return
		}
		if taken := s.nameTakenLocked(id.TenantID, name, p.ID); taken != nil {
			http.Error(w, fmt.Sprintf("名称「%s」已被另一条预授权占用（%s）", name, taken.ID), http.StatusConflict)
			return
		}
	}
	serial := p.Serial
	if req.Serial != nil {
		serial = strings.TrimSpace(*req.Serial)
		if serial != "" {
			if !validPreAuthSerial(serial) {
				http.Error(w, "序列号需要 1-64 个字符，且不能含空格或控制字符", http.StatusBadRequest)
				return
			}
			if other := s.serialTakenLocked(id.TenantID, serial, p.ID); other != nil {
				http.Error(w, fmt.Sprintf("序列号 %s 已经登记在预授权「%s」上", serial, other.Name), http.StatusConflict)
				return
			}
		}
	}
	siteID := p.SiteID
	if req.SiteID != nil {
		siteID = strings.TrimSpace(*req.SiteID)
		if siteID != "" && s.siteLocked(id.TenantID, siteID) == nil {
			http.Error(w, "站点不存在", http.StatusNotFound)
			return
		}
	}
	p.Name, p.Serial, p.SiteID = name, serial, siteID
	p.UpdatedAt = time.Now().Unix()
	s.save()
	log.Printf("console: tenant %s 修改预授权 %s (%s)", id.TenantID, p.ID, p.Name)
	s.preAuthsResponseLocked(w, id.TenantID, map[string]any{"preauth": s.preAuthViewLocked(p)})
}

// handleAdminPreAuthRevoke drops the record, its unsaved draft and the seat it
// held. Revoking a claimed record is allowed and stops at the record: the device
// keeps its node_id, its serial in the grant's ledger and the config that was
// transferred at binding. Undoing an enrollment is a different act (it has to
// touch the device) and is not this endpoint's job.
func (s *server) handleAdminPreAuthRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req preAuthIDRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.preAuthLocked(id.TenantID, strings.TrimSpace(req.ID))
	if p == nil {
		http.Error(w, "预授权记录不存在", http.StatusNotFound)
		return
	}
	deleted := p.ID
	delete(s.state.PreAuths, deleted)
	delete(s.state.PreAuthConfigs, preAuthConfigKey(id.TenantID, deleted))
	if claim := s.state.Claims[p.ClaimID]; claim != nil && claim.TenantID == id.TenantID {
		claim.Reserved = removeString(claim.Reserved, deleted)
	}
	s.save()
	log.Printf("console: tenant %s 撤销预授权 %s (%s)，%s", id.TenantID, deleted, p.Name,
		func() string {
			if p.claimed() {
				return fmt.Sprintf("已认领记录删除，node %d 与它的配置不受影响", p.BoundNodeID)
			}
			return "释放 1 个名额"
		}())
	s.preAuthsResponseLocked(w, id.TenantID, map[string]any{"revoked": deleted})
}

func removeString(list []string, v string) []string {
	out := list[:0]
	for _, item := range list {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

// ------------------------------------------------------- pre-auth config draft

type savePreAuthConfigRequest struct {
	PreAuthID string      `json:"preauth_id"`
	Config    BasicConfig `json:"config"`
}

// handleAdminPreAuthConfig is the same draft editor as /api/config, pointed at a
// record that has no device yet. Saving works; publishing does not exist here,
// because there is nothing to install -- the bundle, the frozen version and the
// heartbeat pull all stay on the node side of the binding.
func (s *server) handleAdminPreAuthConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	var pid string
	switch r.Method {
	case http.MethodGet:
		pid = strings.TrimSpace(r.URL.Query().Get("preauth"))
	case http.MethodPut:
		var req savePreAuthConfigRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		pid = strings.TrimSpace(req.PreAuthID)
		if pid == "" {
			http.Error(w, "preauth_id required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.preAuthLocked(id.TenantID, pid)
		if p == nil {
			http.Error(w, "预授权记录不存在", http.StatusNotFound)
			return
		}
		if p.claimed() {
			http.Error(w, fmt.Sprintf("这条预授权已被 node %d 认领，配置已交给设备页", p.BoundNodeID),
				http.StatusConflict)
			return
		}
		cfg := req.Config
		normalizeBasicConfig(&cfg)
		ck := preAuthConfigKey(id.TenantID, p.ID)
		dc := s.state.PreAuthConfigs[ck]
		if dc == nil {
			dc = &DeviceConfig{TenantID: id.TenantID, PreAuthID: p.ID}
			s.state.PreAuthConfigs[ck] = dc
		}
		dc.Draft = cfg
		dc.Version++
		dc.UpdatedAt = time.Now().Unix()
		dc.UpdatedBy = id.Username
		s.save()
		writeJSON(w, buildPreAuthConfigView(p, dc))
		return
	default:
		methodNotAllowed(w, "GET, PUT")
		return
	}
	if pid == "" {
		http.Error(w, "preauth must be a pre-authorization id", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.preAuthLocked(id.TenantID, pid)
	if p == nil {
		http.Error(w, "预授权记录不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, buildPreAuthConfigView(p, s.state.PreAuthConfigs[preAuthConfigKey(id.TenantID, p.ID)]))
}

// -------------------------------------------------------------- enrollment side

// preAuthRefusalLocked is the check D3 asks for: an index that cannot be claimed
// for THIS enrollment is a refusal with a reason, never a silent fallback to a
// plain enrollment -- the alternative is a box that enrolls, gets no config and
// looks healthy in the console.
func (s *server) preAuthRefusalLocked(claim *Claim, p *PreAuth, serial string) string {
	if p.claimed() {
		return fmt.Sprintf("预授权「%s」已被 node %d（序列号 %s）认领，一份配置不能给两台设备",
			p.Name, p.BoundNodeID, p.BoundSerial)
	}
	if p.ClaimID != claim.ID {
		want := p.ClaimID
		if other := s.state.Claims[p.ClaimID]; other != nil && other.Name != "" {
			want = other.Name
		}
		return fmt.Sprintf("预授权「%s」挂在授权「%s」上，本次入网用的码属于授权「%s」，请改用前一份授权生成的码",
			p.Name, want, claim.Name)
	}
	if p.Serial != "" && !strings.EqualFold(p.Serial, serial) {
		return fmt.Sprintf("预授权「%s」登记的序列号是 %s，这台设备上报的是 %s", p.Name, p.Serial, serial)
	}
	return ""
}

// bindPreAuthLocked completes the hand-over: the record becomes the proof of
// where this node came from, its seat converts into the activation that was
// already reserved, its site moves onto the device, and its draft is moved --
// not copied -- into the node's config so exactly one draft survives.
//
// Nothing here publishes. The tenant still has to press 发布 once the box is in
// front of them, which is the same gate that protects every other device.
func (s *server) bindPreAuthLocked(rec *NodeRecord, p *PreAuth, claim *Claim, serial string, now int64) {
	p.BoundNodeID = rec.NodeID
	p.BoundSerial = serial
	p.BoundAt = now
	if p.Serial == "" {
		p.Serial = serial
	}
	claim.Reserved = removeString(claim.Reserved, p.ID)
	if si := s.siteLocked(p.TenantID, p.SiteID); si != nil {
		rec.SiteID = si.ID
	}
	ck := preAuthConfigKey(p.TenantID, p.ID)
	if dc := s.state.PreAuthConfigs[ck]; dc != nil {
		dc.NodeID = rec.NodeID
		dc.PreAuthID = ""
		s.state.Configs[configKey(p.TenantID, rec.NodeID)] = dc
		delete(s.state.PreAuthConfigs, ck)
	}
}

// -------------------------------------------------------------- normalization

// normalizePreAuths makes the ledger true again after a load: a reservation must
// name an unclaimed record of the same tenant, a draft must belong to a record
// that still needs one, and a site reference must still resolve. The ledgers are
// rebuilt from the records rather than trusted, because a state file edited by
// hand (or a binding interrupted between the two maps) would otherwise count a
// seat that nothing holds -- and a phantom seat is a device a customer cannot
// enroll.
func normalizePreAuths(st *State) bool {
	if st.PreAuths == nil {
		st.PreAuths = map[string]*PreAuth{}
	}
	if st.PreAuthConfigs == nil {
		st.PreAuthConfigs = map[string]*DeviceConfig{}
	}
	changed := false
	for id, p := range st.PreAuths {
		if p == nil || p.ID != id || p.TenantID == "" || st.Tenants[p.TenantID] == nil {
			delete(st.PreAuths, id)
			changed = true
			continue
		}
		// A grant that is gone means no code can ever resolve to this record, so
		// it can neither be claimed nor hold a seat.
		claim := st.Claims[p.ClaimID]
		if claim == nil || claim.TenantID != p.TenantID {
			delete(st.PreAuths, id)
			delete(st.PreAuthConfigs, preAuthConfigKey(p.TenantID, id))
			changed = true
			continue
		}
		p.Name = strings.TrimSpace(p.Name)
		p.Serial = strings.TrimSpace(p.Serial)
		if p.Name == "" {
			p.Name = id
		}
		if p.CreatedAt == 0 {
			p.CreatedAt = time.Now().Unix()
		}
		// A claimed record whose device no longer exists is history, not a
		// reservation: keep the row for the audit trail but never let it hold a
		// seat again.
		if p.claimed() && st.Nodes[key(p.TenantID, p.BoundNodeID)] == nil {
			p.BoundNodeID = 0
			p.BoundSerial = ""
			p.BoundAt = 0
			changed = true
		}
		// Same rule as a device: a location that no longer resolves is cleared
		// rather than left pointing at somebody else's building.
		if p.SiteID != "" {
			if si := st.Sites[p.SiteID]; si == nil || si.TenantID != p.TenantID {
				p.SiteID = ""
				changed = true
			}
		}
	}
	reserved := map[string][]string{}
	for _, p := range st.PreAuths {
		if !p.claimed() {
			reserved[p.ClaimID] = append(reserved[p.ClaimID], p.ID)
		}
	}
	for _, c := range st.Claims {
		if c == nil {
			continue
		}
		got := reserved[c.ID]
		if got == nil {
			got = []string{}
		}
		sort.Strings(got)
		if !sameStrings(c.Reserved, got) {
			c.Reserved = got
			changed = true
		}
	}
	drafts := make(map[string]*DeviceConfig, len(st.PreAuthConfigs))
	for _, dc := range st.PreAuthConfigs {
		if dc == nil || dc.TenantID == "" || dc.PreAuthID == "" {
			changed = true
			continue
		}
		p := st.PreAuths[dc.PreAuthID]
		// A draft hanging off a claimed record is the residue of an interrupted
		// binding: the node's own draft is authoritative, this one is unreachable.
		if p == nil || p.TenantID != dc.TenantID || p.claimed() {
			changed = true
			continue
		}
		normalizeBasicConfig(&dc.Draft)
		if dc.Version <= 0 {
			dc.Version = 1
		}
		drafts[preAuthConfigKey(dc.TenantID, dc.PreAuthID)] = dc
	}
	st.PreAuthConfigs = drafts
	return changed
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
