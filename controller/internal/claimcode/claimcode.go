// Package claimcode gives the enrollment claim code a self-describing envelope.
//
// The classic code is an opaque secret ("srk_" + 32 hand-typeable characters).
// It authorizes enrollment, but the device still needs two more manual inputs
// on its serial console: the controller address (-ctrl) and the pinned CA
// bundle (-ca). On a rack box with no network setup that is exactly the
// file-copying pain provisioning wants to avoid.
//
// The envelope fixes that by wrapping the same secret in a pasteable string
// that ALSO carries the controller endpoints and the CA PEM:
//
//	srs1-<base64url(JSON)>
//
// with JSON {"code": "srk_...", "ctrl": ["host:port"...], "ca": "<PEM>"}.
// Security does not move: the envelope is what the customer pastes, but only
// the inner secret is hashed, stored and checked -- the CA and the addresses
// are public facts. TTL, revocation and seat accounting are untouched; they
// key off the grant record, not off the presented string's shape.
//
// Compatibility: a code without the prefix is treated as the classic secret
// unchanged, so codes minted before this package existed keep working. A
// prefixed but malformed string is an error, not a fallback -- a typo in an
// envelope must fail loudly rather than silently enroll as a different grant.
package claimcode

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Prefix marks a self-describing claim code. It is chosen not to collide with
// the classic "srk_" secret.
const Prefix = "srs1-"

// Payload is what an envelope carries beside the secret. CA is the controller
// CA bundle in PEM (public); Ctrl lists the device-facing endpoints in the
// same host:port shape -ctrl takes.
type Payload struct {
	Code string   `json:"code"`
	Ctrl []string `json:"ctrl,omitempty"`
	CA   string   `json:"ca,omitempty"`
}

// ErrMalformed says the string claimed to be an envelope but is not one.
var ErrMalformed = errors.New("claimcode: malformed envelope")

// Wrap builds the pasteable string. The secret is the only confidential part;
// everything else rides along for the device's benefit.
func Wrap(secret, caPEM string, ctrls []string) string {
	p := Payload{Code: secret, Ctrl: ctrls, CA: caPEM}
	if len(p.Ctrl) == 0 {
		p.Ctrl = nil
	}
	if p.CA == "" {
		p.CA = ""
	}
	blob, err := json.Marshal(p)
	if err != nil {
		// Fields are strings and string slices; marshalling cannot fail. A
		// panic here would be a programming error worth seeing immediately.
		panic(fmt.Sprintf("claimcode: marshal: %v", err))
	}
	var b strings.Builder
	b.WriteString(Prefix)
	enc := base64.RawURLEncoding
	b.WriteString(enc.EncodeToString(blob))
	return b.String()
}

// Parse splits a presented code into the secret to hash and, when the string
// was an envelope, the payload the device should use. A plain "srk_" code
// returns (code, nil, nil). An envelope with a bad body is ErrMalformed.
func Parse(presented string) (secret string, p *Payload, err error) {
	s := strings.TrimSpace(presented)
	if s == "" {
		return "", nil, nil
	}
	if !strings.HasPrefix(s, Prefix) {
		return s, nil, nil
	}
	raw, derr := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if derr != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrMalformed, derr)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out Payload
	if uerr := dec.Decode(&out); uerr != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrMalformed, uerr)
	}
	if strings.TrimSpace(out.Code) == "" {
		return "", nil, ErrMalformed
	}
	return out.Code, &out, nil
}

// Hash returns the digest stored for a presented code. Only the inner secret
// is hashed: the envelope's CA and endpoint list are public, and hashing them
// would make the stored digest depend on values the mint side may legitimately
// re-derive differently (an added endpoint, a re-issued CA bundle).
func Hash(presented string) (string, error) {
	secret, _, err := Parse(presented)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("%x", sum), nil
}
