// Tests for RR identity issuance and the grid's read side (O9-b2 b2c).
//
// What a console cannot show, and therefore what is pinned here:
//
//   - the certificate really names the row: URI role/key, CN, the address host
//     in the SAN, and both EKUs (an RR both dials and is dialled);
//   - the private key is delivered once and never lands in state -- a rotation
//     that silently kept the old key would be indistinguishable from a rotation
//     that did not happen;
//   - /v1/rr/config answers exactly one tenant's grid, without self, and a node
//     certificate cannot read it;
//   - last_fetch is persisted, because it is the only "this RR really came"
//     signal the platform has;
//   - an address-host or owner change flags the row as needing a new identity
//     instead of re-signing behind the operator's back.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sdwan/controller/internal/keybinding"
)

// rrIdentityFixture gives the RR fixture a real CA: issuance reads ca.pem from
// dataDir and signs with the in-memory key, exactly as the running controller
// does.
func rrIdentityFixture(t *testing.T) (*server, string, string) {
	t.Helper()
	s, ten, two, _ := rrFixture(t)
	dir := t.TempDir()
	caCert, caKey, _, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	s.caCert, s.caKey, s.dataDir = caCert, caKey, dir
	return s, ten, two
}

func rrIssue(t *testing.T, s *server, sid, id string) (*httptest.ResponseRecorder, rrIdentityResponse) {
	t.Helper()
	body, _ := json.Marshal(rrIdentityRequest{ID: id})
	r := httptest.NewRequest(http.MethodPost, "/api/rrs/identity", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	s.handleTenantRRIdentity(rec, r)
	var out rrIdentityResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode identity response: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

func parsePEMCert(t *testing.T, pemStr string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		t.Fatal("no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

// rrCertReq is an mTLS request carrying the URI a signed RR certificate would
// assert. Chain validation happens at the TLS handshake; these tests are about
// what the handler does with an already-authenticated identity.
func rrCertReq(method, target, tenant, rrID string) *http.Request {
	u, err := url.Parse(fmt.Sprintf("srou://%s/rr/%s", tenant, rrID))
	if err != nil {
		panic(err)
	}
	r := httptest.NewRequest(method, target, nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{u}}}}
	return r
}

func TestRRIdentityCertificateShape(t *testing.T) {
	s, ten, _ := rrIdentityFixture(t)
	byName := rrCreate(t, s, ten, "RR 上海", "rr-sh.example.com:20200")
	byIP := rrCreate(t, s, ten, "RR 北京", "192.0.2.9:20200")

	rec, out := rrIssue(t, s, ten, byName)
	if rec.Code != http.StatusOK {
		t.Fatalf("identity = %d (%s)", rec.Code, rec.Body.String())
	}
	if out.KeyPEM == "" || out.CertPEM == "" || out.CAPEM == "" {
		t.Fatal("response must carry cert, key and CA")
	}
	cert := parsePEMCert(t, out.CertPEM)

	if cert.Subject.CommonName != "t-rr-rr1" {
		t.Fatalf("CN = %q", cert.Subject.CommonName)
	}
	ref, err := keybinding.RefOf(cert)
	if err != nil {
		t.Fatalf("RefOf: %v", err)
	}
	if ref.Tenant != "t" || ref.Role != keybinding.RoleRR || ref.RRID != "rr1" {
		t.Fatalf("identity = %+v", ref)
	}
	if len(cert.ExtKeyUsage) != 2 ||
		!hasEKU(cert, x509.ExtKeyUsageClientAuth) || !hasEKU(cert, x509.ExtKeyUsageServerAuth) {
		t.Fatalf("EKU = %v, want clientAuth+serverAuth", cert.ExtKeyUsage)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "rr-sh.example.com" || len(cert.IPAddresses) != 0 {
		t.Fatalf("SAN = dns %v ip %v", cert.DNSNames, cert.IPAddresses)
	}
	if cert.Subject.CommonName == "" || cert.SerialNumber == nil {
		t.Fatal("certificate carries no serial/CN")
	}

	sum := sha256.Sum256(cert.Raw)
	wantFP := hex.EncodeToString(sum[:16])
	if out.Fingerprint != wantFP || len(out.Fingerprint) != 32 {
		t.Fatalf("fingerprint = %q, want %q", out.Fingerprint, wantFP)
	}
	if out.Serial != cert.SerialNumber.Text(16) {
		t.Fatalf("serial = %q, cert has %q", out.Serial, cert.SerialNumber.Text(16))
	}
	if out.Tenant != "t" || out.RRID != "rr1" || out.Addr != "rr-sh.example.com:20200" {
		t.Fatalf("response identity = %+v", out)
	}

	// The private key half must match the certificate's public key, or the pair
	// we handed over could never complete a handshake.
	kb, _ := pem.Decode([]byte(out.KeyPEM))
	if kb == nil || kb.Type != "PRIVATE KEY" {
		t.Fatal("key_pem is not a PRIVATE KEY block")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	priv, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("key type %T", keyAny)
	}
	certPub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(certPub, priv.Public().(ed25519.PublicKey)) {
		t.Fatal("private key does not match certificate")
	}

	// ca_pem is the controller CA, so the box can verify the controller and its peers.
	ca := parsePEMCert(t, out.CAPEM)
	if !bytes.Equal(ca.Raw, s.caCert.Raw) {
		t.Fatal("ca_pem is not the controller CA")
	}

	// An IP row puts the IP in the SAN, not a DNS name: devices dial by addr host
	// and use it as ServerName verbatim.
	_, ipOut := rrIssue(t, s, ten, byIP)
	ipCert := parsePEMCert(t, ipOut.CertPEM)
	if len(ipCert.IPAddresses) != 1 || ipCert.IPAddresses[0].String() != "192.0.2.9" || len(ipCert.DNSNames) != 0 {
		t.Fatalf("IP row SAN = dns %v ip %v", ipCert.DNSNames, ipCert.IPAddresses)
	}
}

func hasEKU(cert *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, e := range cert.ExtKeyUsage {
		if e == want {
			return true
		}
	}
	return false
}

func TestRRIdentityKeyNeverLandsInState(t *testing.T) {
	s, ten, _ := rrIdentityFixture(t)
	id := rrCreate(t, s, ten, "RR 上海", "rr-sh.example.com:20200")

	rec, first := rrIssue(t, s, ten, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("identity = %d (%s)", rec.Code, rec.Body.String())
	}

	// The stored row carries metadata only.
	row := s.state.RRs.get(id)
	if row == nil || row.Identity == nil {
		t.Fatal("row has no identity metadata")
	}
	if row.Identity.Fingerprint != first.Fingerprint || row.Identity.Serial != first.Serial {
		t.Fatalf("stored identity = %+v, response = %s/%s", row.Identity, first.Serial, first.Fingerprint)
	}
	if len(row.Identity.SAN) != 1 || row.Identity.SAN[0] != "rr-sh.example.com" {
		t.Fatalf("stored SAN = %v", row.Identity.SAN)
	}
	stateJSON, err := json.Marshal(s.state)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateJSON, []byte("PRIVATE KEY")) || bytes.Contains(stateJSON, []byte(first.KeyPEM)) {
		t.Fatal("private key material leaked into state")
	}
}

func TestRRIdentityReissueRotatesKeyAndSerial(t *testing.T) {
	s, ten, _ := rrIdentityFixture(t)
	id := rrCreate(t, s, ten, "RR 上海", "rr-sh.example.com:20200")

	_, first := rrIssue(t, s, ten, id)
	_, second := rrIssue(t, s, ten, id)
	if first.Serial == second.Serial {
		t.Fatal("re-issuance reused the serial")
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("re-issuance reused the certificate")
	}
	if first.KeyPEM == second.KeyPEM {
		t.Fatal("re-issuance reused the private key")
	}
	if row := s.state.RRs.get(id); row.Identity.Serial != second.Serial {
		t.Fatalf("row kept old identity: %+v", row.Identity)
	}
}

func TestRRIdentityNeedsResignAfterAddressOrOwnerChange(t *testing.T) {
	s, ten, _ := rrIdentityFixture(t)
	id := rrCreate(t, s, ten, "RR 上海", "rr-sh.example.com:20200")
	if rec, _ := rrIssue(t, s, ten, id); rec.Code != http.StatusOK {
		t.Fatalf("identity = %d (%s)", rec.Code, rec.Body.String())
	}
	if reason := s.state.RRs.get(id).resignReason(); reason != "" {
		t.Fatalf("fresh identity already needs a re-sign: %s", reason)
	}

	// Port only: the SAN is built from the host, so nothing is stale.
	body, _ := json.Marshal(rrWriteRequest{ID: id, Name: "RR 上海", Addr: "rr-sh.example.com:20443", Location: "lab"})
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs", string(body), ten); rec.Code != http.StatusOK {
		t.Fatalf("edit port = %d (%s)", rec.Code, rec.Body.String())
	}
	if reason := s.state.RRs.get(id).resignReason(); reason != "" {
		t.Fatalf("port change flagged a re-sign: %s", reason)
	}

	// Host change: the certificate names the old host, so it can no longer be
	// presented under the row's addr.
	body, _ = json.Marshal(rrWriteRequest{ID: id, Name: "RR 上海", Addr: "rr-new.example.com:20200", Location: "lab"})
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs", string(body), ten); rec.Code != http.StatusOK {
		t.Fatalf("edit host = %d (%s)", rec.Code, rec.Body.String())
	}
	if reason := s.state.RRs.get(id).resignReason(); !strings.Contains(reason, "地址已改") {
		t.Fatalf("host change reason = %q", reason)
	}

	// Owner change: unreachable through the API (owner comes from the session),
	// but a restored state file can carry it, so the check must read the tenant
	// out of the certificate record too.
	row := s.state.RRs.get(id)
	row.TenantID = "t2"
	if reason := row.resignReason(); !strings.Contains(reason, "归属或编号已改") {
		t.Fatalf("owner change reason = %q", reason)
	}
}

