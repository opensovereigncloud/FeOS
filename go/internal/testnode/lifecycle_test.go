// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/pki"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// After a renewal the node keeps its new identity, dials a new node
// connection and a new uplink with it, and leaves the old connection to
// drain on the control plane's side.
func TestRenewRedialsWithTheNewCertificate(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	dir := t.TempDir()
	n := e.start("n1", in, func(c *Config) { c.StateDir = dir })
	old := n.Serial()
	eventually(t, 5*time.Second, "a report with the first certificate", func() bool { return e.f.reportsFrom(old) > 0 })

	if err := n.Renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	renewed := n.Serial()
	if renewed == old {
		t.Fatal("the serial did not change")
	}
	if id, err := loadIdentity(dir, testTD); err != nil || id.serial() != renewed {
		t.Fatalf("kept identity: %v", err)
	}
	eventually(t, 5*time.Second, "a node connection and an uplink with the new certificate", func() bool {
		return e.f.acceptChecks(renewed) >= 2 && e.f.reportsFrom(renewed) > 0
	})
	// The old node connection drains on the control plane's side, which
	// closes it once it is idle.
	eventually(t, 5*time.Second, "one connection left", func() bool {
		return in.srv.Connections("n1") == 1 && len(n.Connections()) == 1
	})
	reportsOld := e.f.reportsFrom(old)
	never(t, 1500*time.Millisecond, "a report with the old certificate", func() bool { return e.f.reportsFrom(old) != reportsOld })
	if _, err := pods(t, in, "n1").ListPods(context.Background(), &podv1.ListPodsRequest{}); err != nil {
		t.Fatalf("a call on the new connection: %v", err)
	}
}

// The node renews by itself at renew_after, and not again before the
// minimum gap.
func TestRenewAtRenewAfter(t *testing.T) {
	e := newEnv(t)
	// NotBefore lies 10 minutes back: renew_after = NotBefore + 1/2 of
	// (10 min + ttl) is about 1.5 s ahead with this ttl.
	e.f.ttl = 603 * time.Second
	in := e.instance("i1", caIss)
	cfg := e.config("n1", in)
	n := e.newNode(cfg)
	n.renewMinGap = time.Hour
	e.run(n)
	eventually(t, 10*time.Second, "connected", func() bool { return in.srv.Connected("n1") })
	first := n.Serial()
	eventually(t, 10*time.Second, "renewed", func() bool { return n.Serial() != first })
	never(t, 2*time.Second, "a second renewal", func() bool {
		e.f.mu.Lock()
		defer e.f.mu.Unlock()
		return e.f.renewals != 1
	})
}

// A node connection without a call survives two minutes of the control
// plane's pings every 10 s.
func TestIdleNodeConnectionSurvivesPings(t *testing.T) {
	if testing.Short() {
		t.Skip("runs two minutes")
	}
	t.Parallel()
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	serial := n.Serial()
	eventually(t, 5*time.Second, "the uplink", func() bool { return e.f.reportsFrom(serial) > 0 })
	accepts := e.f.acceptChecks(serial)
	never(t, 2*time.Minute, "the node connection dropped", func() bool {
		return !in.srv.Connected("n1") || len(n.Connections()) != 1
	})
	if got := e.f.acceptChecks(serial); got != accepts {
		t.Fatalf("redialled: %d accept checks, were %d", got, accepts)
	}
}

func TestCrashAndRestart(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	uid, _ := createRunning(t, pods(t, in, "n1"))
	start := n.StartID()
	n.Crash()
	eventually(t, 5*time.Second, "disconnected", func() bool { return !in.srv.Connected("n1") })
	if len(n.Connections()) != 0 || n.UplinkAddr() != "" {
		t.Fatalf("connections %v, uplink %q", n.Connections(), n.UplinkAddr())
	}
	reports := e.f.reportsFrom(n.Serial())
	never(t, 1500*time.Millisecond, "a crashed node reported or dialled", func() bool {
		return e.f.reportsFrom(n.Serial()) != reports || in.srv.Connected("n1")
	})
	n.Restart(false)
	eventually(t, 5*time.Second, "reconnected", func() bool { return in.srv.Connected("n1") })
	if n.StartID() == start {
		t.Fatal("same start after a crash")
	}
	if p := n.Pods(); len(p) != 1 || p[0].PodId != uid.String() || p[0].State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("pods %v", p)
	}
	eventually(t, 5*time.Second, "the new start's report", func() bool {
		r, ok := e.f.lastReport()
		return ok && r.t.StartId == n.StartID() && r.t.PodsRunning == 1
	})
	for _, s := range e.f.storedEvents() {
		if s.ev.GetNode().GetKind() == "stopping" {
			t.Fatal("a crash said goodbye")
		}
	}
}

