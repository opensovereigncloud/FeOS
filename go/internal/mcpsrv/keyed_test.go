// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// runCalls runs the door's Runner's Run, which records calls with an id,
// until the test ends (runRunner).
func (h *harness) runCalls() {
	h.t.Helper()
	h.runRunner(h.runner)
}

// runRunner runs r.Run until the test ends, and waits until r records
// calls with an id. At the end it stops Run, which cuts the calls still
// running, and waits until each wrote its row.
func (h *harness) runRunner(r *tools.Runner) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("Run: %v", err)
		}
		dctx, dcancel := context.WithTimeout(bg, 30*time.Second)
		defer dcancel()
		r.Drain(dctx)
	})
	// Until Run records calls with an id, one is refused not_run as from
	// a stopping instance, and nothing runs.
	probe := "probe-" + uuid.NewString()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, _, err := r.CallKeyed(bg, h.sb, tools.ToolBash, json.RawMessage(`{"command":"true"}`), probe, nil)
		if err == nil {
			return
		}
		if !errors.Is(err, model.ErrStopping) || time.Now().After(deadline) {
			h.t.Fatalf("a call with an id before Run records them: %v", err)
		}
	}
}

// heldExec scripts the node's Execs: each writes "begun\n", counts
// itself and tells started, then waits until release is closed (exit 0)
// or its context ends (told on cancelled).
type heldExec struct {
	started, cancelled chan struct{}
	release            chan struct{}
	n                  atomic.Int32
}

func holdExec(env *toolstest.Env) *heldExec {
	x := &heldExec{started: make(chan struct{}, 100), cancelled: make(chan struct{}, 100), release: make(chan struct{})}
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		x.n.Add(1)
		_ = out.Stdout([]byte("begun\n"))
		x.started <- struct{}{}
		select {
		case <-x.release:
			return 0, true, nil
		case <-ctx.Done():
			x.cancelled <- struct{}{}
			return 0, true, status.FromContextError(ctx.Err()).Err()
		}
	})
	return x
}

// keyedCall runs a tool with id (any value) as _meta ferun.callId.
func keyedCall(ctx context.Context, cs *mcp.ClientSession, tool string, args map[string]any, id any) (*mcp.CallToolResult, error) {
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: mcp.Meta{"ferun.callId": id}})
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// callMeta checks a keyed result's ferun.* _meta (the SDK adds keys of
// its own on the newest protocol).
func callMeta(t *testing.T, res *mcp.CallToolResult, id, state string, replayed bool) {
	t.Helper()
	want := map[string]any{"ferun.callId": id, "ferun.callState": state, "ferun.callReplayed": replayed}
	ours := map[string]any{}
	for k, v := range res.Meta {
		if strings.HasPrefix(k, "ferun.") {
			ours[k] = v
		}
	}
	got, _ := json.Marshal(ours)
	if w, _ := json.Marshal(want); string(got) != string(w) {
		t.Errorf("_meta %s, want %s", got, w)
	}
}

// noCallMeta checks that a result has no ferun.* _meta.
func noCallMeta(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	for k := range res.Meta {
		if strings.HasPrefix(k, "ferun.") {
			t.Errorf("_meta %v on a call without a record", res.Meta)
		}
	}
}

