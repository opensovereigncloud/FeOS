// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package podid

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// The formats as ADR 0019 and the schema's checks write them.
var (
	shortRE  = regexp.MustCompile(`^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$`)
	legacyRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func TestMint(t *testing.T) {
	seen := map[ID]bool{}
	first := map[byte]bool{}
	chars := map[byte]bool{}
	for range 20000 {
		id := Mint()
		if !shortRE.MatchString(string(id)) {
			t.Fatalf("Mint() = %q, not a short ID", id)
		}
		if id.IsLegacy() || !Valid(string(id)) || len(id.TAP()) > 15 {
			t.Fatalf("Mint() = %q: legacy %v, valid %v, TAP %q", id, id.IsLegacy(), Valid(string(id)), id.TAP())
		}
		if seen[id] {
			t.Fatalf("Mint() drew %q twice", id)
		}
		seen[id] = true
		first[id[0]] = true
		for i := 1; i < Len; i++ {
			chars[id[i]] = true
		}
	}
	if len(first) != len(letters) || len(chars) != len(Alphabet) {
		t.Fatalf("Mint() used %d first characters, %d others; want %d and %d", len(first), len(chars), len(letters), len(Alphabet))
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		legacy   bool
	}{
		{"abcdefghjkm", "abcdefghjkm", false},
		{"z0123456789", "z0123456789", false},
		{"0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3", "0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3", true},
		{"0190A7C4-1E2F-7A3B-9C4D-5E6F708192A3", "0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3", true},
	} {
		id, err := Parse(tc.in)
		if err != nil || string(id) != tc.want || id.IsLegacy() != tc.legacy {
			t.Errorf("Parse(%q) = %q, %v (legacy %v); want %q (legacy %v)", tc.in, id, err, id.IsLegacy(), tc.want, tc.legacy)
		}
		if !shortRE.MatchString(string(id)) && !legacyRE.MatchString(string(id)) {
			t.Errorf("Parse(%q) = %q matches neither format", tc.in, id)
		}
	}
	for _, in := range []string{
		"", "abc", "0bcdefghjkm", // a digit first
		"abcdefghjki", "abcdefghjkl", "abcdefghjko", "abcdefghjku", // not Crockford
		"Abcdefghjkm", "abcdefghjk-", "abcdefghjkmn", "abcdefghjk",
		"0190a7c4-1e2f-7a3b-9c4d-5e6f708192ag", "0190a7c41e2f7a3b9c4d5e6f708192a3",
		"{0190a7c4-1e2f-7a3b-9c4d-5e6f708192a}", "0190a7c4_1e2f-7a3b-9c4d-5e6f708192a3",
		"urn:uuid:0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3",
	} {
		if id, err := Parse(in); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %q, %v; want ErrInvalid", in, id, err)
		}
		if Valid(in) {
			t.Errorf("Valid(%q) = true", in)
		}
	}
	if Valid("0190A7C4-1E2F-7A3B-9C4D-5E6F708192A3") {
		t.Error("Valid accepts an uppercase UUID, which is not canonical")
	}
}

func TestTAP(t *testing.T) {
	short := ID("abcdefghjkm")
	if short.TAPKey() != "abcdefghjkm" || short.TAP() != "pod-abcdefghjkm" {
		t.Errorf("short: %q %q", short.TAPKey(), short.TAP())
	}
	a := ID("0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3")
	b := ID("0190a7c4-ffff-7a3b-9c4d-5e6f708192a3")
	c := ID("0190a7c5-1e2f-7a3b-9c4d-5e6f708192a3")
	if a.TAP() != "pod-0190a7c4" {
		t.Errorf("legacy TAP %q", a.TAP())
	}
	for _, tc := range []struct {
		x, y ID
		want bool
	}{
		{a, b, true}, {b, a, true}, {a, a, false}, {a, c, false},
		{short, short, false}, {short, ID("abcdefghjkn"), false},
		// A short ID never shares a UUID's TAP name, also when they start alike.
		{ID("0190a7c4zzz"), a, false}, {a, ID("0190a7c4zzz"), false},
	} {
		if got := TAPConflict(tc.x, tc.y); got != tc.want {
			t.Errorf("TAPConflict(%s, %s) = %v", tc.x, tc.y, got)
		}
	}
}

func TestText(t *testing.T) {
	var v struct {
		ID ID `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id":"0190A7C4-1E2F-7A3B-9C4D-5E6F708192A3"}`), &v); err != nil || v.ID != "0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3" {
		t.Fatalf("unmarshal: %q, %v", v.ID, err)
	}
	if err := json.Unmarshal([]byte(`{"id":"nope"}`), &v); err == nil {
		t.Fatal("unmarshal accepted an invalid ID")
	}
	if err := json.Unmarshal([]byte(`{"id":""}`), &v); err != nil || !v.ID.IsZero() {
		t.Fatalf("unmarshal of \"\": %q, %v", v.ID, err)
	}
	v.ID = "abcdefghjkm"
	b, err := json.Marshal(v)
	if err != nil || !strings.Contains(string(b), `"abcdefghjkm"`) {
		t.Fatalf("marshal: %s, %v", b, err)
	}
}