// A graceful restart publishes "stopping" and waits for its
// acknowledgement, lets a call in flight finish, and comes back under a
// new start.
func TestGracefulRestart(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	first := n.StartID()
	uid, _ := createRunning(t, pods(t, in, "n1"))
	eventually(t, 5*time.Second, "started stored", func() bool { return len(e.f.storedEvents()) == 3 })

	var wg sync.WaitGroup
	var r execResult
	var execErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, execErr = execOut(t, in, "n1", uid.String(), "bash", "-c", "touch started; sleep 1; echo done")
	}()
	waitStarted(t, n, uid)
	n.Restart(true)
	stored := e.f.storedEvents()
	if s := stored[len(stored)-1]; s.start != first || s.ev.GetNode().GetKind() != "stopping" {
		t.Fatalf("not acknowledged before the restart: %v", s)
	}
	wg.Wait()
	if execErr != nil || r.stdout != "done\n" {
		t.Fatalf("the call in flight: %+v, %v", r, execErr)
	}
	if n.StartID() == first {
		t.Fatal("same start")
	}
	eventually(t, 5*time.Second, "back", func() bool { return in.srv.Connected("n1") })
	eventually(t, 5*time.Second, "the new start's first event", func() bool {
		got := e.f.storedEvents()
		last := got[len(got)-1]
		return last.start == n.StartID() && last.ev.Seq == 1 && last.ev.GetNode().GetKind() == "started"
	})
}

// waitStarted waits until a command of execOut began: it touches
// "started" in the pod's directory.
func waitStarted(t *testing.T, n *Node, uid podid.ID) {
	t.Helper()
	eventually(t, 10*time.Second, "the command started", func() bool {
		_, err := os.Stat(filepath.Join(n.PodDir(uid), "started"))
		return err == nil
	})
}

// A graceful restart dials nothing under the old start while its
// connections drain: a connection that drained at once is not dialled
// again and then cut, and the calls in flight on the others finish.
func TestGracefulRestartDialsNothingWhileDraining(t *testing.T) {
	e := newEnv(t)
	i1 := e.instance("i1", caIss)
	i2 := e.instance("i2", caIss)
	e.f.setInstances(i1.listing(), i2.listing())
	n := e.start("n1", i1)
	eventually(t, 5*time.Second, "connected to both", func() bool {
		return i2.srv.Connected("n1") && slices.Equal(n.Connections(), sortedAddrs(i1, i2))
	})
	first := n.StartID()
	uid, _ := createRunning(t, pods(t, i1, "n1"))

	var restarting atomic.Bool
	var late atomic.Int32
	e.f.mu.Lock()
	e.f.onAccept = func(c check) {
		if restarting.Load() && n.StartID() == first {
			late.Add(1)
		}
	}
	e.f.mu.Unlock()

	var wg sync.WaitGroup
	var r execResult
	var execErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, execErr = execOut(t, i1, "n1", uid.String(), "bash", "-c", "touch started; sleep 1.5; echo done")
	}()
	waitStarted(t, n, uid)
	restarting.Store(true)
	n.Restart(true)
	restarting.Store(false)
	wg.Wait()
	if execErr != nil || r.stdout != "done\n" {
		t.Fatalf("the call in flight: %+v, %v", r, execErr)
	}
	if got := late.Load(); got != 0 {
		t.Fatalf("%d connections accepted under the old start while it drained", got)
	}
	eventually(t, 5*time.Second, "back on both", func() bool {
		return i1.srv.Connected("n1") && i2.srv.Connected("n1")
	})
}

