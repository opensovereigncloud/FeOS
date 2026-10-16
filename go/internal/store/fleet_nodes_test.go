// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ironcore-dev/feos/go/internal/store"
)

func TestExpireNodesBatch(t *testing.T) {
	s, d := newStore(t)
	for _, n := range []string{"a", "b", "c", "d", "e", "fresh", "gone", "held"} {
		mkNode(t, s, n)
	}
	exec(t, s, "UPDATE nodes SET joined_at = now() - interval '1 minute'")
	exec(t, s, "UPDATE nodes SET status = 'joined' WHERE name = 'c'")
	exec(t, s, "UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'gone'")
	exec(t, s, "INSERT INTO node_telemetry (node, last_seen) VALUES ('fresh', now()), ('b', now() - interval '1 minute')")

	// A node whose row another transaction holds is skipped, not waited
	// for.
	tx, err := d.Pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(bg)
	if _, err := tx.Exec(bg, "SELECT 1 FROM nodes WHERE name = 'held' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	var batches [][]string
	for range 4 {
		names, err := s.ExpireNodesBatch(bg, time.Second, 2)
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, names)
	}
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}, nil}
	for i := range want {
		if !slices.Equal(batches[i], want[i]) {
			t.Fatalf("batches %v; want %v", batches, want)
		}
	}
	for _, n := range []string{"a", "c", "e"} {
		if got, _ := s.GetNode(bg, n); got.Status != "down" || got.StatusDescription != "heartbeat expired" || got.DownSince.IsZero() {
			t.Fatalf("expired node %+v", got)
		}
	}
	for n, status := range map[string]string{"fresh": "ready", "gone": "removed", "held": "ready"} {
		if got, _ := s.GetNode(bg, n); got.Status != status {
			t.Fatalf("node %s is %s; want %s", n, got.Status, status)
		}
	}
	must(t, tx.Rollback(bg))
	if names, err := s.ExpireNodesBatch(bg, time.Second, 2); err != nil || !slices.Equal(names, []string{"held"}) {
		t.Fatalf("after the lock: %v, %v", names, err)
	}
}

func TestLockPodCredentials(t *testing.T) {
	s, d := newStore(t)
	pod, other := uuid.New(), uuid.New()
	locked := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- pgx.BeginFunc(bg, d.Pool, func(tx pgx.Tx) error {
			if err := store.On(tx).LockPodCredentials(bg, pod); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	// Another pod's requests are not held up.
	must(t, s.InTx(bg, func(tx *store.Store) error { return tx.LockPodCredentials(bg, other) }))

	// The same pod's wait for the first transaction to end.
	second := make(chan error, 1)
	go func() {
		second <- s.InTx(bg, func(tx *store.Store) error { return tx.LockPodCredentials(bg, pod) })
	}()
	select {
	case err := <-second:
		t.Fatalf("a second lock of the pod while the first is held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	must(t, <-first)
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	select {
	case err := <-second:
		must(t, err)
	case <-ctx.Done():
		t.Fatal("the second lock was not granted after the first transaction ended")
	}
}
