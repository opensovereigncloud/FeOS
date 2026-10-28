// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Calls with an id (ADR 0023). A call of a tool with a side effect that
// carries an id (a key, unique per sandbox) is a row of tool_calls and
// runs on a context of its own, in the instance that received it: the
// request only waits for it, so a request that ends cancels nothing. A
// repeat of the id with the same call attaches to it, through memory on
// this instance and through the row on another; a repeat with another
// call is refused. The row commits sent before the call's request leaves
// for the guest, so its outcome is exact: done, interrupted (it may have
// run, and stopped without a recorded result) or not_run (it cannot have
// run; the same call may be sent again under the same id). A stopping
// instance drains its calls (keyed_drain.go).

// MaxCallKey is the longest call id, in bytes.
const MaxCallKey = 255

// MaxKeyedTimeout is the longest timeout a bash call with an id may name.
// It bounds a call's detached life, and with it a stopping instance's
// wait for its calls.
const MaxKeyedTimeout = 10 * time.Minute

// The states of a call with an id, as CallInfo and CallRecord report them
// (the rows' states).
const (
	CallDone        = store.ToolCallDone
	CallInterrupted = store.ToolCallInterrupted
	CallNotRun      = store.ToolCallNotRun
	CallRunning     = store.ToolCallRunning
)

// Fixed bounds of the calls with an id.
const (
	// callCap is how many calls with an id one sandbox may have in flight
	// on this instance.
	callCap = 64
	// waiterCap is how many requests of one sandbox may wait on this
	// instance for calls with an id they did not start: attaches, in
	// memory or on another instance's row, and record waits (WaitCall).
	// Each costs a goroutine, and each that waits on a row reads it every
	// callPoll.
	waiterCap = 4 * callCap
	// callPoll is how often a request that waits for another instance's
	// call reads its row without a notification.
	callPoll = time.Second
	// callSlack is what a call's budget allows beyond the sandbox's
	// preparation and the tool's timeout.
	callSlack = time.Minute
	// sendMargin is what a stopping instance allows beyond a call's
	// timeout before its shutdown grace, for the call to be sent.
	sendMargin = 30 * time.Second
	// finishTimeout bounds the retries of a call's final write.
	finishTimeout = 5 * time.Minute
	// cutWrite bounds the final writes after the calls were cut (Run
	// ended, or the drain passed the shutdown grace).
	cutWrite = 15 * time.Second
	// waitErrors is how long a request that waits for a row tolerates
	// database errors.
	waitErrors = time.Minute
	// maxCallError is where a stored error text is cut, in bytes.
	maxCallError = 4 << 10
	// maxAttempts is how many times a request joins a call whose row was
	// replaced or deleted while it waited.
	maxAttempts = 3
	// maxRecordWait is the longest wait of WaitCall (the record route's
	// ?wait).
	maxRecordWait = 60 * time.Second
)

// CallInfo is what CallKeyed tells about the call besides its answer.
// Keyed is false when the call ran on its request without a record (no
// key, a read-only tool, a Runner without a database); then State is "".
type CallInfo struct {
	Keyed bool
	// State is CallDone, CallInterrupted, CallNotRun or CallRunning, or ""
	// when the call was refused before it had one (a conflict).
	State string
	// Replay is true when the answer is another request's call: attached
	// to while it ran, or read from its record.
	Replay bool
	// Instance is the instance that ran (or runs) the call.
	Instance string
}

// Leader is what the leader loops of the calls with an id need of the
// elector (*leader.Elector): its fence and how long it has led.
type Leader interface {
	Fence(ctx context.Context, q db.Querier) error
	LeadingSince() time.Time
}

// CallConfig tunes the calls with an id: Lease and Retention default to
// config.CallLease and config.CallRetention when not positive; Leader is
// the elector for the leader loops (nil: no loops).
type CallConfig struct {
	Lease, Retention time.Duration
	Leader           Leader
}

// SetCallConfig sets the Runner's CallConfig. Call it before Run.
func (r *Runner) SetCallConfig(c CallConfig) {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	r.k.lease, r.k.retention, r.k.leader = config.CallLease, config.CallRetention, c.Leader
	if c.Lease > 0 {
		r.k.lease = c.Lease
	}
	if c.Retention > 0 {
		r.k.retention = c.Retention
	}
}

