// Tests for the two inputs a factory-fresh box used to need by hand: the
// controller address and the pinned CA. With a self-describing claim code
// (srs1-…) both ride inside the pasted string, so the only manual input left
// is the code itself.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/claimcode"
)

// mintTestCA makes a real (self-signed) CA PEM — AppendCertsFromPEM parses the
// DER, so a fake body would be rejected for the wrong reason.
func mintTestCA(t *testing.T) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "srou-controller-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestPinnedRootsAcceptsTheCACarryInClaimCode(t *testing.T) {
	testCA := mintTestCA(t)
	// A box with NO files on disk enrolls with just the pasted code: the
	// embedded PEM is the trust anchor. This is the whole point of the
	// envelope — zero file copies on the serial console.
	pool, err := pinnedRoots("", testCA, t.TempDir())
	if err != nil {
		t.Fatalf("embedded CA rejected: %v", err)
	}
	if pool == nil {
		t.Fatal("no pool")
	}
	// The pool must actually contain the certificate, not an empty pool.
	subjects := pool.Subjects()
	if len(subjects) == 0 {
		t.Fatal("pool has no subjects")
	}
}

func TestPinnedRootsEmbeddedGibberishFailsLoudly(t *testing.T) {
	if _, err := pinnedRoots("", "this is not pem", t.TempDir()); err == nil {
		t.Fatal("non-PEM embedded CA accepted")
	}
}

func TestPinnedRootsFileFallbackStillWorks(t *testing.T) {
	// The classic path — operator places pinned_ca.pem — must keep working
	// for old codes that carry no CA.
	testCA := mintTestCA(t)
	dir := t.TempDir()
	if err := writeFileForTest(dir+"/pinned_ca.pem", testCA); err != nil {
		t.Fatal(err)
	}
	pool, err := pinnedRoots("", "", dir)
	if err != nil {
		t.Fatalf("file fallback broken: %v", err)
	}
	if len(pool.Subjects()) == 0 {
		t.Fatal("pool empty")
	}
}

func TestPinnedRootsNothingAtAllIsAnError(t *testing.T) {
	_, err := pinnedRoots("", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no CA bundle") {
		t.Fatalf("err = %v, want the actionable hint", err)
	}
}

func TestPoolFromEmbeddedCAVerifiesLikeAFile(t *testing.T) {
	// Sanity: the two paths produce equivalent pools (same subject set).
	testCA := mintTestCA(t)
	dir := t.TempDir()
	if err := writeFileForTest(dir+"/pinned_ca.pem", testCA); err != nil {
		t.Fatal(err)
	}
	fromFile, err := pinnedRoots("", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	fromCode, err := pinnedRoots("", testCA, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile.Subjects()) != len(fromCode.Subjects()) {
		t.Fatal("embedded and file pools differ")
	}
	if !poolHasSubject(fromCode, fromFile.Subjects()[0]) {
		t.Fatal("embedded pool lacks the CA subject")
	}
}

func poolHasSubject(p *x509.CertPool, subject []byte) bool {
	for _, s := range p.Subjects() {
		if string(s) == string(subject) {
			return true
		}
	}
	return false
}

// claimCodeSeedCtrl mirrors the main() logic that picks -ctrl: the flag wins,
// then the envelope's first endpoint, then it is a hard error.
func TestCtrlFromClaimEnvelopeOrder(t *testing.T) {
	testCA := mintTestCA(t)
	secret, env, err := claimcode.Parse(
		claimcode.Wrap("srk_S", testCA, []string{"192.168.1.100:8444", "10.0.0.9:8444"}))
	if err != nil {
		t.Fatal(err)
	}
	if secret != "srk_S" || len(env.Ctrl) != 2 {
		t.Fatalf("parse = %q, %v", secret, env)
	}
	// The agent takes the first endpoint; the rest arrive via the address book
	// once the first heartbeat lands.
	if env.Ctrl[0] != "192.168.1.100:8444" {
		t.Fatalf("first endpoint = %q", env.Ctrl[0])
	}
}

func writeFileForTest(p, content string) error {
	return os.WriteFile(p, []byte(content), 0600)
}
