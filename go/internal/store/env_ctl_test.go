// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The environment controller's queries.
func TestEnvCtlQueries(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	env := mkEnv(t, s, "acme", "stage")
	other := mkEnv(t, s, "acme", "prod")
	a := mkInstance(t, s, env)
	b := mkInstance(t, s, env)
	mkInstance(t, s, other)
	sbx := mkSandbox(t, s, "acme")
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::2' WHERE id = $1`, a.ID)
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::9' WHERE id = $1`, sbx.ID)

	// Members: the instances' addresses and those of the attached
	// services' sandboxes.
	if m, err := s.EnvCtlMembers(bg, env.ID); err != nil || !slices.Equal(m, []string{"fd00::2"}) {
		t.Fatalf("members %v %v", m, err)
	}
	if _, err := s.InsertAttached(bg, store.EnvAttached{EnvID: env.ID, Service: "web", SandboxID: sbx.ID}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.EnvCtlMembers(bg, env.ID); err != nil || !slices.Equal(m, []string{"fd00::2", "fd00::9"}) {
		t.Fatalf("members with an attached service %v %v", m, err)
	}
	if envs, err := s.EnvCtlWithAttached(bg, sbx.ID); err != nil || !slices.Equal(envs, []uuid.UUID{env.ID}) {
		t.Fatalf("attached to %v %v", envs, err)
	}
	// A sandbox the attached service may not use is neither a member nor
	// attached: another tenant's, an instance row, or another incarnation
	// than the one the service recorded.
	mkTenant(t, s, "evil")
	foreign := mkSandbox(t, s, "evil")
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::7' WHERE id = $1`, foreign.ID)
	if _, err := s.InsertAttached(bg, store.EnvAttached{EnvID: env.ID, Service: "api", SandboxID: foreign.ID}); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::8' WHERE id = $1`, b.ID)
	if _, err := s.InsertAttached(bg, store.EnvAttached{EnvID: other.ID, Service: "inst", SandboxID: b.ID}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.EnvCtlMembers(bg, env.ID); err != nil || !slices.Equal(m, []string{"fd00::2", "fd00::8", "fd00::9"}) {
		t.Fatalf("members with a foreign sandbox attached %v %v", m, err)
	}
	if m, err := s.EnvCtlMembers(bg, other.ID); err != nil || len(m) != 0 {
		t.Fatalf("members with an instance row attached %v %v", m, err)
	}
	if envs, err := s.EnvCtlWithAttached(bg, foreign.ID); err != nil || len(envs) != 0 {
		t.Fatalf("a foreign sandbox attached to %v %v", envs, err)
	}
	if envs, err := s.EnvCtlWithAttached(bg, b.ID); err != nil || len(envs) != 0 {
		t.Fatalf("an instance row attached to %v %v", envs, err)
	}
	exec(t, s, `UPDATE env_attached SET sandbox_incarnation = $2 WHERE env_id = $1 AND service = 'web'`, env.ID, uuid.New())
	if m, err := s.EnvCtlMembers(bg, env.ID); err != nil || !slices.Equal(m, []string{"fd00::2", "fd00::8"}) {
		t.Fatalf("members with another incarnation recorded %v %v", m, err)
	}
	if envs, err := s.EnvCtlWithAttached(bg, sbx.ID); err != nil || len(envs) != 0 {
		t.Fatalf("another incarnation attached to %v %v", envs, err)
	}
	exec(t, s, `UPDATE env_attached SET sandbox_incarnation = $2 WHERE env_id = $1 AND service = 'web'`, env.ID, sbx.Incarnation)
	if envs, err := s.EnvCtlWithAttached(bg, sbx.ID); err != nil || !slices.Equal(envs, []uuid.UUID{env.ID}) {
		t.Fatalf("its incarnation attached to %v %v", envs, err)
	}
	exec(t, s, `DELETE FROM env_attached WHERE service IN ('api', 'inst')`)
	if rows, err := s.EnvCtlSandboxes(bg, env.ID); err != nil || len(rows) != 2 {
		t.Fatalf("instance rows %v %v", rows, err)
	}

	// Pin and idle: written without a lifecycle write.
	before := get(t, s, a.ID)
	if changed, err := s.EnvCtlSetPin(bg, a.ID, "n1"); err != nil || !changed {
		t.Fatalf("pin %v %v", changed, err)
	}
	if changed, err := s.EnvCtlSetPin(bg, a.ID, "n1"); err != nil || changed {
		t.Fatalf("the same pin again %v %v", changed, err)
	}
	if changed, err := s.EnvCtlSetIdle(bg, a.ID, 900); err != nil || !changed {
		t.Fatalf("idle %v %v", changed, err)
	}
	if changed, err := s.EnvCtlSetIdle(bg, a.ID, 900); err != nil || changed {
		t.Fatalf("the same idle again %v %v", changed, err)
	}
	after := get(t, s, a.ID)
	if after.PinNode != "n1" || after.IdleAfterS == nil || *after.IdleAfterS != 900 || after.Gen != before.Gen {
		t.Fatalf("row %+v", after)
	}

	// Templated rows on a node: by node, op target or lost_from.
	place(t, s, a.ID, "n1", model.StateRunning)
	exec(t, s, `UPDATE sandboxes SET pod_template = '{"command":["sleep","infinity"]}'::jsonb WHERE id = $1`, sbx.ID)
	place(t, s, sbx.ID, "n1", model.StateRunning)
	on, err := s.EnvCtlTemplatedOnNode(bg, "n1")
	if err != nil || len(on) != 2 {
		t.Fatalf("on n1 %v %v", on, err)
	}

	// Starved rows: pending without capacity for long enough.
	exec(t, s, `UPDATE sandboxes SET status_description = 'no capacity (retryable): no feasible node',
		pending_since = now() - interval '3 minutes' WHERE id = $1`, b.ID)
	starved, err := s.EnvCtlStarved(bg, env.ID, 2*time.Minute)
	if err != nil || len(starved) != 1 || starved[b.ID] == "" {
		t.Fatalf("starved %v %v", starved, err)
	}
	if starved, _ := s.EnvCtlStarved(bg, env.ID, 5*time.Minute); len(starved) != 0 {
		t.Fatalf("starved for 5 minutes %v", starved)
	}

	// Incidents closed by release and by object.
	open := func(object string, release int, condition string) int64 {
		i, _, err := s.OpenIncident(bg, store.EnvIncident{EnvID: env.ID, Object: object, Release: release, Condition: condition})
		if err != nil {
			t.Fatal(err)
		}
		return i.ID
	}
	open("service/web", 1, model.ConditionRolloutFailed)
	open("job/m", 2, model.ConditionJobFailed)
	open("service/web", 3, model.ConditionRolloutFailed)
	open("service/web", 1, model.ConditionCrashLoop)
	if n, err := s.EnvCtlCloseIncidents(bg, env.ID, 3, model.ConditionRolloutFailed, model.ConditionJobFailed); err != nil || n != 2 {
		t.Fatalf("closed %d %v", n, err)
	}
	if n, err := s.EnvCtlCloseObjectIncidents(bg, env.ID, "service/web", model.ConditionCrashLoop); err != nil || n != 1 {
		t.Fatalf("closed %d %v", n, err)
	}
	if left, _ := s.CountOpenIncidents(bg, env.ID); left != 1 {
		t.Fatalf("%d open", left)
	}

	// Running runs.
	if _, err := s.InsertJobRun(bg, store.EnvJobRun{EnvID: env.ID, Job: "m", TemplateHash: "h", Release: 1}); err != nil {
		t.Fatal(err)
	}
	if runs, err := s.EnvCtlRunningJobRuns(bg, env.ID); err != nil || len(runs) != 1 {
		t.Fatalf("runs %v %v", runs, err)
	}

	// Volumes marked deleting.
	if _, err := s.InsertEnvVolume(bg, store.EnvVolume{EnvID: env.ID, Object: "addon/pg", VolumeID: uuid.New(), SizeMiB: 1024}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EnvCtlMarkVolumesDeleting(bg, env.ID); err != nil || n != 1 {
		t.Fatalf("marked %d %v", n, err)
	}
	if n, _ := s.EnvCtlMarkVolumesDeleting(bg, env.ID); n != 0 {
		t.Fatalf("marked again %d", n)
	}
	// A touched volume goes to the end of the deleting ones.
	if _, err := s.InsertEnvVolume(bg, store.EnvVolume{EnvID: env.ID, Object: "addon/kv", VolumeID: uuid.New(), SizeMiB: 256,
		Deleting: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnvCtlTouchVolume(bg, env.ID, "addon/pg"); err != nil {
		t.Fatal(err)
	}
	if vols, err := s.DeletingEnvVolumes(bg, 10); err != nil || len(vols) != 2 || vols[0].Object != "addon/kv" {
		t.Fatalf("deleting volumes %+v %v", vols, err)
	}
	if err := s.EnvCtlTouchVolume(bg, env.ID, "addon/none"); err != store.ErrNoRow {
		t.Fatalf("touch a volume that is not there: %v", err)
	}
	kv, _ := s.GetEnvVolume(bg, env.ID, "addon/kv")
	byID, err := s.EnvCtlVolumesByID(bg, []uuid.UUID{kv.VolumeID, uuid.New()})
	if err != nil || len(byID) != 1 || byID[kv.VolumeID].Object != "addon/kv" {
		t.Fatalf("volumes by id %+v %v", byID, err)
	}
	if byID, err := s.EnvCtlVolumesByID(bg, nil); err != nil || len(byID) != 0 {
		t.Fatalf("volumes by no id %+v %v", byID, err)
	}

	// Which environments and sandboxes exist.
	gone := uuid.New()
	envs, sbs, err := s.EnvCtlExisting(bg, []uuid.UUID{env.ID, gone}, []uuid.UUID{sbx.ID, gone})
	if err != nil || !envs[env.ID] || envs[gone] || !sbs[sbx.ID] || sbs[gone] {
		t.Fatalf("existing %v %v %v", envs, sbs, err)
	}
	if envs, sbs, err := s.EnvCtlExisting(bg, nil, nil); err != nil || len(envs) != 0 || len(sbs) != 0 {
		t.Fatalf("existing of none %v %v %v", envs, sbs, err)
	}
}