// CallRetention is how long the record of a call with an id is kept after
// the call ended.
func (r *Runner) CallRetention() time.Duration {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return r.k.retention
}

// ValidCallKey reports whether key is a call id: 1 to MaxCallKey bytes,
// each one of A-Z a-z 0-9 . _ ~ : @ -. Such an id is never escaped in a
// URL path and is safe inside an RFC 8941 sf-string.
func ValidCallKey(key string) bool {
	if len(key) == 0 || len(key) > MaxCallKey {
		return false
	}
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == '~', c == ':', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// KeyedTools are the tools whose calls with an id are recorded: those with
// a side effect, sorted. On the other tools an id is checked and ignored.
func KeyedTools() []string {
	return []string{ToolBash, ToolEdit, ToolProcessStart, ToolProcessStop, ToolWrite}
}

// keyedTool reports whether tool is one of KeyedTools.
func keyedTool(tool string) bool {
	switch tool {
	case ToolBash, ToolEdit, ToolProcessStart, ToolProcessStop, ToolWrite:
		return true
	}
	return false
}

// callHash is the SHA-256 of a keyed tool and its normalised arguments:
// decoded into the tool's parameters and encoded again, so that spacing,
// key order, unknown keys and zero values spelled out do not make another
// call. write hashes as writeHash.
func callHash(tool string, args json.RawMessage) ([]byte, error) {
	var p any
	switch tool {
	case ToolBash:
		p = &BashParams{}
	case ToolEdit:
		p = &EditParams{}
	case ToolProcessStart:
		p = &ProcessStartParams{}
	case ToolProcessStop:
		p = &ProcessStopParams{}
	case ToolWrite:
		var w WriteParams
		if err := json.Unmarshal(args, &w); err != nil {
			return nil, err
		}
		return writeHash(w.Path, []byte(w.Content)), nil
	default:
		return nil, fmt.Errorf("tool %q has no calls with an id", tool)
	}
	if err := json.Unmarshal(args, p); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(b)
	return h.Sum(nil), nil
}

// writeHash is the argument hash of a write of content into path: SHA-256
// over "write", a zero byte, the path's length as a uvarint, the path and
// the content. REST's raw body and MCP's string of the same bytes hash
// equally. The path is hashed as given.
func writeHash(path string, content []byte) []byte {
	h := sha256.New()
	h.Write([]byte(ToolWrite + "\x00"))
	h.Write(binary.AppendUvarint(nil, uint64(len(path))))
	h.Write([]byte(path))
	h.Write(content)
	return h.Sum(nil)
}

// callRef names a call with an id: the sandbox and the key.
type callRef struct {
	sandbox podid.ID
	key     string
}

// refLock is the per-key lock of a callRef, kept in keyedState.locks
// while anyone holds or waits for it.
type refLock struct {
	mu   sync.Mutex
	refs int
}

// keyedCall is a call with an id that runs on this instance, from its
// insert until its final write is done (settle).
type keyedCall struct {
	id       uuid.UUID // the row's id
	ref      callRef
	tool     string
	hash     []byte
	instance string
	started  time.Time // the row's started_at
	ctx      context.Context
	cancel   context.CancelCauseFunc
	stop     func() // ends ctx's timer and its tie to keyedState.bg

	// sentRecorded: MarkToolCallSent committed sent. Only the call's own
	// goroutine touches it.
	sentRecorded bool

	mu         sync.Mutex
	beats      map[chan struct{}]struct{} // the waiters' heartbeats
	ended      bool                       // callRun returned: renewal misses are expected
	lostWarned bool

	done chan struct{} // closed by settle
	out  outcome       // set by settle before done closes
}

// outcome is how a call with an id ended, as its local waiters get it.
type outcome struct {
	res   Result
	err   error
	state string
}

// keyedState is the Runner's part for the calls with an id.
type keyedState struct {
	lease, retention time.Duration
	leader           Leader
	cap, waiterCap   int
	budget           func(timeout time.Duration) time.Duration
	// poll is how often a request that waits for another run's row reads
	// it without a notification: callPoll; tests replace it.
	poll time.Duration
	// bg ends every call with an id: Run's end (errRunnerStopped) and the
	// drain's cut (errStopping) cancel it.
	bg       context.Context
	bgCancel context.CancelCauseFunc
	// The row writes that tests replace: Store.BeginToolCall,
	// Store.FinishToolCall and Store.AbandonToolCall.
	begin   func(ctx context.Context, c store.NewToolCall) (store.ToolCall, bool, error)
	finish  func(ctx context.Context, id uuid.UUID, o store.ToolCallOutcome) (bool, error)
	abandon func(ctx context.Context, id podid.ID, key, instanceID, reason string) (bool, error)

	mu    sync.Mutex
	calls map[callRef]*keyedCall
	locks map[callRef]*refLock
	// waiters counts by sandbox the requests that wait for a call with an
	// id they did not start (attach).
	waiters map[podid.ID]int
	// perSandbox counts the reserved slots by sandbox: the calls in flight
	// and the joins between their reservation and their insert.
	perSandbox    map[podid.ID]int
	renewing      bool // Run runs: calls with an id are recorded
	draining      bool
	drainDeadline time.Time
	release       chan struct{} // closed when the drain begins
	// wg counts the reserved slots, so that the drain waits for them.
	wg sync.WaitGroup
}

// lock takes the per-key lock of ref and returns its unlock, which may be
// called more than once.
func (k *keyedState) lock(ref callRef) func() {
	k.mu.Lock()
	l := k.locks[ref]
	if l == nil {
		l = &refLock{}
		k.locks[ref] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Unlock()
			k.mu.Lock()
			if l.refs--; l.refs == 0 {
				delete(k.locks, ref)
			}
			k.mu.Unlock()
		})
	}
}

