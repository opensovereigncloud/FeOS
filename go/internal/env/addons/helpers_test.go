// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
)

// addon reads an addon of a spec as ferund does (spec.Addon from JSON,
// so options are []any, bool, float64 and string) and names it.
func addon(t *testing.T, name, raw string) Addon {
	t.Helper()
	var a spec.Addon
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("addon %s: %v", raw, err)
	}
	return Addon{Name: name, Type: a.Type, Version: a.Version, Plan: a.Plan, Options: a.Options}
}

// ptr is a pointer to a copy of a.
func ptr(a Addon) *Addon { return &a }

// keys are the findings as "<severity> <path>", in their order.
func keys(fs []spec.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f.Severity)+" "+f.Path)
	}
	return out
}

// checkFindings compares the findings' severities and paths with want,
// in order, and that every finding says what is wrong; each of problems
// must appear in some finding's problem.
func checkFindings(t *testing.T, fs []spec.Finding, want []string, problems ...string) {
	t.Helper()
	if got := keys(fs); !slices.Equal(got, want) {
		t.Fatalf("findings %q, want %q\n%s", got, want, findingText(fs))
	}
	for _, f := range fs {
		if f.Problem == "" {
			t.Errorf("finding at %q without a problem", f.Path)
		}
		if f.Severity == spec.SeverityError && f.Remedy == "" {
			t.Errorf("error at %q without a remedy: %s", f.Path, f.Problem)
		}
	}
	for _, p := range problems {
		if !slices.ContainsFunc(fs, func(f spec.Finding) bool { return strings.Contains(f.Problem, p) }) {
			t.Errorf("no finding says %q\n%s", p, findingText(fs))
		}
	}
}

func findingText(fs []spec.Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(string(f.Severity) + " " + f.Path + ": " + f.Problem + " — " + f.Remedy + "\n")
	}
	return b.String()
}

// jsonOf is v's JSON, for comparing renderings in their stored form.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// resolve joins a value's parts the way the engine will once the
// controller has mapped credentials to secrets: creds by key, escaped for
// a URL's userinfo where a part says so.
func resolve(t *testing.T, v model.TemplateValue, creds map[string]string) string {
	t.Helper()
	if v.Secret != "" {
		t.Fatalf("a blueprint rendered a secret %q; it may only name credentials", v.Secret)
	}
	if v.Parts == nil {
		return v.Value
	}
	return resolveParts(t, v.Parts, creds)
}

func resolveParts(t *testing.T, parts []model.TemplatePart, creds map[string]string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range parts {
		switch {
		case p.Credential != "":
			val, ok := creds[p.Credential]
			if !ok {
				t.Fatalf("credential %q is not among the addon's credentials", p.Credential)
			}
			if p.Escape == model.EscapeURL {
				val = escapeUserinfo(val)
			}
			b.WriteString(val)
		case p.Secret != "":
			t.Fatalf("a blueprint rendered a secret %q; it may only name credentials", p.Secret)
		default:
			b.WriteString(p.Value)
		}
	}
	return b.String()
}

// credentialKeys are the credential keys a's rendering, provisioning and
// parts name.
func credentialKeys(vals map[string]model.TemplateValue, parts ...[]model.TemplatePart) []string {
	var out []string
	add := func(ps []model.TemplatePart) {
		for _, p := range ps {
			if p.Credential != "" && !slices.Contains(out, p.Credential) {
				out = append(out, p.Credential)
			}
		}
	}
	for _, v := range vals {
		add(v.Parts)
	}
	for _, ps := range parts {
		add(ps)
	}
	slices.Sort(out)
	return out
}
