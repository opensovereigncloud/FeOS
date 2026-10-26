// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	networkv1 "github.com/ironcore-dev/feos/go/gen/feos/network/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// The env group is on a node before the pod that names it (the node
// checks), with the instances' addresses as members; a member that comes
// or goes is put again with a new version; a group a node has with other
// content is put again.
func TestEnvGroups(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	id := model.EnvGroupID(env.ID)
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	one := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	var g1 *networkv1.SecurityGroup
	eventually(t, 30*time.Second, "the group with its member", func() bool {
		g1 = h.group("n1", id)
		return g1 != nil && slices.Equal(g1.Members, []string{one[0].Sandbox.Address})
	})
	if !slices.EqualFunc(g1.Rules, envRules(id), func(a, b *networkv1.Rule) bool {
		return sameContent(
			&networkv1.SecurityGroup{Rules: []*networkv1.Rule{a}}, &networkv1.SecurityGroup{Rules: []*networkv1.Rule{b}})
	}) {
		t.Errorf("rules %v", g1.Rules)
	}
	r2 := h.apply(env.ID, serviceSpec(2, "1", ""))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 30*time.Second)
	two := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	var g2 *networkv1.SecurityGroup
	eventually(t, 30*time.Second, "the group with both members", func() bool {
		g2 = h.group("n1", id)
		return g2 != nil && len(g2.Members) == 2 && slices.Contains(g2.Members, two[1].Sandbox.Address)
	})
	if g2.Version <= g1.Version {
		t.Errorf("version %d after %d", g2.Version, g1.Version)
	}
	e, _ := h.st.GetEnvironment(bg, env.ID)
	if uint64(e.Gen) != g2.Version {
		t.Errorf("gen %d, version %d", e.Gen, g2.Version)
	}
	r3 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitRelease(env.ID, r3, model.ReleaseRealised, 30*time.Second)
	var g3 *networkv1.SecurityGroup
	eventually(t, 30*time.Second, "the group without the retired member", func() bool {
		g3 = h.group("n1", id)
		return g3 != nil && slices.Equal(g3.Members, []string{one[0].Sandbox.Address})
	})
	if g3.Version <= g2.Version {
		t.Errorf("version %d after %d", g3.Version, g2.Version)
	}
	// Someone else's content at the same version: put again, newer.
	tampered := &networkv1.SecurityGroup{Id: id, Version: g3.Version, Rules: egressTable()}
	if _, err := networkv1.NewSecurityGroupServiceClient(h.conn("n1")).PutSecurityGroup(bg,
		&networkv1.PutSecurityGroupRequest{Group: tampered}); err != nil {
		t.Fatal(err)
	}
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the group repaired", func() bool {
		g := h.group("n1", id)
		return g != nil && g.Version > g3.Version && len(g.Rules) == 4 && len(g.Members) == 1
	})
	// A group of an environment that is gone goes from the node.
	gone := model.EnvGroupID(uuid.New())
	if _, err := networkv1.NewSecurityGroupServiceClient(h.conn("n1")).PutSecurityGroup(bg,
		&networkv1.PutSecurityGroupRequest{Group: &networkv1.SecurityGroup{Id: gone, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the stale group gone", func() bool { return h.group("n1", gone) == nil })
}

// Deleting an environment leaves nothing: its instances and their pods,
// its volume on its node, its attached rows, its group on the node, its
// row. A deleting tenant's environments are deleted too.
func TestEnvDeletion(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, `{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"services":{"api":{"image":"`+testImage+`","replicas":2}}}`)
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 60*time.Second)
	var pods []podid.ID
	for _, r := range h.rows(env.ID) {
		pods = append(pods, r.Sandbox.ID)
	}
	if len(pods) != 3 || len(n1.Volumes()) != 1 || h.group("n1", model.EnvGroupID(env.ID)) == nil {
		t.Fatalf("before: %d pods, %d volumes", len(pods), len(n1.Volumes()))
	}
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	for _, p := range pods {
		if info := h.pod("n1", p); info != nil {
			t.Errorf("pod %s stays: %v", p, info.State)
		}
	}
	if len(n1.Volumes()) != 0 {
		t.Errorf("volumes stay: %v", n1.Volumes())
	}
	// env-groups may have put the group again from rows it read just
	// before they went; its next pass removes it as of an environment
	// that is gone.
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the env group gone", func() bool { return h.group("n1", model.EnvGroupID(env.ID)) == nil })
	var n int
	if err := h.d.Pool.QueryRow(bg, `SELECT (SELECT count(*) FROM sandboxes WHERE environment_id IS NOT NULL)
		+ (SELECT count(*) FROM env_volumes) + (SELECT count(*) FROM env_secrets) + (SELECT count(*) FROM env_releases)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows stay, %v", n, err)
	}

	// A deleting tenant's environment.
	if _, _, err := h.st.CreateTenant(bg, "t2"); err != nil {
		t.Fatal(err)
	}
	e2, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "t2", Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	r := h.apply(e2.ID, serviceSpec(1, "1", ""))
	h.waitRelease(e2.ID, r, model.ReleaseRealised, 30*time.Second)
	if _, err := h.st.MarkTenantDeleting(bg, "t2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "the tenant's environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, e2.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	// A deleting tenant gets no new instance.
	if _, _, err := h.st.CreateTenant(bg, "t3"); err != nil {
		t.Fatal(err)
	}
	e3, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "t3", Name: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.MarkTenantDeleting(bg, "t3"); err != nil {
		t.Fatal(err)
	}
	v, err := h.c.load(bg, e3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatal("a view of an environment without a release")
	}
	vw := &view{env: e3, target: &spec.Spec{}, latestSpec: &spec.Spec{}}
	p := h.c.planService(vw, "web", spec.Service{Image: testImage})
	if _, err := h.c.insertInstance(bg, vw, insert{plan: p, release: 1}); !errors.Is(err, errTenantDeleting) {
		t.Errorf("an insert for a deleting tenant: %v", err)
	}
}

// LogTail reads a replica's container log with tail_lines, a job's kept
// tail, refuses a paused instance (ErrPaused, without waking it) and maps
// a node without logs to ErrNoContainerLogs; RestartService restarts the
// replicas one at a time.
func TestLogsAndRestart(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"image":%q,"replicas":2},
		"idler":{"image":%q,"idle":"15m"}},"jobs":{"once":{"image":%q,"on":"manual"}}}`, testImage, testImage, testImage))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	rows := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	for i := range 5 {
		_ = n1.Script().AppendLog(rows[1].Sandbox.ID, "main", fmt.Sprintf("line %d", i))
	}
	lines, source, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 1, 2)
	if err != nil || source != SourceContainer || !slices.Equal(lines, []string{"line 3", "line 4"}) {
		t.Fatalf("logs %q %s %v", lines, source, err)
	}
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 5, 2); !errors.Is(err, ErrNoObject) ||
		err.Error() != "service web has no replica 5 (it has 2)" {
		t.Errorf("an unknown replica: %v", err)
	}
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindAddon, "pg", 0, 2); err != ErrNoObject {
		t.Errorf("an unknown addon: %v", err)
	}
	if _, _, err := h.c.LogTail(bg, uuid.New(), spec.KindService, "web", 0, 2); err != ErrNoObject {
		t.Errorf("an unknown environment: %v", err)
	}
	// A node that predates the logs.
	n1.Script().SetHook(containerv1.ContainerService_StreamContainerLogs_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Unimplemented, "unknown method")
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 2); !errors.Is(err, ErrNoContainerLogs) {
		t.Errorf("an old node: %v", err)
	}
	n1.Script().SetHook(containerv1.ContainerService_StreamContainerLogs_FullMethodName, nil)
	// A paused (idle) instance is not woken for its logs.
	idler := h.waitRows(env.ID, spec.KindService, "idler", 1, 10*time.Second, "ready", ready)[0]
	if err := h.eng.Pause(bg, idler.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState == model.StatePaused && sb.Op == ""
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "idler", 0, 2); !errors.Is(err, ErrPaused) {
		t.Errorf("a paused instance: %v", err)
	}
	never(t, 3*time.Second, "the idle instance was woken", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState != model.StatePaused
	})
	// A job's log once its row is gone: the tail its run kept.
	if err := h.c.RunJob(bg, env.ID, "once"); err != nil {
		t.Fatal(err)
	}
	var job podid.ID
	eventually(t, 30*time.Second, "the job running", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindJob, "once") {
			if running(r) && h.pod("n1", r.Sandbox.ID) != nil {
				job = r.Sandbox.ID
				return true
			}
		}
		return false
	})
	_ = n1.Script().AppendLog(job, "main", "job says hi")
	if err := n1.Script().ExitContainer(job, "main", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the job's row gone", func() bool {
		_, err := h.st.GetSandbox(bg, job)
		return err != nil
	})
	if lines, _, err := h.c.LogTail(bg, env.ID, spec.KindJob, "once", 0, 10); err != nil || !slices.Equal(lines, []string{"job says hi"}) {
		t.Errorf("the job's kept log %q %v", lines, err)
	}

	// RestartService: one replica at a time, each once the one before is
	// ready again.
	var mu sync.Mutex
	var starts []string
	n1.Script().SetHook(podv1.PodService_StartPod_FullMethodName, func(_ context.Context, _ string, req any) error {
		mu.Lock()
		defer mu.Unlock()
		starts = append(starts, req.(*podv1.StartPodRequest).GetPodId())
		return nil
	})
	if err := h.c.RestartService(bg, env.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.RestartService(bg, env.ID, "nope"); err != ErrNoObject {
		t.Errorf("restart an unknown service: %v", err)
	}
	eventually(t, 60*time.Second, "both replicas restarted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(starts) >= 2
	})
	mu.Lock()
	if starts[0] != rows[0].Sandbox.ID.String() || starts[1] != rows[1].Sandbox.ID.String() {
		t.Errorf("restarts %v", starts)
	}
	mu.Unlock()
	h.waitRows(env.ID, spec.KindService, "web", 2, 30*time.Second, "ready after the restarts", ready)
}

