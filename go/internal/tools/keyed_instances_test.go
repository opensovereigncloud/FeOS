// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// twoRunners are two instances, i1 and i2, on one environment: one
// database, one node connection.
func twoRunners(env *toolstest.Env) (a, b *Runner) {
	a = NewRunner(env.Engine, env.Port, env.Recorder, env.DB, "i1", env.Log)
	b = NewRunner(env.Engine, env.Port, env.Recorder, env.DB, "i2", env.Log)
	return a, b
}

// foreignRow records a running bash call with args under key for the
// instance i9, which no Runner of the test is: sent or not, its lease
// ending lease from now (negative: it ran out), by the database's clock.
func foreignRow(t *testing.T, env *toolstest.Env, sb auth.Sandbox, key, args string, sent bool, lease time.Duration) store.ToolCall {
	t.Helper()
	hash, err := callHash(ToolBash, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	row, inserted, err := env.Store.BeginToolCall(bg, store.NewToolCall{
		SandboxID: sb.ID, Key: key, Tool: ToolBash, ArgsHash: hash, InstanceID: "i9", Lease: time.Minute,
	})
	if err != nil || !inserted {
		t.Fatal(inserted, err)
	}
	if _, err := env.DB.Pool.Exec(bg, `UPDATE tool_calls SET sent = $2, lease_until = now() + make_interval(secs => $3)
		WHERE id = $1`, row.ID, sent, lease.Seconds()); err != nil {
		t.Fatal(err)
	}
	return row
}

// hangUp starts the call of key on r, waits until started (the node's
// Exec began, or the row exists when started is nil) and ends the request,
// which leaves the call running.
func hangUp(t *testing.T, env *toolstest.Env, r *Runner, sb auth.Sandbox, args, key string, started chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	ch := callAsync(ctx, r, sb, ToolBash, args, key, nil)
	if started != nil {
		waitN(t, started, 1)
	} else {
		waitFor(t, "the row of "+key, func() bool {
			_, err := env.Store.GetToolCall(bg, sb.ID, key)
			return err == nil
		})
	}
	cancel()
	if a := await(t, ch); !errors.Is(a.err, context.Canceled) {
		t.Fatalf("the hung-up request of %s: %+v", key, a)
	}
}

// waitEnded waits until the row of key is no longer running, and returns
// it.
func waitEnded(t *testing.T, env *toolstest.Env, sb auth.Sandbox, key string) store.ToolCall {
	t.Helper()
	waitFor(t, "the row of "+key+" to end", func() bool {
		row, err := env.Store.GetToolCall(bg, sb.ID, key)
		return err == nil && row.State != store.ToolCallRunning
	})
	return callRow(t, env, sb.ID, key)
}

// A repeat on another instance waits for the row of the call that runs
// on the first: its request that ends cancels nothing, and a new one gets
// the stored answer once the call ended, woken by ferun_call.
func TestKeyedAttachOtherInstance(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	a, b := twoRunners(env)
	// b reads the row only when ferun_call names it.
	b.k.poll = time.Hour
	startRun(t, a)
	startRun(t, b)
	sb := env.NewSandbox()
	p := probeExec(env, "out\n")
	args := `{"command":"long"}`

	starter := callAsync(bg, a, sb, ToolBash, args, "k", nil)
	waitN(t, p.started, 1)
	ctx, cancel := context.WithCancel(bg)
	waiting := callAsync(ctx, b, sb, ToolBash, args, "k", nil)
	select {
	case x := <-waiting:
		t.Fatalf("b answered while the call runs on a: %+v", x)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	x := await(t, waiting)
	if !errors.Is(x.err, context.Canceled) || x.info.State != CallRunning || x.info.Instance != "i1" {
		t.Errorf("b's gone request: %+v", x)
	}
	select {
	case <-p.cancelled:
		t.Fatal("the node's exec was cancelled with b's request")
	case <-time.After(100 * time.Millisecond):
	}
	if row := callRow(t, env, sb.ID, "k"); row.State != store.ToolCallRunning || row.InstanceID != "i1" {
		t.Errorf("row %+v", row)
	}

	again := callAsync(bg, b, sb, ToolBash, args, "k", nil)
	select {
	case x := <-again:
		t.Fatalf("b answered again while the call runs on a: %+v", x)
	case <-time.After(200 * time.Millisecond):
	}
	close(p.release)
	want := CallInfo{Keyed: true, State: CallDone, Replay: true, Instance: "i1"}
	if x := await(t, again); x.err != nil || x.res.Text != "out\n" || x.info != want {
		t.Errorf("b's answer %+v", x)
	}
	if s := await(t, starter); s.err != nil || s.res.Text != "out\n" || s.info.Replay || s.info.State != CallDone {
		t.Errorf("a's starter %+v", s)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}
}

// CancelCall on another instance cancels the call where it runs.
func TestKeyedCancelOtherInstance(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	a, b := twoRunners(env)
	startRun(t, a)
	startRun(t, b)
	sb := env.NewSandbox()
	p := probeExec(env, "")

	starter := callAsync(bg, a, sb, ToolBash, `{"command":"x"}`, "c", nil)
	waitN(t, p.started, 1)
	if ok, err := b.CancelCall(bg, sb.ID, "c"); !ok || err != nil {
		t.Fatalf("b's cancel: %v %v", ok, err)
	}
	select {
	case <-p.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the node's exec was not cancelled")
	}
	s := await(t, starter)
	if !errors.Is(s.err, ErrCancelledByCaller) || s.info.State != CallDone || s.info.Instance != "i1" {
		t.Errorf("a's starter %+v", s)
	}
	if row := callRow(t, env, sb.ID, "c"); row.State != store.ToolCallDone || row.Error != ErrCancelledByCaller.Error() {
		t.Errorf("row %+v", row)
	}
}

// The instance that runs a call keeps its lease ahead of the database's
// clock: every renewal moves it a lease past the database's now(), and
// each after the first comes before the lease it renews ran out. (The
// first term ran from the insert's start, before the instance had the
// row: the insert's round trip and up to a third of a lease until the
// first renewal come out of it.)
func TestKeyedLeaseRenewed(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	// Each renewal is recorded in its own transaction, with the lease it
	// renewed: its now() is the clock the expiry judges by.
	for _, sql := range []string{
		`CREATE TABLE renewals (renewed timestamptz NOT NULL, lease_until timestamptz NOT NULL,
			at timestamptz NOT NULL)`,
		`CREATE FUNCTION record_renewal() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				INSERT INTO renewals VALUES (OLD.lease_until, NEW.lease_until, now());
				RETURN NEW;
			END $$`,
		`CREATE TRIGGER renewal AFTER UPDATE OF lease_until ON tool_calls
			FOR EACH ROW EXECUTE FUNCTION record_renewal()`,
	} {
		if _, err := env.DB.Pool.Exec(bg, sql); err != nil {
			t.Fatal(err)
		}
	}
	const lease = 600 * time.Millisecond
	a, _ := twoRunners(env)
	a.SetCallConfig(CallConfig{Lease: lease})
	startRun(t, a)
	sb := env.NewSandbox()
	p := probeExec(env, "")

	starter := callAsync(bg, a, sb, ToolBash, `{"command":"x"}`, "l", nil)
	waitN(t, p.started, 1)
	// Seven renewals, a third of a lease apart: the six terms after the
	// first cover two leases.
	waitFor(t, "seven renewals", func() bool {
		var n int
		if err := env.DB.Pool.QueryRow(bg, `SELECT count(*) FROM renewals`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n >= 7
	})
	rows, err := env.DB.Pool.Query(bg, `SELECT extract(epoch FROM renewed - at)::float8,
		extract(epoch FROM lease_until - at)::float8 FROM renewals ORDER BY at`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; rows.Next(); i++ {
		var left, ahead float64
		if err := rows.Scan(&left, &ahead); err != nil {
			t.Fatal(err)
		}
		if ahead != lease.Seconds() {
			t.Errorf("renewal %d moved the lease %.3f s past now(), want %.3f s", i, ahead, lease.Seconds())
		}
		if i > 1 && left <= 0 {
			t.Errorf("renewal %d came %.3f s after the lease it renewed ran out", i, -left)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	if s := await(t, starter); s.err != nil || s.info.State != CallDone {
		t.Errorf("the starter %+v", s)
	}
}

// A waiter ends the row of an instance that stopped renewing it, once its
// lease ran out more than a lease ago: interrupted when it was sent,
// not_run when not.
func TestWaiterExpires(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	var logged lockedBuffer
	b := NewRunner(env.Engine, env.Port, env.Recorder, env.DB, "i2", slog.New(slog.NewJSONHandler(&logged, nil)))
	b.SetCallConfig(CallConfig{Lease: 600 * time.Millisecond})
	startRun(t, b)
	sb := env.NewSandbox()
	p := probeExec(env, "")
	close(p.release)
	args := `{"command":"x"}`

	answer := func(ch chan keyedAnswer, within time.Duration) keyedAnswer {
		t.Helper()
		select {
		case x := <-ch:
			return x
		case <-time.After(within):
			t.Fatalf("no answer within %s", within)
		}
		return keyedAnswer{}
	}

	foreignRow(t, env, sb, "sent", args, true, -2*time.Second)
	x := answer(callAsync(bg, b, sb, ToolBash, args, "sent", nil), 3*time.Second)
	if x.err == nil || !strings.Contains(x.err.Error(), "instance i9 that ran the call stopped renewing it") ||
		x.info != (CallInfo{Keyed: true, State: CallInterrupted, Replay: true, Instance: "i9"}) {
		t.Errorf("the sent call: %+v", x)
	}
	if row := callRow(t, env, sb.ID, "sent"); row.State != store.ToolCallInterrupted {
		t.Errorf("row %+v", row)
	}

	foreignRow(t, env, sb, "unsent", args, false, -2*time.Second)
	x = answer(callAsync(bg, b, sb, ToolBash, args, "unsent", nil), 3*time.Second)
	if x.err == nil || !strings.Contains(x.err.Error(), "instance i9 that took the call stopped renewing it") ||
		x.info.State != CallNotRun {
		t.Errorf("the unsent call: %+v", x)
	}
	if row := callRow(t, env, sb.ID, "unsent"); row.State != store.ToolCallNotRun {
		t.Errorf("row %+v", row)
	}
	// Each verdict is logged, as the leader's count is.
	got := map[string]string{}
	for _, rec := range logged.records(t, "tool call ended by its waiter: its instance stopped renewing it") {
		if rec["level"] != "INFO" || rec["sandbox"] != sb.ID.String() || rec["instance"] != "i9" {
			t.Errorf("log record %v", rec)
		}
		key, _ := rec["key"].(string)
		got[key], _ = rec["state"].(string)
	}
	if len(got) != 2 || got["sent"] != store.ToolCallInterrupted || got["unsent"] != store.ToolCallNotRun {
		t.Errorf("logged verdicts %v", got)
	}

	// A lease that ran out 100 ms ago is left alone for one lease more, by
	// the database's clock: the waiter ends the row a lease after its
	// lease ran out at the earliest.
	foreignRow(t, env, sb, "recent", args, true, -100*time.Millisecond)
	if x := answer(callAsync(bg, b, sb, ToolBash, args, "recent", nil), 3*time.Second); x.info.State != CallInterrupted {
		t.Errorf("the recent call: %+v", x)
	}
	var after float64
	if err := env.DB.Pool.QueryRow(bg, `SELECT extract(epoch FROM finished_at - lease_until)::float8 FROM tool_calls
		WHERE sandbox_id = $1 AND call_key = $2`, sb.ID.String(), "recent").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after < (600 * time.Millisecond).Seconds() {
		t.Errorf("the waiter ended the recent row %.3f s after its lease ran out, within the margin of a lease (0.6 s)", after)
	}
	if n := p.n.Load(); n != 0 {
		t.Errorf("%d Execs", n)
	}
}

// WaitCall reads a call's record, waiting for its end while it runs, here
// or on another instance: a wait of 0 or less answers while the call runs,
// a call that ends within the wait answers at its end (a remote one woken
// by ferun_call), and a wait that passes answers running, no sooner.
func TestWaitCall(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	a, _ := twoRunners(env)
	// a reads a remote row only when ferun_call names it.
	a.k.poll = time.Hour
	startRun(t, a)
	sb := env.NewSandbox()
	p := probeExec(env, "out\n")
	args := `{"command":"x"}`

	type answered struct {
		rec  CallRecord
		err  error
		took time.Duration
	}
	waitCall := func(key string, wait time.Duration) chan answered {
		ch := make(chan answered, 1)
		go func() {
			start := time.Now()
			rec, err := a.WaitCall(bg, sb.ID, key, wait)
			ch <- answered{rec, err, time.Since(start)}
		}()
		return ch
	}
	got := func(ch chan answered) answered {
		t.Helper()
		select {
		case x := <-ch:
			return x
		case <-time.After(10 * time.Second):
			t.Fatal("WaitCall did not answer within 10 s")
		}
		return answered{}
	}
	// waiting waits until WaitCall waits for the call (attach).
	waiting := func() {
		t.Helper()
		waitFor(t, "WaitCall to wait", func() bool {
			a.k.mu.Lock()
			defer a.k.mu.Unlock()
			return a.k.waiters[sb.ID] == 1
		})
	}

	starter := callAsync(bg, a, sb, ToolBash, args, "w", nil)
	waitN(t, p.started, 1)
	// The call runs until p.release: running is an answer that did not wait
	// for its end.
	if x := got(waitCall("w", 0)); x.err != nil || x.rec.State != CallRunning || x.rec.Key != "w" ||
		x.rec.Tool != ToolBash || x.rec.Instance != "i1" || x.rec.StartedAt.IsZero() {
		t.Errorf("wait 0: %+v %v", x.rec, x.err)
	}
	// Within a wait of a minute, the call's end is the answer.
	ch := waitCall("w", maxRecordWait)
	waiting()
	close(p.release)
	if x := got(ch); x.err != nil || x.rec.State != CallDone || x.rec.Result.Text != "out\n" || x.rec.FinishedAt.IsZero() ||
		x.rec.Err != "" {
		t.Errorf("wait for the end: %+v %v after %s", x.rec, x.err, x.took)
	}
	await(t, starter)

	if _, err := a.WaitCall(bg, sb.ID, "unknown", time.Second); !errors.Is(err, ErrNoCall) {
		t.Errorf("an unknown key: %v", err)
	}
	if _, err := a.WaitCall(bg, sb.ID, "bad key", 0); !errors.Is(err, ErrBadCallKey) {
		t.Errorf("a bad key: %v", err)
	}

	// A remote call that runs on for a minute: a wait passes, and no sooner.
	foreignRow(t, env, sb, "remote", args, true, time.Minute)
	const wait = 300 * time.Millisecond
	if x := got(waitCall("remote", wait)); x.err != nil || x.rec.State != CallRunning || x.rec.Instance != "i9" ||
		x.took < wait {
		t.Errorf("a remote running call: %+v %v after %s, want running after %s at the earliest", x.rec, x.err, x.took, wait)
	}
	// A negative wait does not wait: running.
	if x := got(waitCall("remote", -time.Second)); x.err != nil || x.rec.State != CallRunning {
		t.Errorf("a negative wait: %+v %v", x.rec, x.err)
	}
	// A remote call that ends while the record is waited for.
	row := callRow(t, env, sb.ID, "remote")
	ch = waitCall("remote", maxRecordWait)
	waiting()
	if _, err := env.Store.FinishToolCall(bg, row.ID, store.ToolCallOutcome{State: store.ToolCallDone, Output: []byte("there")}); err != nil {
		t.Fatal(err)
	}
	if x := got(ch); x.err != nil || x.rec.State != CallDone || x.rec.Result.Text != "there" {
		t.Errorf("a remote call that ends: %+v %v after %s", x.rec, x.err, x.took)
	}
}

// The drain waits for a call whose caller left until it ended.
func TestDrainWaits(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	a, _ := twoRunners(env)
	startRun(t, a)
	sb := env.NewSandbox()
	p := probeExec(env, "out\n")
	hangUp(t, env, a, sb, `{"command":"x"}`, "d", p.started)

	a.BeginDrain(time.Now().Add(10 * time.Second))
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	drained := make(chan struct{})
	go func() {
		a.Drain(ctx)
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("Drain returned while a call runs")
	case <-time.After(300 * time.Millisecond):
	}
	close(p.release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return after the call ended")
	}
	if row := callRow(t, env, sb.ID, "d"); row.State != store.ToolCallDone || string(row.Output) != "out\n" {
		t.Errorf("row %+v", row)
	}
}

// At the shutdown grace the drain cuts the calls: a sent one ends
// interrupted with its output so far. New calls are refused not_run.
func TestDrainCuts(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	a, _ := twoRunners(env)
	startRun(t, a)
	sb := env.NewSandbox()
	p := probeExec(env, "partial\n")
	hangUp(t, env, a, sb, `{"command":"x"}`, "cut", p.started)

	start := time.Now()
	a.BeginDrain(time.Now().Add(300 * time.Millisecond))
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	defer cancel()
	a.Drain(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Drain took %s", d)
	}
	select {
	case <-p.cancelled:
	case <-time.After(2 * time.Second):
		t.Error("the node's exec was not cancelled")
	}
	row := callRow(t, env, sb.ID, "cut")
	if row.State != store.ToolCallInterrupted || !strings.HasPrefix(string(row.Output), "partial") ||
		!strings.Contains(row.Error, "shutdown grace") || !strings.Contains(row.Error, "instance i1 ") {
		t.Errorf("row %+v", row)
	}

	_, info, err := a.CallKeyed(bg, sb, ToolBash, json.RawMessage(`{"command":"new"}`), "new", nil)
	if !errors.Is(err, model.ErrStopping) || info.State != CallNotRun {
		t.Errorf("a new call while draining: %+v %v", info, err)
	}
	noCallRow(t, env, sb.ID, "new")
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}
}

// Once the drain began, a call is sent only if it ends before the
// shutdown grace; one that would not ends not_run without reaching the
// guest.
func TestDrainGateRefusesLongSend(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{PendingDelay: 2 * time.Second})
	a, _ := twoRunners(env)
	startRun(t, a)
	long, short := env.NewSandbox(), env.NewSandbox()
	p := probeExec(env, "short\n")
	close(p.release)
	// Both calls wait in Acquire for their pods when the drain begins.
	hangUp(t, env, a, long, `{"command":"long","timeout":60}`, "long", nil)
	hangUp(t, env, a, short, `{"command":"short","timeout":1}`, "short", nil)
	// 60 s + sendMargin do not fit before the deadline, 1 s + sendMargin
	// do.
	a.BeginDrain(time.Now().Add(45 * time.Second))

	row := waitEnded(t, env, long, "long")
	if row.State != store.ToolCallNotRun || row.Sent ||
		!strings.Contains(row.Error, "would not end before its shutdown grace") {
		t.Errorf("the long call's row %+v", row)
	}
	row = waitEnded(t, env, short, "short")
	if row.State != store.ToolCallDone || !row.Sent || string(row.Output) != "short\n" {
		t.Errorf("the short call's row %+v", row)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs", n)
	}
	// A repeat of the refused call here is refused as the instance stops,
	// retryable: its caller sends it to another instance.
	_, info, err := a.CallKeyed(bg, long, ToolBash, json.RawMessage(`{"command":"long","timeout":60}`), "long", nil)
	if !errors.Is(err, model.ErrStopping) || info.State != CallNotRun || !Retryable(err) {
		t.Errorf("a repeat of the refused call: %+v %v", info, err)
	}
}

// The drain's start answers the requests that wait for another instance's
// call at once: it still runs there.
func TestDrainReleasesRemoteWaiters(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	_, b := twoRunners(env)
	startRun(t, b)
	sb := env.NewSandbox()
	args := `{"command":"x"}`
	foreignRow(t, env, sb, "x", args, true, time.Minute)

	waiting := callAsync(bg, b, sb, ToolBash, args, "x", nil)
	type recAnswer struct {
		rec CallRecord
		err error
	}
	record := make(chan recAnswer, 1)
	go func() {
		rec, err := b.WaitCall(bg, sb.ID, "x", time.Minute)
		record <- recAnswer{rec, err}
	}()
	select {
	case x := <-waiting:
		t.Fatalf("answered before the drain: %+v", x)
	case <-time.After(300 * time.Millisecond):
	}
	b.BeginDrain(time.Now().Add(time.Minute))
	select {
	case x := <-waiting:
		var ae *AskAgainError
		if !errors.As(x.err, &ae) || ae.Instance != "i9" || x.info.State != CallRunning || x.info.Instance != "i9" ||
			!Retryable(x.err) {
			t.Errorf("the released waiter: %+v", x)
		}
	case <-time.After(time.Second):
		t.Fatal("the waiter was not released within 1 s")
	}
	select {
	case x := <-record:
		if x.err != nil || x.rec.State != CallRunning || x.rec.Instance != "i9" {
			t.Errorf("the released record wait: %+v", x)
		}
	case <-time.After(time.Second):
		t.Fatal("the record wait was not released within 1 s")
	}
	if row := callRow(t, env, sb.ID, "x"); row.State != store.ToolCallRunning {
		t.Errorf("row %+v", row)
	}
	// A request that arrives while draining is answered at once too.
	start := time.Now()
	_, info, err := b.CallKeyed(bg, sb, ToolBash, json.RawMessage(args), "x", nil)
	var ae *AskAgainError
	if !errors.As(err, &ae) || info.State != CallRunning || time.Since(start) > time.Second {
		t.Errorf("a request while draining: %+v %v after %s", info, err, time.Since(start))
	}
}

// When Run ends, the calls with an id still running are cut: a sent one
// ends interrupted.
func TestRunEndsKeyed(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	a, _ := twoRunners(env)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, "Run to record calls with an id", func() bool {
		a.k.mu.Lock()
		defer a.k.mu.Unlock()
		return a.k.renewing
	})
	sb := env.NewSandbox()
	p := probeExec(env, "")
	starter := callAsync(bg, a, sb, ToolBash, `{"command":"x"}`, "run", nil)
	waitN(t, p.started, 1)
	kc := localCall(a, sb.ID, "run")
	if kc == nil {
		t.Fatal("no local call")
	}
	cancel()
	select {
	case <-kc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the call did not end within 2 s of Run's end")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	s := await(t, starter)
	if s.info.State != CallInterrupted || s.err == nil || !strings.Contains(s.err.Error(), "shutdown grace") {
		t.Errorf("the starter %+v", s)
	}
	if row := callRow(t, env, sb.ID, "run"); row.State != store.ToolCallInterrupted || !row.Sent ||
		!strings.Contains(row.Error, "instance i1 stopped before the call ended (its shutdown grace passed)") {
		t.Errorf("row %+v", row)
	}
}

// A call that was not sent when the drain cuts it ends not_run.
func TestDrainFreesUnsent(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{PendingDelay: 30 * time.Second})
	a, _ := twoRunners(env)
	startRun(t, a)
	sb := env.NewSandbox()
	p := probeExec(env, "")
	hangUp(t, env, a, sb, `{"command":"x"}`, "free", nil)

	start := time.Now()
	a.BeginDrain(time.Now().Add(300 * time.Millisecond))
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	defer cancel()
	a.Drain(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Drain took %s", d)
	}
	row := callRow(t, env, sb.ID, "free")
	if row.State != store.ToolCallNotRun || row.Sent || !strings.Contains(row.Error, "instance i1 is stopping") {
		t.Errorf("row %+v", row)
	}
	if n := p.n.Load(); n != 0 {
		t.Errorf("%d Execs", n)
	}
}

// lockedBuffer is the output of a JSON logger that goroutines share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// records are the logged records with the message msg.
func (l *lockedBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	data := append([]byte(nil), l.buf.Bytes()...)
	l.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}
