// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"testing"
	"time"

	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/store"
)

func TestHeartbeat(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	st := store.New(d)
	info := InstanceInfo{ID: "i1", Address: ":8100", NodeAddress: "10.0.0.1:7443", Version: "v1"}
	h := NewHeartbeat(d, info, dbtest.Logger(t))
	h.every = 50 * time.Millisecond

	if live, err := OwnerLive(bg, d.Pool, "i1"); err != nil || live {
		t.Fatalf("live before the first beat: %v %v", live, err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	waitFor(t, "the first beat", 5*time.Second, func() bool {
		live, err := OwnerLive(bg, d.Pool, "i1")
		return err == nil && live
	})
	in, err := st.GetInstance(bg, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if in.Address != info.Address || in.NodeAddress != info.NodeAddress || in.Version != info.Version || !in.Stopping.IsZero() {
		t.Fatalf("row %+v", in)
	}
	waitFor(t, "a later beat", 5*time.Second, func() bool {
		later, err := st.GetInstance(bg, "i1")
		return err == nil && later.LastSeen.After(in.LastSeen)
	})

	// Stopping: not live any more (its ops may be taken over), still
	// listed to nodes, and the beats keep the mark.
	if err := h.SetStopping(bg); err != nil {
		t.Fatal(err)
	}
	if live, err := OwnerLive(bg, d.Pool, "i1"); err != nil || live {
		t.Fatalf("live while stopping: %v %v", live, err)
	}
	time.Sleep(150 * time.Millisecond)
	in, err = st.GetInstance(bg, "i1")
	if err != nil || in.Stopping.IsZero() || in.Live {
		t.Fatalf("row while stopping %+v, %v", in, err)
	}
	listed, err := st.InstancesForNodes(bg)
	if err != nil || len(listed) != 1 || listed[0].ID != "i1" {
		t.Fatalf("instances for nodes while stopping %+v, %v", listed, err)
	}

	// Remove deletes the row, and no beat brings it back.
	if err := h.Remove(bg); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Remove")
	}
	if _, err := st.GetInstance(bg, "i1"); err != store.ErrNoRow {
		t.Fatalf("row after Remove: %v", err)
	}
}

func TestSetStoppingBeforeFirstBeat(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	h := NewHeartbeat(d, InstanceInfo{ID: "i2"}, dbtest.Logger(t))
	if err := h.SetStopping(bg); err != nil {
		t.Fatal(err)
	}
	in, err := store.New(d).GetInstance(bg, "i2")
	if err != nil || in.Stopping.IsZero() {
		t.Fatalf("row %+v, %v", in, err)
	}
}

func TestOwnerLiveStale(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	if err := store.New(d).UpsertInstance(bg, store.InstanceInfo{ID: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(bg, "UPDATE instances SET last_seen = now() - interval '16 seconds' WHERE id = 'old'"); err != nil {
		t.Fatal(err)
	}
	if live, err := OwnerLive(bg, d.Pool, "old"); err != nil || live {
		t.Fatalf("a row past the TTL is live: %v %v", live, err)
	}
}
