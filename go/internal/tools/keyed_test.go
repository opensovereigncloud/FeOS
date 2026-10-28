// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// startRun runs r.Run until the test ends, and waits until r records
// calls with an id. At the end it stops Run, which cuts the calls still
// running, and waits until every call wrote its row.
func startRun(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
		settled := make(chan struct{})
		go func() {
			r.k.wg.Wait()
			close(settled)
		}()
		select {
		case <-settled:
		case <-time.After(30 * time.Second):
			t.Error("calls with an id still run 30 s after Run ended")
		}
	})
	waitFor(t, "Run to record calls with an id", func() bool {
		r.k.mu.Lock()
		defer r.k.mu.Unlock()
		return r.k.renewing
	})
}

// execProbe scripts the node's Execs: each writes its output, counts
// itself on started, and waits until release is closed (exit 0) or its
// context ends (counted on cancelled).
type execProbe struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	n         atomic.Int32
}

func probeExec(env *toolstest.Env, out string) *execProbe {
	p := &execProbe{started: make(chan struct{}, 100), cancelled: make(chan struct{}, 100), release: make(chan struct{})}
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, o testnode.ExecOutput) (int32, bool, error) {
		p.n.Add(1)
		if out != "" {
			_ = o.Stdout([]byte(out))
		}
		p.started <- struct{}{}
		select {
		case <-p.release:
			return 0, true, nil
		case <-ctx.Done():
			p.cancelled <- struct{}{}
			return 0, true, status.FromContextError(ctx.Err()).Err()
		}
	})
	return p
}

// keyedAnswer is what CallKeyed returned.
type keyedAnswer struct {
	res  Result
	info CallInfo
	err  error
}

// callAsync runs CallKeyed in a goroutine; the channel gets its answer.
func callAsync(ctx context.Context, r *Runner, sb auth.Sandbox, tool, args, key string, progress func(string)) chan keyedAnswer {
	ch := make(chan keyedAnswer, 1)
	go func() {
		res, info, err := r.CallKeyed(ctx, sb, tool, json.RawMessage(args), key, progress)
		ch <- keyedAnswer{res, info, err}
	}()
	return ch
}

func await(t *testing.T, ch chan keyedAnswer) keyedAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("no answer within 10 s")
	}
	return keyedAnswer{}
}

func callRow(t *testing.T, env *toolstest.Env, id podid.ID, key string) store.ToolCall {
	t.Helper()
	row, err := env.Store.GetToolCall(bg, id, key)
	if err != nil {
		t.Fatalf("the row of %s: %v", key, err)
	}
	return row
}

func noCallRow(t *testing.T, env *toolstest.Env, id podid.ID, key string) {
	t.Helper()
	if row, err := env.Store.GetToolCall(bg, id, key); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("%s has a row: %+v %v", key, row, err)
	}
}

// localCall is the call of the key running on r, or nil.
func localCall(r *Runner, id podid.ID, key string) *keyedCall {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return r.k.calls[callRef{sandbox: id, key: key}]
}

func beats(kc *keyedCall) int {
	if kc == nil {
		return 0
	}
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return len(kc.beats)
}

func noop(string) {}

// A call with an id runs on a context of its own: the request that ends
// cancels nothing, the result is recorded, and a repeat reads it.
func TestKeyedCallOutlivesRequest(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "out\n")
	args := `{"command":"work"}`

	ctx, cancel := context.WithCancel(bg)
	go func() {
		<-p.started
		cancel()
	}()
	_, info, err := r.CallKeyed(ctx, sb, ToolBash, json.RawMessage(args), "k1", nil)
	if !errors.Is(err, context.Canceled) || !info.Keyed || info.Replay {
		t.Fatalf("the hung-up request: %+v %v", info, err)
	}
	select {
	case <-p.cancelled:
		t.Fatal("the node's exec was cancelled with the request")
	case <-time.After(300 * time.Millisecond):
	}
	if row := callRow(t, env, sb.ID, "k1"); row.State != store.ToolCallRunning || !row.Sent {
		t.Errorf("row while running %+v", row)
	}
	close(p.release)
	waitFor(t, "the call's row to end", func() bool {
		row, err := env.Store.GetToolCall(bg, sb.ID, "k1")
		return err == nil && row.State != store.ToolCallRunning
	})
	row := callRow(t, env, sb.ID, "k1")
	if row.State != store.ToolCallDone || string(row.Output) != "out\n" || !row.Sent || row.ExitCode != 0 || row.IsError ||
		row.Error != "" || row.InstanceID != toolstest.InstanceID || row.FinishedAt.IsZero() {
		t.Errorf("row %+v", row)
	}
	res, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(args), "k1", nil)
	want := CallInfo{Keyed: true, State: CallDone, Replay: true, Instance: toolstest.InstanceID}
	if err != nil || res.Text != "out\n" || res.ExitCode != 0 || info != want {
		t.Errorf("the repeat: %+v %+v %v", res, info, err)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}
	waitFor(t, "the exec hold's release", func() bool { return env.Engine.Holds(sb.ID) == 0 })

	// The tool_start row names the call's id.
	found := false
	for _, a := range env.Activity(sb.ID, 2) {
		if a.Kind == activity.KindToolStart && strings.Contains(string(a.Detail), `"callId": "k1"`) {
			found = true
		}
	}
	if !found {
		t.Error("no tool_start row with the call id")
	}
}

// Two requests of one key on one instance: one Exec, one answer for both,
// and only the first is not a replay.
func TestKeyedAttachSameInstance(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "shared\n")
	args := `{"command":"once"}`

	first := callAsync(bg, r, sb, ToolBash, args, "same", noop)
	waitN(t, p.started, 1)
	second := callAsync(bg, r, sb, ToolBash, args, "same", noop)
	waitFor(t, "both requests to wait for the call", func() bool { return beats(localCall(r, sb.ID, "same")) == 2 })
	close(p.release)
	a, b := await(t, first), await(t, second)
	for _, x := range []keyedAnswer{a, b} {
		if x.err != nil || x.res.Text != "shared\n" || x.info.State != CallDone || !x.info.Keyed {
			t.Errorf("answer %+v", x)
		}
	}
	if a.info.Replay == b.info.Replay || a.info.Replay {
		t.Errorf("replays: first %v, second %v", a.info.Replay, b.info.Replay)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}
}

