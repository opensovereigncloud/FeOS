// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package routing

import "testing"

func TestMatch(t *testing.T) {
	prefixes := []string{"/", "/api", "/api/v2/", "/a b"}
	for _, c := range []struct {
		raw  string
		want string
		n    int
	}{
		{"/", "/", 0}, {"/api", "/api", 1}, {"/api/", "/api", 1}, {"/apis", "/", 0}, {"/api/v2", "/api/v2/", 2},
		{"/api/v2/x", "/api/v2/", 2}, {"/api/v3", "/api", 1}, {"/a%20b/c", "/a b", 1}, {"/API", "/", 0},
	} {
		i, n, ok := Match(prefixes, c.raw)
		if !ok || prefixes[i] != c.want || n != c.n {
			t.Errorf("%s: %d %d %v, want %s %d", c.raw, i, n, ok, c.want, c.n)
		}
	}
	if _, _, ok := Match([]string{"/api"}, "/web"); ok {
		t.Error("matched without a root")
	}
	if _, _, ok := Match(nil, "/"); ok {
		t.Error("matched without prefixes")
	}
	// A segment that does not unescape matches nothing but the root.
	if i, n, ok := Match([]string{"/", "/a%zz"}, "/a%zz/b"); !ok || i != 0 || n != 0 {
		t.Errorf("bad escape: %d %d %v", i, n, ok)
	}
	// Of two prefixes of one length, the first wins.
	if i, _, ok := Match([]string{"/x/", "/x"}, "/x/y"); !ok || i != 0 {
		t.Errorf("tie: %d %v", i, ok)
	}
}

func TestStripSegments(t *testing.T) {
	for _, c := range []struct {
		raw, path, rawPath string
		n                  int
	}{
		{"/api/x", "/x", "", 1}, {"/api", "/", "", 1}, {"/api/a%2Fb", "/a/b", "/a%2Fb", 1}, {"/a/b/c", "/a/b/c", "", 0},
		{"/a/b%20c", "/b c", "", 1}, {"/a/b%41", "/bA", "/b%41", 1},
	} {
		p, rp, err := StripSegments(c.raw, c.n)
		if err != nil || p != c.path || rp != c.rawPath {
			t.Errorf("strip %s %d: %q %q %v", c.raw, c.n, p, rp, err)
		}
	}
	if _, _, err := StripSegments("/a/%zz", 1); err == nil {
		t.Error("stripped a path that does not unescape")
	}
}

func TestRawOf(t *testing.T) {
	for _, c := range []struct{ path, raw, want string }{
		{"/a/b", "/a/b", ""}, {"/a b", "/a%20b", ""}, {"/a/b", "/a%2Fb", "/a%2Fb"}, {"/A", "/%41", "/%41"},
	} {
		if got := RawOf(c.path, c.raw); got != c.want {
			t.Errorf("raw of %q %q: %q, want %q", c.path, c.raw, got, c.want)
		}
	}
}

// Only a segment that is, or decodes once or twice to, . or .. is
// refused; a % that starts no escape after a decode is a character.
func TestDotSegment(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"/a/../b", ".."}, {"/a/%2e%2e/b", ".."}, {"/a/%252e%252e/b", ".."}, {"/a%2F..%2Fb", ".."},
		{"/%252e%252e/x%25", ".."}, {"/%252e%2e/x", ".."}, {"/a/%2e/b", "."}, {"/a/%252E", "."},
		{"/a/..b/c", ""}, {"/a/b", ""}, {"/q/100%25", ""}, {"/search/50%25off", ""}, {"/a%2541", ""},
		{"/a/%25252e%25252e/b", ""}, {"/%2e%2e%zz", ""}, {"/files/a%25zz/..x", ""},
	} {
		if got := DotSegment(c.raw); got != c.want {
			t.Errorf("dot segment of %s: %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestLenientUnescape(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/a", "/a"}, {"/%41%62", "/Ab"}, {"/100%", "/100%"}, {"/%zz%4", "/%zz%4"}, {"/%2541", "/%41"},
	} {
		if got := LenientUnescape(c.in); got != c.want {
			t.Errorf("unescape %q: %q, want %q", c.in, got, c.want)
		}
	}
	for c, v := range map[byte]byte{'0': 0, '9': 9, 'a': 10, 'f': 15, 'A': 10, 'F': 15} {
		if !IsHex(c) || Unhex(c) != v {
			t.Errorf("hex %c: %v %d", c, IsHex(c), Unhex(c))
		}
	}
	for _, c := range []byte{'g', 'G', '/', '%', ' '} {
		if IsHex(c) {
			t.Errorf("%c is hex", c)
		}
	}
}
