// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"regexp"
	"strings"
	"testing"
)

// A claim's name is trimmed, lowercased and loses one trailing dot; it is
// a host name of LDH labels, at least two, ending in a top-level domain;
// a Unicode name is refused with the A-label's hint, and neither a
// wildcard nor an address passes.
func TestNormalize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"dashboard.fecore.dev":          "dashboard.fecore.dev",
		"  Dashboard.FeCore.DEV.  ":     "dashboard.fecore.dev",
		"fecore.dev":                    "fecore.dev",
		"a-b.c0.example.org":            "a-b.c0.example.org",
		"xn--bcher-kva.example":         "xn--bcher-kva.example",
		"shop.xn--p1ai":                 "shop.xn--p1ai",
		"1.2.3.example":                 "1.2.3.example",
		strings.Repeat("a", 63) + ".io": strings.Repeat("a", 63) + ".io",
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for in, hint := range map[string]string{
		"":                                     "required",
		".":                                    "required",
		"bücher.example":                       "A-label",
		"*.example.com":                        "wildcard",
		"localhost":                            "single label",
		"example.com..":                        "empty label",
		"a..example.com":                       "empty label",
		"-a.example.com":                       "hyphen",
		"a-.example.com":                       "hyphen",
		"a_b.example.com":                      "only letters",
		"_ferun-challenge.example.com":         "only letters",
		"a b.example.com":                      "only letters",
		"10.0.0.1":                             "IP address",
		"2001:db8::1":                          "IP address",
		"example.c":                            "top-level",
		"example.1com":                         "top-level",
		strings.Repeat("a", 64) + ".io":        "longer than 63",
		strings.Repeat("abcdefgh.", 29) + "io": "longer than 253",
	} {
		_, err := Normalize(in)
		if err == nil || !strings.Contains(err.Error(), hint) {
			t.Errorf("%q: %v, want an error with %q", in, err, hint)
		}
	}
}

// The schema's rule of a name (migration 0012) accepts every name
// Normalize gives.
func TestNormalizeMatchesSchema(t *testing.T) {
	t.Parallel()
	rule := regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	for _, in := range []string{"a.io", "x.y.z.example", "xn--bcher-kva.example", "9.example", "a-b-c.d-e.fg"} {
		got, err := Normalize(in)
		if err != nil || !rule.MatchString(got) || len(got) > 253 {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
}

func TestUnder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, zone string
		want       bool
	}{
		{"fecode.dev", "fecode.dev", true},
		{"run.fecode.dev", "fecode.dev", true},
		{"a.apps.fecode.dev", "apps.fecode.dev", true},
		{"notfecode.dev", "fecode.dev", false},
		{"fecode.dev", "run.fecode.dev", false},
		{"fecode.dev", "", false},
	} {
		if got := Under(c.name, c.zone); got != c.want {
			t.Errorf("Under(%q, %q) = %v", c.name, c.zone, got)
		}
	}
}

// A token is 32 characters of lowercase base32, as the schema wants, and
// new every time.
func TestNewToken(t *testing.T) {
	t.Parallel()
	rule := regexp.MustCompile(`^[a-z2-7]{32}$`)
	seen := map[string]bool{}
	for range 100 {
		tok, err := NewToken()
		if err != nil || !rule.MatchString(tok) || seen[tok] {
			t.Fatalf("%q, %v", tok, err)
		}
		seen[tok] = true
	}
}
