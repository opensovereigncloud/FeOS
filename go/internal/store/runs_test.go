// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// insertRun inserts a run of t1 with its pod's row, as a create does.
func insertRun(t *testing.T, s *store.Store, key string, labels map[string]string) store.Run {
	t.Helper()
	var run store.Run
	err := s.InTx(bg, func(tx *store.Store) error {
		idle := 0
		sb, err := tx.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
			return store.NewSandbox{ID: id, Owner: "t1", Image: "img", VCPUs: 1, MemoryMiB: 512, ImageRef: "img@sha256:00",
				PodTemplate: &model.PodTemplate{}, IdleAfterS: &idle, TenantRun: true}
		})
		if err != nil {
			return err
		}
		run, err = tx.InsertRun(bg, store.Run{ID: sb.ID, Owner: "t1", Key: key, RequestHash: "h", Image: "img",
			ImageRef: "img@sha256:00", VCPUs: 1, MemoryMiB: 512, Labels: labels}, []byte("sealed"), time.Minute)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A run's pod is no sandbox of its tenant; a key is unique per tenant; a
// run ends once, which drops its secrets; the pods of ended runs are
// left for deletion; the sweep takes the runs that ended long enough ago.
func TestRuns(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "t1")
	a := insertRun(t, s, "k1", map[string]string{"project": "p1"})
	b := insertRun(t, s, "", nil)
	if a.Phase != model.RunQueued || a.Key != "k1" || a.DeadlineAt.Sub(a.CreatedAt) != time.Minute || a.Ended() {
		t.Fatalf("inserted %+v", a)
	}
	if list, err := s.ListSandboxes(bg, "t1"); err != nil || len(list) != 0 {
		t.Fatalf("the tenant's sandboxes %d %v", len(list), err)
	}
	if err := s.InTx(bg, func(tx *store.Store) error {
		_, err := tx.InsertRun(bg, store.Run{ID: podid.Mint(), Owner: "t1", Key: "k1", RequestHash: "h2", Image: "i",
			ImageRef: "i"}, nil, time.Minute)
		return err
	}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("a second run under k1: %v", err)
	}
	if r, err := s.RunByKey(bg, "t1", "k1"); err != nil || r.ID != a.ID {
		t.Fatalf("by key %+v %v", r, err)
	}
	if list, err := s.ListRuns(bg, "t1", map[string]string{"project": "p1"}); err != nil || len(list) != 1 ||
		list[0].ID != a.ID {
		t.Fatalf("by label %+v %v", list, err)
	}
	if ok, err := s.StartRun(bg, a.ID); err != nil || !ok {
		t.Fatalf("start %v %v", ok, err)
	}
	code := 0
	end := store.RunEnd{Phase: model.RunSucceeded, ExitCode: &code, Result: "sha256:1", LogTail: []string{"done"},
		Exited: true}
	if ok, err := s.EndRun(bg, a.ID, end); err != nil || !ok {
		t.Fatalf("end %v %v", ok, err)
	}
	if ok, err := s.EndRun(bg, a.ID, store.RunEnd{Phase: model.RunCancelled}); err != nil || ok {
		t.Fatalf("a second end %v %v", ok, err)
	}
	got, err := s.GetRun(bg, "t1", a.ID)
	if err != nil || got.Phase != model.RunSucceeded || got.Result != "sha256:1" || got.StartedAt.IsZero() ||
		!got.Ended() || len(got.LogTail) != 1 {
		t.Fatalf("ended %+v %v", got, err)
	}
	if sealed, err := s.RunSealed(bg, a.ID); err != nil || sealed != nil {
		t.Fatalf("sealed after the end %v %v", sealed, err)
	}
	if _, err := s.GetRun(bg, "t2", a.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("another tenant's read: %v", err)
	}
	// An exit never seen running gets its start at its end.
	if ok, err := s.EndRun(bg, b.ID, store.RunEnd{Phase: model.RunFailed, Reason: model.RunReasonExit, ExitCode: &code,
		Exited: true}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, _ := s.GetRun(bg, "t1", b.ID); got.StartedAt.IsZero() {
		t.Error("the exit has no start")
	}
	left, err := s.LeftRunPods(bg)
	if err != nil || len(left) != 2 {
		t.Fatalf("left pods %d %v", len(left), err)
	}
	exec(t, s, "UPDATE runs SET finished_at = now() - interval '8 days' WHERE id = $1", a.ID)
	if n, err := s.SweepRuns(bg, 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("swept %d %v", n, err)
	}
	if _, err := s.GetRun(bg, "t1", a.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the swept run: %v", err)
	}
}
