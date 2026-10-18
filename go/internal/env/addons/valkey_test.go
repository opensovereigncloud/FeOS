// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ironcore-dev/feos/go/internal/model"
)

func TestValkeyVersionsPlans(t *testing.T) {
	var v valkey
	if b, ok := Get("valkey"); !ok || b != (valkey{}) {
		t.Fatalf("Get(valkey) = %v, %v", b, ok)
	}
	if got := v.Versions(); !slices.Equal(got, []string{"7.2", "8"}) {
		t.Fatal(got)
	}
	want := map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 128, VolumeMib: 256},
		"small":  {VCPUs: 1, MemoryMib: 512, VolumeMib: 2048},
		"medium": {VCPUs: 1, MemoryMib: 2048, VolumeMib: 8192},
		"large":  {VCPUs: 2, MemoryMib: 8192, VolumeMib: 32768},
	}
	if got := v.Plans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plans %v, want %v", got, want)
	}
	if v.DefaultPlan() != "small" {
		t.Fatal(v.DefaultPlan())
	}
	if k := v.OutputKeys(); !slices.Equal(k, []string{"host", "port", "user", "password", "url"}) {
		t.Fatal(k)
	}
	if c := v.Credentials(addon(t, "cache", `{"type":"valkey","version":"8"}`)); !slices.Equal(c, []string{"password"}) {
		t.Fatal(c)
	}
	if s := v.Provision(addon(t, "cache", `{"type":"valkey","version":"8"}`), Context{}); s != nil {
		t.Fatal(s)
	}
}