// invalidParamsOf checks that err is a JSON-RPC error -32602 whose data has
// reason, and returns its message and data.
func invalidParamsOf(t *testing.T, err error, reason string) (string, map[string]any) {
	t.Helper()
	var e *jsonrpc.Error
	if !errors.As(err, &e) {
		t.Fatalf("not a JSON-RPC error: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("data %q: %v", e.Data, err)
	}
	if e.Code != jsonrpc.CodeInvalidParams || data["reason"] != reason {
		t.Errorf("error %d %q %v, want -32602 with reason %s", e.Code, e.Message, data, reason)
	}
	return e.Message, data
}

// waitRow waits until the sandbox's row of key is in state, and returns it.
func (h *harness) waitRow(key, state string) store.ToolCall {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		row, err := h.env.Store.GetToolCall(bg, h.sb.ID, key)
		if err == nil && row.State == state {
			return row
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the row of %s: %+v %v, want state %s", key, row, err, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) noRow(key string) {
	h.t.Helper()
	if row, err := h.env.Store.GetToolCall(bg, h.sb.ID, key); !errors.Is(err, store.ErrNoRow) {
		h.t.Errorf("%s has a row: %+v %v", key, row, err)
	}
}

// keyedBody is a raw tools/call of bash "wait" with the request id id and
// the call id key.
func keyedBody(id int, key string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"bash","arguments":{"command":"wait"},"_meta":{"ferun.callId":%q}}}`,
		id, key)
}

// cancelled sends a notifications/cancelled of the session sid for the
// request id to the door at url.
func (h *harness) cancelled(url, sid string, id int) {
	h.t.Helper()
	resp := h.post(url, h.token, sid,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d,"reason":"user"}}`, id),
		"Mcp-Protocol-Version", "2025-11-25")
	if got := body(h.t, resp); resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("cancel: %d %q", resp.StatusCode, got)
	}
}

func received(t *testing.T, ch chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("%s did not happen within %s", what, within)
	}
}

func notReceived(t *testing.T, ch chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s", what)
	case <-time.After(within):
	}
}

// Both protocol paths advertise the calls with an id, and keep the SDK's
// other capabilities.
func TestKeyedCapability(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	for _, tc := range []struct {
		name string
		opts *mcp.ClientSessionOptions
	}{{"discover", nil}, {"initialize", legacy}} {
		t.Run(tc.name, func(t *testing.T) {
			caps := h.session(tc.opts).InitializeResult().Capabilities
			if caps == nil || caps.Tools == nil || caps.Logging == nil || caps.Resources != nil || caps.Prompts != nil {
				t.Fatalf("capabilities %+v", caps)
			}
			got, _ := json.Marshal(caps.Experimental["ferun.callId"])
			if want := `{"maxTimeoutSeconds":600,"retentionSeconds":3600,"tools":["bash","edit","process_start","process_stop","write"]}`; string(got) != want {
				t.Errorf("experimental ferun.callId %s, want %s", got, want)
			}
		})
	}
	if !h.methods.has("initialize") || !h.methods.has("server/discover") {
		t.Errorf("methods seen: %v", h.methods.seen)
	}
}