// reserve takes one of the sandbox's call slots on this instance, and
// counts it in wg, or refuses: a stopping instance, one that does not
// record calls with an id now (Run does not run), a full cap.
func (k *keyedState) reserve(id podid.ID, instanceID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case k.draining:
		return fmt.Errorf("instance %s is stopping: %w", instanceID, model.ErrStopping)
	case !k.renewing:
		return fmt.Errorf("instance %s does not record calls with an id now (it is starting or stopping): %w",
			instanceID, model.ErrStopping)
	case k.perSandbox[id] >= k.cap:
		return fmt.Errorf("sandbox %s: %w (at most %d on this instance)", id, ErrCallCap, k.cap)
	}
	k.perSandbox[id]++
	k.wg.Add(1)
	return nil
}

// unreserve gives a slot back.
func (k *keyedState) unreserve(id podid.ID) {
	k.mu.Lock()
	if k.perSandbox[id]--; k.perSandbox[id] <= 0 {
		delete(k.perSandbox, id)
	}
	k.mu.Unlock()
	k.wg.Done()
}

// attach counts a request of the sandbox id that waits for a call with an
// id it did not start, or refuses it with ErrCallCap when the sandbox has
// waiterCap such requests on this instance. The returned function ends
// the count.
func (k *keyedState) attach(id podid.ID) (func(), error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.waiters[id] >= k.waiterCap {
		return nil, fmt.Errorf("sandbox %s: %w (at most %d requests waiting for them on this instance)",
			id, ErrCallCap, k.waiterCap)
	}
	k.waiters[id]++
	return func() {
		k.mu.Lock()
		if k.waiters[id]--; k.waiters[id] <= 0 {
			delete(k.waiters, id)
		}
		k.mu.Unlock()
	}, nil
}