func TestValkeyValidate(t *testing.T) {
	cases := []struct {
		name     string
		prev     *Addon
		next     string
		want     []string
		problems []string
	}{
		{name: "8", next: `{"type":"valkey","version":"8"}`},
		{name: "7.2 dev", next: `{"type":"valkey","version":"7.2","plan":"dev"}`},
		{name: "policy", next: `{"type":"valkey","version":"8","options":{"maxmemoryPolicy":"allkeys-lru"}}`},
		{name: "no version", next: `{"type":"valkey"}`, want: []string{"error version"}},
		{name: "unknown version", next: `{"type":"valkey","version":"7"}`,
			want: []string{"error version"}, problems: []string{`no version "7"`}},
		{name: "unknown plan", next: `{"type":"valkey","version":"8","plan":"tiny"}`, want: []string{"error plan"}},
		{name: "unknown policy", next: `{"type":"valkey","version":"8","options":{"maxmemoryPolicy":"lru"}}`,
			want: []string{"error options.maxmemoryPolicy"}, problems: []string{`no maxmemory policy "lru"`}},
		{name: "policy not a string", next: `{"type":"valkey","version":"8","options":{"maxmemoryPolicy":1}}`,
			want: []string{"error options.maxmemoryPolicy"}, problems: []string{"want a string, not a number"}},
		{name: "unknown option", next: `{"type":"valkey","version":"8","options":{"maxmemory":"1gb"}}`,
			want: []string{"error options.maxmemory"}},
		{name: "upgrade", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"7.2"}`)),
			next: `{"type":"valkey","version":"8"}`,
			want: []string{"info version"}, problems: []string{"from 7.2 to 8 on the same volume"}},
		{name: "downgrade", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"8"}`)),
			next: `{"type":"valkey","version":"7.2"}`,
			want: []string{"error version"}, problems: []string{"cannot go back from 8 to 7.2"}},
		{name: "unchanged", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"8"}`)),
			next: `{"type":"valkey","version":"8","plan":"small"}`},
		{name: "plan change", prev: ptr(addon(t, "cache", `{"type":"valkey","version":"8","plan":"dev"}`)),
			next: `{"type":"valkey","version":"8","plan":"large"}`,
			want: []string{"warning plan"}, problems: []string{"from dev to large", "large's 32768 MiB is not applied"}},
		{name: "from another type", prev: ptr(addon(t, "cache", `{"type":"postgres","version":"17","plan":"dev"}`)),
			next: `{"type":"valkey","version":"8"}`},
	}
	for _, p := range valkeyPolicies {
		cases = append(cases, struct {
			name     string
			prev     *Addon
			next     string
			want     []string
			problems []string
		}{name: p, next: `{"type":"valkey","version":"8","options":{"maxmemoryPolicy":"` + p + `"}}`})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFindings(t, valkey{}.Validate(c.prev, addon(t, "cache", c.next)), c.want, c.problems...)
		})
	}
	if len(valkeyPolicies) != 8 {
		t.Fatalf("%d policies", len(valkeyPolicies))
	}
}

func TestValkeyRender(t *testing.T) {
	probe := []string{"sh", "-c", `REDISCLI_AUTH="$VALKEY_PASSWORD" valkey-cli ping | grep -qx PONG`}
	cases := []struct {
		addon, image, tail string
	}{
		{`{"type":"valkey","version":"8","plan":"dev"}`, "valkey/valkey:8-alpine",
			" --maxmemory 89mb --maxmemory-policy noeviction"},
		{`{"type":"valkey","version":"8"}`, "valkey/valkey:8-alpine",
			" --maxmemory 358mb --maxmemory-policy noeviction"},
		{`{"type":"valkey","version":"7.2","plan":"medium","options":{"maxmemoryPolicy":"volatile-ttl"}}`,
			"valkey/valkey:7.2-alpine", " --maxmemory 1433mb --maxmemory-policy volatile-ttl"},
		{`{"type":"valkey","version":"8","plan":"large","options":{"maxmemoryPolicy":"allkeys-lru"}}`,
			"valkey/valkey:8-alpine", " --maxmemory 5734mb --maxmemory-policy allkeys-lru"},
	}
	for _, c := range cases {
		t.Run(c.addon, func(t *testing.T) {
			got, err := valkey{}.Render(addon(t, "cache", c.addon), Context{Env: "stage", Name: "cache", Address: "fd6b::7"})
			if err != nil {
				t.Fatal(err)
			}
			want := Instance{
				Image: c.image,
				Env: map[string]model.TemplateValue{
					"VALKEY_EXTRA_FLAGS": {Parts: []model.TemplatePart{
						{Value: "--appendonly yes --requirepass "}, {Credential: "password"}, {Value: c.tail}}},
					"VALKEY_PASSWORD": model.CredentialValue("password"),
				},
				Port:  6379,
				Mount: "/data",
				Probe: probe,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("render\n%#v\nwant\n%#v", got, want)
			}
			if got.Command != nil {
				t.Fatal("valkey must run the image's command: its entrypoint drops privileges")
			}
			flags := resolve(t, got.Env["VALKEY_EXTRA_FLAGS"], map[string]string{"password": "0123abcd"})
			if want := "--appendonly yes --requirepass 0123abcd" + c.tail; flags != want {
				t.Fatalf("flags %q, want %q", flags, want)
			}
		})
	}
	for _, bad := range []string{`{"type":"valkey"}`, `{"type":"valkey","version":"9"}`,
		`{"type":"valkey","version":"8","plan":"huge"}`,
		`{"type":"valkey","version":"8","options":{"maxmemoryPolicy":"lru"}}`,
		`{"type":"valkey","version":"8","options":{"x":1}}`} {
		if _, err := (valkey{}).Render(addon(t, "cache", bad), Context{}); err == nil {
			t.Errorf("Render(%s) did not fail", bad)
		}
	}
}

func TestValkeyOutputs(t *testing.T) {
	a := addon(t, "cache", `{"type":"valkey","version":"8"}`)
	v6 := Context{Env: "stage", Name: "cache", Address: "fd6b:59f2:b4a2:3::7"}
	none := Context{Env: "stage", Name: "cache"}
	cases := []struct {
		name string
		c    Context
		key  string
		want []model.TemplatePart
	}{
		{"host", v6, "host", []model.TemplatePart{{Value: "fd6b:59f2:b4a2:3::7"}}},
		{"host without address", none, "host", []model.TemplatePart{{Value: "cache"}}},
		{"port", v6, "port", []model.TemplatePart{{Value: "6379"}}},
		{"user", v6, "user", []model.TemplatePart{{Value: "default"}}},
		{"password", v6, "password", []model.TemplatePart{{Credential: "password"}}},
		{"url", v6, "url", []model.TemplatePart{{Value: "redis://default:"},
			{Credential: "password", Escape: model.EscapeURL}, {Value: "@[fd6b:59f2:b4a2:3::7]:6379/0"}}},
		{"url without address", none, "url", []model.TemplatePart{{Value: "redis://default:"},
			{Credential: "password", Escape: model.EscapeURL}, {Value: "@cache:6379/0"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := valkey{}.Outputs(a, c.c, Ref{Key: c.key})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("outputs %s, want %s", jsonOf(t, got), jsonOf(t, c.want))
			}
		})
	}
	for _, bad := range []Ref{{}, {Key: "database"}, {Key: "url", Fields: map[string]string{"role": "x"}}} {
		if _, err := (valkey{}).Outputs(a, v6, bad); err == nil {
			t.Errorf("Outputs(%v) did not fail", bad)
		}
	}
	_, err := valkey{}.Outputs(a, v6, Ref{Key: "url", Fields: map[string]string{"db": "1"}})
	if err == nil || !strings.Contains(err.Error(), "takes only addon and key") {
		t.Fatalf("error %v", err)
	}
	parts, _ := valkey{}.Outputs(a, v6, Ref{Key: "url"})
	raw := resolveParts(t, parts, map[string]string{"password": "a:b@c/d"})
	u, err := url.Parse(raw)
	if pw, _ := u.User.Password(); err != nil || u.User.Username() != "default" || pw != "a:b@c/d" ||
		u.Hostname() != "fd6b:59f2:b4a2:3::7" || u.Port() != "6379" {
		t.Fatalf("%s parses to %#v, %v", raw, u, err)
	}
}
