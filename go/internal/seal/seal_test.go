// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package seal

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	s, err := New(key(t))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("env_secrets|e|name|1")
	blob, err := s.Seal([]byte("hunter2"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if blob[0] != Version || len(blob) != 1+12+len("hunter2")+16 || bytes.Contains(blob, []byte("hunter2")) {
		t.Fatalf("blob % x", blob)
	}
	pt, err := s.Open(blob, aad)
	if err != nil || string(pt) != "hunter2" {
		t.Fatalf("open: %q %v", pt, err)
	}
	// A new nonce every time.
	again, _ := s.Seal([]byte("hunter2"), aad)
	if bytes.Equal(again, blob) {
		t.Fatal("two seals are equal")
	}
	// An empty plaintext seals too.
	empty, err := s.Seal(nil, aad)
	if pt, err2 := s.Open(empty, aad); err != nil || err2 != nil || len(pt) != 0 {
		t.Fatalf("empty: %q %v %v", pt, err, err2)
	}
}

func TestOpenRefuses(t *testing.T) {
	s, _ := New(key(t))
	aad := []byte("row 1")
	blob, _ := s.Seal([]byte("secret"), aad)
	if _, err := s.Open(blob, []byte("row 2")); err == nil || !strings.Contains(err.Error(), "does not open") {
		t.Fatalf("another row's additional data: %v", err)
	}
	other, _ := New(key(t))
	if _, err := other.Open(blob, aad); err == nil || !strings.Contains(err.Error(), "--secret-key-file") {
		t.Fatalf("another key: %v", err)
	}
	flipped := bytes.Clone(blob)
	flipped[len(flipped)-1] ^= 1
	if _, err := s.Open(flipped, aad); err == nil {
		t.Fatal("a changed blob opened")
	}
	for _, bad := range [][]byte{nil, {Version}, bytes.Repeat([]byte{Version}, 12), append([]byte{2}, blob[1:]...)} {
		if _, err := s.Open(bad, aad); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("malformed % x: %v", bad, err)
		}
	}
}

func TestNewKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil || !strings.Contains(err.Error(), "want 32") {
			t.Fatalf("a key of %d bytes: %v", n, err)
		}
	}
}
