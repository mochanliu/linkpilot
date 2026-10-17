// Package keybinding attests a node's X25519 (key-agreement) public key with
// the Ed25519 identity key inside its controller-issued certificate.
//
// The problem it solves. The controller is a MEDIATOR for each node's
// WireGuard STATIC public key: it carries that key-agreement material to the
// other side, where the peer installs it verbatim as a wireguard peer, and it
// must never be able to substitute it. Without an attestation, "the controller
// handed me B's X25519 public key" is an unverifiable claim, and a controller
// that swaps in a key it owns sits in the middle of every payload.
//
// With the attestation the key material is ATTRIBUTABLE: verifying a binding
// proves that the named device signed this exact public key, and only that
// device holds the identity key. The controller can relay a binding or drop it.
// It cannot forge one.
//
// Scope, stated honestly. This closes "swap the key in transit" — the cheap
// attack, and the one a lying relay would actually attempt. It does NOT by
// itself stop a controller that also holds the tenant CA *private* key from
// minting a whole new identity for a node, because then the attacker is the
// identity authority. Closing that requires the identity root to leave the
// online controller (offline signing / HSM) or per-peer identity pinning; both
// are open items 1 and 2 in doc 25 §10, and the caveat itself is written up in
// doc 25 §5.2. Saying "the controller cannot MITM"
// without this caveat would be a claim we have not tested.
package keybinding

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// DomainSeparator is the fixed prefix of every binding message. It keeps a
// signature made here from being valid in any other context: a signature is
// only as narrow as the message it covers, and an unprefixed message invites
// one protocol's signature to authorise another's action.
const DomainSeparator = "srou-keybinding-v1"

const (
	// X25519PubLen is the size of an X25519 public key.
	X25519PubLen = 32
	// SigLen is the size of an Ed25519 signature.
	SigLen = ed25519.SignatureSize
	// maxTenantLen bounds the length-prefixed tenant field (uint16).
	maxTenantLen = 0xFFFF
)

// Binding is what actually travels over the wire and through the controller.
//
// KeyGen is a per-device counter that the device bumps whenever it replaces its
// X25519 key. Without it, an old binding replayed after a rotation would be
// indistinguishable from the current one. The tenant's crypto epoch is NOT part
// of the binding: an epoch bump must not invalidate every signature in the
// tenant (that would be a re-signing storm), it only re-derives the O-key.
type Binding struct {
	X25519Pub string `json:"x25519_pub"` // base64, X25519PubLen bytes
	KeyGen    uint32 `json:"key_gen"`
	Sig       string `json:"sig"` // base64, SigLen bytes
}

// Signer is the IDENTITY-KEY SEAM (doc 24 §7.2).
//
// This interface exists so that "sign with the device identity key" is one
// implementation detail rather than a call scattered through the code. Phase 2
// implements it with a PKCS#8 Ed25519 key on disk; a secure element (the
// secureid-style part) later implements the same two methods and nothing above
// this line changes. Pinning the seam now is what makes that a hardening step
// instead of a refactor.
type Signer interface {
	Sign(msg []byte) ([]byte, error)
	Public() ed25519.PublicKey
}

// SoftwareSigner is the phase-2 Signer: an Ed25519 private key in memory,
// loaded from the device's identity file.
type SoftwareSigner struct{ priv ed25519.PrivateKey }

// NewSoftwareSigner loads the identity key from PKCS#8 DER (which is the form
// the agent persists, and what crypto/tls parses back).
func NewSoftwareSigner(pkcs8 []byte) (*SoftwareSigner, error) {
	k, err := x509.ParsePKCS8PrivateKey(pkcs8)
	if err != nil {
		return nil, fmt.Errorf("identity key: %w", err)
	}
	ed, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity key is %T, want ed25519", k)
	}
	return &SoftwareSigner{priv: ed}, nil
}

func (s *SoftwareSigner) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, msg), nil
}

