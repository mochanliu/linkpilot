package claimcode

import (
	"errors"
	"strings"
	"testing"
)

const testCA = "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"

func TestPlainCodePassesThroughUnchanged(t *testing.T) {
	// A classic "srk_" code must survive round-trip untouched: codes minted
	// before the envelope existed keep working with zero migration.
	in := "srk_BCDFGHJKLMNPQRSTVWXZ23456789BCDFGHJ"
	secret, p, err := Parse(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if secret != in || p != nil {
		t.Fatalf("plain code mutated: secret=%q payload=%v", secret, p)
	}
	h, err := Hash(in)
	if err != nil || len(h) != 64 {
		t.Fatalf("hash = %q, %v", h, err)
	}
}

func TestEnvelopeCarriesSecretEndpointsAndCA(t *testing.T) {
	in := Wrap("srk_SECRET", testCA, []string{"192.168.1.100:8444", "10.0.0.9:8444"})
	if !strings.HasPrefix(in, Prefix) {
		t.Fatalf("envelope lacks prefix: %q", in[:min(20, len(in))])
	}
	secret, p, err := Parse(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if secret != "srk_SECRET" {
		t.Fatalf("secret = %q", secret)
	}
	if p == nil || len(p.Ctrl) != 2 || p.Ctrl[0] != "192.168.1.100:8444" || p.CA != testCA {
		t.Fatalf("payload = %+v", p)
	}
	// Paste-safe: no whitespace, nothing a terminal mangles.
	if strings.ContainsAny(in, " \n\r\t") {
		t.Fatal("envelope contains whitespace")
	}
}

func TestHashIgnoresEnvelopeButMatchesPlainSecret(t *testing.T) {
	// The stored digest is over the inner secret only, so a code re-wrapped
	// with a different endpoint list still matches the same grant.
	hPlain, err := Hash("srk_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	hWrapped, err := Hash(Wrap("srk_SECRET", "", []string{"x:1"}))
	if err != nil {
		t.Fatal(err)
	}
	if hPlain != hWrapped {
		t.Fatalf("hash depends on the envelope, not the secret: %s != %s", hPlain, hWrapped)
	}
}

func TestMalformedEnvelopeFailsLoudly(t *testing.T) {
	cases := map[string]string{
		"bad base64":  Prefix + "!!!not-base64!!!",
		"bad json":    Prefix + "e30",
		"empty code":  Prefix + "eyJjb2RlIjoiICJ9", // {"code":" "}
		"prefix only": Prefix,
	}
	for name, in := range cases {
		_, _, err := Parse(in)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
		if _, err := Hash(in); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: hash err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestEmptyInputIsNotAnEnvelope(t *testing.T) {
	secret, p, err := Parse("   ")
	if err != nil || secret != "" || p != nil {
		t.Fatalf("empty input = %q, %v, %v", secret, p, err)
	}
}

func TestWrapSurvivesPasteThroughShellQuotes(t *testing.T) {
	// The string is pasted into a serial console and often through ssh. It must
	// survive single- and double-quoting unchanged (no quote chars of its own).
	in := Wrap("srk_Q", testCA, []string{"h:1"})
	if strings.ContainsAny(in, "\"'`$\\") {
		t.Fatalf("envelope contains shell-hostile characters: %q", in[:40])
	}
	secret, _, err := Parse(in)
	if err != nil || secret != "srk_Q" {
		t.Fatalf("round trip: %q, %v", secret, err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
