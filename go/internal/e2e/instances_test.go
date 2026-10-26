// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/config"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// timedReply is the answer of a request made in the background, and how
// long it took.
type timedReply struct {
	r    reply
	err  error
	took time.Duration
}

// execAsync runs command over REST through in in the background.
func (c *cluster) execAsync(in *instance, sb sandbox, command string) <-chan timedReply {
	done := make(chan timedReply, 1)
	go func() {
		start := time.Now()
		r, err := c.execErr(bg, in, sb, command)
		done <- timedReply{r, err, time.Since(start)}
	}()
	return done
}

// expectOutput checks that a background exec answered 200 with exit code
// 0 and output want.
func expectOutput(t *testing.T, what string, res timedReply, want string) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("%s: %v", what, res.err)
	}
	out, err := decodeExec(res.r)
	if err != nil || out.ExitCode != 0 || out.Output != want {
		t.Fatalf("%s: %+v %v, want %q", what, out, err, want)
	}
}

// markExecs signals on the channel it returns whenever the node begins an
// exec whose command contains marker; the command then runs.
func markExecs(n *node, marker string) <-chan struct{} {
	ch := make(chan struct{}, 16)
	n.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if argv := req.GetCommand(); strings.Contains(argv[len(argv)-1], marker) {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		return 0, false, nil
	})
	return ch
}

// Scenario 5: when the leader stops gracefully, it releases its lease:
// another instance leads within the lease's TTL, and its loops run.
func TestLeaderFailover(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) { cfg.IdlePause = time.Second })
	c.start("a")
	c.start("b")
	c.start("c")
	c.join("n1")
	old := c.leader()
	_, epoch, _, err := old.Elector.Leader(bg)
	if err != nil {
		t.Fatal(err)
	}

	begun := time.Now()
	stopped := old.stopAsync()
	next := c.leader()
	if took := time.Since(begun); took > config.LeaderLeaseTTL {
		t.Errorf("a new leader after %v; the lease's TTL is %v", took, config.LeaderLeaseTTL)
	}
	if next == old {
		t.Fatal("the stopping instance still leads")
	}
	holder, epoch2, _, err := next.Elector.Leader(bg)
	if err != nil || holder != next.ID() || epoch2 <= epoch {
		t.Fatalf("lease %s epoch %d (was %d): %v", holder, epoch2, epoch, err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}

	// The new leader's loops run: an idle sandbox is paused.
	sb := c.create(nil)
	c.run(nil, sb, "true")
	c.waitState(sb.id, model.StatePaused, 30*time.Second)
}

// Scenario 5: the leader crashes. Cut off from the database, it can
// neither renew nor release its lease: it steps down by its own deadline
// (the last renew's send plus the TTL less the margin), before the lease
// expires; another instance leads once the lease has expired, within the
// TTL and one read interval, in a later epoch; at no time do two
// instances lead; the new leader's loops run, and the old instance does
// not lead again when it is back.
func TestLeaderCrash(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) { cfg.IdlePause = time.Second })
	proxy := c.newDBProxy()
	old := c.start("a", func(cfg *config.Config) { cfg.DatabaseURL = proxy.dsn })
	eventually(t, 30*time.Second, "a leading", old.Elector.IsLeader)
	others := []*instance{c.start("b"), c.start("c")}
	c.join("n1")
	_, epoch, _, err := old.Elector.Leader(bg)
	if err != nil {
		t.Fatal(err)
	}

	// The last renew that succeeded was sent before the cut, so the
	// old leader's deadline and the lease's expiry both lie within the
	// step-down time and the TTL from here.
	cut := time.Now()
	proxy.cutOff()
	var next *instance
	var steppedDown time.Duration
	eventually(t, 60*time.Second, "another leader", func() bool {
		if steppedDown == 0 && !old.Elector.IsLeader() {
			steppedDown = time.Since(cut)
		}
		for _, in := range others {
			if in.Elector.IsLeader() {
				if old.Elector.IsLeader() {
					t.Fatalf("%s and the cut-off instance lead at once", in.name)
				}
				next = in
				return true
			}
		}
		return false
	})
	took := time.Since(cut)
	t.Logf("the cut-off leader stepped down after %v; %s leads after %v", steppedDown, next.name, took)
	// The slack covers the polls and a loaded machine.
	const slack = 2 * time.Second
	if limit := leader.LeaseTTL - leader.StepDownMargin + slack; steppedDown == 0 || steppedDown > limit {
		t.Errorf("the cut-off leader stepped down after %v; at most %v", steppedDown, limit)
	}
	// The last renew that reached the database was at most one renew
	// interval before the cut: the lease lasted at least the rest.
	if limit := leader.LeaseTTL - leader.RenewInterval - time.Second; took < limit {
		t.Errorf("a new leader after %v, before the lease can have expired (%v)", took, limit)
	}
	if limit := leader.LeaseTTL + leader.ReadInterval + slack; took > limit {
		t.Errorf("a new leader after %v; at most the TTL and a read interval, %v", took, limit)
	}
	holder, epoch2, _, err := next.Elector.Leader(bg)
	if err != nil || holder != next.ID() || epoch2 <= epoch {
		t.Fatalf("lease %s epoch %d (was %d): %v", holder, epoch2, epoch, err)
	}

	// The old instance is back; the new leader's loops run: an idle
	// sandbox is paused.
	proxy.heal()
	sb := c.create(next)
	c.run(next, sb, "true")
	c.waitState(sb.id, model.StatePaused, 30*time.Second)
	if old.Elector.IsLeader() {
		t.Fatal("the instance that was cut off leads again")
	}
	if holder, epoch3, _, err := next.Elector.Leader(bg); err != nil || holder != next.ID() || epoch3 != epoch2 {
		t.Fatalf("lease %s epoch %d (was %d): %v", holder, epoch3, epoch2, err)
	}
}