// A call with an id over both protocol paths: its _meta, the replay, and
// the -32602 errors of a conflict, an invalid id and a timeout over the
// cap; a read-only tool ignores its id.
func TestKeyedCalls(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()
	for _, tc := range []struct {
		name string
		opts *mcp.ClientSessionOptions
	}{{"discover", nil}, {"initialize", legacy}} {
		t.Run(tc.name, func(t *testing.T) {
			cs := h.session(tc.opts)
			key := "fecode:" + tc.name + ":r1:c1"

			res, err := keyedCall(bg, cs, "bash", map[string]any{"command": "echo once"}, key)
			if err != nil || res.IsError || textOf(res) != "once\n" {
				t.Fatalf("first: %+v %v", res, err)
			}
			callMeta(t, res, key, "done", false)
			res, err = keyedCall(bg, cs, "bash", map[string]any{"command": "echo once"}, key)
			if err != nil || res.IsError || textOf(res) != "once\n" {
				t.Fatalf("again: %+v %v", res, err)
			}
			callMeta(t, res, key, "done", true)
			if row := h.waitRow(key, "done"); string(row.Output) != "once\n" || row.ExitCode != 0 {
				t.Errorf("row %+v", row)
			}

			// The same id with another command: refused, and nothing runs.
			_, err = keyedCall(bg, cs, "bash", map[string]any{"command": "echo other"}, key)
			msg, data := invalidParamsOf(t, err, "call_id_conflict")
			if !strings.HasPrefix(msg, "call id conflict") || strings.Contains(msg, key) || data["tool"] != "bash" {
				t.Errorf("conflict %q %v", msg, data)
			}

			// Ids that are not ids.
			for _, bad := range []any{42, "a/b", "", strings.Repeat("k", 256), nil} {
				_, err := keyedCall(bg, cs, "bash", map[string]any{"command": "echo bad"}, bad)
				msg, _ := invalidParamsOf(t, err, "invalid_call_id")
				if msg != "invalid _meta ferun.callId: "+tools.ErrBadCallKey.Error() {
					t.Errorf("invalid id %v: %q", bad, msg)
				}
			}

			// A keyed bash may name at most 600 s; nothing is recorded.
			long := key + ":long"
			_, err = keyedCall(bg, cs, "bash", map[string]any{"command": "true", "timeout": 601}, long)
			msg, data = invalidParamsOf(t, err, "timeout_too_long")
			if data["maxTimeoutSeconds"] != float64(600) || !strings.Contains(msg, "at most 600 s") {
				t.Errorf("timeout too long %q %v", msg, data)
			}
			h.noRow(long)
			// Without an id, as before.
			if text, isError, err := call(cs, "bash", map[string]any{"command": "echo long", "timeout": 601}); err != nil || isError || text != "long\n" {
				t.Errorf("unkeyed timeout 601: %q %v %v", text, isError, err)
			}

			// A write is recorded; a read with an id is not.
			path := "/w/" + tc.name + ".txt"
			res, err = keyedCall(bg, cs, "write", map[string]any{"path": path, "content": "alpha\n"}, key+":w")
			if err != nil || res.IsError || textOf(res) != "wrote "+path+"\n" {
				t.Fatalf("write: %+v %v", res, err)
			}
			callMeta(t, res, key+":w", "done", false)
			res, err = keyedCall(bg, cs, "read", map[string]any{"path": path}, key+":r")
			if err != nil || res.IsError || textOf(res) != "     1\talpha\n" {
				t.Fatalf("read: %+v %v", res, err)
			}
			noCallMeta(t, res)
			h.noRow(key + ":r")
			// A call without an id has no _meta of its own.
			res, err = cs.CallTool(bg, &mcp.CallToolParams{Name: "bash", Arguments: map[string]any{"command": "echo plain"}})
			if err != nil || textOf(res) != "plain\n" {
				t.Fatalf("plain: %+v %v", res, err)
			}
			noCallMeta(t, res)
		})
	}
}