// CallKeyed runs one tool of the baseline in the sandbox sb like Call,
// as the call with the id key (ADR 0023).
//
// Without a key, on a tool without a side effect (KeyedTools) and on a
// Runner without a database, it is Call, and CallInfo is zero. Otherwise
// the key must be ValidCallKey (ErrBadCallKey), a bash timeout must not
// pass MaxKeyedTimeout (*KeyedTimeoutError), and:
//   - the first request of the key records the call and runs it on a
//     context of its own; the request only waits for it (Replay false);
//   - a repeat with the same call attaches to it while it runs, or reads
//     its stored answer (Replay true), on whichever instance; an attach
//     while the sandbox has waiterCap requests waiting on this instance
//     is refused with ErrCallCap, state running;
//   - a repeat with another tool or other arguments is a
//     *CallIDConflictError, and nothing runs;
//   - a call that did not run (CallNotRun: not prepared, refused, not
//     sent) may be sent again with the same key, and then runs.
//
// A request whose ctx ends returns ctx's error; the call runs on. A
// request that waits for another instance's call while this instance
// drains (BeginDrain) gets *AskAgainError, state running. progress gets ""
// for every chunk of a call that runs here (heartbeats only).
func (r *Runner) CallKeyed(ctx context.Context, sb auth.Sandbox, tool string, args json.RawMessage, key string,
	progress func(chunk string)) (Result, CallInfo, error) {
	if !known(tool) {
		return Result{}, CallInfo{}, fmt.Errorf("unknown tool %q", tool)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	c, err := prepare(tool, args, r.defaultTimeout)
	if err != nil {
		return Result{}, CallInfo{}, err
	}
	// The key is checked on every tool, and ignored on those without a
	// side effect (ADR 0023).
	if key != "" && !ValidCallKey(key) {
		return Result{}, CallInfo{}, ErrBadCallKey
	}
	if key == "" || r.d == nil || !keyedTool(tool) {
		res, err := r.callPrepared(ctx, sb, c, args, progress)
		return res, CallInfo{}, err
	}
	if tool == ToolBash && c.timeout > MaxKeyedTimeout {
		return Result{}, CallInfo{}, &KeyedTimeoutError{Max: MaxKeyedTimeout}
	}
	hash, err := callHash(tool, args)
	if err != nil {
		return Result{}, CallInfo{}, err
	}
	return r.callKeyed(ctx, sb, c, args, key, hash, progress)
}

// callKeyed joins the call c with the id key and the argument hash hash,
// up to maxAttempts times while the row it waited for was replaced or
// deleted.
func (r *Runner) callKeyed(ctx context.Context, sb auth.Sandbox, c *call, args json.RawMessage, key string, hash []byte,
	progress func(chunk string)) (Result, CallInfo, error) {
	ref := callRef{sandbox: sb.ID, key: key}
	for range maxAttempts {
		res, info, again, err := r.join(ctx, sb, c, args, ref, hash, progress)
		if !again {
			return res, info, err
		}
	}
	return Result{}, CallInfo{Keyed: true, State: CallNotRun}, ErrCallNotRun
}

// notRun is the CallInfo of a call that did not run here.
func (r *Runner) notRun() CallInfo {
	return CallInfo{Keyed: true, State: CallNotRun, Instance: r.instanceID}
}

// join is one turn of a request of the call c with the id ref: it attaches
// to the call or its record, refuses it, or records and starts it, and
// waits for the answer. again reports that the row it waited for was
// replaced or deleted: the caller joins once more.
func (r *Runner) join(ctx context.Context, sb auth.Sandbox, c *call, args json.RawMessage, ref callRef, hash []byte,
	progress func(chunk string)) (res Result, info CallInfo, again bool, err error) {
	// A request already gone starts nothing.
	if err := ctx.Err(); err != nil {
		return Result{}, CallInfo{}, false, err
	}
	unlock := r.k.lock(ref)
	defer unlock()

	r.k.mu.Lock()
	kc := r.k.calls[ref]
	r.k.mu.Unlock()
	if kc != nil {
		if kc.tool != c.tool || !bytes.Equal(kc.hash, hash) {
			return Result{}, CallInfo{Keyed: true}, false, &CallIDConflictError{Tool: kc.tool}
		}
		unlock()
		detach, err := r.k.attach(ref.sandbox)
		if err != nil {
			return Result{}, CallInfo{Keyed: true, State: CallRunning, Replay: true, Instance: kc.instance}, false, err
		}
		defer detach()
		res, info, err := r.waitLocal(ctx, kc, progress, true)
		return res, info, false, err
	}

	if refusal := r.k.reserve(ref.sandbox, r.instanceID); refusal != nil {
		return r.refused(ctx, ref, c.tool, hash, refusal, unlock)
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	row, inserted, err := r.k.begin(bctx, store.NewToolCall{
		SandboxID: ref.sandbox, Key: ref.key, Tool: c.tool, ArgsHash: hash, InstanceID: r.instanceID, Lease: r.lease(),
	})
	cancel()
	if err != nil {
		r.k.unreserve(ref.sandbox)
		if errors.Is(err, model.ErrNotFound) {
			return Result{}, r.notRun(), false, fmt.Errorf("ensure sandbox: %w", err)
		}
		// The insert may have committed without its answer: end that row
		// now, so that a repeat does not wait for its lease. If even this
		// fails, its expiry ends it not_run, as it was never sent. The key
		// stays locked until then: the abandon matches the key's unsent
		// running row of this instance, not the row's id, and would end
		// the run of a request of the key that inserted meanwhile.
		r.abandon(ctx, ref, err)
		return Result{}, r.notRun(), false, fmt.Errorf("%w: %w", ErrRecord, err)
	}
	if !inserted {
		r.k.unreserve(ref.sandbox)
		return r.existing(ctx, ref, row, c.tool, hash, unlock)
	}
	kc = r.startKeyed(ctx, sb, c, args, ref, row)
	unlock()
	res, info, err = r.waitLocal(ctx, kc, progress, false)
	return res, info, false, err
}

// lease is the call lease.
func (r *Runner) lease() time.Duration {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return r.k.lease
}

// abandon ends the row of ref as not_run when it is this instance's
// running call that was not sent: the insert that failed with cause may
// have committed. Best effort, on a detached context.
func (r *Runner) abandon(ctx context.Context, ref callRef, cause error) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	reason := sanitizeError(fmt.Errorf("not run: the record could not be written: %w", cause))
	if _, err := r.k.abandon(actx, ref.sandbox, ref.key, r.instanceID, reason); err != nil {
		r.rl.warn("abandon", "end a tool call whose record could not be written", "sandbox", ref.sandbox, "err", err)
	}
}

// refused answers a request that reserve refused with refusal: the key's
// row, if it has a usable one (running, or ended other than not_run, is
// attached to or answered; a not_run row of another call is a conflict),
// and otherwise refusal itself, not_run. Nothing is written.
func (r *Runner) refused(ctx context.Context, ref callRef, tool string, hash []byte, refusal error,
	unlock func()) (Result, CallInfo, bool, error) {
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	row, err := r.st.GetToolCall(gctx, ref.sandbox, ref.key)
	cancel()
	switch {
	case errors.Is(err, store.ErrNoRow):
		return Result{}, r.notRun(), false, refusal
	case err != nil:
		return Result{}, r.notRun(), false, fmt.Errorf("%w: %w", ErrRecord, err)
	case row.State != store.ToolCallNotRun:
		return r.existing(ctx, ref, row, tool, hash, unlock)
	case row.Tool != tool || !bytes.Equal(row.ArgsHash, hash):
		return Result{}, CallInfo{Keyed: true}, false, &CallIDConflictError{Tool: row.Tool}
	}
	return Result{}, r.notRun(), false, refusal
}

// existing answers a request from the key's row of another run: a
// conflict for another call, the stored answer of an ended one, or the
// wait for a running one (another instance's, or this instance's orphan,
// which expires).
func (r *Runner) existing(ctx context.Context, ref callRef, row store.ToolCall, tool string, hash []byte,
	unlock func()) (Result, CallInfo, bool, error) {
	if row.Tool != tool || !bytes.Equal(row.ArgsHash, hash) {
		return Result{}, CallInfo{Keyed: true}, false, &CallIDConflictError{Tool: row.Tool}
	}
	if row.State != store.ToolCallRunning {
		res, info, err := stored(row)
		return res, info, false, err
	}
	unlock()
	return r.waitRow(ctx, ref, row)
}

// stored is the answer of an ended row, as a replay.
func stored(row store.ToolCall) (Result, CallInfo, error) {
	return Result{Text: string(row.Output), ExitCode: row.ExitCode, Truncated: row.Truncated, IsError: row.IsError},
		CallInfo{Keyed: true, State: row.State, Replay: true, Instance: row.InstanceID},
		storedError(row.Error)
}

// waitLocal waits for the call kc of this instance: its outcome, or ctx's
// end, which cancels nothing. Every chunk the call emits beats progress
// with "".
func (r *Runner) waitLocal(ctx context.Context, kc *keyedCall, progress func(chunk string), replay bool) (Result, CallInfo, error) {
	var beat chan struct{}
	if progress != nil {
		beat = make(chan struct{}, 1)
		kc.mu.Lock()
		kc.beats[beat] = struct{}{}
		kc.mu.Unlock()
		defer func() {
			kc.mu.Lock()
			delete(kc.beats, beat)
			kc.mu.Unlock()
		}()
	}
	for {
		select {
		case <-kc.done:
			return kc.out.res, CallInfo{Keyed: true, State: kc.out.state, Replay: replay, Instance: kc.instance}, kc.out.err
		case <-beat:
			progress("")
		case <-ctx.Done():
			return Result{}, CallInfo{Keyed: true, State: CallRunning, Replay: replay, Instance: kc.instance}, ctx.Err()
		}
	}
}

// waitRow waits for the running row of another run of ref (another
// instance's, or an orphan of this one): its stored answer once it ended
// (awaitRow, which may end it itself when its instance stopped renewing
// it). again reports that the row was deleted or replaced by another run.
// Once the drain begins (BeginDrain), it answers *AskAgainError, state
// running, at once: the call still runs, and this instance stops. A
// sandbox with waiterCap waiting requests on this instance is answered
// ErrCallCap, state running, at once.
func (r *Runner) waitRow(ctx context.Context, ref callRef, row store.ToolCall) (res Result, info CallInfo, again bool, err error) {
	running := CallInfo{Keyed: true, State: CallRunning, Replay: true, Instance: row.InstanceID}
	detach, err := r.k.attach(ref.sandbox)
	if err != nil {
		return Result{}, running, false, err
	}
	defer detach()
	cur, err := r.awaitRow(ctx, ref, row)
	switch {
	case errors.Is(err, errRowGone):
		return Result{}, running, true, nil
	case errors.Is(err, errReleased):
		return Result{}, running, false, &AskAgainError{Instance: row.InstanceID}
	case err != nil:
		return Result{}, running, false, err
	}
	res, info, err = stored(cur)
	return res, info, false, err
}

// errRowGone is awaitRow's answer for a row that was deleted, or replaced
// by another run, while it waited.
var errRowGone = errors.New("the call's row was deleted or replaced")

// awaitRow waits until the running row of ref, the run row.ID, ended, and
// returns it as it ended. It reads the row whenever ferun_call names it,
// and every callPoll. A row still running is ended by the look itself, at
// most once per look, when the waiter's guard allows it (ADR 0023 D5):
// this instance's database has not failed within a lease (mayExpire), and
// the row's lease ran out more than a lease ago, which the database's
// clock decides (Store.ExpireToolCall's margin). The verdict is the
// expiry's: interrupted when the call was sent, not_run when not, and
// logged.
//
// It returns errRowGone when the row was deleted or replaced by another
// run, errReleased once the drain begins, ctx's error when ctx ends, and
// ErrRecord when the row could not be read for waitErrors.
func (r *Runner) awaitRow(ctx context.Context, ref callRef, row store.ToolCall) (store.ToolCall, error) {
	lease := r.lease()
	wctx, stop := r.k.untilRelease(ctx)
	defer stop()
	var (
		failing time.Time
		ended   store.ToolCall
		gone    bool
	)
	// look reads the row once; done reports that waiting is over (ended or
	// gone); running reports a row that still runs.
	look := func(ctx context.Context) (done, running bool, err error) {
		gctx, cancel := context.WithTimeout(ctx, bookkeepingTimeout)
		cur, err := r.st.GetToolCall(gctx, ref.sandbox, ref.key)
		cancel()
		switch {
		case errors.Is(err, store.ErrNoRow):
			gone = true
			return true, false, nil
		case err != nil:
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			if failing.IsZero() {
				failing = time.Now()
			}
			if time.Since(failing) > waitErrors {
				return false, false, fmt.Errorf("%w: %w", ErrRecord, err)
			}
			return false, false, nil
		}
		failing = time.Time{}
		switch {
		case cur.ID != row.ID:
			gone = true
			return true, false, nil
		case cur.State == store.ToolCallRunning:
			return false, true, nil
		}
		ended = cur
		return true, false, nil
	}
	err := r.d.WaitFor(wctx, db.ChanCall, row.ID.String(), r.k.poll, func(ctx context.Context) (bool, error) {
		done, running, err := look(ctx)
		if done || err != nil || !running || !r.mayExpire(lease) {
			return done, err
		}
		ectx, cancel := context.WithTimeout(ctx, bookkeepingTimeout)
		verdict, err := r.st.ExpireToolCall(ectx, row.ID, lease)
		cancel()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			r.rl.warn("expire", "end a tool call whose instance stopped renewing it", "call", row.ID,
				"instance", row.InstanceID, "err", err)
			return false, nil
		case verdict == "":
			return false, nil
		}
		r.log.Info("tool call ended by its waiter: its instance stopped renewing it", "sandbox", ref.sandbox,
			"key", ref.key, "call", row.ID, "instance", row.InstanceID, "state", verdict)
		done, _, err = look(ctx)
		return done, err
	})
	switch {
	case err == nil && gone:
		return store.ToolCall{}, errRowGone
	case err == nil:
		return ended, nil
	case ctx.Err() != nil:
		return store.ToolCall{}, ctx.Err()
	case errors.Is(context.Cause(wctx), errReleased):
		return store.ToolCall{}, errReleased
	}
	return store.ToolCall{}, err
}