// Scenario 5: a rolling restart. The stopping instance finishes the tool
// call it serves; one that reaches it in its readiness window and needs a
// resume succeeds; its op in flight is taken over by another instance at
// once, so no sandbox waits longer than the instance TTL and the op;
// nothing stays owned by it; a new instance takes its place.
func TestRollingRestart(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a", func(cfg *config.Config) { cfg.ReadinessDelay = 3 * time.Second })
	b := c.start("b")
	n1 := c.join("n1")
	s1 := c.create(nil)
	c.run(b, s1, "true")
	s2 := c.create(nil)
	c.run(b, s2, "echo two > f")
	c.pause(b, s2)
	s3 := c.create(nil)
	c.run(b, s3, "echo three > f")
	c.pause(b, s3)

	const resumeTakes = 4 * time.Second
	n1.Script().SetDelay(podv1.PodService_ResumePod_FullMethodName, resumeTakes)
	slowStarted := markExecs(n1, "e2e-slow")
	slow := c.execAsync(a, s1, "sleep 3; echo slow # e2e-slow")
	select {
	case <-slowStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow exec did not start")
	}
	onA := c.execAsync(a, s3, "cat f")
	eventually(t, 30*time.Second, "a's resume of s3 in flight", func() bool {
		row := c.row(s3.id)
		return row.Op == "resume" && row.OpOwner == a.ID()
	})

	stopped := a.stopAsync()
	eventually(t, 10*time.Second, "a failing /readyz", func() bool {
		r, err := c.callErr(bg, a, "GET", "/readyz", "", "")
		return err == nil && r.code == http.StatusServiceUnavailable
	})
	// In the readiness window: a request that needs a resume.
	window := c.execAsync(a, s2, "cat f")
	// Elsewhere: the sandbox whose op the stopping instance owns.
	onB := c.execAsync(b, s3, "cat f")

	resB := <-onB
	expectOutput(t, "s3 on b", resB, "three\n")
	if limit := config.InstanceTTL + resumeTakes + 5*time.Second; resB.took > limit {
		t.Errorf("s3 waited %v on b; at most %v", resB.took, limit)
	}
	expectOutput(t, "the slow exec on the stopping instance", <-slow, "slow\n")
	expectOutput(t, "s3 on the stopping instance", <-onA, "three\n")
	expectOutput(t, "s2 in the readiness window", <-window, "two\n")
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if n := c.count("SELECT count(*) FROM sandboxes WHERE op_owner = $1", a.ID()); n != 0 {
		t.Fatalf("%d sandboxes still owned by the stopped instance", n)
	}
	if n := c.count("SELECT count(*) FROM instances WHERE id = $1", a.ID()); n != 0 {
		t.Fatal("the stopped instance's row stays")
	}

	// The restart goes on: a new instance, then the other one stops.
	n1.Script().SetDelay(podv1.PodService_ResumePod_FullMethodName, 0)
	a2 := c.start("a2")
	c.waitNode(n1)
	if err := b.stop(); err != nil {
		t.Fatal(err)
	}
	for _, sb := range []sandbox{s1, s2, s3} {
		if out := c.run(a2, sb, "echo ok"); out != "ok\n" {
			t.Fatalf("after the restart: %q", out)
		}
	}
	c.leader()
}

