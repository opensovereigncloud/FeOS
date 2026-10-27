// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/gardenlinux/feos/go/internal/model"
)

func TestNATSVersionsPlans(t *testing.T) {
	var n nats
	if b, ok := Get("nats"); !ok || b != (nats{}) {
		t.Fatalf("Get(nats) = %v, %v", b, ok)
	}
	if got := n.Versions(); !slices.Equal(got, []string{"2"}) {
		t.Fatal(got)
	}
	want := map[string]Plan{
		"dev":   {VCPUs: 1, MemoryMib: 128, VolumeMib: 1024},
		"small": {VCPUs: 1, MemoryMib: 512, VolumeMib: 10240},
	}
	if got := n.Plans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plans %v, want %v", got, want)
	}
	if n.DefaultPlan() != "small" {
		t.Fatal(n.DefaultPlan())
	}
	if k := n.OutputKeys(); !slices.Equal(k, []string{"host", "port", "user", "password", "url"}) {
		t.Fatal(k)
	}
	if c := n.Credentials(addon(t, "nats", `{"type":"nats","version":"2"}`)); !slices.Equal(c, []string{"password"}) {
		t.Fatal(c)
	}
	if s := n.Provision(addon(t, "nats", `{"type":"nats","version":"2"}`), Context{}); s != nil {
		t.Fatal(s)
	}
}

