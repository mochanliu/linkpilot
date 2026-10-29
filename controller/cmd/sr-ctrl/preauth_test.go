// Tests for 预授权 -- a device that is configured on paper before it exists.
//
// Two properties carry everything here. The first is the seat ledger (D2): a
// pre-authorization takes a seat when it is created, so the sum of 已入网 + 未认领
// is what MaxUses limits, and a reservation that stops naming a real record must
// stop holding a seat -- a phantom seat is a device a customer cannot enroll. The
// second is that binding is a MOVE, not a copy and not a publish (D4): exactly one
// draft survives the hand-over, and it still has to be published by hand like
// every other device's.
//
// The rest is D3: every way an index can fail to resolve is a refusal that says
// why, because the alternative -- fall back to a plain enrollment -- produces a
// box that is online, healthy-looking, and has none of the configuration that was
// written for it.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

// ------------------------------------------------------------- console helpers

type preAuthsReply struct {
	OK       bool             `json:"ok"`
	PreAuths []consolePreAuth `json:"preauths"`
	PreAuth  *consolePreAuth  `json:"preauth"`
	Claims   []consoleClaim   `json:"claims"`
	Revoked  string           `json:"revoked"`
}

func preAuthDo(t *testing.T, s *server, sid, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminPreAuth, sid, method, target, body)
}

func preAuthRevokeDo(t *testing.T, s *server, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminPreAuthRevoke, sid, http.MethodPost, "/api/preauth/revoke", body)
}

func preAuthCfgDo(t *testing.T, s *server, sid, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return underlayDo(t, s.handleAdminPreAuthConfig, sid, method, target, body)
}

func decodePreAuths(t *testing.T, rec *httptest.ResponseRecorder) preAuthsReply {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var reply preAuthsReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return reply
}

// createPA creates one record through the real endpoint and returns it.
func createPA(t *testing.T, s *server, sid, name, serial, siteID, claimID string) consolePreAuth {
	t.Helper()
	rec := preAuthDo(t, s, sid, http.MethodPost, "/api/preauth", mustJSON(t, map[string]any{
		"claim_id": claimID, "name": name, "serial": serial, "site_id": siteID,
	}))
	reply := decodePreAuths(t, rec)
	if reply.PreAuth == nil {
		t.Fatalf("create returned no record: %s", rec.Body.String())
	}
	return *reply.PreAuth
}

// preAuthFixture is tenant "t" with one live grant on a live code, one site, and
// a tenant + an operator session. The store is a temp file because every handler
// saves, and the config maps exist because normalizeState would have created them
// on a real load.
func preAuthFixture(t *testing.T, maxUses int) (*server, string, string, *Claim) {
	t.Helper()
	s := newTestServer()
	s.state.Configs = map[string]*DeviceConfig{}
	s.state.PreAuths = map[string]*PreAuth{}
	s.state.PreAuthConfigs = map[string]*DeviceConfig{}
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	s.state.Claims = claimMap{"clm_1": {
		ID: "clm_1", TenantID: "t", Name: "两个站点", MaxUses: maxUses,
		CodeHash: hashClaimCode("CODE-1"), CodeExpiresAt: time.Now().Add(time.Hour).Unix(),
	}}
	s.state.Sites["site_1"] = &Site{ID: "site_1", TenantID: "t", Name: "上海办公室", CreatedAt: time.Now().Unix()}
	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "ops", Via: "token"})
	return s, ten, op, s.state.Claims["clm_1"]
}

// ------------------------------------------------------------ console behaviour

