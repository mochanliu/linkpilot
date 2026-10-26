package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Console account passwords are stored as PBKDF2-HMAC-SHA256, encoded as
//
//	pbkdf2$sha256$<iterations>$<salt-b64>$<hash-b64>
//
// The KDF is implemented here rather than pulled from golang.org/x/crypto: the
// controller's zero-dependency property is asserted by the acceptance harness,
// and PBKDF2 is a short, well-specified construction on top of HMAC.
//
// MFA is not implemented. Issuing a session is a separate step from verifying
// credentials on purpose: when a second factor arrives it becomes another
// check between verifyPassword and the session insert, not a rewrite of login.
const (
	passwordIterations = 210000
	passwordSaltLen    = 16
	passwordKeyLen     = 32
	passwordMinLen     = 10
	passwordMaxLen     = 128
)

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	h := hmac.New(sha256.New, password)
	hashLen := h.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	idx := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		h.Reset()
		h.Write(salt)
		idx[0] = byte(block >> 24)
		idx[1] = byte(block >> 16)
		idx[2] = byte(block >> 8)
		idx[3] = byte(block)
		h.Write(idx)

		u := h.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			h.Reset()
			h.Write(u)
			u = h.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(pw string) (string, error) {
	if err := checkPasswordPolicy(pw); err != nil {
		return "", err
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(pw), salt, passwordIterations, passwordKeyLen)
	return encodePassword(passwordIterations, salt, sum), nil
}

func encodePassword(iter int, salt, sum []byte) string {
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s", iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum))
}

// verifyPassword reports whether pw matches encoded. A malformed encoding is a
// "no", not an error: a corrupted row must fail closed.
func verifyPassword(encoded, pw string) bool {
	iter, salt, want, ok := decodePassword(encoded)
	if !ok {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func decodePassword(encoded string) (iter int, salt, sum []byte, ok bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return 0, nil, nil, false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return 0, nil, nil, false
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) == 0 {
		return 0, nil, nil, false
	}
	sum, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(sum) == 0 {
		return 0, nil, nil, false
	}
	return iter, salt, sum, true
}

func checkPasswordPolicy(pw string) error {
	if len(pw) < passwordMinLen {
		return fmt.Errorf("password must be at least %d characters", passwordMinLen)
	}
	if len(pw) > passwordMaxLen {
		return errors.New("password too long")
	}
	return nil
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// burnPasswordWork keeps an unknown username from being distinguishable by
// response time: a login for a user that does not exist costs the same KDF run
// as one that does.
func burnPasswordWork(pw string) {
	dummyHashOnce.Do(func() {
		salt := make([]byte, passwordSaltLen)
		sum := pbkdf2SHA256([]byte("not-a-real-password"), salt, passwordIterations, passwordKeyLen)
		dummyHash = encodePassword(passwordIterations, salt, sum)
	})
	verifyPassword(dummyHash, pw)
}

// newPassword is the one-time initial password an operator hands to a customer.
// Same alphabet as claim codes for the same reason: it is read aloud or typed
// from a screen, and 0/O/1/l confusion turns into a support call.
func newPassword() string {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("console: entropy unavailable")
	}
	out := make([]byte, 0, len(buf)+3)
	for i, b := range buf {
		if i == 4 || i == 8 || i == 12 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	return string(out)
}