// mayExpire reports whether this instance may end another instance's row
// whose lease ran out: its own database has not failed within lease, so
// that the lease did not run out only because the database was away
// (ADR 0023 D5).
func (r *Runner) mayExpire(lease time.Duration) bool {
	f := r.d.LastFailure()
	return f.IsZero() || time.Since(f) > lease
}

// CallRecord is the record of a call with an id: its row, or the call
// running on this instance. Result and Err are set once it ended.
type CallRecord struct {
	Key, Tool, State, Instance string
	StartedAt, FinishedAt      time.Time
	Result                     Result
	Err                        string
}

// LookupCall reads the record of the sandbox's call with the id key:
// running from memory when it runs on this instance, otherwise its row.
// ErrNoCall when there is none.
func (r *Runner) LookupCall(ctx context.Context, id podid.ID, key string) (CallRecord, error) {
	r.k.mu.Lock()
	kc := r.k.calls[callRef{sandbox: id, key: key}]
	r.k.mu.Unlock()
	if kc != nil {
		return CallRecord{Key: key, Tool: kc.tool, State: CallRunning, Instance: kc.instance, StartedAt: kc.started}, nil
	}
	if r.st == nil {
		return CallRecord{}, ErrNoCall
	}
	row, err := r.st.GetToolCall(ctx, id, key)
	if errors.Is(err, store.ErrNoRow) {
		return CallRecord{}, ErrNoCall
	}
	if err != nil {
		return CallRecord{}, err
	}
	return recordOf(row), nil
}

