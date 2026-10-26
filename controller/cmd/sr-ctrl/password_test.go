package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

// PBKDF2-HMAC-SHA256 vectors, cross-checked against Python's hashlib. The
// 40-byte case is the one that catches a broken block counter: it needs two
// HMAC blocks, the others only need one.
func TestPBKDF2SHA256Vectors(t *testing.T) {
	cases := []struct {
		pw, salt string
		iter     int
		keyLen   int
		wantHex  string
	}{
		{"password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, tc := range cases {
		got := hex.EncodeToString(pbkdf2SHA256([]byte(tc.pw), []byte(tc.salt), tc.iter, tc.keyLen))
		if got != tc.wantHex {
			t.Errorf("pbkdf2(%q, %q, %d, %d) = %s, want %s",
				tc.pw, tc.salt, tc.iter, tc.keyLen, got, tc.wantHex)
		}
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	const pw = "correct-horse-9"
	encoded, err := hashPassword(pw)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}

	iter, salt, sum, ok := decodePassword(encoded)
	if !ok {
		t.Fatalf("decodePassword rejected our own encoding: %q", encoded)
	}
	if iter != passwordIterations {
		t.Errorf("iterations = %d, want %d", iter, passwordIterations)
	}
	if len(salt) != passwordSaltLen || len(sum) != passwordKeyLen {
		t.Errorf("salt/key = %d/%d bytes, want %d/%d", len(salt), len(sum), passwordSaltLen, passwordKeyLen)
	}
	if !verifyPassword(encoded, pw) {
		t.Error("verifyPassword rejected the correct password")
	}
	if verifyPassword(encoded, "correct-horse-8") {
		t.Error("verifyPassword accepted a wrong password")
	}

	// A fresh salt per hash: two accounts with the same password must not share
	// a row, and neither may leak the fact that they match.
	again, err := hashPassword(pw)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if again == encoded {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	if !verifyPassword(again, pw) || !verifyPassword(encoded, pw) {
		t.Error("hash with a second salt does not verify")
	}
}

func TestPasswordPolicy(t *testing.T) {
	short := strings.Repeat("a", passwordMinLen-1)
	if err := checkPasswordPolicy(short); err == nil {
		t.Errorf("accepted a %d-character password", len(short))
	}
	long := strings.Repeat("a", passwordMaxLen+1)
	if err := checkPasswordPolicy(long); err == nil {
		t.Errorf("accepted a %d-character password", len(long))
	}
	for _, pw := range []string{strings.Repeat("a", passwordMinLen), strings.Repeat("a", passwordMaxLen)} {
		if err := checkPasswordPolicy(pw); err != nil {
			t.Errorf("rejected a %d-character password: %v", len(pw), err)
		}
	}
	if _, err := hashPassword(short); err == nil {
		t.Error("hashPassword hashed a password that violates the policy")
	}
}

// A corrupted row must authenticate nobody. Every malformed shape returns false
// rather than an error or, worse, a panic.
func TestVerifyPasswordFailsClosed(t *testing.T) {
	bad := []string{
		"",
		"plaintext",
		"pbkdf2$sha1$210000$AAAA$AAAA",
		"bcrypt$sha256$210000$AAAA$AAAA",
		"pbkdf2$sha256$abc$AAAA$AAAA",
		"pbkdf2$sha256$0$AAAA$AAAA",
		"pbkdf2$sha256$10000001$AAAA$AAAA",
		"pbkdf2$sha256$210000$!!!$AAAA",
		"pbkdf2$sha256$210000$$AAAA",
		"pbkdf2$sha256$210000$AAAA$",
		"pbkdf2$sha256$210000$AAAA",
		"pbkdf2$sha256$210000$AAAA$AAAA$extra",
	}
	for _, enc := range bad {
		if _, _, _, ok := decodePassword(enc); ok {
			t.Errorf("decodePassword accepted %q", enc)
		}
		if verifyPassword(enc, "anything-at-all") {
			t.Errorf("verifyPassword accepted a malformed encoding %q", enc)
		}
	}
}

// The unknown-username path must cost a real KDF run, not a fast branch that a
// remote caller can time. Correctness here is "the dummy hash is a genuine
// hash of a password nobody knows", which is what makes the work comparable.
func TestBurnPasswordWorkUsesARealHash(t *testing.T) {
	burnPasswordWork("whatever-the-attacker-tried")
	if dummyHash == "" {
		t.Fatal("burnPasswordWork did not initialize the dummy hash")
	}
	if verifyPassword(dummyHash, "whatever-the-attacker-tried") {
		t.Error("the dummy hash matches an arbitrary password; unknown-user path is not burning a KDF")
	}
	if !verifyPassword(dummyHash, "not-a-real-password") {
		t.Error("dummy hash is not a hash of the fixed filler password")
	}
	burnPasswordWork("second-call") // must be idempotent, not a panic
}

func TestNewPasswordShape(t *testing.T) {
	const alphabet = "23456789BCDFGHJKLMNPQRSTVWXZ"
	pw := newPassword()
	if len(pw) != 19 {
		t.Errorf("length = %d, want 19 (16 chars in 4 groups)", len(pw))
	}
	for i, r := range pw {
		if i == 4 || i == 9 || i == 14 {
			if r != '-' {
				t.Errorf("position %d = %q, want a dash", i, r)
			}
			continue
		}
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("position %d = %q, outside the unambiguous alphabet", i, r)
		}
	}
	if err := checkPasswordPolicy(pw); err != nil {
		t.Errorf("generated password violates policy: %v", err)
	}
	if newPassword() == pw {
		t.Error("two generated passwords are identical; entropy source is not random")
	}
}
