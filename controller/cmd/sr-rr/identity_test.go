// Tests for the single-file boot path: one rr-identity.json downloaded from
// the console must be enough to run an RR, with no separate cert/key/ca files
// and no hand-written config. The properties that matter:
//
//   - the certificate is the authority on rr_id/tenant; file fields may only
//     confirm it, never contradict it;
//   - the listen address comes from the file's own advertised addr (port only,
//     bound on every interface);
//   - controller_addr seeds the same mesh the config path does, and its
//     absence still yields a working inbound-only RR.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestIdentity mints a real CA + RR certificate (srou://<tenant>/rr/<id>,
// SAN <host>) and writes the console-shaped identity file to a temp dir. It
// returns the file path.
func writeTestIdentity(t *testing.T, tenant, rrID, host, ctrlAddr string) string {
	t.Helper()
	caPub, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "srou-controller-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(fmt.Sprintf("srou://%s/rr/%s", tenant, rrID))
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("%s-rr-%s", tenant, rrID)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:         []*url.URL{uri},
		DNSNames:     []string{host},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, pub, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	doc := map[string]any{
		"rr_id": rrID, "tenant": tenant, "addr": host + ":8443",
		"cert_pem": string(certPEM), "key_pem": string(keyPEM), "ca_pem": string(caPEM),
		"fingerprint": "deadbeef", "serial": "2", "issued_at_unix": time.Now().Unix(),
	}
	if ctrlAddr != "" {
		doc["controller_addr"] = ctrlAddr
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "rr-identity.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIdentityFileBootsAnRRWithOneInput(t *testing.T) {
	p := writeTestIdentity(t, "t", "rr1", "rr1.example.com", "https://192.168.1.100:8444")
	rt, err := loadIdentity(p, "", "")
	if err != nil {
		t.Fatalf("loadIdentity: %v", err)
	}
	if rt.rrID != "rr1" || rt.tenant != "t" {
		t.Fatalf("identity from certificate = %s/%s", rt.rrID, rt.tenant)
	}
	// listen binds every interface at the file's advertised port: the host part
	// of addr is what OTHERS dial, not necessarily an IP this box holds.
	if rt.listen != ":8443" {
		t.Fatalf("listen = %q, want :8443", rt.listen)
	}
	if rt.ctrlAddr != "https://192.168.1.100:8444" {
		t.Fatalf("ctrlAddr = %q", rt.ctrlAddr)
	}
	if rt.cert.Leaf == nil && len(rt.cert.Certificate) == 0 {
		t.Fatal("no certificate loaded")
	}
	if len(rt.caPEM) == 0 || !rt.pool.Equal(rt.pool) {
		t.Fatal("CA pool missing")
	}
	// The address book lands next to the identity file, per rr_id, exactly as
	// the config path does beside its config file.
	want := filepath.Join(filepath.Dir(p), "ctrl-addrs-rr1.json")
	if filepath.Join(rt.stateDir, "ctrl-addrs-rr1.json") != want {
		t.Fatalf("stateDir = %s, want %s", rt.stateDir, filepath.Dir(p))
	}
}

func TestIdentityFileWithoutControllerIsInboundOnly(t *testing.T) {
	p := writeTestIdentity(t, "t", "rr2", "rr2.example.com", "")
	rt, err := loadIdentity(p, "", "")
	if err != nil {
		t.Fatalf("loadIdentity: %v", err)
	}
	if rt.ctrlAddr != "" {
		t.Fatalf("ctrlAddr = %q, want empty (inbound only)", rt.ctrlAddr)
	}
	if rt.listen != ":8443" {
		t.Fatalf("listen = %q", rt.listen)
	}
}

func TestIdentityFileRejectsContradictingFields(t *testing.T) {
	// The certificate is the authority. A hand-edited file claiming another
	// rr_id must fail loudly instead of routing sessions under a wrong name.
	p := writeTestIdentity(t, "t", "rr3", "rr3.example.com", "")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	doc["rr_id"] = "rr-evil"
	tampered, _ := json.Marshal(doc)
	p2 := filepath.Join(t.TempDir(), "rr-identity.json")
	if err := os.WriteFile(p2, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIdentity(p2, "", ""); err == nil {
		t.Fatal("a file contradicting its own certificate loaded")
	}
}

func TestIdentityFileRequiresUsableCA(t *testing.T) {
	p := writeTestIdentity(t, "t", "rr4", "rr4.example.com", "")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	doc["ca_pem"] = "not a pem"
	tampered, _ := json.Marshal(doc)
	p2 := filepath.Join(t.TempDir(), "rr-identity.json")
	if err := os.WriteFile(p2, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIdentity(p2, "", ""); err == nil {
		t.Fatal("a file with a broken CA loaded")
	}
}

func TestIdentityFileListenOverrideWins(t *testing.T) {
	p := writeTestIdentity(t, "t", "rr5", "rr5.example.com", "")
	rt, err := loadIdentity(p, "127.0.0.1:9443", "127.0.0.1:9444")
	if err != nil {
		t.Fatalf("loadIdentity: %v", err)
	}
	if rt.listen != "127.0.0.1:9443" || rt.statusAddr != "127.0.0.1:9444" {
		t.Fatalf("overrides ignored: listen=%q status=%q", rt.listen, rt.statusAddr)
	}
}