// Scenario 5: a test node restarts gracefully while a sandbox of it is
// paused: a tool call for that sandbox waits for the node beyond the node
// wait, within the rollout grace, and succeeds once the node is back; the
// exec in flight on the node finishes.
func TestNodeGracefulRestart(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) {
		cfg.NodeWait = time.Second
		cfg.RolloutGrace = 30 * time.Second
	})
	a := c.start("a")
	b := c.start("b")
	n1 := c.join("n1")
	s1 := c.create(nil)
	c.run(nil, s1, "true")
	s2 := c.create(nil)
	c.run(nil, s2, "echo two > f")
	c.pause(nil, s2)

	slowStarted := markExecs(n1, "e2e-slow")
	slow := c.execAsync(a, s1, "sleep 4; echo slow # e2e-slow")
	select {
	case <-slowStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow exec did not start")
	}
	restarted := make(chan struct{})
	go func() {
		defer close(restarted)
		n1.Restart(true)
	}()
	// The node announced its stop and drains its connections: no
	// instance has a current one.
	eventually(t, 10*time.Second, "n1 stopping and away", func() bool {
		row, err := c.st.GetNode(bg, "n1")
		return err == nil && row.StoppingUntil.After(time.Now()) && !a.Links.Connected("n1") && !b.Links.Connected("n1")
	})
	// Let the instances read the node's rollout deadline (ferun_node).
	time.Sleep(time.Second)
	res := <-c.execAsync(b, s2, "cat f")
	expectOutput(t, "the paused sandbox's call", res, "two\n")
	if res.took <= c.cfg.NodeWait {
		t.Errorf("the call waited %v, within the node wait: the node was not away", res.took)
	}
	expectOutput(t, "the exec in flight", <-slow, "slow\n")
	<-restarted
	if row := c.row(s2.id); row.ClientState != model.StateRunning || row.Node != "n1" {
		t.Fatalf("row %+v", row)
	}
}

// Scenario 6: two instances placing at once never overbook a node: of
// ten sandboxes woken together, as many run as the nodes have slots, the
// others are refused for capacity, and nothing is left booked for them.
func TestConcurrentPlacement(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	b := c.start("b")
	nodes := map[string]*node{"n1": c.join("n1"), "n2": c.join("n2")}
	slots := map[string]int{"n1": 3, "n2": 2}
	for name, s := range slots {
		if r := c.call(nil, "PATCH", "/api/v1/admin/nodes/"+name, adminAuth, fmt.Sprintf(`{"slots":%d}`, s)); r.code != 200 {
			t.Fatalf("slots of %s: %d %s", name, r.code, r.body)
		}
	}
	var sbs []sandbox
	for range 10 {
		sbs = append(sbs, c.create(nil))
	}
	results := make([]reply, len(sbs))
	errs := make([]error, len(sbs))
	var wg sync.WaitGroup
	for i, sb := range sbs {
		in := a
		if i%2 == 1 {
			in = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.execErr(bg, in, sb, "true")
		}()
	}
	wg.Wait()
	ran := 0
	for i, r := range results {
		switch {
		case errs[i] != nil:
			t.Fatalf("exec %d: %v", i, errs[i])
		case r.code == http.StatusOK:
			ran++
		case r.code == http.StatusBadGateway && strings.Contains(r.body, "ensure sandbox: no capacity"):
		default:
			t.Errorf("exec %d: %d %s", i, r.code, r.body)
		}
	}
	if ran != 5 {
		t.Errorf("%d sandboxes run on 5 slots", ran)
	}
	for name, n := range nodes {
		if got := c.count("SELECT count(*) FROM bookings WHERE node = $1", name); got > slots[name] {
			t.Errorf("node %s: %d bookings on %d slots", name, got, slots[name])
		}
		if got := len(n.Pods()); got > slots[name] {
			t.Errorf("node %s: %d pods on %d slots", name, got, slots[name])
		}
	}
	if got := c.count("SELECT count(*) FROM bookings"); got != 5 {
		t.Errorf("%d bookings for 5 running sandboxes", got)
	}
	if got := c.count("SELECT count(*) FROM sandboxes WHERE client_state = 'running'"); got != 5 {
		t.Errorf("%d running sandboxes", got)
	}
}
