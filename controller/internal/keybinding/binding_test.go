package keybinding

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// fakeElement is a stand-in for the secure element described in doc 24 §7.2.
//
// It exists to prove the Signer seam is real rather than aspirational: swapping
// the key-holding implementation must not require touching anything else. If
// this type ever stops compiling, the seam has been bypassed.
type fakeElement struct{ priv ed25519.PrivateKey }

func (f *fakeElement) Sign(msg []byte) ([]byte, error) { return ed25519.Sign(f.priv, msg), nil }
func (f *fakeElement) Public() ed25519.PublicKey       { return f.priv.Public().(ed25519.PublicKey) }

func newIdentity(t *testing.T) (*fakeElement, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return &fakeElement{priv: priv}, pub
}

func x25519Pub(t *testing.T, fill byte) []byte {
	t.Helper()
	b := make([]byte, X25519PubLen)
	for i := range b {
		b[i] = fill
	}
	return b
}

func TestSignVerifyRoundTrip(t *testing.T) {
	signer, pub := newIdentity(t)
	pubKey := x25519Pub(t, 0xa5)

	b, err := Sign(signer, "tenant-a", 2, 1, pubKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := b.Verify("tenant-a", 2, pub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if string(got) != string(pubKey) {
		t.Fatal("Verify returned a different public key than was signed")
	}
}

// TestTamperedX25519PubIsRejected is the W4 DoD: a mediator that swaps the
// key-agreement public key in transit must be detected, not merely ineffective.
func TestTamperedX25519PubIsRejected(t *testing.T) {
	signer, pub := newIdentity(t)
	b, err := Sign(signer, "tenant-a", 2, 1, x25519Pub(t, 0xa5))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Swap in a different (valid-looking, right-length) public key. This is
	// what a compromised controller would do.
	b.X25519Pub = b64(x25519Pub(t, 0x5a))
	if _, err := b.Verify("tenant-a", 2, pub); err == nil {
		t.Fatal("a tampered x25519 public key verified -- the controller could MITM")
	}
}

func TestTamperedSignatureIsRejected(t *testing.T) {
	signer, pub := newIdentity(t)
	b, _ := Sign(signer, "tenant-a", 2, 1, x25519Pub(t, 0xa5))

	raw := []byte(b.Sig)
	raw[0] ^= 0x01
	b.Sig = b64(raw)
	if _, err := b.Verify("tenant-a", 2, pub); err == nil {
		t.Fatal("a tampered signature verified")
	}
}

func TestTamperedKeyGenIsRejected(t *testing.T) {
	signer, pub := newIdentity(t)
	b, _ := Sign(signer, "tenant-a", 2, 1, x25519Pub(t, 0xa5))
	b.KeyGen = 2 // a replayed binding from a later generation
	if _, err := b.Verify("tenant-a", 2, pub); err == nil {
		t.Fatal("changing key_gen still verified")
	}
}

// TestBindingsAreNotTransferableAcrossIdentity contexts: a binding made for one
// tenant or one node must not be usable as another.
func TestBindingsAreNotTransferableAcrossIdentity(t *testing.T) {
	signer, pub := newIdentity(t)
	b, _ := Sign(signer, "tenant-a", 2, 1, x25519Pub(t, 0xa5))

	for _, tc := range []struct {
		name   string
		tenant string
		node   uint16
	}{
		{"other tenant", "tenant-b", 2},
		{"other node", "tenant-a", 3},
		{"other tenant and node", "tenant-b", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := b.Verify(tc.tenant, tc.node, pub); err == nil {
				t.Fatalf("a binding for tenant-a/node/2 verified as %s/node/%d", tc.tenant, tc.node)
			}
		})
	}
}

func TestAnotherIdentityKeyIsRejected(t *testing.T) {
	signer, _ := newIdentity(t)
	_, otherPub := newIdentity(t)
	b, _ := Sign(signer, "tenant-a", 2, 1, x25519Pub(t, 0xa5))
	if _, err := b.Verify("tenant-a", 2, otherPub); err == nil {
		t.Fatal("a binding verified under an identity key that did not sign it")
	}
}

// TestMessageIsInjective runs the encoder over a deliberately adversarial set
// of near-miss inputs. If two different identities ever produced the same
// signed bytes, one device's signature would authorise another device's key --
// which is the whole hole this package exists to close.
func TestMessageIsInjective(t *testing.T) {
	pub := x25519Pub(t, 0x11)
	inputs := []struct {
		tenant string
		node   uint16
		keyGen uint32
	}{
		{"a", 0x0102, 3},
		{"a\x01", 0x0200, 3},
		{"a", 0x0001, 3},
		{"a", 0x0100, 3},
		{"a\x00", 0x0102, 3},
		{"ab", 0x0102, 3},
		{"", 0x0102, 3},
		{"a", 0x0102, 0},
		{"a", 0x0102, 0x03000000},
	}
	seen := map[string]string{}
	for _, in := range inputs {
		label := fmt.Sprintf("tenant=%q node=0x%04x keygen=%d", in.tenant, in.node, in.keyGen)
		key := string(Message(in.tenant, in.node, in.keyGen, pub))
		if prev, dup := seen[key]; dup {
			t.Fatalf("two inputs share signed bytes: %s and %s", prev, label)
		}
		seen[key] = label
	}
}

// TestWhyFieldsAreLengthPrefixed documents the rule Message follows by building
// the failure it prevents.
//
// Two ADJACENT variable-length fields concatenated bare are ambiguous:
// ("a","bc") and ("ab","c") produce the same bytes, and nothing detects it
// until two identities actually collide. Our message has a single
// variable-length field and is followed by fixed-width fields, so a bare
// concatenation would HAPPEN to be unambiguous today. The explicit length
// prefix is there so that appending another variable-length field in a later
// revision cannot quietly reintroduce the ambiguity -- it is a rule with a
// demonstrated failure behind it, not a stylistic preference.
//
// (Worth recording: while writing this I first asserted a collision that did
// not exist, having miscounted 61 01 02 00 00 00 07 against
// 61 01 02 00 00 00 00 07. The test caught the arithmetic, not the encoder.)
func TestWhyFieldsAreLengthPrefixed(t *testing.T) {
	bare := func(f1, f2 string) string { return f1 + f2 }
	prefixed := func(f1, f2 string) string {
		return fmt.Sprintf("%d:%s%d:%s", len(f1), f1, len(f2), f2)
	}
	if bare("a", "bc") != bare("ab", "c") {
		t.Fatal("expected a bare concatenation of two variable fields to be ambiguous")
	}
	if prefixed("a", "bc") == prefixed("ab", "c") {
		t.Fatal("length prefixes are still ambiguous")
	}
}

// TestVerifyPeerCert covers the three-part relying-party check end to end with
// a real certificate chain.
func TestVerifyPeerCert(t *testing.T) {
	caCert, caKey := newCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	const tenant = "tenant-a"
	const node = uint16(2)

	certPEM, devPriv := newDeviceCert(t, caCert, caKey, tenant, node)
	signer := &fakeElement{priv: devPriv}
	b, err := Sign(signer, tenant, node, 1, x25519Pub(t, 0xa5))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := VerifyPeerCert(pool, certPEM, tenant, node, b)
	if err != nil {
		t.Fatalf("VerifyPeerCert on a good chain: %v", err)
	}
	if got == nil {
		t.Fatal("VerifyPeerCert returned no certificate")
	}

	t.Run("wrong expected node", func(t *testing.T) {
		// The controller substitutes a DIFFERENT (but genuinely enrolled)
		// device's certificate and binding, hoping nobody checks the identity.
		otherPEM, otherPriv := newDeviceCert(t, caCert, caKey, tenant, 3)
		otherSigner := &fakeElement{priv: otherPriv}
		otherBinding, _ := Sign(otherSigner, tenant, 3, 1, x25519Pub(t, 0x77))
		if _, err := VerifyPeerCert(pool, otherPEM, tenant, node, otherBinding); err == nil {
			t.Fatal("node 3's certificate was accepted as node 2")
		}
	})

	t.Run("wrong tenant", func(t *testing.T) {
		if _, err := VerifyPeerCert(pool, certPEM, "tenant-b", node, b); err == nil {
			t.Fatal("a tenant-a certificate was accepted as tenant-b")
		}
	})

	t.Run("unpinned CA", func(t *testing.T) {
		otherCA, _ := newCA(t)
		otherPool := x509.NewCertPool()
		otherPool.AddCert(otherCA)
		if _, err := VerifyPeerCert(otherPool, certPEM, tenant, node, b); err == nil {
			t.Fatal("a certificate from a foreign CA was accepted")
		}
	})

	t.Run("binding does not match the certificate's key", func(t *testing.T) {
		// A different device signs a binding claiming to be tenant-a/node/2.
		_, strangerPriv := newDeviceCert(t, caCert, caKey, tenant, 9)
		strangerSigner := &fakeElement{priv: strangerPriv}
		forged, _ := Sign(strangerSigner, tenant, node, 1, x25519Pub(t, 0x33))
		if _, err := VerifyPeerCert(pool, certPEM, tenant, node, forged); err == nil {
			t.Fatal("a binding signed by another device was accepted")
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		expiredPEM, expiredPriv := newExpiredDeviceCert(t, caCert, caKey, tenant, node)
		expiredSigner := &fakeElement{priv: expiredPriv}
		expiredBinding, _ := Sign(expiredSigner, tenant, node, 1, x25519Pub(t, 0x55))
		if _, err := VerifyPeerCert(pool, expiredPEM, tenant, node, expiredBinding); err == nil {
			t.Fatal("an expired device certificate was accepted")
		}
	})

	t.Run("garbage input", func(t *testing.T) {
		if _, err := VerifyPeerCert(pool, "not a certificate", tenant, node, b); err == nil {
			t.Fatal("garbage was accepted as a certificate")
		}
	})
}

func TestDecodeRejectsMalformedBindings(t *testing.T) {
	signer, _ := newIdentity(t)
	good, _ := Sign(signer, "tenant-a", 2, 1, x25519Pub(t, 0xa5))

	for _, tc := range []struct {
		name string
		b    Binding
	}{
		{"pub not base64", Binding{X25519Pub: "!!!", Sig: good.Sig}},
		{"pub wrong length", Binding{X25519Pub: b64([]byte{1, 2, 3}), Sig: good.Sig}},
		{"sig not base64", Binding{X25519Pub: good.X25519Pub, Sig: "!!!"}},
		{"sig wrong length", Binding{X25519Pub: good.X25519Pub, Sig: b64([]byte{1, 2, 3})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tc.b.Decode(); err == nil {
				t.Fatal("malformed binding decoded successfully")
			}
		})
	}
}

func TestParseURI(t *testing.T) {
	u, err := url.Parse("srou://tenant-a/node/42")
	if err != nil {
		t.Fatal(err)
	}
	tenant, node, ok := ParseURI(u)
	if !ok || tenant != "tenant-a" || node != 42 {
		t.Fatalf("ParseURI = (%q,%d,%v), want (tenant-a,42,true)", tenant, node, ok)
	}
	for _, bad := range []string{"https://tenant-a/node/42", "srou://tenant-a/nope", "srou://tenant-a/node/70000"} {
		u, err := url.Parse(bad)
		if err != nil {
			continue
		}
		if _, _, ok := ParseURI(u); ok {
			t.Fatalf("ParseURI accepted %q", bad)
		}
	}
}

func TestParseRef(t *testing.T) {
	ok := []struct {
		uri  string
		want Ref
	}{
		{"srou://tenant-a/node/42", Ref{Tenant: "tenant-a", Role: RoleNode, NodeID: 42}},
		{"srou://tenant-a/rr/rr1", Ref{Tenant: "tenant-a", Role: RoleRR, RRID: "rr1"}},
		{"srou://tenant-a/rr/RR-A_2.5", Ref{Tenant: "tenant-a", Role: RoleRR, RRID: "RR-A_2.5"}},
	}
	for _, tc := range ok {
		u, err := url.Parse(tc.uri)
		if err != nil {
			t.Fatal(err)
		}
		got, valid := ParseRef(u)
		if !valid || got != tc.want {
			t.Fatalf("ParseRef(%q) = (%+v,%v), want (%+v,true)", tc.uri, got, valid, tc.want)
		}
		if got.String() != tc.uri {
			t.Fatalf("Ref.String() = %q, want %q", got.String(), tc.uri)
		}
	}

	bad := []string{
		"https://tenant-a/node/42",
		"",
		"srou://tenant-a/nope",
		"srou://tenant-a/node/70000",
		"srou://tenant-a/node/",
		"srou://tenant-a/node/4x",
		"srou://tenant-a/node/42/extra",
		"srou://tenant-a/rr/",
		"srou://tenant-a/rr/a/b",
		"srou://tenant-a/rr/has space",
		"srou:///rr/rr1",
	}
	for _, s := range bad {
		u, err := url.Parse(s)
		if err != nil {
			continue
		}
		if ref, valid := ParseRef(u); valid {
			t.Fatalf("ParseRef accepted %q as %+v", s, ref)
		}
	}
}

func TestRefOfDistinguishesRoles(t *testing.T) {
	ca, caKey := newCA(t)
	pemStr, _ := newDeviceCert(t, ca, caKey, "tenant-a", 7)
	cert := mustCert(t, pemStr)
	ref, err := RefOf(cert)
	if err != nil || ref != (Ref{Tenant: "tenant-a", Role: RoleNode, NodeID: 7}) {
		t.Fatalf("RefOf(device) = (%+v,%v)", ref, err)
	}

	rrURI, err := url.Parse("srou://tenant-a/rr/rr2")
	if err != nil {
		t.Fatal(err)
	}
	cert.URIs = []*url.URL{rrURI}
	ref, err = RefOf(cert)
	if err != nil || ref != (Ref{Tenant: "tenant-a", Role: RoleRR, RRID: "rr2"}) {
		t.Fatalf("RefOf(rr) = (%+v,%v)", ref, err)
	}

	// A certificate with no srou URI is nobody: RefOf must not guess a role.
	cert.URIs = nil
	if _, err := RefOf(cert); err == nil {
		t.Fatal("RefOf accepted a certificate with no identity URI")
	}
}

// ------------------------------------------------------------------ helpers

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func uriFor(tenant string, node uint16) string {
	return fmt.Sprintf("srou://%s/node/%d", tenant, node)
}

func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func mustCert(t *testing.T, pemStr string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		t.Fatal("no CERTIFICATE block in PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func newCA(t *testing.T) (*x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "srou-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, priv
}

func newDeviceCert(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, tenant string, node uint16) (string, ed25519.PrivateKey) {
	t.Helper()
	return deviceCert(t, ca, caKey, tenant, node, time.Now().Add(-time.Hour), time.Now().AddDate(1, 0, 0))
}

func newExpiredDeviceCert(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, tenant string, node uint16) (string, ed25519.PrivateKey) {
	t.Helper()
	return deviceCert(t, ca, caKey, tenant, node, time.Now().AddDate(0, -2, 0), time.Now().Add(-time.Hour))
}

func deviceCert(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, tenant string, node uint16, notBefore, notAfter time.Time) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(uriFor(tenant, node))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(int64(node) + 10),
		Subject:      pkix.Name{CommonName: tenant + "-node"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip through PEM, which is what actually travels on the wire.
	return pemCert(der), priv
}