func TestNATSValidate(t *testing.T) {
	cases := []struct {
		name     string
		prev     *Addon
		next     string
		want     []string
		problems []string
	}{
		{name: "default", next: `{"type":"nats","version":"2"}`},
		{name: "jetstream", next: `{"type":"nats","version":"2","plan":"small","options":{"jetstream":true}}`},
		{name: "no jetstream", next: `{"type":"nats","version":"2","plan":"dev","options":{"jetstream":false}}`},
		{name: "no version", next: `{"type":"nats"}`, want: []string{"error version"}},
		{name: "unknown version", next: `{"type":"nats","version":"2.10"}`, want: []string{"error version"}},
		{name: "unknown plan", next: `{"type":"nats","version":"2","plan":"medium"}`,
			want: []string{"error plan"}, problems: []string{`no plan "medium"`}},
		{name: "jetstream not a boolean", next: `{"type":"nats","version":"2","options":{"jetstream":"yes"}}`,
			want: []string{"error options.jetstream"}, problems: []string{`want true or false, not "yes"`}},
		{name: "unknown option", next: `{"type":"nats","version":"2","options":{"cluster":true}}`,
			want: []string{"error options.cluster"}},
		{name: "plan change", prev: ptr(addon(t, "nats", `{"type":"nats","version":"2","plan":"small"}`)),
			next: `{"type":"nats","version":"2","plan":"dev"}`,
			want: []string{"warning plan"}, problems: []string{"from small to dev"}},
		{name: "plan change without a volume",
			prev: ptr(addon(t, "nats", `{"type":"nats","version":"2","plan":"small","options":{"jetstream":false}}`)),
			next: `{"type":"nats","version":"2","plan":"dev","options":{"jetstream":false}}`},
		{name: "jetstream off", prev: ptr(addon(t, "nats", `{"type":"nats","version":"2"}`)),
			next: `{"type":"nats","version":"2","options":{"jetstream":false}}`,
			want: []string{"warning options.jetstream"}, problems: []string{"JetStream is turned off"}},
		{name: "jetstream on", prev: ptr(addon(t, "nats", `{"type":"nats","version":"2","options":{"jetstream":false}}`)),
			next: `{"type":"nats","version":"2"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFindings(t, nats{}.Validate(c.prev, addon(t, "nats", c.next)), c.want, c.problems...)
		})
	}
}

func TestNATSRender(t *testing.T) {
	const serve = `printf 'http: "127.0.0.1:8222"\n' > /tmp/nats.conf && exec nats-server -c /tmp/nats.conf -a :: -p 4222 --user nats --pass "$NATS_PASSWORD"`
	cases := []struct {
		addon, command, mount string
	}{
		{`{"type":"nats","version":"2"}`, serve + " -js -sd /data", "/data"},
		{`{"type":"nats","version":"2","plan":"dev","options":{"jetstream":true}}`, serve + " -js -sd /data", "/data"},
		{`{"type":"nats","version":"2","options":{"jetstream":false}}`, serve, ""},
	}
	for _, c := range cases {
		t.Run(c.addon, func(t *testing.T) {
			got, err := nats{}.Render(addon(t, "nats", c.addon), Context{Env: "stage", Name: "nats", Address: "fd6b::9"})
			if err != nil {
				t.Fatal(err)
			}
			want := Instance{
				Image:   "nats:2-alpine",
				Command: []string{"sh", "-c", c.command},
				Env:     map[string]model.TemplateValue{"NATS_PASSWORD": model.CredentialValue("password")},
				Port:    4222,
				Mount:   c.mount,
				Probe:   []string{"wget", "-qO-", "http://127.0.0.1:8222/healthz"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("render\n%#v\nwant\n%#v", got, want)
			}
		})
	}
	for _, bad := range []string{`{"type":"nats"}`, `{"type":"nats","version":"3"}`,
		`{"type":"nats","version":"2","plan":"large"}`, `{"type":"nats","version":"2","options":{"jetstream":1}}`} {
		if _, err := (nats{}).Render(addon(t, "nats", bad), Context{}); err == nil {
			t.Errorf("Render(%s) did not fail", bad)
		}
	}
}

func TestNATSOutputs(t *testing.T) {
	a := addon(t, "nats", `{"type":"nats","version":"2"}`)
	v6 := Context{Env: "stage", Name: "bus", Address: "fd6b:59f2:b4a2:3::9"}
	none := Context{Env: "stage", Name: "bus"}
	cases := []struct {
		name string
		c    Context
		key  string
		want []model.TemplatePart
	}{
		{"host", v6, "host", []model.TemplatePart{{Value: "fd6b:59f2:b4a2:3::9"}}},
		{"host without address", none, "host", []model.TemplatePart{{Value: "bus"}}},
		{"port", v6, "port", []model.TemplatePart{{Value: "4222"}}},
		{"user", v6, "user", []model.TemplatePart{{Value: "nats"}}},
		{"password", v6, "password", []model.TemplatePart{{Credential: "password"}}},
		{"url", v6, "url", []model.TemplatePart{{Value: "nats://nats:"},
			{Credential: "password", Escape: model.EscapeURL}, {Value: "@[fd6b:59f2:b4a2:3::9]:4222"}}},
		{"url without address", none, "url", []model.TemplatePart{{Value: "nats://nats:"},
			{Credential: "password", Escape: model.EscapeURL}, {Value: "@bus:4222"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := nats{}.Outputs(a, c.c, Ref{Key: c.key})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("outputs %s, want %s", jsonOf(t, got), jsonOf(t, c.want))
			}
		})
	}
	for _, bad := range []Ref{{}, {Key: "subject"}, {Key: "url", Fields: map[string]string{"user": "x"}}} {
		if _, err := (nats{}).Outputs(a, v6, bad); err == nil {
			t.Errorf("Outputs(%v) did not fail", bad)
		}
	}
	parts, _ := nats{}.Outputs(a, v6, Ref{Key: "url"})
	raw := resolveParts(t, parts, map[string]string{"password": "0f1e2d3c4b5a69788796a5b4c3d2e1f0"})
	if raw != "nats://nats:0f1e2d3c4b5a69788796a5b4c3d2e1f0@[fd6b:59f2:b4a2:3::9]:4222" {
		t.Fatal(raw)
	}
	if u, err := url.Parse(raw); err != nil || u.Hostname() != "fd6b:59f2:b4a2:3::9" || u.Port() != "4222" {
		t.Fatalf("%s parses to %#v, %v", raw, u, err)
	}
}