// recordOf is the CallRecord of a row.
func recordOf(row store.ToolCall) CallRecord {
	rec := CallRecord{Key: row.Key, Tool: row.Tool, State: row.State, Instance: row.InstanceID,
		StartedAt: row.StartedAt, FinishedAt: row.FinishedAt}
	if row.State != store.ToolCallRunning {
		rec.Result = Result{Text: string(row.Output), ExitCode: row.ExitCode, Truncated: row.Truncated, IsError: row.IsError}
		rec.Err = row.Error
	}
	return rec
}

// WaitCall reads the record of the sandbox's call with the id key, as
// LookupCall does, and while the call runs waits up to wait for its end
// (ADR 0023 D20). wait is clamped to 0 (no wait) to maxRecordWait. The
// record says running when the wait passed, or at once when the drain
// began (BeginDrain). A call running on this instance is waited for in
// memory, another instance's through its row, which the wait may end
// when that instance stopped renewing it (as a request's wait does,
// awaitRow). It never runs anything. ErrNoCall when there is no record;
// ErrBadCallKey for a key that is not ValidCallKey; ErrCallCap, at once,
// for a wait on a running call while the sandbox has waiterCap waiting
// requests on this instance (CallKeyed's attaches count too); ctx's error
// when ctx ends first.
func (r *Runner) WaitCall(ctx context.Context, id podid.ID, key string, wait time.Duration) (CallRecord, error) {
	if !ValidCallKey(key) {
		return CallRecord{}, ErrBadCallKey
	}
	wait = min(max(wait, 0), maxRecordWait)
	ref := callRef{sandbox: id, key: key}
	r.k.mu.Lock()
	kc := r.k.calls[ref]
	r.k.mu.Unlock()
	if kc != nil {
		return r.waitLocalRecord(ctx, kc, wait)
	}
	if r.st == nil {
		return CallRecord{}, ErrNoCall
	}
	row, err := r.st.GetToolCall(ctx, id, key)
	if errors.Is(err, store.ErrNoRow) {
		return CallRecord{}, ErrNoCall
	}
	if err != nil {
		return CallRecord{}, err
	}
	if row.State != store.ToolCallRunning || wait == 0 {
		return recordOf(row), nil
	}
	detach, err := r.k.attach(id)
	if err != nil {
		return CallRecord{}, err
	}
	defer detach()
	wctx, cancel := context.WithTimeout(ctx, wait)
	cur, err := r.awaitRow(wctx, ref, row)
	cancel()
	switch {
	case err == nil:
		return recordOf(cur), nil
	case ctx.Err() != nil:
		return CallRecord{}, ctx.Err()
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errReleased), errors.Is(err, errRowGone):
		// The wait passed, the drain began, or another run took the key:
		// the record as it is now.
		return r.LookupCall(ctx, id, key)
	}
	return CallRecord{}, err
}