func (s *SoftwareSigner) Public() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// Message returns the exact bytes a binding signature covers.
//
// Layout (integers big-endian):
//
//	"srou-keybinding-v1"   17 bytes, fixed domain separator
//	uint16 len(tenant)
//	tenant                 len(tenant) bytes
//	uint16 node_id
//	uint32 key_gen
//	[32]byte x25519_pub
//
// Every field is either length-prefixed or fixed-width, which makes the
// encoding self-delimiting.
//
// On why the prefix is there even though it is not yet load-bearing: with a
// single variable-length field followed by fixed-width ones, a bare
// concatenation happens to be unambiguous today. It stops being so the moment
// two variable-length fields sit next to each other -- ("a","bc") and ("ab","c")
// concatenate to identical bytes. TestWhyFieldsAreLengthPrefixed builds exactly
// that failure. The rule is applied now so that a later revision cannot
// reintroduce it by appending a field.
//
// This is the same class of failure as the byte-order iron rule in doc 18 §8:
// when this breaks, it breaks in the ENCODING, never in the maths.
func Message(tenant string, nodeID uint16, keyGen uint32, x25519Pub []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(DomainSeparator) + 2 + len(tenant) + 2 + 4 + len(x25519Pub))
	b.WriteString(DomainSeparator)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], uint16(len(tenant)))
	b.Write(u16[:])
	b.WriteString(tenant)
	binary.BigEndian.PutUint16(u16[:], nodeID)
	b.Write(u16[:])
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], keyGen)
	b.Write(u32[:])
	b.Write(x25519Pub)
	return b.Bytes()
}

