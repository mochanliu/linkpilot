// Tests for the enrollment contract. Enrollment is the one endpoint an
// unauthenticated caller can reach, and the one place a mistake is least
// visible later: an enroll response that quietly carries configuration is how
// a device ends up believing it has a tenant's ports and keys before anyone
// configured it.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

func TestEnrollResponseIsIdentityOnly(t *testing.T) {
	s := newTestServer()
	s.tlsAddrs = []string{"ctrl:8444"}
	rec := &NodeRecord{TenantID: "t", NodeID: 7}
	resp := s.enrollResponse("t", rec, "cert-pem", "ca-pem")

	if resp.NodeID != 7 || resp.TenantID != "t" || resp.CertPEM != "cert-pem" ||
		resp.CAPEM != "ca-pem" || resp.HeartbeatSec != 5 || resp.Epoch == 0 {
		t.Fatalf("identity fields lost: %+v", resp)
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var onWire map[string]json.RawMessage
	if err := json.Unmarshal(b, &onWire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tenant_id", "node_id", "cert_pem", "ca_pem", "tls_addrs", "heartbeat_sec", "epoch"} {
		if _, ok := onWire[k]; !ok {
			t.Errorf("enroll response is missing %q", k)
		}
	}
	// Everything below is CONFIGURATION. It belongs to the node plan, which is
	// pushed after the customer configures the device in the console. If one of
	// these ever reappears here, enroll has silently become a config channel
	// again and every device boots with invented values.
	for _, k := range []string{"udp_reflector", "rrs", "vpn_port", "transit_port", "p_keys", "tunnel_mtu"} {
		if _, ok := onWire[k]; ok {
			t.Errorf("enroll response must not carry %q", k)
		}
	}
}

// enrollFixture stands up a real CA so the handler can sign, with a temp store.
func enrollFixture(t *testing.T, claims claimMap) *server {
	t.Helper()
	dir := t.TempDir()
	ca, key, _, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	s := newTestServer()
	s.state.Tenants = map[string]*Tenant{"t": {ID: "t"}}
	s.state.Claims = claims
	s.caCert = ca
	s.caKey = key
	s.dataDir = dir
	s.store = &fileStore{path: dir + "/state.json"}
	s.admin = newAdminHub()
	return s
}

func enrollReq(t *testing.T, claim string) *http.Request {
	t.Helper()
	return enrollReqSerial(t, claim, "sn-1")
}

func enrollReqSerial(t *testing.T, claim, serial string) *http.Request {
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
	})
	return httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader(string(body)))
}

func TestEnrollRejectsClaimsThatAuthorizeNothing(t *testing.T) {
	cases := []struct {
		name  string
		claim *Claim
	}{
		{"revoked", &Claim{Code: "c", TenantID: "t", Revoked: true}},
		{"expired", &Claim{Code: "c", TenantID: "t", ExpiresAt: time.Now().Add(-time.Hour).Unix()}},
		{"unknown tenant", &Claim{Code: "c", TenantID: "nope"}},
		{"no tenant", &Claim{Code: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := enrollFixture(t, claimMap{"c": tc.claim})
			rec := httptest.NewRecorder()
			s.handleEnroll(rec, enrollReq(t, "c"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
			if len(s.state.Nodes) != 0 {
				t.Fatalf("a rejected enrollment created a node: %+v", s.state.Nodes)
			}
		})
	}
}

func TestEnrollRecordsSeatOnceAndIsIdempotent(t *testing.T) {
	claim := &Claim{Code: "c", TenantID: "t", MaxUses: 5}
	s := enrollFixture(t, claimMap{"c": claim})

	rec := httptest.NewRecorder()
	s.handleEnroll(rec, enrollReq(t, "c"))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.state.Nodes) != 1 || s.state.Nodes[key("t", 1)] == nil {
		t.Fatalf("node not registered: %+v", s.state.Nodes)
	}
	if len(claim.Serials) != 1 || claim.Serials[0] != "sn-1" {
		t.Fatalf("claim usage not recorded: %+v", claim.Serials)
	}

	// Same serial again: same node id, same row, and no second seat. A seat is
	// a device, not an enrollment; otherwise a reboot loop would exhaust the code.
	rec = httptest.NewRecorder()
	s.handleEnroll(rec, enrollReq(t, "c"))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enroll failed: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.state.Nodes) != 1 || s.state.Nodes[key("t", 1)] == nil {
		t.Fatalf("re-enrollment created a second node: %+v", s.state.Nodes)
	}
	if len(claim.Serials) != 1 {
		t.Fatalf("re-enrollment consumed a seat: %+v", claim.Serials)
	}
}

func TestEnrollStopsAtTheActivationLimit(t *testing.T) {
	claim := &Claim{Code: "c", TenantID: "t", MaxUses: 2}
	s := enrollFixture(t, claimMap{"c": claim})

	for _, serial := range []string{"sn-1", "sn-2"} {
		rec := httptest.NewRecorder()
		s.handleEnroll(rec, enrollReqSerial(t, "c", serial))
		if rec.Code != http.StatusOK {
			t.Fatalf("enroll %s failed: %d %s", serial, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, "c", "sn-3"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("enroll past the limit = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if len(s.state.Nodes) != 2 {
		t.Fatalf("a rejected enrollment created a node: %+v", s.state.Nodes)
	}
	if len(claim.Serials) != 2 {
		t.Fatalf("a rejected enrollment consumed a seat: %+v", claim.Serials)
	}

	// Already-enrolled devices are unaffected by a full code: they still hold
	// certificates, and re-enrolling one must keep working.
	rec = httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, "c", "sn-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enroll of an existing serial on a full code = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestNormalizeClaimMaxUses(t *testing.T) {
	cases := []struct {
		in      int
		want    int
		wantErr bool
	}{
		{0, claimDefaultMaxUses, false},
		{1, 1, false},
		{claimMaxUsesLimit, claimMaxUsesLimit, false},
		{-1, 0, true},
		{claimMaxUsesLimit + 1, 0, true},
	}
	for _, tc := range cases {
		got, err := normalizeClaimMaxUses(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("normalizeClaimMaxUses(%d) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("normalizeClaimMaxUses(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