func TestRRConfigAuthRefusals(t *testing.T) {
	s, ten, two := rrIdentityFixture(t)
	mine := rrCreate(t, s, ten, "RR 1", "rr1.example.com:20200")
	rrCreate(t, s, ten, "RR 2", "rr2.example.com:20200")
	other := rrCreate(t, s, two, "乙 的 RR", "rr9.example.com:20200")

	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"no certificate", httptest.NewRequest(http.MethodGet, "/v1/rr/config", nil), http.StatusUnauthorized},
		{"node certificate", peerReq(http.MethodGet, "/v1/rr/config", "", "t", 1), http.StatusUnauthorized},
		{"unknown rr id", rrCertReq(http.MethodGet, "/v1/rr/config", "t", "rr404"), http.StatusNotFound},
		{"another tenant's row", rrCertReq(http.MethodGet, "/v1/rr/config", "t", other), http.StatusNotFound},
		{"wrong method", rrCertReq(http.MethodPost, "/v1/rr/config", "t", mine), http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.handleRRConfig(rec, tc.req)
		if rec.Code != tc.want {
			t.Fatalf("%s: code = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestRRConfigPeersAndLastFetch(t *testing.T) {
	s, ten, _ := rrIdentityFixture(t)
	first := rrCreate(t, s, ten, "RR 1", "rr1.example.com:20200")
	second := rrCreate(t, s, ten, "RR 2", "rr2.example.com:20200")
	third := rrCreate(t, s, ten, "RR 3", "rr3.example.com:20200")

	req := rrCertReq(http.MethodGet, "/v1/rr/config", "t", first)
	req.RemoteAddr = "198.51.100.7:40000"
	rec := httptest.NewRecorder()
	s.handleRRConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rr config = %d (%s)", rec.Code, rec.Body.String())
	}
	var out rrConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Tenant != "t" || out.RRID != first || out.GeneratedAtUnix == 0 {
		t.Fatalf("response = %+v", out)
	}
	if len(out.Peers) != 2 || out.Peers[0].ID != second || out.Peers[1].ID != third {
		t.Fatalf("peers = %+v, want rr2, rr3 in id order", out.Peers)
	}
	for _, p := range out.Peers {
		if p.ID == first {
			t.Fatal("peers contains self")
		}
	}
	if out.Peers[0].Addr != "rr2.example.com:20200" {
		t.Fatalf("peer addr = %q", out.Peers[0].Addr)
	}

	// 这一笔只在内存里（读数是读数，写进硬状态就是每 5 秒一轮共识），
	// 但页面必须看得到 —— 下面那条 /api/rrs 的断言才是这条要求的落点。
	if f := s.rrFetch[first]; f.At == 0 || f.Addr != "198.51.100.7" {
		t.Fatalf("last fetch = %d/%q", f.At, f.Addr)
	}
	if row := s.state.RRs.get(first); row.LastFetchAt != 0 {
		t.Fatalf("last fetch 不该进状态：%d", row.LastFetchAt)
	}

	// The earned fetch signal is what the row shows, so it must survive the
	// state round-trip the page's read goes through.
	list := rrReq(t, s, http.MethodGet, "/api/rrs", "", ten)
	var listed struct {
		RRs []rrRow `json:"rrs"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, list.Body.String())
	}
	if len(listed.RRs) != 3 || listed.RRs[0].LastFetchAt == 0 || listed.RRs[0].LastFetchAddr != "198.51.100.7" {
		t.Fatalf("listed rows = %+v", listed.RRs)
	}
}

func TestRRConfigOtherTenantSeesOnlyItsOwnGrid(t *testing.T) {
	s, ten, two := rrIdentityFixture(t)
	rrCreate(t, s, ten, "RR 1", "rr1.example.com:20200")
	other := rrCreate(t, s, two, "乙 的 RR", "rr9.example.com:20200")

	rec := httptest.NewRecorder()
	s.handleRRConfig(rec, rrCertReq(http.MethodGet, "/v1/rr/config", "t2", other))
	if rec.Code != http.StatusOK {
		t.Fatalf("rr config = %d (%s)", rec.Code, rec.Body.String())
	}
	var out rrConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Peers) != 0 {
		t.Fatalf("t2's grid leaked rows: %+v", out.Peers)
	}
}
