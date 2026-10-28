// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// fakeLeader is the elector of the loops' tests: it has led since since
// (zero: it does not lead), and its fence fails with fenceErr.
type fakeLeader struct {
	since    time.Time
	fenceErr error
}

func (l *fakeLeader) Fence(context.Context, db.Querier) error { return l.fenceErr }
func (l *fakeLeader) LeadingSince() time.Time                 { return l.since }

// loopRunner is the instance i1, whose loops lead through l.
func loopRunner(env *toolstest.Env, l *fakeLeader) *Runner {
	r := NewRunner(env.Engine, env.Port, env.Recorder, env.DB, "i1", env.Log)
	r.SetCallConfig(CallConfig{Leader: l})
	return r
}

// countCalls counts the sandbox's rows whose key starts with prefix and
// whose state is state ("": any).
func countCalls(t *testing.T, env *toolstest.Env, sb, prefix, state string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(bg, `SELECT count(*) FROM tool_calls WHERE sandbox_id = $1
		AND starts_with(call_key, $2) AND ($3 = '' OR state = $3)`, sb, prefix, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The leader's expiry ends the running rows whose lease ran out: a sent
// one interrupted, naming the instance that ran it, an unsent one not_run,
// each with ferun_call; a row whose lease runs on is left alone. One run
// ends more rows than a batch.
func TestExpireCalls(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := loopRunner(env, &fakeLeader{since: time.Now().Add(-time.Hour)})
	sb := env.NewSandbox()
	args := `{"command":"x"}`
	sent := foreignRow(t, env, sb, "sent", args, false, -time.Minute)
	if ok, err := env.Store.MarkToolCallSent(bg, sent.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	unsent := foreignRow(t, env, sb, "unsent", args, false, -time.Minute)
	foreignRow(t, env, sb, "live", args, true, time.Minute)
	calls, unsubscribe := env.DB.Subscribe(db.ChanCall)
	defer unsubscribe()

	if err := r.expireCalls(bg); err != nil {
		t.Fatal(err)
	}
	if row := callRow(t, env, sb.ID, "sent"); row.State != store.ToolCallInterrupted || row.FinishedAt.IsZero() ||
		!strings.Contains(row.Error, "interrupted: instance i9 that ran the call stopped renewing it") {
		t.Errorf("the sent row %+v", row)
	}
	if row := callRow(t, env, sb.ID, "unsent"); row.State != store.ToolCallNotRun ||
		!strings.Contains(row.Error, "not run: instance i9 that took the call stopped renewing it") {
		t.Errorf("the unsent row %+v", row)
	}
	if row := callRow(t, env, sb.ID, "live"); row.State != store.ToolCallRunning {
		t.Errorf("the row with a lease %+v", row)
	}
	want := map[string]bool{sent.ID.String(): true, unsent.ID.String(): true}
	deadline := time.After(5 * time.Second)
	for len(want) > 0 {
		select {
		case p := <-calls:
			delete(want, p)
		case <-deadline:
			t.Fatalf("no ferun_call for %v", want)
		}
	}
	unsubscribe()

	if _, err := env.DB.Pool.Exec(bg, `INSERT INTO tool_calls (sandbox_id, call_key, tool, args_hash, instance_id, sent, lease_until)
		SELECT $1, 'bulk-' || i, 'bash', sha256(i::text::bytea), 'i9', true, now() - interval '1 minute'
		FROM generate_series(1, $2::int) i`, sb.ID.String(), expiryBatch+44); err != nil {
		t.Fatal(err)
	}
	if err := r.expireCalls(bg); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(t, env, sb.ID.String(), "bulk-", store.ToolCallInterrupted); n != expiryBatch+44 {
		t.Errorf("%d of %d rows interrupted in one run", n, expiryBatch+44)
	}
}

// The leader ends no row while it has not led for a lease, once it lost
// the lease, nor within a lease of a failure of its database; past that
// lease it does.
func TestExpireGuard(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	l := &fakeLeader{since: time.Now()}
	r := loopRunner(env, l)
	sb := env.NewSandbox()
	foreignRow(t, env, sb, "k", `{"command":"x"}`, true, -time.Minute)
	expire := func(what, state string) {
		t.Helper()
		if err := r.expireCalls(bg); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if row := callRow(t, env, sb.ID, "k"); row.State != state {
			t.Fatalf("%s: the row %+v, want %s", what, row, state)
		}
	}

	expire("a leader of a moment", store.ToolCallRunning)
	l.since = time.Time{}
	expire("no leader", store.ToolCallRunning)
	l.since, l.fenceErr = time.Now().Add(-time.Hour), leader.ErrNotLeader
	expire("a lost lease", store.ToolCallRunning)

	// The database fails: the listener's connection is cut.
	l.fenceErr = nil
	var n int
	if err := env.DB.Pool.QueryRow(bg, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = 'ferund-listener'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminated %d listeners: %v", n, err)
	}
	waitFor(t, "the listener's failure", func() bool { return !env.DB.LastFailure().IsZero() })
	expire("a database failure within the lease", store.ToolCallRunning)

	r.SetCallConfig(CallConfig{Leader: l, Lease: 200 * time.Millisecond})
	waitFor(t, "a lease past the failure", r.callsHealthy)
	expire("a lease after the failure", store.ToolCallInterrupted)
}

// The retention deletes the records that ended more than the call
// retention ago, whatever their state, and keeps the recent and the
// running ones. One run deletes more rows than a batch.
func TestRetention(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := loopRunner(env, &fakeLeader{since: time.Now().Add(-time.Hour)})
	sb := env.NewSandbox()
	args := `{"command":"x"}`
	end := func(key, state string, ago time.Duration) {
		t.Helper()
		row := foreignRow(t, env, sb, key, args, true, time.Minute)
		if _, err := env.DB.Pool.Exec(bg, `UPDATE tool_calls SET state = $2, finished_at = now() - make_interval(secs => $3)
			WHERE id = $1`, row.ID, state, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{store.ToolCallDone, store.ToolCallInterrupted, store.ToolCallNotRun} {
		end("old-"+s, s, 2*time.Hour)
	}
	end("recent", store.ToolCallDone, 10*time.Minute)
	foreignRow(t, env, sb, "running", args, true, -2*time.Hour)

	if err := r.deleteFinished(bg); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{store.ToolCallDone, store.ToolCallInterrupted, store.ToolCallNotRun} {
		noCallRow(t, env, sb.ID, "old-"+s)
	}
	if row := callRow(t, env, sb.ID, "recent"); row.State != store.ToolCallDone {
		t.Errorf("the recent row %+v", row)
	}
	if row := callRow(t, env, sb.ID, "running"); row.State != store.ToolCallRunning {
		t.Errorf("the running row %+v", row)
	}

	if _, err := env.DB.Pool.Exec(bg, `INSERT INTO tool_calls (sandbox_id, call_key, tool, args_hash, instance_id, state,
		lease_until, finished_at)
		SELECT $1, 'bulk-' || i, 'bash', sha256(i::text::bytea), 'i9', 'done', now(), now() - interval '2 hours'
		FROM generate_series(1, $2::int) i`, sb.ID.String(), retentionBatch+500); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteFinished(bg); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(t, env, sb.ID.String(), "bulk-", ""); n != 0 {
		t.Errorf("%d of %d old rows left after one run", n, retentionBatch+500)
	}
}

// A Runner has the two loops with a database and a Leader, none without
// either; the hint HintCalls wakes both.
func TestCallLoops(t *testing.T) {
	r := NewRunner(nil, nil, nil, nil, "i1", nil)
	r.SetCallConfig(CallConfig{Leader: &fakeLeader{}})
	if got := r.Loops(); got != nil {
		t.Errorf("loops without a database: %+v", got)
	}

	d := dbtest.New(t)
	r = NewRunner(nil, nil, nil, d, "i1", nil)
	if got := r.Loops(); got != nil {
		t.Errorf("loops without a leader: %+v", got)
	}
	r.SetCallConfig(CallConfig{Leader: &fakeLeader{}})
	got := r.Loops()
	if len(got) != 2 || got[0].Name != LoopCallsExpiry || got[0].Interval != 5*time.Second || got[0].Run == nil ||
		got[1].Name != LoopCallsRetention || got[1].Interval != time.Minute || got[1].Run == nil {
		t.Fatalf("loops %+v", got)
	}
	for _, l := range got {
		if !strings.Contains(l.Name, HintCalls) {
			t.Errorf("the hint %q does not wake %s", HintCalls, l.Name)
		}
	}
}