// The same key with another call is refused, without the key in the text,
// whether the call runs or ended; nothing runs.
func TestKeyedConflict(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "ok\n")
	close(p.release)
	const key = "fecode:s1:42:call_a"
	if _, _, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"a"}`), key, nil); err != nil {
		t.Fatal(err)
	}
	check := func(what string, err error, info CallInfo) {
		t.Helper()
		var ce *CallIDConflictError
		if !errors.As(err, &ce) || ce.Tool != ToolBash || strings.Contains(err.Error(), key) || Retryable(err) {
			t.Errorf("%s: %v", what, err)
		}
		if info.State != "" {
			t.Errorf("%s: state %q", what, info.State)
		}
	}
	for _, tc := range []struct{ tool, args string }{
		{ToolBash, `{"command":"b"}`},
		{ToolBash, `{"command":"a","timeout":5}`},
		{ToolWrite, `{"path":"/f","content":"a"}`},
	} {
		_, info, err := r.CallKeyed(bg, sb, tc.tool, json.RawMessage(tc.args), key, nil)
		check(tc.tool+" "+tc.args, err, info)
	}
	want := "call id conflict: the id names another call in this sandbox (bash with other arguments); a new call needs a new id"
	if _, _, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"b"}`), key, nil); err == nil || err.Error() != want {
		t.Errorf("text %v", err)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}

	// Against the call while it runs.
	blocked := probeExec(env, "")
	running := callAsync(bg, r, sb, ToolBash, `{"command":"slow"}`, "running", nil)
	waitN(t, blocked.started, 1)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"fast"}`), "running", nil)
	check("against a running call", err, info)
	close(blocked.release)
	if a := await(t, running); a.err != nil {
		t.Errorf("the running call: %v", a.err)
	}
	if n := blocked.n.Load(); n != 1 {
		t.Errorf("%d Execs of the running call", n)
	}
}

// A call that did not reach the guest is stored not_run, unsent; the same
// call under the key runs again as a new run, another one is a conflict.
func TestKeyedNotRunStored(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	args := json.RawMessage(`{"command":"echo ran"}`)

	env.Engine.FailAcquire(sb.ID, model.ErrNoCapacity)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, args, "k", nil)
	if !Retryable(err) || !errors.Is(err, model.ErrNoCapacity) || info.State != CallNotRun || info.Replay || !info.Keyed {
		t.Fatalf("refused acquire: %+v %v", info, err)
	}
	first := callRow(t, env, sb.ID, "k")
	if first.State != store.ToolCallNotRun || first.Sent || first.Error != "ensure sandbox: no capacity" {
		t.Errorf("not_run row %+v", first)
	}

	res, info, err := r.CallKeyed(bg, sb, ToolBash, args, "k", nil)
	if err != nil || res.Text != "ran\n" || info.State != CallDone || info.Replay {
		t.Fatalf("the repeat: %+v %+v %v", res, info, err)
	}
	second := callRow(t, env, sb.ID, "k")
	if second.ID == first.ID || second.State != store.ToolCallDone || !second.Sent || string(second.Output) != "ran\n" {
		t.Errorf("the new run %+v (first %s)", second, first.ID)
	}

	env.Engine.FailAcquire(sb.ID, model.ErrNoCapacity)
	if _, info, _ := r.CallKeyed(bg, sb, ToolBash, args, "k2", nil); info.State != CallNotRun {
		t.Fatalf("k2: %+v", info)
	}
	var ce *CallIDConflictError
	if _, _, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"echo other"}`), "k2", nil); !errors.As(err, &ce) {
		t.Errorf("other arguments after not_run: %v", err)
	}
	if row := callRow(t, env, sb.ID, "k2"); row.State != store.ToolCallNotRun {
		t.Errorf("k2 %+v", row)
	}
}

