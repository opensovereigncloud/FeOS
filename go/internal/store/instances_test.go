// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/ironcore-dev/feos/go/internal/store"
)

// A stopping instance that still writes its row is fresh but not live;
// one whose row aged past the TTL is neither.
func TestInstanceFresh(t *testing.T) {
	s, _ := newStore(t)
	for _, id := range []string{"running", "stopping", "gone"} {
		must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: id}))
	}
	must(t, s.SetInstanceStopping(bg, "stopping"))
	exec(t, s, "UPDATE instances SET last_seen = now() - interval '16 seconds' WHERE id = 'gone'")
	list, err := s.ListInstances(bg)
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	want := map[string][2]bool{"running": {true, true}, "stopping": {true, false}, "gone": {false, false}}
	for _, in := range list {
		if w := want[in.ID]; in.Fresh != w[0] || in.Live != w[1] {
			t.Errorf("%s: fresh %v live %v, want %v", in.ID, in.Fresh, in.Live, w)
		}
	}
}