// An instance no longer listed loses its node connection gracefully: the
// control plane gets a GOAWAY, and a call in flight there finishes.
func TestUnlistedInstanceDrains(t *testing.T) {
	e := newEnv(t)
	i1 := e.instance("i1", caIss)
	i2 := e.instance("i2", caIss)
	e.f.setInstances(i1.listing(), i2.listing())
	n := e.start("n1", i1)
	eventually(t, 5*time.Second, "connected to both", func() bool { return i2.srv.Connected("n1") })
	uid, _ := createRunning(t, pods(t, i1, "n1"))

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	n.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out ExecOutput) (int32, bool, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return 0, true, status.FromContextError(ctx.Err()).Err()
		}
		return 0, true, out.Stdout([]byte("done\n"))
	})
	var wg sync.WaitGroup
	var r execResult
	var execErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, execErr = execOut(t, i2, "n1", uid.String(), "true")
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the exec did not start")
	}
	e.f.setInstances(i1.listing())
	eventually(t, 10*time.Second, "the GOAWAY at i2", func() bool { return !i2.srv.Connected("n1") })
	close(release)
	wg.Wait()
	if execErr != nil || r.stdout != "done\n" {
		t.Fatalf("the call in flight at the unlisted instance: %+v, %v", r, execErr)
	}
	eventually(t, 5*time.Second, "only the listed instance", func() bool {
		return slices.Equal(n.Connections(), []string{i1.addr()}) && rawConns(n, i2.addr()) == 0
	})
}

func TestPartition(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	createRunning(t, pods(t, in, "n1"))
	start := n.StartID()
	n.Partition(true)
	eventually(t, 5*time.Second, "cut off", func() bool { return !in.srv.Connected("n1") })
	reports := e.f.reportsFrom(n.Serial())
	never(t, 1500*time.Millisecond, "reported while cut off", func() bool { return e.f.reportsFrom(n.Serial()) != reports })
	if p := n.Pods(); len(p) != 1 || p[0].State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("pods %v", p)
	}
	n.Partition(false)
	eventually(t, 5*time.Second, "healed", func() bool { return in.srv.Connected("n1") })
	if n.StartID() != start {
		t.Fatal("a partition restarted the node")
	}
}

// The node keeps a node connection to every listed instance, drops those
// no longer listed, and moves its uplink when its instance goes.
func TestFollowsTheInstances(t *testing.T) {
	e := newEnv(t)
	i1 := e.instance("i1", caIss)
	i2 := e.instance("i2", caIss)
	e.f.setInstances(i1.listing(), i2.listing())
	n := e.start("n1", i1)
	eventually(t, 5*time.Second, "connected to both", func() bool {
		return i2.srv.Connected("n1") && slices.Equal(n.Connections(), sortedAddrs(i1, i2))
	})
	eventually(t, 5*time.Second, "the uplink on the first", func() bool { return n.UplinkAddr() == i1.addr() })

	i1.shutdown(t)
	eventually(t, 10*time.Second, "the uplink moved", func() bool {
		r, ok := e.f.lastReport()
		return ok && r.instance == "i2" && n.UplinkAddr() == i2.addr()
	})

	e.f.setInstances(i2.listing())
	eventually(t, 5*time.Second, "only the listed instance", func() bool {
		return slices.Equal(n.Connections(), []string{i2.addr()})
	})
	if !i2.srv.Connected("n1") {
		t.Fatal("lost the listed instance")
	}
}

// The node dials an uplink to every listed instance and uses one; when
// the instance of the one it uses is no longer listed, it moves to another
// without a failure, and closes the connections to the unlisted one.
func TestUplinkMovesOffAnUnlistedInstance(t *testing.T) {
	e := newEnv(t)
	i1 := e.instance("i1", caIss)
	i2 := e.instance("i2", caIss)
	e.f.setInstances(i1.listing(), i2.listing())
	n := e.start("n1", i1)
	serial := n.Serial()
	eventually(t, 5*time.Second, "the uplink on the first", func() bool { return n.UplinkAddr() == i1.addr() })
	eventually(t, 5*time.Second, "a node connection and an uplink to each", func() bool {
		return rawConns(n, i1.addr()) == 2 && rawConns(n, i2.addr()) == 2 &&
			e.f.acceptsAt("i1", serial) == 2 && e.f.acceptsAt("i2", serial) == 2
	})

	e.f.setInstances(i2.listing())
	eventually(t, 10*time.Second, "the uplink moved to the listed instance", func() bool {
		r, ok := e.f.lastReport()
		return ok && r.instance == "i2" && n.UplinkAddr() == i2.addr()
	})
	eventually(t, 10*time.Second, "no connection to the unlisted instance", func() bool { return rawConns(n, i1.addr()) == 0 })
	if got := e.f.acceptsAt("i2", serial); got != 2 {
		t.Fatalf("i2 accepted %d connections, want its node connection and uplink only", got)
	}
}