// The cap of calls with an id refuses a new key not_run, without a row,
// but never a key whose call runs; concurrent first requests take exactly
// the cap.
func TestKeyedCapVerifiesRow(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	r.k.cap = 1
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "a\n")
	argsA := `{"command":"a"}`

	a := callAsync(bg, r, sb, ToolBash, argsA, "a", nil)
	waitN(t, p.started, 1)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"b"}`), "b", nil)
	want := fmt.Sprintf("sandbox %s: too many calls with an id at once (at most 1 on this instance)", sb.ID)
	if !errors.Is(err, ErrCallCap) || err.Error() != want || !Retryable(err) || info.State != CallNotRun {
		t.Errorf("past the cap: %+v %v", info, err)
	}
	noCallRow(t, env, sb.ID, "b")

	// A's key again, while the cap is full: it attaches.
	attached := callAsync(bg, r, sb, ToolBash, argsA, "a", noop)
	waitFor(t, "the repeat to attach", func() bool { return beats(localCall(r, sb.ID, "a")) == 1 })

	// Five first requests at once against a cap of 2, in another sandbox.
	r.k.mu.Lock()
	r.k.cap = 2
	r.k.mu.Unlock()
	other := env.NewSandbox()
	answers := make([]chan keyedAnswer, 5)
	for i := range answers {
		answers[i] = callAsync(bg, r, other, ToolBash, `{"command":"x"}`, fmt.Sprintf("c%d", i), nil)
	}
	waitN(t, p.started, 2)
	refused := 0
	for i := range answers {
		select {
		case x := <-answers[i]:
			if !errors.Is(x.err, ErrCallCap) || x.info.State != CallNotRun {
				t.Errorf("c%d: %+v", i, x)
			}
			answers[i] = nil
			refused++
		case <-time.After(300 * time.Millisecond):
		}
	}
	if refused != 3 {
		t.Errorf("%d refused", refused)
	}
	var rows int
	if err := env.DB.Pool.QueryRow(bg, "SELECT count(*) FROM tool_calls WHERE sandbox_id = $1", other.ID).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("%d rows %v", rows, err)
	}

	close(p.release)
	x, y := await(t, a), await(t, attached)
	if x.err != nil || y.err != nil || x.res.Text != "a\n" || y.res.Text != "a\n" || x.info.Replay || !y.info.Replay {
		t.Errorf("A %+v, attached %+v", x, y)
	}
	for _, ch := range answers {
		if ch != nil {
			if z := await(t, ch); z.err != nil || z.info.State != CallDone {
				t.Errorf("admitted %+v", z)
			}
		}
	}
	if n := p.n.Load(); n != 3 {
		t.Errorf("%d Execs", n)
	}
}

// waiters is how many requests of the sandbox wait on r for calls with an
// id they did not start.
func waiters(r *Runner, id podid.ID) int {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return r.k.waiters[id]
}

// The waiter cap: the requests of a sandbox that wait for a call with an
// id they did not start (attaches in memory and on another instance's
// row, record waits) count together, and one more is refused at once with
// ErrCallCap, state running, retryably, while the call runs on. A new
// call, a record read without a wait and another sandbox's requests are
// not refused, and the counts are given back.
func TestKeyedWaiterCap(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	r.k.waiterCap = 2
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "out\n")
	args := `{"command":"long"}`

	starter := callAsync(bg, r, sb, ToolBash, args, "k", nil)
	waitN(t, p.started, 1)
	attached := callAsync(bg, r, sb, ToolBash, args, "k", noop)
	waitFor(t, "the attach", func() bool { return beats(localCall(r, sb.ID, "k")) == 1 })
	record := make(chan CallRecord, 1)
	go func() {
		rec, err := r.WaitCall(bg, sb.ID, "k", 30*time.Second)
		if err != nil {
			t.Errorf("the record wait: %v", err)
		}
		record <- rec
	}()
	waitFor(t, "the record wait to count", func() bool { return waiters(r, sb.ID) == 2 })
	if n := starts(r, sb.ID); n != 1 {
		t.Errorf("%d reserved slots", n)
	}

	// The cap is full: an attach and a record wait are refused at once.
	refusedCtx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	want := CallInfo{Keyed: true, State: CallRunning, Replay: true, Instance: toolstest.InstanceID}
	_, info, err := r.CallKeyed(refusedCtx, sb, ToolBash, json.RawMessage(args), "k", noop)
	wantErr := fmt.Sprintf("sandbox %s: too many calls with an id at once (at most 2 requests waiting for them on this instance)", sb.ID)
	if !errors.Is(err, ErrCallCap) || err.Error() != wantErr || !Retryable(err) || info != want {
		t.Errorf("an attach past the cap: %+v %v", info, err)
	}
	if _, err := r.WaitCall(bg, sb.ID, "k", time.Second); !errors.Is(err, ErrCallCap) {
		t.Errorf("a record wait past the cap: %v", err)
	}
	// So is a wait on another instance's row, before any read of it.
	foreignRow(t, env, sb, "remote", args, true, time.Minute)
	_, info, err = r.CallKeyed(refusedCtx, sb, ToolBash, json.RawMessage(args), "remote", nil)
	if want := (CallInfo{Keyed: true, State: CallRunning, Replay: true, Instance: "i9"}); !errors.Is(err, ErrCallCap) || info != want {
		t.Errorf("a row's waiter past the cap: %+v %v", info, err)
	}
	if _, err := r.WaitCall(bg, sb.ID, "remote", time.Second); !errors.Is(err, ErrCallCap) {
		t.Errorf("a row's record wait past the cap: %v", err)
	}
	// A read without a wait waits for nothing; a new call starts.
	if rec, err := r.WaitCall(bg, sb.ID, "k", 0); err != nil || rec.State != CallRunning {
		t.Errorf("a record read: %+v %v", rec, err)
	}
	fresh := callAsync(bg, r, sb, ToolBash, `{"command":"fresh"}`, "fresh", nil)
	waitN(t, p.started, 1)
	// Another sandbox has a cap of its own.
	other := env.NewSandbox()
	otherStarter := callAsync(bg, r, other, ToolBash, args, "k", nil)
	waitN(t, p.started, 1)
	otherAttached := callAsync(bg, r, other, ToolBash, args, "k", noop)
	waitFor(t, "the other sandbox's attach", func() bool { return beats(localCall(r, other.ID, "k")) == 1 })
	if n := waiters(r, sb.ID); n != 2 {
		t.Errorf("%d waiters after the refusals", n)
	}

	close(p.release)
	for name, ch := range map[string]chan keyedAnswer{
		"starter": starter, "attached": attached, "fresh": fresh, "other starter": otherStarter, "other attached": otherAttached,
	} {
		if x := await(t, ch); x.err != nil || x.res.Text != "out\n" || x.info.State != CallDone {
			t.Errorf("%s: %+v", name, x)
		}
	}
	select {
	case rec := <-record:
		if rec.State != CallDone || rec.Result.Text != "out\n" {
			t.Errorf("the record wait: %+v", rec)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the record wait did not end with the call")
	}
	waitFor(t, "the waiters to be given back", func() bool { return waiters(r, sb.ID) == 0 && waiters(r, other.ID) == 0 })
	r.k.mu.Lock()
	left := len(r.k.waiters)
	r.k.mu.Unlock()
	if left != 0 {
		t.Errorf("%d sandboxes counted", left)
	}
	if n := p.n.Load(); n != 3 {
		t.Errorf("%d Execs", n)
	}
}

// starts is how many of the sandbox's call slots are reserved on r.
func starts(r *Runner, id podid.ID) int {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return r.k.perSandbox[id]
}

// The exec cap refuses a call with an id before it is sent: not_run.
func TestKeyedExecCapNotRun(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	r.execCap = 1
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "")
	unkeyed := make(chan error, 1)
	go func() {
		_, err := r.Call(bg, sb, ToolBash, json.RawMessage(`{"command":"wait"}`), nil)
		unkeyed <- err
	}()
	waitN(t, p.started, 1)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), "k", nil)
	if !errors.Is(err, ErrExecCap) || !Retryable(err) || info.State != CallNotRun {
		t.Errorf("past the exec cap: %+v %v", info, err)
	}
	if row := callRow(t, env, sb.ID, "k"); row.State != store.ToolCallNotRun || row.Sent {
		t.Errorf("row %+v", row)
	}
	close(p.release)
	if err := <-unkeyed; err != nil {
		t.Error(err)
	}
}

// CancelCall cancels a call running here: the starter gets
// ErrCancelledByCaller, the row keeps it, the node's exec ends.
func TestKeyedCancelLocal(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "partial\n")
	args := `{"command":"long"}`
	// The starter's heartbeat says the output reached this instance.
	output := make(chan struct{}, 1)
	started := callAsync(bg, r, sb, ToolBash, args, "k", func(string) {
		select {
		case output <- struct{}{}:
		default:
		}
	})
	waitN(t, p.started, 1)
	select {
	case <-output:
	case <-time.After(10 * time.Second):
		t.Fatal("no output arrived")
	}
	ok, err := r.CancelCall(bg, sb.ID, "k")
	if !ok || err != nil {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	a := await(t, started)
	if !errors.Is(a.err, ErrCancelledByCaller) || a.info.State != CallDone || a.res.Text != "partial\n" || Retryable(a.err) {
		t.Errorf("the starter %+v", a)
	}
	select {
	case <-p.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the node's exec was not cancelled")
	}
	row := callRow(t, env, sb.ID, "k")
	if row.State != store.ToolCallDone || row.Error != ErrCancelledByCaller.Error() || string(row.Output) != "partial\n" {
		t.Errorf("row %+v", row)
	}
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(args), "k", nil)
	if err == nil || err.Error() != ErrCancelledByCaller.Error() || !info.Replay || info.State != CallDone {
		t.Errorf("the repeat: %+v %v", info, err)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}
	if ok, err := r.CancelCall(bg, sb.ID, "k"); ok || err != nil {
		t.Errorf("cancel of an ended call: %v %v", ok, err)
	}
	if ok, err := r.CancelCall(bg, sb.ID, "nothing"); ok || err != nil {
		t.Errorf("cancel of no call: %v %v", ok, err)
	}
	if _, err := r.CancelCall(bg, sb.ID, "bad key"); !errors.Is(err, ErrBadCallKey) {
		t.Errorf("cancel with a bad key: %v", err)
	}
}

// A key on a read-only tool is checked and ignored: no row, the call runs
// on its request as without one.
func TestKeyedReadOnlyIgnoresKey(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	if _, info, err := r.CallKeyed(bg, sb, ToolLs, json.RawMessage(`{"path":"/"}`), "k1", nil); err != nil || info != (CallInfo{}) {
		t.Errorf("ls: %+v %v", info, err)
	}
	if _, info, err := r.CallKeyed(bg, sb, ToolRead, json.RawMessage(`{"path":"/nope"}`), "k2", nil); err != nil || info != (CallInfo{}) {
		t.Errorf("read: %+v %v", info, err)
	}
	noCallRow(t, env, sb.ID, "k1")
	noCallRow(t, env, sb.ID, "k2")
	if _, _, err := r.CallKeyed(bg, sb, ToolLs, json.RawMessage(`{"path":"/"}`), "bad/key", nil); !errors.Is(err, ErrBadCallKey) {
		t.Errorf("a bad key on ls: %v", err)
	}

	entered := make(chan struct{}, 1)
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	defer env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, nil)
	ctx, cancel := context.WithCancel(bg)
	go func() {
		<-entered
		cancel()
	}()
	if _, info, err := r.CallKeyed(ctx, sb, ToolRead, json.RawMessage(`{"path":"/x"}`), "k3", nil); !errors.Is(err, context.Canceled) || info.Keyed {
		t.Errorf("a cancelled read: %+v %v", info, err)
	}
	noCallRow(t, env, sb.ID, "k3")
}

// The argument hash is over the normalised arguments: spacing, order and
// spelled-out zero values make no other call, another timeout does; MCP's
// write and REST's write of the same bytes are one call.
func TestKeyedHashNormalised(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "x\n")
	close(p.release)
	if _, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), "h", nil); err != nil || info.Replay {
		t.Fatalf("%+v %v", info, err)
	}
	res, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{ "timeout": 0 , "command":"x"}`), "h", nil)
	if err != nil || !info.Replay || res.Text != "x\n" {
		t.Errorf("respelled: %+v %+v %v", res, info, err)
	}
	var ce *CallIDConflictError
	if _, _, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"x","timeout":7}`), "h", nil); !errors.As(err, &ce) {
		t.Errorf("another timeout: %v", err)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}

	if _, info, err := r.CallKeyed(bg, sb, ToolWrite, json.RawMessage(`{"path":"/f","content":"abc"}`), "w", nil); err != nil || info.Replay {
		t.Fatalf("write: %+v %v", info, err)
	}
	if res, info, err := r.writeFileKeyed(bg, sb, "/f", []byte("abc"), "w"); err != nil || !info.Replay || res.Text != "wrote /f\n" {
		t.Errorf("REST's write of the same bytes: %+v %+v %v", res, info, err)
	}
	if _, _, err := r.writeFileKeyed(bg, sb, "/f", []byte("abd"), "w"); !errors.As(err, &ce) || ce.Tool != ToolWrite {
		t.Errorf("other bytes: %v", err)
	}
	if h, err := callHash(ToolWrite, json.RawMessage(`{"content":"abc","path":"/f"}`)); err != nil || !bytes.Equal(h, writeHash("/f", []byte("abc"))) {
		t.Errorf("write hash %x %v", h, err)
	}
	// The path's length keeps path and content apart.
	if bytes.Equal(writeHash("/a", []byte("bc")), writeHash("/ab", []byte("c"))) {
		t.Error("the write hash does not separate the path from the content")
	}
}

// A keyed bash may name at most MaxKeyedTimeout; past it nothing is
// recorded or run. Without a key the timeout is the call's.
func TestKeyedTimeoutCap(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	args := json.RawMessage(`{"command":"true","timeout":601}`)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, args, "t", nil)
	var te *KeyedTimeoutError
	want := "a call with an id may run at most 600 s: lower its timeout, or start longer work with process_start"
	if !errors.As(err, &te) || te.Max != MaxKeyedTimeout || err.Error() != want || info != (CallInfo{}) {
		t.Errorf("past the cap: %+v %v", info, err)
	}
	noCallRow(t, env, sb.ID, "t")
	if env.Engine.Calls("Acquire", sb.ID) != 0 {
		t.Error("the refused call acquired the sandbox")
	}
	if _, info, err := r.CallKeyed(bg, sb, ToolBash, args, "", nil); err != nil || info.Keyed {
		t.Errorf("without a key: %+v %v", info, err)
	}
	if _, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"true","timeout":600}`), "t", nil); err != nil || info.State != CallDone {
		t.Errorf("at the cap: %+v %v", info, err)
	}
}