// Sign produces the binding for (tenant, nodeID)'s X25519 public key.
func Sign(s Signer, tenant string, nodeID uint16, keyGen uint32, x25519Pub []byte) (Binding, error) {
	if len(x25519Pub) != X25519PubLen {
		return Binding{}, fmt.Errorf("x25519 public key must be %d bytes, got %d", X25519PubLen, len(x25519Pub))
	}
	if len(tenant) > maxTenantLen {
		return Binding{}, fmt.Errorf("tenant name too long (%d bytes)", len(tenant))
	}
	sig, err := s.Sign(Message(tenant, nodeID, keyGen, x25519Pub))
	if err != nil {
		return Binding{}, err
	}
	return Binding{
		X25519Pub: base64.StdEncoding.EncodeToString(x25519Pub),
		KeyGen:    keyGen,
		Sig:       base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// Decode splits the binding into raw bytes. It validates LENGTHS only — never
// trust it as authentication; Verify is what authenticates.
func (b Binding) Decode() (x25519Pub, sig []byte, err error) {
	x25519Pub, err = base64.StdEncoding.DecodeString(b.X25519Pub)
	if err != nil {
		return nil, nil, fmt.Errorf("binding.x25519_pub is not base64: %w", err)
	}
	if len(x25519Pub) != X25519PubLen {
		return nil, nil, fmt.Errorf("binding.x25519_pub must be %d bytes, got %d", X25519PubLen, len(x25519Pub))
	}
	sig, err = base64.StdEncoding.DecodeString(b.Sig)
	if err != nil {
		return nil, nil, fmt.Errorf("binding.sig is not base64: %w", err)
	}
	if len(sig) != SigLen {
		return nil, nil, fmt.Errorf("binding.sig must be %d bytes, got %d", SigLen, len(sig))
	}
	return x25519Pub, sig, nil
}

// Verify checks that `identity` signed this binding for exactly
// (tenant, nodeID), and returns the attested X25519 public key.
//
// Everything the check is worth comes from `identity` being trustworthy: the
// caller must obtain it from a certificate that chains to the pinned tenant CA,
// which is what VerifyPeerCert does. A verified signature under an attacker's
// own key proves nothing.
func (b Binding) Verify(tenant string, nodeID uint16, identity ed25519.PublicKey) ([]byte, error) {
	if len(identity) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity key must be %d bytes, got %d", ed25519.PublicKeySize, len(identity))
	}
	if len(tenant) > maxTenantLen {
		return nil, fmt.Errorf("tenant name too long (%d bytes)", len(tenant))
	}
	x25519Pub, sig, err := b.Decode()
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(identity, Message(tenant, nodeID, b.KeyGen, x25519Pub), sig) {
		// The message is rebuilt here from the VALUES we expect, not from
		// values we parsed out of the binding -- so a binding made for another
		// tenant, another node or another key generation simply does not
		// validate. Nothing is "adjusted" to make it fit.
		return nil, fmt.Errorf("binding signature does not cover (tenant=%q node=%d key_gen=%d)", tenant, nodeID, b.KeyGen)
	}
	return x25519Pub, nil
}

// IdentityOf extracts the (tenant, nodeID) a device certificate asserts.
//
// This is the ONLY place a peer's identity is read out of a certificate, on
// both the controller and the device side, so the two can never disagree about
// how identity is spelled.
func IdentityOf(cert *x509.Certificate) (tenant string, nodeID uint16, err error) {
	for _, u := range cert.URIs {
		if t, n, ok := ParseURI(u); ok {
			return t, n, nil
		}
	}
	return "", 0, fmt.Errorf("certificate carries no srou:// tenant URI")
}

// VerifyPeerCert is the relying-party check, in full: (1) the certificate
// chains to the pinned CA and is inside its validity window, (2) the
// certificate asserts the (tenant, nodeID) we asked about, and (3) the
// certificate's own key signed this binding.
//
// All three are required and they close different holes. (1) stops an unknown
// device. (2) stops a known device from vouching for a different identity.
// (3) stops the key from being swapped while the identity stays right. Drop (2)
// and the controller can hand us any enrolled device's certificate.
func VerifyPeerCert(pool *x509.CertPool, certPEM string, wantTenant string, wantNode uint16, b Binding) (*x509.Certificate, error) {
	blk, err := decodePEMCertificate(certPEM)
	if err != nil {
		return nil, err
	}
	cert := blk
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		// CurrentTime defaults to now: an expired device certificate must not
		// be able to keep a tunnel key alive.
		CurrentTime: time.Now(),
	}); err != nil {
		return nil, fmt.Errorf("peer certificate does not chain to the pinned CA: %w", err)
	}

	gotTenant, gotNode, err := IdentityOf(cert)
	if err != nil {
		return nil, err
	}
	if gotTenant != wantTenant || gotNode != wantNode {
		return nil, fmt.Errorf("peer certificate is for %s/node/%d, expected %s/node/%d",
			gotTenant, gotNode, wantTenant, wantNode)
	}

	id, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("peer certificate key is %T, want ed25519", cert.PublicKey)
	}
	if _, err := b.Verify(wantTenant, wantNode, id); err != nil {
		return nil, err
	}
	return cert, nil
}

func decodePEMCertificate(pemStr string) (*x509.Certificate, error) {
	blk, _ := pem.Decode([]byte(pemStr))
	if blk == nil {
		return nil, fmt.Errorf("no CERTIFICATE block in peer certificate")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse peer certificate: %w", err)
	}
	return cert, nil
}

// ParseURI reads the (tenant, nodeID) out of one certificate URI SAN. It is
// exported because the controller's mTLS enrollment path reads identity from
// the client certificate too, and the two must never spell identity differently.
func ParseURI(u *url.URL) (tenant string, nodeID uint16, ok bool) {
	if u == nil || u.Scheme != "srou" {
		return "", 0, false
	}
	var n int
	if _, e := fmt.Sscanf(strings.TrimPrefix(u.Path, "/node/"), "%d", &n); e != nil {
		return "", 0, false
	}
	if n < 0 || n > 0xFFFF {
		return "", 0, false
	}
	return u.Host, uint16(n), true
}
