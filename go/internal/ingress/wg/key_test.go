// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func hexKey(t *testing.T, s string) Key {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != KeyLen {
		t.Fatalf("bad key %q: %v", s, err)
	}
	return Key(b)
}

// TestPublicKeyRFC7748 checks the public keys of Alice and Bob in RFC 7748,
// section 6.1.
func TestPublicKeyRFC7748(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, priv, pub string }{
		{"Alice", "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a",
			"8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"},
		{"Bob", "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb",
			"de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f"},
	} {
		if got := hexKey(t, c.priv).PublicKey(); got != hexKey(t, c.pub) {
			t.Errorf("%s: public key %x, want %s", c.name, got, c.pub)
		}
	}
}

func TestGenerateKey(t *testing.T) {
	t.Parallel()
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a.IsZero() {
		t.Fatalf("keys %s and %s", a, b)
	}
	if a[0]&7 != 0 || a[31]&0xc0 != 0x40 {
		t.Fatalf("key %x is not clamped", a)
	}
	if a.PublicKey() == a || a.PublicKey().IsZero() {
		t.Fatalf("public key %s of %s", a.PublicKey(), a)
	}
}

func TestParseKey(t *testing.T) {
	t.Parallel()
	k := hexKey(t, "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	s := k.String()
	if s != base64.StdEncoding.EncodeToString(k[:]) || len(s) != 44 {
		t.Fatalf("String: %q", s)
	}
	got, err := ParseKey(s)
	if err != nil || got != k {
		t.Fatalf("ParseKey(%q) = %x, %v", s, got, err)
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(k[:31]),
		base64.StdEncoding.EncodeToString(append(k[:], 0)), s[:43] + "B"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) took it", bad)
		}
	}
	if !(Key{}).IsZero() {
		t.Fatal("the zero key is not zero")
	}
}
