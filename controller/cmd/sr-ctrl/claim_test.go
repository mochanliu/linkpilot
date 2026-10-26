// Tests for the two-level enrollment contract: the operator issues a GRANT
// (name + seats) and the tenant mints a short-lived CODE from it at activation
// time. The properties under test are the ones a leak would be invisible in:
//
//   - the plaintext code exists in exactly one response and never on disk or in
//     /api/state (only its hash is stored);
//   - generating a new code kills the previous one immediately;
//   - the deadline is enforced by the store, not by the page's countdown;
//   - only the owning tenant can mint, and only the operator can grant/revoke.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claimFixture is a console-capable controller: one grant on tenant "t", a
// second tenant "t2" to probe cross-tenant access, and real sessions.
func claimFixture(t *testing.T) (*server, *Claim) {
	t.Helper()
	grant := &Claim{ID: "clm_grant", TenantID: "t", Name: "总部入网", CreatedAt: time.Now().Unix(), MaxUses: 3}
	s := enrollFixture(t, claimMap{grant.ID: grant})
	s.state.Tenants["t2"] = &Tenant{ID: "t2", Name: "另一家"}
	s.state.Accounts = map[string]*Account{}
	return s, grant
}

func newTestSession(s *server, id consoleIdentity) string {
	sid := randToken()
	id.expires = time.Now().Add(time.Hour)
	s.admin.mu.Lock()
	s.admin.sess[sid] = id
	s.admin.mu.Unlock()
	return sid
}

func tenantSession(s *server, tenant string) string {
	return newTestSession(s, consoleIdentity{Role: roleTenant, TenantID: tenant, Username: tenant + "-admin", Via: "password"})
}

func operatorSession(s *server) string {
	return newTestSession(s, consoleIdentity{Role: roleOperator, Username: "ops", Via: "password"})
}

func consoleReq(method, path, body, sid string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	return r
}