// waitLocalRecord waits up to wait for the end of kc, a call running on
// this instance, unless the drain begins or ctx ends, and returns its
// record: ended, or running. A wait counts as an attach (ErrCallCap).
func (r *Runner) waitLocalRecord(ctx context.Context, kc *keyedCall, wait time.Duration) (CallRecord, error) {
	if wait > 0 {
		detach, err := r.k.attach(kc.ref.sandbox)
		if err != nil {
			return CallRecord{}, err
		}
		defer detach()
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-kc.done:
		case <-t.C:
		case <-r.k.release:
		case <-ctx.Done():
			return CallRecord{}, ctx.Err()
		}
	}
	select {
	case <-kc.done:
		return r.endedRecord(ctx, kc), nil
	default:
	}
	return CallRecord{Key: kc.ref.key, Tool: kc.tool, State: CallRunning, Instance: kc.instance, StartedAt: kc.started}, nil
}

// endedRecord is the record of kc, a call of this instance that ended:
// its row, as the final write left it, or, when the row cannot be read or
// is no longer kc's ended run (the final write failed, or a new run of the
// key replaced it), the outcome kc's waiters got.
func (r *Runner) endedRecord(ctx context.Context, kc *keyedCall) CallRecord {
	gctx, cancel := context.WithTimeout(ctx, bookkeepingTimeout)
	row, err := r.st.GetToolCall(gctx, kc.ref.sandbox, kc.ref.key)
	cancel()
	if err == nil && row.ID == kc.id && row.State != store.ToolCallRunning {
		return recordOf(row)
	}
	return CallRecord{Key: kc.ref.key, Tool: kc.tool, State: kc.out.state, Instance: kc.instance,
		StartedAt: kc.started, FinishedAt: time.Now(), Result: kc.out.res, Err: sanitizeError(kc.out.err)}
}