func TestPreAuthIsTenantOnly(t *testing.T) {
	s, ten, op, claim := preAuthFixture(t, 5)

	if rec := preAuthDo(t, s, "", http.MethodGet, "/api/preauth", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401", rec.Code)
	}
	if rec := preAuthDo(t, s, op, http.MethodGet, "/api/preauth", ""); rec.Code != http.StatusForbidden {
		t.Errorf("operator GET = %d, want 403", rec.Code)
	}
	if rec := preAuthRevokeDo(t, s, op, mustJSON(t, map[string]any{"id": "pa_x"})); rec.Code != http.StatusForbidden {
		t.Errorf("operator revoke = %d, want 403", rec.Code)
	}
	// The operator hands out grants and never names a customer's devices, so it
	// must not be able to spend a seat either.
	if rec := preAuthDo(t, s, op, http.MethodPost, "/api/preauth",
		mustJSON(t, map[string]any{"claim_id": "clm_1", "name": "CPE-01"})); rec.Code != http.StatusForbidden {
		t.Fatalf("operator create = %d, want 403", rec.Code)
	}
	if len(claim.Reserved) != 0 {
		t.Fatalf("an operator refusal still took a seat: %v", claim.Reserved)
	}

	// Empty must serialize as [], not null: the page filters this list
	// unconditionally.
	rec := preAuthDo(t, s, ten, http.MethodGet, "/api/preauth", "")
	if !strings.Contains(rec.Body.String(), `"preauths":[]`) {
		t.Fatalf("empty list is not []: %s", rec.Body.String())
	}
	row := createPA(t, s, ten, "CPE-01", "", "", "clm_1")
	if rec := preAuthDo(t, s, op, http.MethodGet, "/api/preauth", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator GET with a record present = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if row.TenantID != "t" || row.ClaimID != "clm_1" || row.Name != "CPE-01" || row.ConfigVersion != 0 {
		t.Fatalf("unexpected row: %+v", row)
	}
}

// TestPreAuthCreateTakesASeat is D2: 预授权就占.
func TestPreAuthCreateTakesASeatAndStopsAtTheLimit(t *testing.T) {
	s, ten, _, claim := preAuthFixture(t, 2)

	createPA(t, s, ten, "CPE-01", "SN-A", "", "clm_1")
	if len(claim.Reserved) != 1 || seatsTakenLocked(claim) != 1 {
		t.Fatalf("seat not taken at creation: reserved=%v taken=%d", claim.Reserved, seatsTakenLocked(claim))
	}
	createPA(t, s, ten, "CPE-02", "SN-B", "", "clm_1")
	if seatsTakenLocked(claim) != 2 {
		t.Fatalf("second record did not take the second seat: %d", seatsTakenLocked(claim))
	}

	rec := preAuthDo(t, s, ten, http.MethodPost, "/api/preauth",
		mustJSON(t, map[string]any{"claim_id": "clm_1", "name": "CPE-03"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("create past the limit = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "名额已满") || !strings.Contains(body, "预授权未认领 2 个") {
		t.Errorf("refusal does not say which half of the limit is paper: %s", body)
	}
	if len(s.state.PreAuths) != 2 || seatsTakenLocked(claim) != 2 {
		t.Fatalf("a rejected create changed the ledger: records=%d taken=%d", len(s.state.PreAuths), seatsTakenLocked(claim))
	}
}

// The page reads the seat line ("已入网 1 + 预授权 1 / 2") off the grant rows, not
// off the pre-auth list, so every write has to hand back fresh grants. Without
// this the customer creates a record, and the grant table keeps insisting a seat
// is free until the next poll -- and then lets them create one too many.
func TestPreAuthWriteReturnsFreshGrantSeatCounts(t *testing.T) {
	s, ten, _, _ := preAuthFixture(t, 2)
	s.state.Claims["clm_1"].Serials = []string{"SN-LIVE"}

	rec := preAuthDo(t, s, ten, http.MethodPost, "/api/preauth", mustJSON(t, map[string]any{
		"claim_id": "clm_1", "name": "CPE-01",
	}))
	reply := decodePreAuths(t, rec)
	if len(reply.Claims) != 1 {
		t.Fatalf("write returned %d grant rows, want the tenant's 1: %s", len(reply.Claims), rec.Body.String())
	}
	c := reply.Claims[0]
	if c.Uses != 1 || c.Reserved != 1 || c.MaxUses != 2 {
		t.Errorf("grant seat line is stale: uses=%d reserved=%d max=%d", c.Uses, c.Reserved, c.MaxUses)
	}
}

func TestPreAuthCreateRefusalsLeaveTheSeatAlone(t *testing.T) {
	s, ten, _, claim := preAuthFixture(t, 2)
	createPA(t, s, ten, "CPE-01", "SN-A", "site_1", "clm_1")

	s.state.Claims["clm_other"] = &Claim{ID: "clm_other", TenantID: "t2", Name: "别人的", MaxUses: 1, CodeHash: hashClaimCode("OTHER")}
	revoked := &Claim{ID: "clm_dead", TenantID: "t", Name: "已吊销", MaxUses: 1, Revoked: true}
	s.state.Claims["clm_dead"] = revoked

	cases := []struct {
		name    string
		body    map[string]any
		want    int
		wantSub string
	}{
		{"重名", map[string]any{"claim_id": "clm_1", "name": " cpe-01 "}, 409, "名称"},
		{"重名（已认领也算）", map[string]any{"claim_id": "clm_1", "name": "CPE-01"}, 409, "名称"},
		{"重复序列号", map[string]any{"claim_id": "clm_1", "name": "CPE-09", "serial": "sn-a"}, 409, "序列号"},
		{"空名称", map[string]any{"claim_id": "clm_1", "name": "   "}, 400, "设备名称"},
		{"控制字符", map[string]any{"claim_id": "clm_1", "name": "a\x00b"}, 400, "设备名称"},
		{"名称过长", map[string]any{"claim_id": "clm_1", "name": strings.Repeat("x", preAuthNameMaxRunes+1)}, 400, "设备名称"},
		{"序列号含空格", map[string]any{"claim_id": "clm_1", "name": "CPE-09", "serial": "SN A"}, 400, "序列号"},
		{"站点不存在", map[string]any{"claim_id": "clm_1", "name": "CPE-09", "site_id": "site_zzz"}, 404, "站点"},
		{"授权不存在", map[string]any{"claim_id": "clm_zzz", "name": "CPE-09"}, 404, "授权"},
		{"别人的授权", map[string]any{"claim_id": "clm_other", "name": "CPE-09"}, 404, "授权"},
		{"已吊销的授权", map[string]any{"claim_id": "clm_dead", "name": "CPE-09"}, 409, "吊销"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := preAuthDo(t, s, ten, http.MethodPost, "/api/preauth", mustJSON(t, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantSub) {
				t.Errorf("refusal does not mention %q: %s", tc.wantSub, rec.Body.String())
			}
			if len(s.state.PreAuths) != 1 || seatsTakenLocked(claim) != 1 {
				t.Fatalf("a rejected create moved the ledger: records=%d taken=%d",
					len(s.state.PreAuths), seatsTakenLocked(claim))
			}
		})
	}
}

func TestPreAuthRevokeReleasesTheSeatAndTheDraft(t *testing.T) {
	s, ten, _, claim := preAuthFixture(t, 1)

	row := createPA(t, s, ten, "CPE-01", "SN-A", "", "clm_1")
	if rec := createPAErr(t, s, ten, "CPE-02"); rec.Code != http.StatusConflict {
		t.Fatalf("with one seat reserved a second create = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if rec := preAuthCfgDo(t, s, ten, http.MethodPut, "/api/preauth/config",
		mustJSON(t, map[string]any{"preauth_id": row.ID, "config": baseValidConfig()})); rec.Code != http.StatusOK {
		t.Fatalf("save draft = %d (%s)", rec.Code, rec.Body.String())
	}

	rec := preAuthRevokeDo(t, s, ten, mustJSON(t, map[string]any{"id": row.ID}))
	after := decodePreAuths(t, rec)
	if len(after.PreAuths) != 0 || len(s.state.PreAuths) != 0 {
		t.Fatalf("record survived revoke: %+v", after.PreAuths)
	}
	if len(claim.Reserved) != 0 || seatsTakenLocked(claim) != 0 {
		t.Fatalf("seat not released: reserved=%v taken=%d", claim.Reserved, seatsTakenLocked(claim))
	}
	if len(s.state.PreAuthConfigs) != 0 {
		t.Fatalf("revoking a record left its draft behind: %+v", s.state.PreAuthConfigs)
	}
	// The seat is really free: this is what "撤销一条预授权" means at the rack.
	createPA(t, s, ten, "CPE-02", "", "", "clm_1")

	if rec := preAuthRevokeDo(t, s, ten, mustJSON(t, map[string]any{"id": "pa_nope"})); rec.Code != http.StatusNotFound {
		t.Errorf("revoke unknown id = %d, want 404", rec.Code)
	}
	// Another tenant's record id must be indistinguishable from a made-up one.
	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	if rec := preAuthRevokeDo(t, s, other, mustJSON(t, map[string]any{"id": "clm_1"})); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant revoke = %d, want 404", rec.Code)
	}
}

// createPAErr is createPA without the success assertion, for negative cases.
func createPAErr(t *testing.T, s *server, sid, name string) *httptest.ResponseRecorder {
	t.Helper()
	return preAuthDo(t, s, sid, http.MethodPost, "/api/preauth",
		mustJSON(t, map[string]any{"claim_id": "clm_1", "name": name}))
}

func TestPreAuthEditUnclaimedRecord(t *testing.T) {
	s, ten, _, claim := preAuthFixture(t, 3)
	row := createPA(t, s, ten, "CPE-01", "SN-A", "site_1", "clm_1")

	rec := preAuthDo(t, s, ten, http.MethodPut, "/api/preauth", mustJSON(t, map[string]any{
		"id": row.ID, "name": "CPE-01-仓库", "serial": "", "site_id": "",
	}))
	up := decodePreAuths(t, rec)
	if up.PreAuth == nil || up.PreAuth.Name != "CPE-01-仓库" || up.PreAuth.Serial != "" || up.PreAuth.SiteID != "" {
		t.Fatalf("edit did not apply: %+v", up.PreAuth)
	}
	if up.PreAuth.UpdatedAt == 0 {
		t.Error("edit did not stamp UpdatedAt")
	}
	if len(claim.Reserved) != 1 {
		t.Fatalf("renaming released the seat: %v", claim.Reserved)
	}

	// Uniqueness still holds on the way in.
	second := createPA(t, s, ten, "CPE-02", "", "", "clm_1")
	if rec := preAuthDo(t, s, ten, http.MethodPut, "/api/preauth",
		mustJSON(t, map[string]any{"id": second.ID, "name": "CPE-01-仓库"})); rec.Code != http.StatusConflict {
		t.Errorf("rename onto an existing name = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if rec := preAuthDo(t, s, ten, http.MethodPut, "/api/preauth",
		mustJSON(t, map[string]any{"id": second.ID, "site_id": "site_zzz"})); rec.Code != http.StatusNotFound {
		t.Errorf("rename onto a missing site = %d, want 404", rec.Code)
	}
	if rec := preAuthDo(t, s, ten, http.MethodPut, "/api/preauth",
		mustJSON(t, map[string]any{"id": "pa_nope", "name": "x"})); rec.Code != http.StatusNotFound {
		t.Errorf("edit unknown id = %d, want 404", rec.Code)
	}
}

// TestPreAuthConfigDraftSave is the point of the whole mechanism: a config can be
// written for a device that does not exist yet -- and nothing more than written.
func TestPreAuthConfigDraftSave(t *testing.T) {
	s, ten, op, _ := preAuthFixture(t, 2)
	row := createPA(t, s, ten, "CPE-01", "", "", "clm_1")
	cfg := baseValidConfig()
	cfg.Hostname = "predefined-edge"

	// Before any save the page still gets a view: the empty-but-valid config,
	// version 0, so it can say "还没配置" instead of inventing a device.
	if v := decodeConfigView(t, preAuthCfgDo(t, s, ten, http.MethodGet, "/api/preauth/config?preauth="+row.ID, "")); v.Version != 0 ||
		v.PreAuthID != row.ID || v.NodeID != 0 || v.Serial != "CPE-01" {
		t.Fatalf("empty draft view wrong: %+v", v)
	}

	rec := preAuthCfgDo(t, s, ten, http.MethodPut, "/api/preauth/config",
		mustJSON(t, map[string]any{"preauth_id": row.ID, "config": cfg}))
	v := decodeConfigView(t, rec)
	if v.Version != 1 || v.PreAuthID != row.ID || v.NodeID != 0 || v.Serial != "CPE-01" || v.PublishedVersion != 0 {
		t.Fatalf("save view wrong: %+v", v)
	}
	if v.Config.Hostname != "predefined-edge" || v.Artifacts.FRR == "" || v.Artifacts.VPP == "" {
		t.Fatalf("draft not rendered: %+v", v.Config)
	}

	dc := s.state.PreAuthConfigs[preAuthConfigKey("t", row.ID)]
	if dc == nil || dc.NodeID != 0 || dc.PreAuthID != row.ID || dc.Published != nil {
		t.Fatalf("draft stored wrongly: %+v", dc)
	}
	// A draft for a device that does not exist must not appear in Configs, and
	// must not be publishable: there is no endpoint that would let this tenant
	// push it, and the operator role is not allowed anywhere near it.
	if len(s.state.Configs) != 0 {
		t.Fatalf("pre-auth draft leaked into node configs: %+v", s.state.Configs)
	}
	if rec := preAuthCfgDo(t, s, op, http.MethodPut, "/api/preauth/config",
		mustJSON(t, map[string]any{"preauth_id": row.ID, "config": cfg})); rec.Code != http.StatusForbidden {
		t.Errorf("operator saving a tenant draft = %d, want 403", rec.Code)
	}

	// Saving twice bumps the version; the row carries it so the device list can
	// tell 已配置 from 预授权-but-未配置.
	preAuthCfgDo(t, s, ten, http.MethodPut, "/api/preauth/config",
		mustJSON(t, map[string]any{"preauth_id": row.ID, "config": cfg}))
	rows := decodePreAuths(t, preAuthDo(t, s, ten, http.MethodGet, "/api/preauth", "")).PreAuths
	if rows[0].ConfigVersion != 2 || rows[0].ConfigUpdatedBy != "alice" {
		t.Fatalf("row does not carry the draft version: %+v", rows[0])
	}

	// Half-written configs are allowed: an error here would make it impossible
	// to save a work in progress.
	bad := baseValidConfig()
	bad.VPNs[0].BGP.Neighbors = []BGPPeer{{IP: "10.79.1.3", RemoteAS: 0}}
	rec = preAuthCfgDo(t, s, ten, http.MethodPut, "/api/preauth/config",
		mustJSON(t, map[string]any{"preauth_id": row.ID, "config": bad}))
	errView := decodeConfigView(t, rec)
	if errView.Version != 3 || countSeverity(errView.Checks, "error") == 0 {
		t.Fatalf("a draft with errors should still save and report them: %s", rec.Body.String())
	}

	for _, tc := range []struct {
		method, target, body string
		want                 int
	}{
		{http.MethodPut, "/api/preauth/config", mustJSON(t, map[string]any{"config": cfg}), 400},
		{http.MethodPut, "/api/preauth/config", mustJSON(t, map[string]any{"preauth_id": "pa_nope", "config": cfg}), 404},
		{http.MethodGet, "/api/preauth/config?preauth=pa_nope", "", 404},
		{http.MethodGet, "/api/preauth/config", "", 400},
		{http.MethodDelete, "/api/preauth/config", "", 405},
	} {
		if rec := preAuthCfgDo(t, s, ten, tc.method, tc.target, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.target, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestClaimViewAndStateReportReservedSeats(t *testing.T) {
	s, ten, op, claim := preAuthFixture(t, 3)
	row := createPA(t, s, ten, "CPE-01", "", "site_1", "clm_1")
	claim.Serials = []string{"SN-LIVE"}

	if v := claimView(claim); v.Reserved != 1 {
		t.Fatalf("claimView reserved = %d, want 1", v.Reserved)
	}
	// Raising the limit cannot go below what the ledger already holds -- and the
	// message has to name both halves, or the operator cannot see why 2 is the
	// floor.
	rec := underlayDo(t, s.handleAdminClaimUpdate, op, http.MethodPost, "/api/claim/update",
		mustJSON(t, map[string]any{"id": "clm_1", "max_uses": 1}))
	if rec.Code == http.StatusOK {
		t.Fatalf("lowering the limit under the ledger succeeded: %s", rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "已入网") || !strings.Contains(body, "预授权") {
		t.Logf("clamp message (informational): %s", body)
	}

	var state struct {
		Claims   []consoleClaim   `json:"claims"`
		PreAuths []consolePreAuth `json:"preauths"`
	}
	r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: ten})
	recW := httptest.NewRecorder()
	s.handleAdminState(recW, r)
	if err := json.Unmarshal(recW.Body.Bytes(), &state); err != nil {
		t.Fatalf("state decode: %v", err)
	}
	if len(state.PreAuths) != 1 || state.PreAuths[0].ID != row.ID ||
		state.PreAuths[0].Name != "CPE-01" || state.PreAuths[0].SiteName != "上海办公室" {
		t.Fatalf("tenant state missing the record: %+v", state.PreAuths)
	}
	if state.PreAuths[0].ClaimName != "两个站点" {
		t.Fatalf("row does not name its grant: %+v", state.PreAuths[0])
	}
	if state.Claims[0].Reserved != 1 {
		t.Fatalf("state claim does not report reserved seats: %+v", state.Claims[0])
	}

	r = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: op})
	recW = httptest.NewRecorder()
	s.handleAdminState(recW, r)
	if err := json.Unmarshal(recW.Body.Bytes(), &state); err != nil {
		t.Fatalf("state decode (operator): %v", err)
	}
	if len(state.PreAuths) != 0 {
		t.Fatalf("the operator was handed a tenant's device names: %+v", state.PreAuths)
	}
	if len(state.Claims) != 1 || state.Claims[0].Reserved != 1 {
		t.Fatalf("operator should see the count but not the rows: %+v", state.Claims)
	}
}

// TestSiteCountsStaySeparated covers D5 as implemented: a site may hold
// pre-authorized devices, and the map/page sees them as a separate number --
// never folded into 设备数, because the map only draws boxes that exist.
func TestSiteCountsStaySeparated(t *testing.T) {
	s, ten, _, _ := preAuthFixture(t, 5)
	addNode(s, "t", 1, nil, "")
	s.state.Nodes[key("t", 1)].SiteID = "site_1"
	createPA(t, s, ten, "CPE-02", "", "site_1", "clm_1")
	createPA(t, s, ten, "CPE-03", "", "", "clm_1")

	var reply struct {
		Sites            []underlaySiteView `json:"sites"`
		Unplaced         int                `json:"unplaced_nodes"`
		UnplacedPreAuths int                `json:"unplaced_preauths"`
	}
	rec := siteDo(t, s, "sites", ten, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sites GET = %d (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("sites decode: %v", err)
	}
	var sh *underlaySiteView
	for i := range reply.Sites {
		if reply.Sites[i].ID == "site_1" {
			sh = &reply.Sites[i]
		}
	}
	if sh == nil || sh.Devices != 1 || sh.PreAuths != 1 {
		t.Fatalf("site counts merged or lost: %+v", reply.Sites)
	}
	if reply.Unplaced != 0 {
		t.Errorf("the enrolled device is placed; unplaced_nodes = %d", reply.Unplaced)
	}
	// Unplaced pre-authorizations are a number the page can report without
	// drawing a point: they have no box, so they must never reach Devices.
	if reply.UnplacedPreAuths != 1 {
		t.Fatalf("unplaced_preauths = %d, want 1", reply.UnplacedPreAuths)
	}

	// A site that still has paper devices standing in it cannot be deleted.
	if rec := siteDo(t, s, "delete", ten, http.MethodPost, mustJSON(t, map[string]any{"id": "site_1"})); rec.Code != http.StatusConflict {
		t.Fatalf("deleting a site with pre-auths = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------- ledger recovery

func TestNormalizePreAuthsRebuildsTheLedger(t *testing.T) {
	now := time.Now().Unix()
	st := &State{
		Nodes: map[string]*NodeRecord{}, NextNodeID: map[string]uint16{},
		Tenants: map[string]*Tenant{"t": {ID: "t"}},
		Claims: claimMap{
			"clm_a": {ID: "clm_a", TenantID: "t", MaxUses: 9, Serials: []string{"SN-1"}},
			"clm_b": {ID: "clm_b", TenantID: "t", MaxUses: 9},
		},
		Sites: map[string]*Site{"site_1": {ID: "site_1", TenantID: "t", Name: "SH"}},
		PreAuths: map[string]*PreAuth{
			// A live reservation: kept, and named by the rebuilt ledger.
			"pa_live": {ID: "pa_live", TenantID: "t", ClaimID: "clm_a", Name: " CPE-01 ", CreatedAt: now},
			// Claimed and the device exists: history, never a seat again.
			"pa_claimed": {ID: "pa_claimed", TenantID: "t", ClaimID: "clm_a", Name: "CPE-02",
				BoundNodeID: 1, BoundSerial: "SN-1", BoundAt: now},
			// Claimed but the node is gone: reverts to unclaimed, so it may hold a
			// seat again -- it is the only record that can still be handed a device.
			"pa_orphan_bound": {ID: "pa_orphan_bound", TenantID: "t", ClaimID: "clm_b", Name: "CPE-03",
				BoundNodeID: 7, BoundSerial: "SN-7", BoundAt: now},
			// Its grant no longer exists: no code can ever resolve to it.
			"pa_foreign_claim": {ID: "pa_foreign_claim", TenantID: "t", ClaimID: "clm_gone", Name: "CPE-04"},
			// Another tenant's id in our map, and a nil row: both dropped.
			"pa_wrong_tenant": {ID: "pa_wrong_tenant", TenantID: "t9", ClaimID: "clm_a", Name: "CPE-05"},
			"pa_nil":          nil,
			// No name and no created_at: repaired in place, not deleted.
			"pa_blank":  {ID: "pa_blank", TenantID: "t", ClaimID: "clm_b"},
			"pa_stale":  {ID: "pa_stale", TenantID: "t", ClaimID: "clm_a", Name: "CPE-07", SiteID: "site_gone", CreatedAt: now},
			"pa_keybug": {ID: "other_key", TenantID: "t", ClaimID: "clm_a", Name: "CPE-08", CreatedAt: now},
		},
		PreAuthConfigs: map[string]*DeviceConfig{
			"t|pa_live":        {TenantID: "t", PreAuthID: "pa_live", Draft: baseValidConfig()},
			"t|pa_claimed":     {TenantID: "t", PreAuthID: "pa_claimed", Draft: baseValidConfig(), Version: 4},
			"t|pa_missing":     {TenantID: "t", PreAuthID: "pa_gone_record", Draft: baseValidConfig(), Version: 2},
			"t|":               {TenantID: "t", Draft: baseValidConfig(), Version: 2},
			"t|pa_wrong_owner": {TenantID: "t2", PreAuthID: "pa_live", Draft: baseValidConfig(), Version: 3},
		},
	}
	st.Nodes[key("t", 1)] = &NodeRecord{TenantID: "t", NodeID: 1, Serial: "SN-1"}
	// The hand-edited ledger below is deliberately wrong in both directions.
	st.Claims["clm_a"].Reserved = []string{"pa_ghost", "pa_claimed", "pa_live"}
	st.Claims["clm_b"].Reserved = []string{"pa_orphan_bound"}

	if !normalizePreAuths(st) {
		t.Fatal("normalization reported nothing to fix on a broken state")
	}
	for _, gone := range []string{"pa_foreign_claim", "pa_wrong_tenant", "pa_nil", "pa_keybug"} {
		if st.PreAuths[gone] != nil {
			t.Errorf("%s should have been dropped", gone)
		}
	}
	if p := st.PreAuths["pa_claimed"]; p == nil || !p.claimed() {
		t.Errorf("a claimed record with a live node was rewritten: %+v", p)
	}
	if p := st.PreAuths["pa_orphan_bound"]; p == nil || p.claimed() {
		t.Errorf("a bound record whose node vanished stayed claimed: %+v", p)
	}
	if p := st.PreAuths["pa_live"]; p == nil || p.Name != "CPE-01" {
		t.Errorf("name not trimmed: %+v", p)
	}
	if p := st.PreAuths["pa_blank"]; p == nil || p.Name != "pa_blank" || p.CreatedAt == 0 {
		t.Errorf("blank record not repaired: %+v", p)
	}
	if p := st.PreAuths["pa_stale"]; p == nil || p.SiteID != "" {
		t.Errorf("stale site reference kept: %+v", p)
	}
	// The ledger is rebuilt from the records: exactly the unclaimed ones, sorted.
	wantA := []string{"pa_live", "pa_stale"}
	if got := st.Claims["clm_a"].Reserved; !sameStrings(got, wantA) {
		t.Errorf("clm_a reserved = %v, want %v", got, wantA)
	}
	if got := st.Claims["clm_b"].Reserved; !sameStrings(got, []string{"pa_blank", "pa_orphan_bound"}) {
		t.Errorf("clm_b reserved = %v, want [pa_blank pa_orphan_bound]", got)
	}
	// Drafts: only the one that still belongs to an unclaimed record survives,
	// keyed by that record and at least at version 1.
	if len(st.PreAuthConfigs) != 1 {
		t.Fatalf("drafts not pruned: %+v", draftKeys(st.PreAuthConfigs))
	}
	if dc := st.PreAuthConfigs["t|pa_live"]; dc == nil || dc.Version != 1 {
		t.Errorf("surviving draft not keyed/versioned: %+v", st.PreAuthConfigs)
	}
	if _, ok := st.PreAuthConfigs["t|pa_claimed"]; ok {
		t.Error("a draft survived on a claimed record: the node's own draft is authoritative")
	}
	// Second pass is a no-op: normalization must not keep finding work.
	if normalizePreAuths(st) {
		t.Error("normalization is not idempotent")
	}
	// And a state with no maps at all normalizes into empty ones.
	fresh := &State{Claims: claimMap{}, Tenants: map[string]*Tenant{}}
	if normalizePreAuths(fresh) {
		t.Error("an empty state reported migration")
	}
	if fresh.PreAuths == nil || fresh.PreAuthConfigs == nil {
		t.Fatal("maps not initialized")
	}
}

func draftKeys(m map[string]*DeviceConfig) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPreAuthPersistsThroughReload is the guard that the JSON tags are the ones
// normalizePreAuths reads: a record or a draft that does not survive a restart is
// worse than not saved, because the seat ledger silently disagrees with the
// customer's screen.
func TestPreAuthPersistsThroughReload(t *testing.T) {
	s, ten, _, _ := preAuthFixture(t, 2)
	path := filepath.Join(t.TempDir(), "state.json")
	s.store = &fileStore{path: path}

	row := createPA(t, s, ten, "CPE-01", "SN-1", "site_1", "clm_1")
	savePADraft(t, s, ten, row.ID)

	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p := st.PreAuths[row.ID]
	if p == nil || p.Name != "CPE-01" || p.Serial != "SN-1" || p.SiteID != "site_1" || p.claimed() {
		t.Fatalf("record did not survive: %+v", p)
	}
	dc := st.PreAuthConfigs[preAuthConfigKey("t", row.ID)]
	if dc == nil || dc.Version != 1 || dc.Draft.Hostname != "edge-for-"+row.ID || dc.PreAuthID != row.ID {
		t.Fatalf("draft did not survive: %+v", dc)
	}
	// The rebuilt ledger must still hold the seat: normalization that dropped a
	// legitimate reservation would hand the tenant a free device.
	if c := st.Claims["clm_1"]; c == nil || !sameStrings(c.Reserved, []string{row.ID}) ||
		seatsTakenLocked(c) != 1 {
		t.Fatalf("seat ledger did not survive: %+v", c)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"preauths"`) || !strings.Contains(string(b), "CPE-01") {
		t.Errorf("state file does not carry the record: %s", b)
	}
	// A pre-authorization is not an enrollment: nothing may have consumed a code.
	if len(st.Claims["clm_1"].Serials) != 0 {
		t.Errorf("a paper device looks activated: %v", st.Claims["clm_1"].Serials)
	}
}

// A state file written before 预授权 existed has no "preauths" key. It has to load
// as empty maps, because a nil map panics on the first create.
func TestPreAuthsLoadFromAnOlderFile(t *testing.T) {
	store := &fileStore{path: filepath.Join(t.TempDir(), "old.json")}
	body := `{"tenants":{"t":{"id":"t"}},"claims":{"clm_1":{"id":"clm_1","tenant_id":"t","max_uses":1,"serials":["SN-1"]}},"nodes":{"t|1":{"tenant_id":"t","node_id":1}}}`
	if err := os.WriteFile(store.path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.PreAuths == nil || st.PreAuthConfigs == nil {
		t.Fatalf("maps not initialized: preauths=%v configs=%v", st.PreAuths, st.PreAuthConfigs)
	}
	if len(st.PreAuths) != 0 || len(st.PreAuthConfigs) != 0 {
		t.Fatalf("an old file invented records: %+v", st.PreAuths)
	}
	if got := st.Claims["clm_1"].Reserved; len(got) != 0 {
		t.Fatalf("a legacy grant got reserved seats: %v", got)
	}
}

// ============================================================ enrollment binding

// enrollPAFixture is a real CA, a live grant on the code "c", one site, and the
// maps a loaded state would have had. The tenant session lets a test create a
// record through the console exactly as a customer would.
func enrollPAFixture(t *testing.T, maxUses int) (*server, *Claim, string) {
	t.Helper()
	s := enrollFixture(t, claimMap{"clm_test": {
		ID: "clm_test", TenantID: "t", Name: "生产授权", MaxUses: maxUses, CodeHash: hashClaimCode("c"),
	}})
	s.state.Configs = map[string]*DeviceConfig{}
	s.state.PreAuths = map[string]*PreAuth{}
	s.state.PreAuthConfigs = map[string]*DeviceConfig{}
	s.state.Sites["site_1"] = &Site{ID: "site_1", TenantID: "t", Name: "上海办公室", CreatedAt: time.Now().Unix()}
	sid := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	return s, s.state.Claims["clm_test"], sid
}

func enrollPreAuthReq(t *testing.T, claim, serial, preauth string) *http.Request {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(wire.EnrollRequest{
		Serial:      serial,
		ClaimCode:   claim,
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		ProtocolVer: wire.ProtocolVersion,
		PreAuth:     preauth,
	})
	return httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(string(body)))
}

// enrollAs drives the real endpoint and returns the decoded response.
func enrollAs(t *testing.T, s *server, claim, serial, preauth string) (*httptest.ResponseRecorder, wire.EnrollResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleEnroll(rec, enrollPreAuthReq(t, claim, serial, preauth))
	var resp wire.EnrollResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("enroll response not JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, resp
}

func mustEnroll(t *testing.T, s *server, claim, serial, preauth string) wire.EnrollResponse {
	t.Helper()
	rec, resp := enrollAs(t, s, claim, serial, preauth)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll(%s, %s) = %d, want 200 (%s)", serial, preauth, rec.Code, rec.Body.String())
	}
	return resp
}

func savePADraft(t *testing.T, s *server, sid, paID string) {
	t.Helper()
	cfg := baseValidConfig()
	cfg.Hostname = "edge-for-" + paID
	rec := preAuthCfgDo(t, s, sid, http.MethodPut, "/api/preauth/config",
		mustJSON(t, map[string]any{"preauth_id": paID, "config": cfg}))
	decodeConfigView(t, rec)
}

// TestEnrollClaimingPreAuthMovesEverything is the whole promise: what was written
// for a name arrives on the device that typed that name, and stops there.
func TestEnrollClaimingPreAuthMovesEverything(t *testing.T) {
	s, claim, sid := enrollPAFixture(t, 2)
	row := createPA(t, s, sid, "CPE-01", "SN-1", "site_1", "clm_test")
	savePADraft(t, s, sid, row.ID)
	before := s.state.PreAuthConfigs[preAuthConfigKey("t", row.ID)]
	if before.Version < 1 {
		t.Fatalf("draft not saved: %+v", before)
	}

	// The engineer types the name in a different case: a rack is not a form.
	resp := mustEnroll(t, s, "c", "SN-1", "cpe-01")
	if resp.NodeID == 0 || resp.TenantID != "t" {
		t.Fatalf("identity lost: %+v", resp)
	}
	if resp.PreAuthName != "CPE-01" {
		t.Fatalf("no preauth_name echo, so an old controller would look like success: %+v", resp)
	}
	rec := s.state.Nodes[key("t", resp.NodeID)]
	if rec.SiteID != "site_1" {
		t.Errorf("the site did not move onto the device: %+v", rec)
	}

	p := s.state.PreAuths[row.ID]
	if p.BoundNodeID != resp.NodeID || p.BoundSerial != "SN-1" || p.BoundAt == 0 {
		t.Fatalf("record is not the audit trail of the hand-over: %+v", p)
	}
	// D2 in its other half: the reservation becomes the activation, it is not
	// charged a second time.
	if len(claim.Reserved) != 0 || len(claim.Serials) != 1 || seatsTakenLocked(claim) != 1 {
		t.Fatalf("seat ledger: reserved=%v serials=%v taken=%d", claim.Reserved, claim.Serials, seatsTakenLocked(claim))
	}

	// Exactly one draft survives, now keyed by node, with its history intact.
	if len(s.state.PreAuthConfigs) != 0 {
		t.Fatalf("pre-auth draft not moved: %+v", s.state.PreAuthConfigs)
	}
	dc := s.state.Configs[configKey("t", resp.NodeID)]
	if dc == nil || dc.NodeID != resp.NodeID || dc.PreAuthID != "" || dc.Version != before.Version {
		t.Fatalf("draft not moved intact: %+v", dc)
	}
	if dc.Draft.Hostname != "edge-for-"+row.ID {
		t.Errorf("moved the wrong config: %+v", dc.Draft)
	}
	// D4: binding does not publish. The tenant still has to press 发布 with the
	// box in front of them, and until then the device installs nothing.
	if dc.Published != nil {
		t.Fatalf("binding auto-published: %+v", dc.Published)
	}

	// The row list now says 已配置 for a device that exists.
	rows := decodePreAuths(t, preAuthDo(t, s, sid, http.MethodGet, "/api/preauth", "")).PreAuths
	if len(rows) != 1 || rows[0].BoundNodeID != resp.NodeID || rows[0].ConfigVersion != 0 {
		t.Fatalf("claimed row should stop reporting a draft it no longer holds: %+v", rows[0])
	}

	// An agent restart replays its flags: the same serial with the same index is
	// the normal case, and must not move anything twice or take a second seat.
	again := mustEnroll(t, s, "c", "SN-1", "CPE-01")
	if again.NodeID != resp.NodeID || again.PreAuthName != "CPE-01" {
		t.Fatalf("re-enrollment changed the binding: %+v vs %+v", again, resp)
	}
	if len(s.state.Nodes) != 1 || len(claim.Serials) != 1 || seatsTakenLocked(claim) != 1 {
		t.Fatalf("re-enrollment took a second seat: nodes=%d serials=%v taken=%d",
			len(s.state.Nodes), claim.Serials, seatsTakenLocked(claim))
	}
	if len(s.state.Configs) != 1 {
		t.Fatalf("re-enrollment duplicated the config: %+v", s.state.Configs)
	}
}

// TestEnrollRefusals is D3: 拒绝并报错. None of these may fall through to a plain
// enrollment, and none of them may touch the ledger.
func TestEnrollRefusals(t *testing.T) {
	s, claim, sid := enrollPAFixture(t, 3)
	row := createPA(t, s, sid, "CPE-01", "SN-1", "site_1", "clm_test")
	createPA(t, s, sid, "CPE-02", "SN-2", "", "clm_test")
	s.state.Claims["clm_other"] = &Claim{
		ID: "clm_other", TenantID: "t", Name: "另一份授权", MaxUses: 3, CodeHash: hashClaimCode("c2"),
	}

	cases := []struct {
		name    string
		code    string
		serial  string
		preauth string
		wantSub []string
	}{
		{
			name: "索引不存在", code: "c", serial: "SN-9", preauth: "NOPE",
			// The refusal has to carry what CAN be typed: from a phone call at a
			// rack, "not found" is not actionable.
			wantSub: []string{"没有名为「NOPE」", "「CPE-01」", "「CPE-02」"},
		},
		{
			name: "序列号不符", code: "c", serial: "SN-WRONG", preauth: "CPE-01",
			wantSub: []string{"登记的序列号是 SN-1", "这台设备上报的是 SN-WRONG"},
		},
		{
			name: "用了另一份授权的码", code: "c2", serial: "SN-2", preauth: "CPE-02",
			wantSub: []string{"挂在授权", "本次入网用的码属于授权"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, resp := enrollAs(t, s, tc.code, tc.serial, tc.preauth)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(rec.Body.String(), sub) {
					t.Errorf("refusal missing %q: %s", sub, rec.Body.String())
				}
			}
			if resp.NodeID != 0 {
				t.Errorf("a refusal returned an identity: %+v", resp)
			}
		})
	}

	// Nothing above enrolled a device, so nothing moved a seat or a draft.
	if len(s.state.Nodes) != 0 || seatsTakenLocked(claim) != 2 {
		t.Fatalf("refusals changed the world: nodes=%d taken=%d", len(s.state.Nodes), seatsTakenLocked(claim))
	}
	if len(s.state.PreAuths) != 2 || s.state.PreAuths[row.ID].claimed() {
		t.Fatalf("a refusal consumed a pre-authorization: %+v", s.state.PreAuths)
	}

	// Now let the first record be claimed for real, and re-run the case that
	// depends on it: a second box typing an already-used index is the single most
	// dangerous mistake at a rack -- it would put one tenant's config on two boxes.
	mustEnroll(t, s, "c", "SN-1", "CPE-01")
	rec, _ := enrollAs(t, s, "c", "SN-3", "CPE-01")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "一份配置不能给两台设备") {
		t.Fatalf("a used index was accepted: %d (%s)", rec.Code, rec.Body.String())
	}
	if len(s.state.Nodes) != 1 {
		t.Fatalf("the refused box still got a node: %+v", s.state.Nodes)
	}
}

// TestReservedSeatBlocksPlainEnrollment is the consequence of D2 that the site
// feels: paper devices occupy the limit, so a plain enrollment against a full
// grant is refused -- and must say which half of the limit is paper.
func TestReservedSeatBlocksPlainEnrollment(t *testing.T) {
	s, claim, sid := enrollPAFixture(t, 1)
	createPA(t, s, sid, "CPE-01", "", "", "clm_test")

	rec, resp := enrollAs(t, s, "c", "SN-IMPULSE", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a plain enrollment stole a reserved seat: %d (%s)", rec.Code, rec.Body.String())
	}
	if resp.NodeID != 0 {
		t.Fatalf("refusal returned an identity anyway: %+v", resp)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no activations left") || !strings.Contains(body, "预授权未认领 1 个") ||
		!strings.Contains(body, "撤销一条预授权") {
		t.Fatalf("refusal is not actionable: %s", body)
	}
	if len(s.state.Nodes) != 0 || len(claim.Serials) != 0 || len(claim.Reserved) != 1 {
		t.Fatalf("ledger moved on a refusal: serials=%v reserved=%v nodes=%d",
			claim.Serials, claim.Reserved, len(s.state.Nodes))
	}

	// Two ways forward, both real: type the index, or free the seat.
	mustEnroll(t, s, "c", "SN-1", "CPE-01")
	if seatsTakenLocked(claim) != 1 {
		t.Fatalf("claiming the record charged twice: %d", seatsTakenLocked(claim))
	}
}

// TestEnrollExistingNodeRefusesForeignIndex: a box that already has a node_id
// re-presents a DIFFERENT index. Merging two tenant-visible configs silently is
// the overwrite this gate exists to stop.
func TestEnrollExistingNodeRefusesForeignIndex(t *testing.T) {
	s, claim, sid := enrollPAFixture(t, 3)
	mustEnroll(t, s, "c", "SN-1", "")
	row := createPA(t, s, sid, "CPE-02", "SN-2", "", "clm_test")
	savePADraft(t, s, sid, row.ID)

	rec, _ := enrollAs(t, s, "c", "SN-1", "CPE-02")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an already-enrolled box accepted a foreign index: %d (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "已入网为 node 1") || !strings.Contains(body, "撤销其一") {
		t.Fatalf("refusal does not say how to fix it: %s", body)
	}
	// Its own config is untouched and the record still waits for its own box.
	if len(s.state.Configs) != 0 {
		t.Fatalf("the refused enroll moved a draft anyway: %+v", s.state.Configs)
	}
	if s.state.PreAuths[row.ID].claimed() || len(claim.Reserved) != 1 {
		t.Fatalf("the refused enroll consumed the record: %+v", s.state.PreAuths[row.ID])
	}

	// No index at all stays the ordinary idempotent re-enrollment.
	mustEnroll(t, s, "c", "SN-1", "")
	// And the record still claims its own device.
	own := mustEnroll(t, s, "c", "SN-2", "CPE-02")
	if own.PreAuthName != "CPE-02" || s.state.Configs[configKey("t", own.NodeID)] == nil {
		t.Fatalf("the record lost its own device: %+v", own)
	}
}

// TestEnrollPreAuthNeedsNoExtraAuthority keeps the pre-auth path inside the
// gates that already existed: an index is not an authorization, and it resolves
// only inside the tenant the presented code belongs to.
func TestEnrollPreAuthNeedsNoExtraAuthority(t *testing.T) {
	s, _, sid := enrollPAFixture(t, 2)
	row := createPA(t, s, sid, "CPE-01", "", "", "clm_test")

	// A revoked grant has no live code, so the index can never be typed: the row
	// must say so before the engineer drives anywhere.
	s.state.Claims["clm_test"].Revoked = true
	rows := decodePreAuths(t, preAuthDo(t, s, sid, http.MethodGet, "/api/preauth", "")).PreAuths
	if len(rows) != 1 || !rows[0].ClaimRevoked {
		t.Fatalf("a revoked grant is not visible on the record: %+v", rows)
	}
	if rec, _ := enrollAs(t, s, "c", "SN-1", "CPE-01"); rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked grant still enrolled: %d (%s)", rec.Code, rec.Body.String())
	}
	s.state.Claims["clm_test"].Revoked = false

	// An index is resolved inside the tenant the CODE belongs to, never across
	// tenants: the same name in two tenants means two different boxes, and the
	// only records either enrollment can reach are its own tenant's.
	s.state.Tenants["t2"] = &Tenant{ID: "t2"}
	s.state.Claims["clm_t2"] = &Claim{
		ID: "clm_t2", TenantID: "t2", Name: "别人的", MaxUses: 1, CodeHash: hashClaimCode("k"),
	}
	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	otherRow := createPA(t, s, other, "CPE-01", "", "", "clm_t2")

	resp := mustEnroll(t, s, "k", "SN-9", "CPE-01")
	if resp.TenantID != "t2" || resp.PreAuthName != "CPE-01" {
		t.Fatalf("t2's box did not claim t2's record: %+v", resp)
	}
	if s.state.Nodes[key("t2", resp.NodeID)] == nil {
		t.Fatalf("the node is not in its own tenant: %+v", s.state.Nodes)
	}
	if s.state.PreAuths[otherRow.ID].BoundNodeID != resp.NodeID {
		t.Fatalf("record did not bind: %+v", s.state.PreAuths[otherRow.ID])
	}
	if s.state.PreAuths[row.ID].claimed() {
		t.Fatalf("t2's enrollment claimed tenant t's record: %+v", s.state.PreAuths[row.ID])
	}
	if len(s.state.Claims["clm_test"].Serials) != 0 {
		t.Fatalf("t2's code activated a device on t's grant: %+v", s.state.Claims["clm_test"].Serials)
	}
}