// The call's budget ends it by a timer, not a deadline: the node sees no
// grpc-timeout.
func TestKeyedBudgetTimer(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	warm(t, r, sb)
	r.k.budget = func(time.Duration) time.Duration { return 500 * time.Millisecond }
	deadlines := make(chan bool, 1)
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		_, has := ctx.Deadline()
		deadlines <- has
		<-ctx.Done()
		return 0, true, status.FromContextError(ctx.Err()).Err()
	})
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), "b", nil)
	if !errors.Is(err, errCallBudget) || info.State != CallDone {
		t.Errorf("past the budget: %+v %v", info, err)
	}
	select {
	case has := <-deadlines:
		if has {
			t.Error("the node's exec had a deadline")
		}
	default:
		t.Fatal("the call was not sent")
	}
	if row := callRow(t, env, sb.ID, "b"); row.State != store.ToolCallDone || row.Error != errCallBudget.Error() {
		t.Errorf("row %+v", row)
	}
}

// The stored output is the cut Result.Text, bytes as they are; a stored
// error is valid UTF-8 without NUL, whatever the node said.
func TestKeyedOutputAndErrorBytes(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	// Two streams of 30,000 bytes each, under the per-stream cap: the text
	// takes both and is cut at MaxToolOutput.
	stdout := bytes.Repeat([]byte{'o'}, 30_000)
	copy(stdout, "x\x00y\xffz")
	stderr := bytes.Repeat([]byte{'e'}, 30_000)
	emitted := append(append([]byte(nil), stdout...), stderr...)
	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stdout(stdout)
		_ = out.Stderr(stderr)
		return 0
	})
	res, _, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), "o", nil)
	if err != nil || !res.Truncated {
		t.Fatalf("%v truncated %v", err, res.Truncated)
	}
	row := callRow(t, env, sb.ID, "o")
	if !bytes.Equal(row.Output, emitted[:MaxToolOutput]) || !row.Truncated {
		t.Errorf("stored %d bytes, truncated %v", len(row.Output), row.Truncated)
	}
	replay, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"x"}`), "o", nil)
	if err != nil || !info.Replay || replay.Text != string(emitted[:MaxToolOutput]) || !replay.Truncated {
		t.Errorf("the replay differs: %d bytes %+v %v", len(replay.Text), info, err)
	}

	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		return 0, true, status.Error(codes.Internal, "bad\x00news \xff here")
	})
	_, info, err = r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"y"}`), "e", nil)
	if err == nil || info.State != CallDone {
		t.Fatalf("a node error: %+v %v", info, err)
	}
	row = callRow(t, env, sb.ID, "e")
	if row.State != store.ToolCallDone || !utf8.ValidString(row.Error) || strings.Contains(row.Error, "\x00") ||
		!strings.HasPrefix(row.Error, "node n1: bad") {
		t.Errorf("stored error %q", row.Error)
	}

	long := errors.New(strings.Repeat("é", maxCallError))
	if s := sanitizeError(long); len(s) > maxCallError || !utf8.ValidString(s) || len(s) < maxCallError-1 {
		t.Errorf("cut to %d bytes", len(s))
	}
	if s := sanitizeError(errors.New("a\x00b\xffc")); s != "ab�c" {
		t.Errorf("sanitised %q", s)
	}
	if sanitizeError(nil) != "" || storedError("") != nil || storedError("x").Error() != "x" {
		t.Error("nil and empty")
	}
}

func TestValidCallKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"", false},
		{strings.Repeat("k", 255), true},
		{strings.Repeat("k", 256), false},
		{"a b", false},
		{"a/b", false},
		{"a%2Fb", false},
		{`a"b`, false},
		{`a\b`, false},
		{"schlüssel", false},
		{"fecode:s1:42:call_x.y~z@w-1", true},
		{"ABCxyz019", true},
	} {
		if got := ValidCallKey(tc.key); got != tc.want {
			t.Errorf("%q: %v", tc.key, got)
		}
	}
	if got := KeyedTools(); strings.Join(got, ",") != "bash,edit,process_start,process_stop,write" {
		t.Errorf("keyed tools %v", got)
	}
}

// A process_start with an id holds its process once; the repeat answers
// the same handle without starting or holding anything.
func TestKeyedProcessStart(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{FakeProcesses: true})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	args := json.RawMessage(`{"command":"serve","timeout":0,"name":"dev"}`)
	res, info, err := r.CallKeyed(bg, sb, ToolProcessStart, args, "p", nil)
	if err != nil || info.State != CallDone || info.Replay {
		t.Fatalf("%+v %+v %v", res, info, err)
	}
	handle, ok := parseHandle(res.Text)
	if !ok {
		t.Fatalf("no handle in %q", res.Text)
	}
	if got := env.Engine.Processes(sb.ID); len(got) != 1 || got[handle] != 0 {
		t.Fatalf("holds %v", got)
	}
	if err := env.Engine.ReleaseProcess(bg, sb.ID, handle); err != nil {
		t.Fatal(err)
	}
	again, info, err := r.CallKeyed(bg, sb, ToolProcessStart, args, "p", nil)
	if err != nil || again.Text != res.Text || !info.Replay {
		t.Errorf("the repeat: %+v %+v %v", again, info, err)
	}
	if got := env.Engine.Processes(sb.ID); len(got) != 0 {
		t.Errorf("the repeat held %v", got)
	}
	list, err := r.Call(bg, sb, ToolProcessList, nil, nil)
	if err != nil || strings.Count(list.Text, `"id"`) != 1 {
		t.Errorf("processes %q %v", list.Text, err)
	}
}

// Without a database a key changes nothing.
func TestKeyedWithoutDatabase(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := NewRunner(env.Engine, env.Port, env.Recorder, nil, toolstest.InstanceID, env.Log)
	sb := env.NewSandbox()
	res, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"echo hi"}`), "k", nil)
	if err != nil || res.Text != "hi\n" || info.Keyed {
		t.Errorf("%+v %+v %v", res, info, err)
	}
	noCallRow(t, env, sb.ID, "k")
}

// A Runner whose Run does not run records no call with an id: refused
// not_run, nothing written, nothing run.
func TestKeyedNotRecordingWithoutRun(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"echo hi"}`), "k", nil)
	want := "instance i1 does not record calls with an id now (it is starting or stopping): instance is stopping"
	if !errors.Is(err, model.ErrStopping) || err.Error() != want || !Retryable(err) || info.State != CallNotRun {
		t.Errorf("%+v %v", info, err)
	}
	noCallRow(t, env, sb.ID, "k")
	if env.Engine.Calls("Acquire", sb.ID) != 0 {
		t.Error("the refused call acquired the sandbox")
	}
}

