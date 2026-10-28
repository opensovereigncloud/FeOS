// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
)

// Host labels across tenants. A dotted tenant or a hyphenated name gets
// the fallback, so the pairs whose natural labels once read the same
// (F4, F14 of the security review) get distinct ones. A label stays with
// the tenant that first claimed it: another tenant never gets it, while
// the first tenant gets it back for a new environment of the same name.
func TestClaimHostLabel(t *testing.T) {
	s, _ := newStore(t)
	for _, tenant := range []string{"acme-io", "acme.io", "io"} {
		mkTenant(t, s, tenant)
	}
	web := mkEnv(t, s, "acme-io", "web")
	if e := publish(t, s, web.ID, true); e.HostLabel != "web-acme-io" {
		t.Fatalf("published %+v", e)
	}

	// The environments whose natural label once read web-acme-io, in a
	// dotted tenant and with a hyphenated name: each its fallback.
	seen := map[string]bool{"web-acme-io": true}
	for _, p := range [][2]string{{"acme.io", "web"}, {"io", "web-acme"}} {
		other := mkEnv(t, s, p[0], p[1])
		_, ok, fallback := model.HostLabels(other.Name, other.Owner, other.ID)
		if ok {
			t.Fatalf("%q: the natural label is usable", p)
		}
		o := publish(t, s, other.ID, true)
		if o.HostLabel != fallback || seen[o.HostLabel] {
			t.Fatalf("%q: %+v (fallback %s)", p, o, fallback)
		}
		seen[o.HostLabel] = true
	}

	// No two pairs share a natural label any more, but a label another
	// tenant holds, written here by hand, still goes to the fallback.
	exec(t, s, "INSERT INTO host_labels (label, owner) VALUES ('api-acme-io', 'io')")
	api := mkEnv(t, s, "acme-io", "api")
	_, ok, fallback := model.HostLabels(api.Name, api.Owner, api.ID)
	if !ok {
		t.Fatal("the natural label is not usable")
	}
	if a := publish(t, s, api.ID, true); a.HostLabel != fallback {
		t.Fatalf("a label another tenant holds %+v (fallback %s)", a, fallback)
	}

	// The first tenant deletes its environments and makes them again
	// under the same names: it gets its own label back, and a label
	// another tenant holds stays out of reach.
	for _, id := range []uuid.UUID{web.ID, api.ID} {
		if ok, err := s.DeleteEnvironment(bg, id); err != nil || !ok {
			t.Fatalf("delete %s: %v %v", id, ok, err)
		}
	}
	web2 := mkEnv(t, s, "acme-io", "web")
	if e := publish(t, s, web2.ID, true); e.HostLabel != "web-acme-io" {
		t.Fatalf("re-created %+v", e)
	}
	api2 := mkEnv(t, s, "acme-io", "api")
	_, _, fallback2 := model.HostLabels(api2.Name, api2.Owner, api2.ID)
	if a := publish(t, s, api2.ID, true); a.HostLabel != fallback2 {
		t.Fatalf("re-created api %+v (fallback %s)", a, fallback2)
	}
}