// An instance pending without capacity for long opens no_capacity, which
// closes once its object places.
func TestNoCapacity(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1", func(c *testnode.Config) { c.MemoryBytes = 8 << 30 })
	env := h.newEnv("stage")
	h.apply(env.ID, fmt.Sprintf(`{"services":{"big":{"image":%q,"resources":{"memoryMib":32768}}}}`, testImage))
	inc := h.incident(env.ID, "service/big", model.ConditionNoCapacity, 30*time.Second)
	if !strings.Contains(inc.Summary, "found no capacity") {
		t.Errorf("summary %q", inc.Summary)
	}
	r2 := h.apply(env.ID, fmt.Sprintf(`{"services":{"big":{"image":%q,"resources":{"memoryMib":512}}}}`, testImage))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 30*time.Second)
	eventually(t, 30*time.Second, "the incident closed", func() bool { return len(h.incidents(env.ID, true)) == 0 })
}

// An environment's ID taken again after its deletion is another
// environment: the controller brings up the new one's release 1, not the
// spec it read of the old one's.
func TestEnvIDReused(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"old":{"image":%q}}}`, testImage))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	again, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: env.ID, Owner: testTenant, Name: "stage"})
	if err != nil {
		t.Fatal(err)
	}
	r := h.apply(again.ID, fmt.Sprintf(`{"services":{"new":{"image":%q}}}`, testImage))
	h.waitRelease(again.ID, r, model.ReleaseRealised, 30*time.Second)
	rows := h.rows(again.ID)
	if len(rows) != 1 || rows[0].Instance.Object != "new" {
		t.Fatalf("rows of the new environment %+v", rows)
	}
}