// Only feosd's own refusals make a sent call unsent; another
// FAILED_PRECONDITION may come from the guest, and the call counts as run.
func TestKeyedSentResetOnlyForFeosd(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	warm(t, r, sb)
	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		return 0, true, status.Errorf(codes.FailedPrecondition, "pod %s is POD_STATE_STOPPED", sb.ID)
	})
	_, info, err := r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"a"}`), "f1", nil)
	if err == nil || info.State != CallNotRun || env.Engine.Calls("Sync", sb.ID) != 1 {
		t.Errorf("feosd's refusal: %+v %v, syncs %d", info, err, env.Engine.Calls("Sync", sb.ID))
	}
	if row := callRow(t, env, sb.ID, "f1"); row.State != store.ToolCallNotRun {
		t.Errorf("row %+v", row)
	}

	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		return 0, true, status.Error(codes.FailedPrecondition, "something else")
	})
	_, info, err = r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"b"}`), "f2", nil)
	if err == nil || err.Error() != "node n1: something else" || info.State != CallDone {
		t.Errorf("another refusal: %+v %v", info, err)
	}
	if row := callRow(t, env, sb.ID, "f2"); row.State != store.ToolCallDone || !row.Sent || row.Error != "node n1: something else" {
		t.Errorf("row %+v", row)
	}

	// An attempt that may have run, then feosd's refusal of the retry: the
	// refusal undoes only its own attempt.
	var execs atomic.Int32
	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		if execs.Add(1) == 1 {
			return 0, true, status.Error(codes.FailedPrecondition, "something else")
		}
		return 0, true, status.Errorf(codes.FailedPrecondition, "pod %s is POD_STATE_STOPPED", sb.ID)
	})
	_, info, err = r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"c"}`), "f3", nil)
	want := fmt.Sprintf("node n1: pod %s is POD_STATE_STOPPED", sb.ID)
	if execs.Load() != 2 || err == nil || err.Error() != want || info.State != CallDone {
		t.Errorf("a refused retry after an attempt that may have run: execs %d, %+v %v", execs.Load(), info, err)
	}
	if row := callRow(t, env, sb.ID, "f3"); row.State != store.ToolCallDone || !row.Sent || row.Error != want {
		t.Errorf("row %+v", row)
	}

	// The guest of a running pod fails the call: feosd says so with the
	// text of a failed connect, also after it handed the request to the
	// guest, so the call may have run.
	env.Node.Script().SetExec(func(context.Context, podid.ID, *containerv1.ExecRequest, testnode.ExecOutput) (int32, bool, error) {
		return 0, true, status.Errorf(codes.Unavailable, "the guest of pod %s: connection reset by peer", sb.ID)
	})
	_, info, err = r.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"d"}`), "f4", nil)
	want = fmt.Sprintf("node n1: the guest of pod %s: connection reset by peer", sb.ID)
	if err == nil || err.Error() != want || !Retryable(err) || info.State != CallDone {
		t.Errorf("an unreachable guest: %+v %v", info, err)
	}
	if row := callRow(t, env, sb.ID, "f4"); row.State != store.ToolCallDone || !row.Sent || row.Error != want {
		t.Errorf("row %+v", row)
	}
	id := podid.Mint()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status.Errorf(codes.FailedPrecondition, "pod %s is POD_STATE_SUSPENDED", id), true},
		{status.Errorf(codes.NotFound, "no pod %s", id), true},
		{status.Errorf(codes.Unavailable, "the guest of pod %s: connection refused", id), false},
		{status.Errorf(codes.FailedPrecondition, "pod %s is POD_STATE_STOPPED", podid.Mint()), false},
		{status.Errorf(codes.NotFound, "no pod %s here", id), false},
		{status.Error(codes.NotFound, "no container main"), false},
		{errors.New("pod is gone"), false},
	} {
		if got := feosdRefusal(tc.err, id); got != tc.want {
			t.Errorf("%v: %v", tc.err, got)
		}
	}
	// Only the nodes' port's own refusal is local; the same code and text
	// from a node is not.
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&nodelink.RefusalError{Status: status.New(codes.ResourceExhausted, "node n1: stream budget exhausted")}, true},
		{&nodelink.RefusalError{Status: status.New(codes.Unavailable, "node n1 is not connected to instance i1")}, true},
		{fmt.Errorf("x: %w", &nodelink.RefusalError{Status: status.New(codes.Unavailable, "node n1: connection is closing")}), true},
		{status.Error(codes.ResourceExhausted, "node n1: stream budget exhausted"), false},
		{status.Error(codes.Unavailable, "node n1 is not connected to instance i1"), false},
		{status.Error(codes.ResourceExhausted, "the guest ran out of memory"), false},
	} {
		if got := localRefusal(tc.err); got != tc.want {
			t.Errorf("localRefusal(%v): %v", tc.err, got)
		}
	}
}

// dropConn is the nodes' port, except that, when armed, the node's
// connection drops once Conn has handed it out: the call that follows is
// refused by the port after it was marked sent.
type dropConn struct {
	nodelink.Conns
	env   *toolstest.Env
	armed atomic.Bool
}

