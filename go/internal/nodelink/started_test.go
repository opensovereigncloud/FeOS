// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
)

// TestGoawayCallsGoToTheNextConnection: a node that stops sends GOAWAY.
// The calls in flight on its connection end there; a call that had not
// reached it waits for the node's next connection, within its rollout
// deadline, instead of failing UNAVAILABLE "the node's connection is
// gone", as calls did that took the connection just before the GOAWAY
// took it out. Streams, as an exec, do so too.
func TestGoawayCallsGoToTheNextConnection(t *testing.T) {
	for round := range 10 {
		e := newEnv(t)
		n := e.node("n1", time.Hour)
		a := e.dialNode(n, "A")
		eventually(t, 2*time.Second, "A current", func() bool { return e.srv.Connected("n1") })
		e.srv.SetStoppingUntil("n1", time.Now().Add(5*time.Second))
		cc, err := e.srv.Conn(context.Background(), "n1")
		if err != nil {
			t.Fatal(err)
		}

		// A call in flight on A when it stops.
		inFlight := make(chan error, 1)
		go func() {
			_, err := toolClient(cc).Read(context.Background(), readReq("x"))
			inFlight <- err
		}()
		<-a.tools.entered

		var mu sync.Mutex
		var failed []error
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for i := range 8 {
			wg.Add(1)
			// Half unary calls, half streams.
			stream := i%2 == 1
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					var err error
					if stream {
						// A stream opened around the GOAWAY: its first
						// frame comes from one connection or the next.
						var s grpc.ServerStreamingClient[containerv1.ExecStreamFrame]
						s, err = containerv1.NewContainerServiceClient(cc).Exec(ctx, &containerv1.ExecRequest{})
						if err == nil {
							_, err = s.Recv()
						}
					} else {
						_, err = podv1.NewPodServiceClient(cc).GetPod(ctx, &podv1.GetPodRequest{PodId: "p"})
					}
					cancel()
					if err != nil {
						mu.Lock()
						failed = append(failed, err)
						mu.Unlock()
					}
				}
			}()
		}
		time.Sleep(20 * time.Millisecond)
		stopped := make(chan struct{})
		go func() { a.srv.GracefulStop(); close(stopped) }()
		time.Sleep(100 * time.Millisecond)
		close(a.tools.gate)
		if err := <-inFlight; err != nil {
			t.Fatalf("round %d: the call in flight: %v", round, err)
		}
		<-stopped
		e.dialNode(n, "B")
		time.Sleep(100 * time.Millisecond)
		close(stop)
		wg.Wait()
		for _, err := range failed {
			t.Errorf("round %d: a call around the GOAWAY: %v", round, err)
		}
		if len(failed) > 0 {
			return
		}
	}
}

// TestCutCallSaysWhy: a call that reached the node and whose connection
// ended without an answer reads that the node stopped during the call
// when it had announced its stop, and that the connection ended
// otherwise; the node's own UNAVAILABLE, as feosd's "the node stops" for
// a call it cut at its bound, is as the node sent it.
func TestCutCallSaysWhy(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)

	unary := func(stopping bool) error {
		ns := e.dialNode(n, "A")
		eventually(t, 2*time.Second, "current", func() bool { return e.srv.Connected("n1") })
		if stopping {
			e.srv.SetStoppingUntil("n1", time.Now().Add(time.Minute))
		} else {
			e.srv.SetStoppingUntil("n1", time.Time{})
		}
		cc, err := e.srv.Conn(context.Background(), "n1")
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan error, 1)
		go func() {
			_, err := toolClient(cc).Read(context.Background(), readReq("x"))
			got <- err
		}()
		<-ns.tools.entered
		ns.conn.Close()
		err = <-got
		eventually(t, 2*time.Second, "gone", func() bool { return !e.srv.Connected("n1") })
		return err
	}
	if err := unary(true); status.Code(err) != codes.Unavailable ||
		!strings.HasPrefix(status.Convert(err).Message(), "node n1 stopped during the call: ") {
		t.Errorf("a call cut as its node stopped: %v", err)
	}
	if err := unary(false); status.Code(err) != codes.Unavailable ||
		!strings.HasPrefix(status.Convert(err).Message(), "node n1: the connection ended during the call: ") {
		t.Errorf("a call whose connection ended: %v", err)
	}

	// A stream, cut after its first frame.
	ns := e.dialNode(n, "B")
	eventually(t, 2*time.Second, "current", func() bool { return e.srv.Connected("n1") })
	e.srv.SetStoppingUntil("n1", time.Now().Add(time.Minute))
	cc, err := e.srv.Conn(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	exec, err := containerv1.NewContainerServiceClient(cc).Exec(context.Background(), &containerv1.ExecRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Recv(); err != nil {
		t.Fatal(err)
	}
	ns.conn.Close()
	if _, err := exec.Recv(); status.Code(err) != codes.Unavailable ||
		!strings.HasPrefix(status.Convert(err).Message(), "node n1 stopped during the call: ") {
		t.Errorf("a stream cut as its node stopped: %v", err)
	}
	eventually(t, 2*time.Second, "gone", func() bool { return !e.srv.Connected("n1") })

	// The node's own answer.
	ns = e.dialNode(n, "C")
	ns.tools.fail = status.Error(codes.Unavailable, "the node stops: the call did not end within 30s of feosd's stop")
	close(ns.tools.gate)
	eventually(t, 2*time.Second, "current", func() bool { return e.srv.Connected("n1") })
	cc, err = e.srv.Conn(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = toolClient(cc).Read(context.Background(), readReq("x"))
	if status.Convert(err).Message() != "the node stops: the call did not end within 30s of feosd's stop" {
		t.Errorf("the node's own UNAVAILABLE: %v", err)
	}
	var re *RefusalError
	if errors.As(err, &re) {
		t.Errorf("the node's answer is a local refusal: %v", err)
	}
}
