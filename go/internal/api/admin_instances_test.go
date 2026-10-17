// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// A stopping instance that still writes its row is fresh, though not
// live: it still serves its requests and ops, and the UI shows it so. One
// whose row aged past the TTL is neither.
func TestInstancesFresh(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	st := store.New(d)
	for _, id := range []string{"running", "stopping", "gone"} {
		if err := st.UpsertInstance(ctx, store.InstanceInfo{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetInstanceStopping(ctx, "stopping"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, "UPDATE instances SET last_seen = now() - interval '16 seconds' WHERE id = 'gone'"); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, envOptions{db: d})
	w := e.do("GET", "/api/v1/admin/instances", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	out := decode[struct {
		Instances []struct {
			ID    string `json:"id"`
			Fresh *bool  `json:"fresh"`
			Live  bool   `json:"live"`
		} `json:"instances"`
	}](t, w)
	want := map[string][2]bool{"running": {true, true}, "stopping": {true, false}, "gone": {false, false}}
	if len(out.Instances) != len(want) {
		t.Fatalf("instances %+v", out.Instances)
	}
	for _, in := range out.Instances {
		if w := want[in.ID]; in.Fresh == nil || *in.Fresh != w[0] || in.Live != w[1] {
			t.Errorf("%s: fresh %v live %v, want %v", in.ID, in.Fresh, in.Live, w)
		}
	}
}