func (d *dropConn) Conn(ctx context.Context, node string) (grpc.ClientConnInterface, error) {
	cc, err := d.Conns.Conn(ctx, node)
	if err == nil && d.armed.CompareAndSwap(true, false) {
		d.env.Node.Crash()
		deadline := time.Now().Add(10 * time.Second)
		for d.env.Port.Connected(node) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return cc, err
}

// A call that the nodes' port refuses after the call was marked sent (its
// node left this instance between Conn and the call) never left the
// instance: not_run and retryable, a stream (bash) and a unary call
// (write) alike, and the same call under the key runs anew once the node
// is back.
func TestKeyedPortRefusalNotRun(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	conns := &dropConn{Conns: env.Port, env: env}
	r := NewRunner(env.Engine, conns, env.Recorder, env.DB, toolstest.InstanceID, env.Log)
	startRun(t, r)
	sb := env.NewSandbox()
	warm(t, r, sb)
	want := "node n1 is not connected to instance i1"
	for _, tc := range []struct {
		tool string
		args string
	}{
		{ToolBash, `{"command":"echo once"}`},
		{ToolWrite, `{"path":"/f","content":"a"}`},
	} {
		key := "k-" + tc.tool
		conns.armed.Store(true)
		_, info, err := r.CallKeyed(bg, sb, tc.tool, json.RawMessage(tc.args), key, nil)
		if err == nil || err.Error() != want || !Retryable(err) || status.Code(err) != codes.Unavailable ||
			info.State != CallNotRun {
			t.Errorf("%s refused by the port: %+v %v", tc.tool, info, err)
		}
		if row := callRow(t, env, sb.ID, key); row.State != store.ToolCallNotRun || row.Error != want {
			t.Errorf("%s: row %+v", tc.tool, row)
		}
		env.Node.Restart(false)
		deadline := time.Now().Add(30 * time.Second)
		for !env.Port.Connected(toolstest.NodeName) {
			if time.Now().After(deadline) {
				t.Fatal("the test node did not come back within 30 s")
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, info, err = r.CallKeyed(bg, sb, tc.tool, json.RawMessage(tc.args), key, nil)
		if err != nil || info.State != CallDone || info.Replay {
			t.Errorf("%s repeated: %+v %v", tc.tool, info, err)
		}
		if row := callRow(t, env, sb.ID, key); row.State != store.ToolCallDone || !row.Sent {
			t.Errorf("%s repeated: row %+v", tc.tool, row)
		}
	}
}

// A request that is gone before its call starts starts nothing.
func TestKeyedGoneRequestStartsNothing(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, _, err := r.CallKeyed(ctx, sb, ToolBash, json.RawMessage(`{"command":"x"}`), "k", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
	noCallRow(t, env, sb.ID, "k")
	if env.Engine.Calls("Acquire", sb.ID) != 0 {
		t.Error("the gone request acquired the sandbox")
	}
}

// An insert whose answer is lost is ended not_run at once, so that the
// repeat replaces it.
func TestKeyedAmbiguousInsertAbandoned(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	realBegin := r.k.begin
	r.k.begin = func(ctx context.Context, c store.NewToolCall) (store.ToolCall, bool, error) {
		if _, _, err := realBegin(ctx, c); err != nil {
			return store.ToolCall{}, false, err
		}
		return store.ToolCall{}, false, errors.New("the answer was lost")
	}
	startRun(t, r)
	sb := env.NewSandbox()
	args := json.RawMessage(`{"command":"echo once"}`)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, args, "k", nil)
	if !errors.Is(err, ErrRecord) || !Retryable(err) || info.State != CallNotRun ||
		err.Error() != "the call could not be recorded: the answer was lost" {
		t.Errorf("%+v %v", info, err)
	}
	row := callRow(t, env, sb.ID, "k")
	if row.State != store.ToolCallNotRun || row.Sent || row.Error != "not run: the record could not be written: the answer was lost" {
		t.Errorf("row %+v", row)
	}
	if env.Engine.Calls("Acquire", sb.ID) != 0 {
		t.Error("the unrecorded call acquired the sandbox")
	}
	r.k.begin = realBegin
	if res, info, err := r.CallKeyed(bg, sb, ToolBash, args, "k", nil); err != nil || res.Text != "once\n" || info.Replay {
		t.Errorf("the repeat: %+v %+v %v", res, info, err)
	}
	r.k.mu.Lock()
	reserved := len(r.k.perSandbox)
	r.k.mu.Unlock()
	if reserved != 0 {
		t.Errorf("%d sandboxes with reserved slots", reserved)
	}

	// A sandbox the database does not know: no such sandbox, not_run.
	_, info, err = r.CallKeyed(bg, toolstest.Sandbox(), ToolBash, args, "k", nil)
	if !errors.Is(err, model.ErrNotFound) || err.Error() != "ensure sandbox: no such sandbox" || Retryable(err) ||
		info.State != CallNotRun {
		t.Errorf("an unknown sandbox: %+v %v", info, err)
	}
}

// A failed insert is abandoned while the key stays locked: a request of
// the same key that waited for the lock records and runs its own call,
// which the abandon does not end (it matches the key's unsent running row
// of this instance, not the row's id).
func TestKeyedAbandonHoldsKey(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	args := json.RawMessage(`{"command":"echo two"}`)
	ref := callRef{sandbox: sb.ID, key: "k"}
	type answer struct {
		res  Result
		info CallInfo
		err  error
	}
	second := make(chan answer, 1)
	inserted := make(chan struct{}, 1)
	firstDone := make(chan struct{})
	var begins atomic.Int32
	realBegin, realAbandon := r.k.begin, r.k.abandon
	r.k.begin = func(ctx context.Context, c store.NewToolCall) (store.ToolCall, bool, error) {
		if begins.Add(1) > 1 {
			// The second request's insert commits, then waits until the
			// first request answered.
			row, ok, err := realBegin(ctx, c)
			inserted <- struct{}{}
			select {
			case <-firstDone:
			case <-time.After(10 * time.Second):
				return store.ToolCall{}, false, errors.New("the first request did not answer")
			}
			return row, ok, err
		}
		go func() {
			res, info, err := r.CallKeyed(bg, sb, ToolBash, args, "k", nil)
			second <- answer{res, info, err}
		}()
		waitFor(t, "the second request to wait for the key", func() bool {
			r.k.mu.Lock()
			defer r.k.mu.Unlock()
			l := r.k.locks[ref]
			return l != nil && l.refs == 2
		})
		return store.ToolCall{}, false, errors.New("the database went away")
	}
	// The abandon waits for the second request's insert, which cannot come
	// while the key is locked.
	r.k.abandon = func(ctx context.Context, id podid.ID, key, instanceID, reason string) (bool, error) {
		select {
		case <-inserted:
		case <-time.After(500 * time.Millisecond):
		}
		return realAbandon(ctx, id, key, instanceID, reason)
	}
	startRun(t, r)
	_, info, err := r.CallKeyed(bg, sb, ToolBash, args, "k", nil)
	close(firstDone)
	if !errors.Is(err, ErrRecord) || info.State != CallNotRun {
		t.Errorf("the first request: %+v %v", info, err)
	}
	select {
	case a := <-second:
		if a.err != nil || a.res.Text != "two\n" || a.info.State != CallDone || a.info.Replay {
			t.Errorf("the second request: %+v %+v %v", a.res, a.info, a.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second request did not answer")
	}
	if row := callRow(t, env, sb.ID, "k"); row.State != store.ToolCallDone || !row.Sent {
		t.Errorf("row %+v", row)
	}
}

// A row that another writer ended while the call ran: the next renewal
// misses it and cancels the call; its write is refused, and the starter
// gets the stored row.
func TestKeyedRecordLost(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "")
	started := callAsync(bg, r, sb, ToolBash, `{"command":"x"}`, "l", nil)
	waitN(t, p.started, 1)
	row := callRow(t, env, sb.ID, "l")
	r.k.mu.Lock()
	kc := r.k.calls[callRef{sandbox: sb.ID, key: "l"}]
	r.k.mu.Unlock()
	if kc == nil {
		t.Fatal("the call does not run")
	}
	if _, err := env.DB.Pool.Exec(bg, `UPDATE tool_calls SET state = 'interrupted', finished_at = now(), error = 'x'
		WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	// The renewal after the row's end (here the test's; the loop's comes
	// every third of a lease) cancels the call.
	r.renewCallsOnce(bg, r.lease())
	if cause := context.Cause(kc.ctx); !errors.Is(cause, errCallLost) {
		t.Fatalf("the call after the renewal: %v, want %v", cause, errCallLost)
	}
	waitN(t, p.cancelled, 1)
	a := await(t, started)
	if a.err == nil || a.err.Error() != "x" || a.info.State != CallInterrupted || a.info.Replay {
		t.Errorf("the starter %+v", a)
	}
	after := callRow(t, env, sb.ID, "l")
	if after.ID != row.ID || after.State != store.ToolCallInterrupted || after.Error != "x" {
		t.Errorf("row %+v", after)
	}
}

// A final write that loses to the expiry answers the stored row.
func TestKeyedFinishLosesToExpiry(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	realFinish := r.k.finish
	r.k.finish = func(ctx context.Context, id uuid.UUID, o store.ToolCallOutcome) (bool, error) {
		once.Do(func() { close(entered) })
		<-gate
		return realFinish(ctx, id, o)
	}
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "done\n")
	close(p.release)
	started := callAsync(bg, r, sb, ToolBash, `{"command":"x"}`, "f", nil)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no final write")
	}
	row := callRow(t, env, sb.ID, "f")
	if _, err := env.DB.Pool.Exec(bg, `UPDATE tool_calls SET state = 'interrupted', finished_at = now(), error = 'y'
		WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	close(gate)
	a := await(t, started)
	if a.err == nil || a.err.Error() != "y" || a.info.State != CallInterrupted {
		t.Errorf("the starter %+v", a)
	}
	if after := callRow(t, env, sb.ID, "f"); after.State != store.ToolCallInterrupted || len(after.Output) != 0 {
		t.Errorf("row %+v", after)
	}
}

// A repeat of another instance's running call waits for its row and
// answers its stored result.
func TestKeyedWaitRowBasic(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	p := probeExec(env, "")
	args := `{"command":"elsewhere"}`
	hash, err := callHash(ToolBash, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	row, inserted, err := env.Store.BeginToolCall(bg, store.NewToolCall{
		SandboxID: sb.ID, Key: "w", Tool: ToolBash, ArgsHash: hash, InstanceID: "i9", Lease: time.Minute,
	})
	if err != nil || !inserted {
		t.Fatal(inserted, err)
	}
	waiting := callAsync(bg, r, sb, ToolBash, args, "w", nil)
	select {
	case a := <-waiting:
		t.Fatalf("answered while the call runs elsewhere: %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
	if ok, err := env.Store.FinishToolCall(bg, row.ID, store.ToolCallOutcome{State: store.ToolCallDone, Output: []byte("x")}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	select {
	case a := <-waiting:
		want := CallInfo{Keyed: true, State: CallDone, Replay: true, Instance: "i9"}
		if a.err != nil || a.res.Text != "x" || a.info != want {
			t.Errorf("%+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no answer within 2 s of the row's end")
	}
	if n := p.n.Load(); n != 0 {
		t.Errorf("%d Execs", n)
	}
	// A waiting request that goes away leaves the row alone.
	other, _, err := env.Store.BeginToolCall(bg, store.NewToolCall{
		SandboxID: sb.ID, Key: "w2", Tool: ToolBash, ArgsHash: hash, InstanceID: "i9", Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	defer cancel()
	if _, info, err := r.CallKeyed(ctx, sb, ToolBash, json.RawMessage(args), "w2", nil); !errors.Is(err, context.DeadlineExceeded) ||
		info.State != CallRunning || info.Instance != "i9" {
		t.Errorf("the gone waiter: %+v %v", info, err)
	}
	if row := callRow(t, env, sb.ID, "w2"); row.ID != other.ID || row.State != store.ToolCallRunning {
		t.Errorf("row %+v", row)
	}
}

// CancelCall of another instance's running call sends ferun_cancel of
// kind call.
func TestKeyedCancelRemoteRow(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	hash, err := callHash(ToolBash, json.RawMessage(`{"command":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := env.Store.BeginToolCall(bg, store.NewToolCall{
		SandboxID: sb.ID, Key: "r", Tool: ToolBash, ArgsHash: hash, InstanceID: "i9", Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := env.DB.Subscribe(db.ChanCancel)
	defer unsubscribe()
	if ok, err := r.CancelCall(bg, sb.ID, "r"); !ok || err != nil {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	deadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case payload := <-ch:
			var p cancelPayload
			if json.Unmarshal([]byte(payload), &p) == nil && p.Kind == "call" {
				if p.Sandbox != sb.ID.String() || p.Key != "r" {
					t.Errorf("payload %s", payload)
				}
				got = true
			}
		case <-deadline:
			t.Fatal("no ferun_cancel of kind call")
		}
	}
	if ok, err := env.Store.FinishToolCall(bg, row.ID, store.ToolCallOutcome{State: store.ToolCallDone}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := r.CancelCall(bg, sb.ID, "r"); ok || err != nil {
		t.Errorf("cancel of an ended call: %v %v", ok, err)
	}
}

// A ferun_cancel of kind call that arrives while a join of its key writes
// the row cancels the call once it is registered.
func TestCancelKindCallWaitsForJoin(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	entered, gate := make(chan struct{}), make(chan struct{})
	realBegin := r.k.begin
	r.k.begin = func(ctx context.Context, c store.NewToolCall) (store.ToolCall, bool, error) {
		row, inserted, err := realBegin(ctx, c)
		close(entered)
		<-gate
		return row, inserted, err
	}
	startRun(t, r)
	sb := env.NewSandbox()
	probeExec(env, "")
	started := callAsync(bg, r, sb, ToolBash, `{"command":"x"}`, "c", nil)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the join did not insert")
	}
	payload := `{"kind":"call","sandbox":"` + sb.ID.String() + `","key":"c"}`
	if err := db.Notify(bg, env.DB.Pool, db.ChanCancel, payload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cancel to wait for the key's lock", func() bool {
		r.k.mu.Lock()
		defer r.k.mu.Unlock()
		l := r.k.locks[callRef{sandbox: sb.ID, key: "c"}]
		return l != nil && l.refs == 2
	})
	close(gate)
	a := await(t, started)
	if !errors.Is(a.err, ErrCancelledByCaller) || a.info.State != CallDone {
		t.Errorf("the starter %+v", a)
	}
	if row := callRow(t, env, sb.ID, "c"); row.State != store.ToolCallDone || row.Error != ErrCancelledByCaller.Error() {
		t.Errorf("row %+v", row)
	}
}