// A request that ends does not cancel its call with an id, on either
// protocol path: FeCode's client (server/discover) that gives up, and a
// session client's request that hangs up without a cancel. The call ends
// on its own, and a repeat of the id reads its answer.
func TestKeyedHangUp(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()

	t.Run("discover", func(t *testing.T) {
		cs := h.session(nil)
		x := holdExec(h.env)
		ctx, cancel := context.WithCancel(bg)
		done := make(chan error, 1)
		go func() {
			_, err := keyedCall(ctx, cs, "bash", map[string]any{"command": "wait"}, "hang-up")
			done <- err
		}()
		wait(t, x.started, "the call's exec")
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Error("the abandoned call returned no error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the abandoned call did not return")
		}
		notReceived(t, x.cancelled, time.Second, "the hang-up cancelled the call with an id")
		if row := h.waitRow("hang-up", "running"); row.InstanceID != toolstest.InstanceID {
			t.Errorf("row %+v", row)
		}
		close(x.release)
		if row := h.waitRow("hang-up", "done"); string(row.Output) != "begun\n" || row.ExitCode != 0 || row.Error != "" {
			t.Errorf("row %+v", row)
		}
		res, err := keyedCall(bg, cs, "bash", map[string]any{"command": "wait"}, "hang-up")
		if err != nil || res.IsError || textOf(res) != "begun\n" {
			t.Fatalf("asked again: %+v %v", res, err)
		}
		callMeta(t, res, "hang-up", "done", true)
		if n := x.n.Load(); n != 1 {
			t.Errorf("%d execs", n)
		}
		h.env.Node.Script().SetExec(nil)
		waitHolds(t, h, 0)
	})

	t.Run("session", func(t *testing.T) {
		x := holdExec(h.env)
		sid := h.initialize(h.http.URL, h.token)
		ctx, cancel := context.WithCancel(bg)
		req, _ := http.NewRequestWithContext(ctx, "POST", h.http.URL, strings.NewReader(keyedBody(3, "hang-up-session")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set(sessionIDHeader, sid)
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		go func() {
			if resp, err := (&http.Client{Transport: transport()}).Do(req); err == nil {
				_ = body(t, resp)
			}
		}()
		wait(t, x.started, "the call's exec")
		cancel()
		notReceived(t, x.cancelled, time.Second, "the hang-up cancelled the call with an id")
		close(x.release)
		if row := h.waitRow("hang-up-session", "done"); string(row.Output) != "begun\n" || row.Error != "" {
			t.Errorf("row %+v", row)
		}
		h.env.Node.Script().SetExec(nil)
		waitHolds(t, h, 0)
	})
}

// A session client's abort through the SDK hangs up first and sends
// notifications/cancelled after: the cancel finds the call with an id of
// the request that ended, and cancels it in the guest.
func TestKeyedAbort(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()
	cs := h.session(legacy)
	x := holdExec(h.env)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() {
		_, err := keyedCall(ctx, cs, "bash", map[string]any{"command": "wait"}, "abort")
		done <- err
	}()
	wait(t, x.started, "the call's exec")
	cancel()
	received(t, x.cancelled, 5*time.Second, "the abort's cancel of the exec")
	if err := <-done; err == nil {
		t.Error("the aborted call returned no error")
	}
	if row := h.waitRow("abort", "done"); row.Error != tools.ErrCancelledByCaller.Error() || string(row.Output) != "begun\n" {
		t.Errorf("row %+v", row)
	}
	h.env.Node.Script().SetExec(nil)
	waitHolds(t, h, 0)
	// The session goes on, and the cancelled call's id answers its record.
	res, err := keyedCall(bg, cs, "bash", map[string]any{"command": "wait"}, "abort")
	if err != nil || !res.IsError || textOf(res) != "begun\n\nerror: "+tools.ErrCancelledByCaller.Error() {
		t.Fatalf("asked again: %+v %v", res, err)
	}
	callMeta(t, res, "abort", "done", true)
	if res, err := keyedCall(bg, cs, "bash", map[string]any{"command": "echo next"}, "next"); err != nil || textOf(res) != "next\n" {
		t.Errorf("after the abort: %+v %v", res, err)
	}
}

// A cancel that comes before its request, on this door or another, makes
// the request start nothing.
func TestKeyedEarlyCancel(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()
	other := h.newServer(Config{})
	otherHTTP := httptest.NewServer(other)
	defer otherHTTP.Close()
	var execs atomic.Int32
	h.env.Node.Script().SetExec(func(_ context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		execs.Add(1)
		_ = out.Stdout([]byte("ran\n"))
		return 0, true, nil
	})
	sid := h.initialize(h.http.URL, h.token)
	send := func(id int, key string) string {
		t.Helper()
		resp := h.post(h.http.URL, h.token, sid, keyedBody(id, key), "Mcp-Protocol-Version", "2025-11-25")
		return body(t, resp)
	}

	// The door's own memory.
	h.srv.cancelKey(clientOf(store.HashToken(h.token)), callKey(sid, json.RawMessage("7")))
	if got := send(7, "early-7"); !strings.Contains(got, `"id":7`) || !strings.Contains(got, "context canceled") {
		t.Errorf("answer %q", got)
	}
	// A notifications/cancelled through this door.
	h.cancelled(h.http.URL, sid, 8)
	if got := send(8, "early-8"); !strings.Contains(got, "context canceled") {
		t.Errorf("answer %q", got)
	}
	// One through another door, which tells this one with ferun_cancel.
	h.cancelled(otherHTTP.URL, sid, 9)
	deadline := time.Now().Add(10 * time.Second)
	for !h.srv.recentlyCancelled(callKey(sid, json.RawMessage("9"))) {
		if time.Now().After(deadline) {
			t.Fatal("the other door's cancel did not reach this one")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := send(9, "early-9"); !strings.Contains(got, "context canceled") {
		t.Errorf("answer %q", got)
	}
	if n := execs.Load(); n != 0 {
		t.Errorf("%d execs of cancelled requests", n)
	}
	for _, key := range []string{"early-7", "early-8", "early-9"} {
		h.noRow(key)
	}
	// A request that was not cancelled runs.
	if got := send(10, "early-10"); !strings.Contains(got, `ran\n`) {
		t.Errorf("answer %q", got)
	}
	if n := execs.Load(); n != 1 {
		t.Errorf("%d execs", n)
	}
	h.env.Node.Script().SetExec(nil)
}

// A request for another instance's call that this instance releases at
// its drain's start is an isError result with state running: the call
// still runs there.
func TestKeyedAskAgain(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()
	i2 := tools.NewRunner(h.env.Engine, h.env.Port, h.env.Recorder, h.env.DB, "i2", h.env.Log)
	h.runRunner(i2)
	x := holdExec(h.env)
	defer close(x.release)
	go func() {
		_, _, _ = i2.CallKeyed(bg, h.sb, tools.ToolBash, json.RawMessage(`{"command":"wait"}`), "elsewhere", nil)
	}()
	wait(t, x.started, "the other instance's exec")
	h.runner.BeginDrain(time.Now().Add(time.Minute))
	for _, opts := range []*mcp.ClientSessionOptions{nil, legacy} {
		res, err := keyedCall(bg, h.session(opts), "bash", map[string]any{"command": "wait"}, "elsewhere")
		if err != nil || !res.IsError || textOf(res) != "error: "+(&tools.AskAgainError{Instance: "i2"}).Error() {
			t.Fatalf("released: %+v %v", res, err)
		}
		callMeta(t, res, "elsewhere", "running", true)
	}
	if n := x.n.Load(); n != 1 {
		t.Errorf("%d execs", n)
	}
}

// A notifications/cancelled that reaches another door cancels the call
// with an id, through ferun_cancel: while its request runs, and after it
// hung up.
func TestKeyedCancelAcrossServers(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()
	other := httptest.NewServer(h.newServer(Config{}))
	defer other.Close()
	sid := h.initialize(h.http.URL, h.token)

	t.Run("running", func(t *testing.T) {
		x := holdExec(h.env)
		answered := make(chan string, 1)
		go func() {
			resp := h.post(h.http.URL, h.token, sid, keyedBody(5, "across"), "Mcp-Protocol-Version", "2025-11-25")
			answered <- body(t, resp)
		}()
		wait(t, x.started, "the call's exec")
		h.cancelled(other.URL, sid, 6) // another request
		notReceived(t, x.cancelled, 500*time.Millisecond, "a cancel of another request cancelled the call")
		h.cancelled(other.URL, sid, 5)
		received(t, x.cancelled, 5*time.Second, "the cancel of the exec")
		select {
		case got := <-answered:
			if !strings.Contains(got, `"id":5`) || !strings.Contains(got, "context canceled") {
				t.Errorf("answer of the cancelled request %q", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the cancelled request did not answer")
		}
		if row := h.waitRow("across", "done"); row.Error != tools.ErrCancelledByCaller.Error() {
			t.Errorf("row %+v", row)
		}
		h.env.Node.Script().SetExec(nil)
		waitHolds(t, h, 0)
	})

	t.Run("after the hang-up", func(t *testing.T) {
		x := holdExec(h.env)
		ctx, cancel := context.WithCancel(bg)
		req, _ := http.NewRequestWithContext(ctx, "POST", h.http.URL, strings.NewReader(keyedBody(11, "across-linger")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set(sessionIDHeader, sid)
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			if resp, err := (&http.Client{Transport: transport()}).Do(req); err == nil {
				_ = body(t, resp)
			}
		}()
		wait(t, x.started, "the call's exec")
		cancel()
		<-returned
		// The request's handler ended: its call lingers under its key.
		mcpKey := callKey(sid, json.RawMessage("11"))
		deadline := time.Now().Add(10 * time.Second)
		for {
			h.srv.mu.Lock()
			e := h.srv.keyed.entries[mcpKey]
			lingers := e != nil && !e.pinned()
			h.srv.mu.Unlock()
			if lingers {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the hung-up request's handler did not end")
			}
			time.Sleep(10 * time.Millisecond)
		}
		notReceived(t, x.cancelled, 500*time.Millisecond, "the hang-up cancelled the call with an id")
		h.cancelled(other.URL, sid, 11)
		received(t, x.cancelled, 5*time.Second, "the late cancel of the exec")
		if row := h.waitRow("across-linger", "done"); row.Error != tools.ErrCancelledByCaller.Error() {
			t.Errorf("row %+v", row)
		}
		h.env.Node.Script().SetExec(nil)
		waitHolds(t, h, 0)
	})
}

// The door's memory of calls with an id and of early cancels: an entry
// lingers cancelLinger after its request, a cancel that found none counts
// for recentFor, and a client's entries are bounded: its oldest ended ones
// go, its running ones stay, and a client whose part is full of running
// requests is refused the next one, retryably.
func TestKeyedCancelMemory(t *testing.T) {
	s, clk := bareServer()
	id := podid.ID("sbx")

	// While the request runs, and within the linger: counted.
	end, err := s.registerKeyed("c", "s:1", id, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if n := s.cancelKey("c", "s:1"); n != 1 {
		t.Errorf("running: %d", n)
	}
	end()
	clk.add(cancelLinger - time.Millisecond)
	if n := s.cancelKey("c", "s:1"); n != 1 || s.recentlyCancelled("s:1") {
		t.Errorf("lingering: %d", n)
	}
	// After the linger: not counted, and remembered as an early cancel.
	clk.add(time.Millisecond)
	if n := s.cancelKey("c", "s:1"); n != 0 || !s.recentlyCancelled("s:1") {
		t.Errorf("after the linger: %d", n)
	}
	clk.add(recentFor)
	if s.recentlyCancelled("s:1") {
		t.Error("an early cancel counts after recentFor")
	}
	if nk, nr := s.held(); nk != 0 || nr != 0 {
		t.Errorf("%d entries and %d cancels kept past their time", nk, nr)
	}

	// Bounds: the client's oldest ended entries and cancels go; its
	// running entry stays.
	running, err := s.registerKeyed("c", "run", id, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer running()
	for i := range maxPerClient + 10 {
		clk.add(time.Millisecond)
		end, err := s.registerKeyed("c", fmt.Sprintf("s:%d", i+2), id, "k")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		end()
		s.cancelKey("c", fmt.Sprintf("c:%d", i))
	}
	s.mu.Lock()
	_, runs := s.keyed.entries["run"]
	_, oldest := s.keyed.entries["s:2"]
	_, newest := s.keyed.entries[fmt.Sprintf("s:%d", maxPerClient+11)]
	_, first := s.recent.entries["c:0"]
	_, last := s.recent.entries[fmt.Sprintf("c:%d", maxPerClient+9)]
	s.mu.Unlock()
	if nk, nr := s.held(); nk != maxPerClient || nr != maxPerClient || !runs || oldest || !newest || first || !last {
		t.Errorf("%d entries, %d cancels, running %v, oldest %v, newest %v, first cancel %v, last cancel %v",
			nk, nr, runs, oldest, newest, first, last)
	}

	// A client whose part is full of running requests.
	var ends []func()
	for i := range maxPerClient {
		end, err := s.registerKeyed("full", fmt.Sprintf("f:%d", i), id, "k")
		if err != nil {
			t.Fatalf("running request %d: %v", i, err)
		}
		ends = append(ends, end)
	}
	if _, err := s.registerKeyed("full", "f:more", id, "k"); !errors.Is(err, errClientFull) ||
		!errors.Is(err, tools.ErrCallCap) || !tools.Retryable(err) {
		t.Errorf("past the cap: %v", err)
	}
	if end, err := s.registerKeyed("other", "o:1", id, "k"); err != nil {
		t.Errorf("another client: %v", err)
	} else {
		end()
	}
	// A request that ended makes room: its entry goes for the new one.
	ends[0]()
	if end, err := s.registerKeyed("full", "f:more", id, "k"); err != nil {
		t.Errorf("after a request ended: %v", err)
	} else {
		end()
	}
	for _, end := range ends[1:] {
		end()
	}
	s.mu.Lock()
	n := s.keyed.clients["full"].n
	s.mu.Unlock()
	if n != maxPerClient {
		t.Errorf("the client holds %d entries", n)
	}
}