func mintCode(t *testing.T, s *server, sid, grantID string, ttl int) *claimCodeResponse {
	t.Helper()
	body, _ := json.Marshal(claimCodeRequest{ID: grantID, TTLSec: ttl})
	rec := httptest.NewRecorder()
	s.handleAdminClaimCode(rec, consoleReq(http.MethodPost, "/api/claims/code", string(body), sid))
	if rec.Code != http.StatusOK {
		t.Fatalf("generate = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out claimCodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestClaimCodeIsShownOnceAndOnlyTheHashIsStored(t *testing.T) {
	s, grant := claimFixture(t)
	resp := mintCode(t, s, tenantSession(s, "t"), grant.ID, 300)

	if !strings.HasPrefix(resp.Code, "srk_") || len(resp.Code) != 36 {
		t.Fatalf("code shape = %q", resp.Code)
	}
	wantExp := resp.IssuedAt + 300
	if resp.ExpiresAt != wantExp {
		t.Fatalf("expires_at = %d, want %d", resp.ExpiresAt, wantExp)
	}
	if grant.CodeHash != hashClaimCode(resp.Code) {
		t.Fatal("stored hash does not match the code handed out")
	}
	if grant.CodeExpiresAt != resp.ExpiresAt || grant.CodeIssuedBy != "t-admin" {
		t.Fatalf("issue metadata not recorded: %+v", grant)
	}

	// On disk and in the state API the secret must not exist. A hash is fine; the
	// code itself is what must never be written.
	disk, err := os.ReadFile(filepath.Join(s.dataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{resp.Code, "code_hash\":" + resp.Code} {
		if strings.Contains(string(disk), leak) {
			t.Fatalf("state file contains the code: %s", leak)
		}
	}
	if !strings.Contains(string(disk), hashClaimCode(resp.Code)) {
		t.Fatal("state file does not carry the hash; the code would not survive a restart")
	}

	rec := httptest.NewRecorder()
	s.handleAdminState(rec, consoleReq(http.MethodGet, "/api/state", "", tenantSession(s, "t")))
	raw := rec.Body.String()
	if strings.Contains(raw, resp.Code) || strings.Contains(raw, "code_hash") {
		t.Fatalf("/api/state leaks the secret or its hash: %s", raw)
	}
	if !strings.Contains(raw, `"code_active":true`) {
		t.Fatalf("tenant cannot see that a code is live: %s", raw)
	}
}

func TestClaimCodeGenerateIsTenantOnly(t *testing.T) {
	s, grant := claimFixture(t)
	body, _ := json.Marshal(claimCodeRequest{ID: grant.ID, TTLSec: 300})

	cases := []struct {
		name string
		sid  string
		want int
	}{
		// The operator may grant seats, never mint the credential itself.
		{"operator account", operatorSession(s), http.StatusForbidden},
		{"operator token", newTestSession(s, consoleIdentity{Role: roleOperator, Username: "operator", Via: "token"}), http.StatusForbidden},
		{"other tenant", tenantSession(s, "t2"), http.StatusNotFound},
		{"anonymous", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleAdminClaimCode(rec, consoleReq(http.MethodPost, "/api/claims/code", string(body), tc.sid))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if grant.CodeHash != "" {
		t.Fatal("a refused request minted a code")
	}
}

func TestClaimCodeRotatesAndExpires(t *testing.T) {
	s, grant := claimFixture(t)
	sid := tenantSession(s, "t")

	first := mintCode(t, s, sid, grant.ID, 300)
	second := mintCode(t, s, sid, grant.ID, 300)
	if first.Code == second.Code {
		t.Fatal("rotation returned the same code")
	}

	// The old code is dead the moment a new one is issued: one grant, one secret.
	rec := httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, first.Code, "sn-old"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rotated-out code still enrolls: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, second.Code, "sn-new"))
	if rec.Code != http.StatusOK {
		t.Fatalf("live code failed to enroll: %d %s", rec.Code, rec.Body.String())
	}
	if len(grant.Serials) != 1 || grant.Serials[0] != "sn-new" {
		t.Fatalf("seat ledger wrong after rotation: %+v", grant.Serials)
	}

	// Expiry is a property of the record, not of the page's countdown.
	grant.CodeExpiresAt = time.Now().Add(-time.Second).Unix()
	s.expireClaimCodesLocked(time.Now().Unix())
	if grant.CodeHash != "" {
		t.Fatal("sweeper left an expired code live")
	}
	if claimView(grant).CodeActive {
		t.Fatal("an expired grant still renders as code_active")
	}
	rec = httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, second.Code, "sn-late"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expired code still enrolls: %d %s", rec.Code, rec.Body.String())
	}
}

func TestClaimCodeTTLBounds(t *testing.T) {
	cases := []struct {
		in      int
		want    int
		wantErr bool
	}{
		{0, claimCodeTTLDefault, false},
		{1, 1, false},
		{claimCodeTTLMax, claimCodeTTLMax, false},
		{-1, 0, true},
		{claimCodeTTLMax + 1, 0, true},
	}
	for _, tc := range cases {
		got, err := normalizeClaimCodeTTL(tc.in)
		if (err != nil) != tc.wantErr || (!tc.wantErr && got != tc.want) {
			t.Errorf("normalizeClaimCodeTTL(%d) = %d, %v; want %d, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}

	s, grant := claimFixture(t)
	resp := mintCode(t, s, tenantSession(s, "t"), grant.ID, 0)
	if d := resp.ExpiresAt - resp.IssuedAt; d != claimCodeTTLDefault {
		t.Fatalf("default window = %ds, want %ds", d, claimCodeTTLDefault)
	}
	body, _ := json.Marshal(claimCodeRequest{ID: grant.ID, TTLSec: claimCodeTTLMax + 1})
	rec := httptest.NewRecorder()
	s.handleAdminClaimCode(rec, consoleReq(http.MethodPost, "/api/claims/code", string(body), tenantSession(s, "t")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized ttl = %d, want 400", rec.Code)
	}
}

func TestGrantIssueAndRevokeByID(t *testing.T) {
	s, _ := claimFixture(t)
	ops := operatorSession(s)

	// A tenant cannot issue itself more seats.
	rec := httptest.NewRecorder()
	s.handleAdminClaims(rec, consoleReq(http.MethodPost, "/api/claims",
		`{"tenant_id":"t","name":"x","max_uses":99}`, tenantSession(s, "t")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant issued a grant: %d", rec.Code)
	}

	// The operator issues one; the response carries the row, never a code.
	rec = httptest.NewRecorder()
	s.handleAdminClaims(rec, consoleReq(http.MethodPost, "/api/claims",
		`{"tenant_id":"t","name":"分支入网","max_uses":2}`, ops))
	if rec.Code != http.StatusOK {
		t.Fatalf("issue = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "srk_") {
		t.Fatal("grant creation returned a code")
	}
	var view consoleClaim
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "分支入网" || view.MaxUses != 2 || view.CodeActive || !strings.HasPrefix(view.ID, "clm_") {
		t.Fatalf("grant view = %+v", view)
	}

	// Name defaults rather than rendering blank; overlong names are refused.
	rec = httptest.NewRecorder()
	s.handleAdminClaims(rec, consoleReq(http.MethodPost, "/api/claims", `{"tenant_id":"t"}`, ops))
	var dflt consoleClaim
	json.Unmarshal(rec.Body.Bytes(), &dflt)
	if rec.Code != http.StatusOK || dflt.Name == "" {
		t.Fatalf("default name = %q (%d)", dflt.Name, rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleAdminClaims(rec, consoleReq(http.MethodPost, "/api/claims",
		`{"tenant_id":"t","name":"`+strings.Repeat("长", 61)+`"}`, ops))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong name = %d, want 400", rec.Code)
	}

	// Revoke by id kills the live code too, not just future enrollments.
	code := mintCode(t, s, tenantSession(s, "t"), view.ID, 300)
	rec = httptest.NewRecorder()
	s.handleAdminClaimRevoke(rec, consoleReq(http.MethodPost, "/api/claims/revoke",
		`{"id":"`+view.ID+`"}`, ops))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, code.Code, "sn-after-revoke"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked grant still enrolls: %d", rec.Code)
	}
	// A revoked grant cannot mint either.
	rec = httptest.NewRecorder()
	s.handleAdminClaimCode(rec, consoleReq(http.MethodPost, "/api/claims/code",
		`{"id":"`+view.ID+`"}`, tenantSession(s, "t")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked grant minted a code: %d", rec.Code)
	}
}

// TestMigrateLegacyPlaintextCodes is the upgrade path: an old state file keeps
// the code as the map key and in "code". Loading it must rewrite the file
// without the secret, and the already-handed-out code must keep working.
func TestMigrateLegacyPlaintextCodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{
	  "nodes": {}, "next_node_id": {}, "epoch": 7,
	  "tenants": {"t": {"id": "t", "name": "老租户", "created_at_unix": 100}},
	  "claims": {"srk_LEGACYSECRET": {"tenant_id": "t", "note": "现场入网", "max_uses": 4, "created_at_unix": 100}},
	  "p_key_rings": {}, "accounts": {}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := (&fileStore{path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	var grant *Claim
	for _, c := range st.Claims {
		grant = c
	}
	if grant == nil || grant.ID == "" || grant.TenantID != "t" {
		t.Fatalf("legacy record not migrated to an id: %+v", st.Claims)
	}
	if grant.Name != "现场入网" || grant.MaxUses != 4 {
		t.Fatalf("note/max_uses lost in migration: %+v", grant)
	}
	if grant.CodeHash != hashClaimCode("srk_LEGACYSECRET") {
		t.Fatal("legacy code was not hashed")
	}
	if grant.LegacyCode != "" || grant.LegacyNote != "" || grant.LegacyExpiresAt != 0 {
		t.Fatalf("legacy fields survived normalization: %+v", grant)
	}

	// The migration's whole point: the file on disk no longer holds the secret.
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "srk_LEGACYSECRET") {
		t.Fatalf("migrated state still contains the plaintext code:\n%s", rewritten)
	}

	// And the code that was already given to a device still enrolls once.
	s := enrollFixture(t, claimMap{grant.ID: grant})
	s.state.Tenants = st.Tenants
	rec := httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, "srk_LEGACYSECRET", "sn-legacy"))
	if rec.Code != http.StatusOK {
		t.Fatalf("migrated code stopped working: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNormalizeStateIsIdempotentForNewRecords(t *testing.T) {
	now := time.Now().Unix()
	st := &State{
		Claims: claimMap{"clm_ok": {
			ID: "clm_ok", TenantID: "t", Name: "名", CreatedAt: now, MaxUses: 2,
			CodeHash: hashClaimCode("x"), CodeExpiresAt: now + 60,
		}},
		Tenants: map[string]*Tenant{"t": {ID: "t"}},
	}
	if normalizeState(st) {
		t.Fatal("a current-format state was reported as migrated")
	}
	if st.Claims["clm_ok"] == nil || st.Claims["clm_ok"].CodeHash != hashClaimCode("x") {
		t.Fatalf("normalization damaged a current record: %+v", st.Claims)
	}
}