// A renewal moves the node to new uplinks before it closes the old ones,
// and closes an old one only once the calls begun on it have ended.
func TestRenewKeepsCallsInFlight(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	old := n.Serial()
	eventually(t, 5*time.Second, "a report", func() bool { return e.f.reportsFrom(old) > 0 })

	e.f.mu.Lock()
	e.f.credsGate, e.f.credsEntered = make(chan struct{}), make(chan struct{}, 1)
	gateCh, entered := e.f.credsGate, e.f.credsEntered
	e.f.mu.Unlock()
	creds := make(chan error, 1)
	go func() {
		_, err := n.IssuePodCredentials(context.Background(), podid.Mint())
		creds <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not arrive")
	}

	var noUplink atomic.Int32
	watching, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-watching:
				return
			default:
			}
			if n.UplinkAddr() == "" {
				noUplink.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := n.Renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	renewed := n.Serial()
	eventually(t, 5*time.Second, "a report on the new uplink", func() bool { return e.f.reportsFrom(renewed) > 0 })
	close(watching)
	<-watched
	if got := noUplink.Load(); got != 0 {
		t.Fatalf("the node had no uplink %d times during the renewal", got)
	}

	e.f.mu.Lock()
	e.f.credsGate = nil
	e.f.mu.Unlock()
	close(gateCh)
	select {
	case err := <-creds:
		if err != nil {
			t.Fatalf("the call in flight on the old uplink: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call in flight did not end")
	}
	eventually(t, 5*time.Second, "the old uplink closed", func() bool { return rawConns(n, in.addr()) == 2 })
}

// A renewal that takes long holds back no report.
func TestSlowRenewalKeepsReporting(t *testing.T) {
	e := newEnv(t)
	// renew_after about 1.5 s ahead, as in TestRenewAtRenewAfter.
	e.f.ttl = 603 * time.Second
	e.f.renewGate, e.f.renewEntered = make(chan struct{}), make(chan struct{}, 1)
	gateCh, entered := e.f.renewGate, e.f.renewEntered
	in := e.instance("i1", caIss)
	n := e.newNode(e.config("n1", in))
	n.renewMinGap = time.Hour
	e.run(n)
	eventually(t, 10*time.Second, "connected", func() bool { return in.srv.Connected("n1") })
	old := n.Serial()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no renewal")
	}
	before := e.f.reportsFrom(old)
	eventually(t, 5*time.Second, "reports while the renewal waits", func() bool { return e.f.reportsFrom(old) >= before+2 })
	e.f.mu.Lock()
	e.f.renewGate = nil
	e.f.mu.Unlock()
	close(gateCh)
	eventually(t, 10*time.Second, "renewed", func() bool { return n.Serial() != old })
}

func sortedAddrs(ins ...*instance) []string {
	var out []string
	for _, in := range ins {
		out = append(out, in.addr())
	}
	slices.Sort(out)
	return out
}

func TestIssuePodCredentials(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	eventually(t, 5*time.Second, "an uplink", func() bool { return n.UplinkAddr() != "" })
	pod := podid.Mint()
	want := &fleetv1.PodBindings{Kube: []*fleetv1.KubeBinding{{Name: "prod", Port: 6443, Server: "https://k8s.example.test", Credential: &fleetv1.KubeBinding_Token{Token: "secret"}}}}
	e.f.mu.Lock()
	e.f.bindings = want
	e.f.mu.Unlock()
	c, err := n.IssuePodCredentials(t.Context(), pod)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(c.Bindings, want) || c.Version != "v7" {
		t.Fatalf("bindings %v version %q", c.Bindings, c.Version)
	}
	if len(c.Leaf.URIs) != 1 || c.Leaf.URIs[0].String() != pki.SandboxID(testTD, "t1", pod) {
		t.Fatalf("certificate names %v", c.Leaf.URIs)
	}
	// A repeat with the same keys, whose answer was lost.
	again, err := n.IssuePodCredentialsWith(t.Context(), pod, c.Key, c.HPKEKey)
	if err != nil || !proto.Equal(again.Bindings, want) {
		t.Fatalf("repeat: %v", err)
	}
	e.f.mu.Lock()
	e.f.bindings = nil
	e.f.mu.Unlock()
	none, err := n.IssuePodCredentials(t.Context(), pod)
	if err != nil || none.Bindings != nil || none.Version != "" {
		t.Fatalf("without bindings: %v, %v", none, err)
	}
}