// CancelCall cancels the sandbox's running call with the id key
// (ErrCancelledByCaller): here when it runs here, otherwise through
// ferun_cancel {"kind":"call"} to the instance that runs it. false when no
// call of the key runs.
func (r *Runner) CancelCall(ctx context.Context, id podid.ID, key string) (bool, error) {
	if !ValidCallKey(key) {
		return false, ErrBadCallKey
	}
	ref := callRef{sandbox: id, key: key}
	if r.cancelLocal(ref, ErrCancelledByCaller) {
		return true, nil
	}
	if r.st == nil {
		return false, nil
	}
	row, err := r.st.GetToolCall(ctx, id, key)
	if errors.Is(err, store.ErrNoRow) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.State != store.ToolCallRunning {
		return false, nil
	}
	payload, err := json.Marshal(cancelPayload{Kind: "call", Sandbox: id.String(), Key: key})
	if err != nil {
		return false, err
	}
	if err := r.st.Notify(ctx, db.ChanCancel, string(payload)); err != nil {
		return false, err
	}
	return true, nil
}

// cancelLocal cancels the call of ref on this instance with cause and
// reports whether there was one. It takes the per-key lock first, so that
// a join of ref that is writing the row registers its call before.
func (r *Runner) cancelLocal(ref callRef, cause error) bool {
	unlock := r.k.lock(ref)
	defer unlock()
	r.k.mu.Lock()
	kc := r.k.calls[ref]
	r.k.mu.Unlock()
	if kc == nil {
		return false
	}
	kc.cancel(cause)
	return true
}
