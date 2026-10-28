# ADR 0023: A tool call outlives its caller: ferund runs it detached from the request, keyed by the caller's call ID, and a repeated call attaches to it or gets its result

**Status:** Proposed · **Date:** 2026-10-27

## Context

An agent's tool call reaches `ferund` as an MCP call or a REST exec and
runs on the request's context: `tools/runner.go`'s `Call` hands the
request's `ctx` to `callPrepared`, and the exec in the sandbox ends
when that context does. A caller that goes away mid-call, because its
process crashed, was deployed, or lost the connection, cancels a `bash`
that may have run half-way, with its side effects half done and its
result nowhere. FeCode's own setup commands run for up to 600 s, and
the model's `bash` names whatever timeout it likes: `ferund` caps none;
a default call has 120 s. ADR 0015 says "a broken stream is the
client's to reopen", which is right for a console or a port forward and
wrong for a call with side effects: reopening does not tell the caller
what the first call did, and repeating it does the work twice.

FeCode's ADR 0010 takes its turns over between instances on a deploy or
a crash. A turn taken over in the middle of a tool call can only write
"interrupted; the result is unknown" into the transcript, because
`ferund` forgot the call with the request. The process tools show the
other shape: `process_start` runs a process detached from any request,
records it, and lets a later call list it, read its output and stop it
(`tools/detached.go`). The MCP server is stateless, with signed session
IDs for the clients that open with `initialize`, and a cancel reaches
"whichever instance runs it" through `ferun_cancel`
(`docs/architecture/control-plane.md`): the pieces for a call that is a
record exist. FeCode's MCP client opens with `server/discover`, the
go-sdk's default, which has no session: its cancels never reach a call,
and its abort works only because hanging up cancels.

## Decision

### A call with a key is a record

- A tool call may carry an **idempotency key**: in MCP the call's
  `_meta` field `ferun.callId`, in REST the header `Idempotency-Key`,
  bare or as one quoted sf-string. The key is the caller's: FeCode
  derives its keys from its session, its reply and the model's tool call
  ID (FeCode ADR 0010). A call without a key runs as today, on its
  request's context.
- **The key is unique per sandbox**, not per token: tokens are revocable
  and deleted with their sandbox, and every token of a sandbox sees all
  of its files. The argument hash refuses a key reused for another call.
- **Keys are `[A-Za-z0-9._~:@-]{1,255}`**: never percent-encoded in a URL
  path, and safe as an RFC 8941 sf-string, which therefore holds no
  escape.
- **Only tools with side effects are recorded**: `bash`, `write`,
  `edit`, `process_start` and `process_stop`. A read-only call's key is
  accepted and not recorded; the call runs on its request.
- A call with a key is written to a table `tool_calls` before it runs:
  the sandbox, the key, the tool, a hash of the arguments, the instance
  that runs it, its state, `sent`, its lease, and, once it ended, its
  result. The row is the call; the request is one way of waiting for it.
- **"The same arguments"** means the same SHA-256 over the tool and its
  normalised parameters: decoded into the tool's parameter struct and
  encoded again, `write` by its path and its content's bytes. The door,
  whitespace, key order and absent versus zero make no difference.
- **Four states, and `sent`.** The states are `running`, `done`,
  `interrupted` and `not_run`.
  - The row commits `sent` before the call's request leaves for the
    guest. A call whose record cannot be marked is not sent.
  - `not_run` is a stored state: the guest never got the call. A repeat
    of the same call under the same key replaces the row with a new run.
  - `interrupted` means the call may have run and stopped without a
    recorded result.
  - A cancel by the caller is final (`done`, with the cancel's error),
    even before the send.
- **The record is bounded.** The output is the tool's text as cut
  (50,000 bytes), stored as bytes. The error text is made valid UTF-8
  without NUL, at most 4 KiB.

### The call runs detached from the request

- The call runs on a context of its own, bounded by the sandbox's
  preparation and the tool's timeout and not by the request's: the
  request's end, by a client that hung up or an instance that drained
  the request, leaves the call running. The instance renews the row's
  lease while the call runs, and writes the result when it ends, whether
  or not anyone waits.
- **A keyed `bash` names at most 600 s.** A larger timeout is refused
  (REST 400 `call_timeout_too_long`, MCP `-32602` `timeout_too_long`),
  and the limit is advertised. At most 64 keyed calls run per sandbox
  and instance; the slot is reserved atomically, and a refusal is
  answered only after checking that no record of the key exists. At
  most 256 requests per sandbox and instance wait for keyed calls they
  did not start, attaches and record waits together; one more is
  refused at once and retryably, state `running`, so that one token
  cannot hold unbounded goroutines and row reads on an instance.
- A request waits for its call's result. A second request with the same
  key and the same arguments attaches to the running call and gets its
  result when it ends, or gets the stored result at once when it ended;
  on another instance it waits on the row, woken by `ferun_call` NOTIFY,
  since the call runs where it started and the node connection is that
  instance's (ADR 0015). **Concurrent requests with one key attach**,
  where the IETF Idempotency-Key draft answers 409. **The same key with
  other arguments is refused**, REST 422 (the draft's code) and MCP
  `-32602` `call_id_conflict`, so a key never silently runs a second
  command.
- **Progress after a reattach is heartbeats only.** No door streams a
  call's output today, so nothing is replayed to a request that
  attaches.
- **The record is readable without running anything.** `GET
  /api/v1/sandboxes/{id}/calls/{key}?wait=N` (N ≤ 60) waits for the end
  and returns the result rendered as MCP renders it. A client that lost
  an answer reads it there instead of sending the call again. `DELETE`
  on the same path cancels.
- **Cancellation is explicit**; a hung-up caller cancels nothing.
  - REST `DELETE` works for every client.
  - MCP `notifications/cancelled` reaches a keyed call only for clients
    with an initialize session: the door keeps the mapping for 30 s
    after the request ended, and remembers cancels that arrive first.
    Both are kept per token, 256 of each, and a token that passes that
    loses only its own oldest entries, so that one client cannot flush
    another's cancels.
    The go-sdk's default protocol (`server/discover`) has no session, so
    such clients, FeCode among them, cancel with `DELETE`.
  - Across instances the cancel travels as `ferun_cancel`
    `{"kind":"call"}`.
  - A request that has already ended when its call would start starts
    nothing.
- Results are kept for an hour after the call ended, and the leader
  deletes them; a key may be used again after that, and the caller that
  needs a result longer has read it by then.

### A dead instance's call ends by its lease and is never run again

- **The lease** is 30 s, renewed every 10 s in one statement per
  instance. A key is recorded only while the instance's renewal runs;
  otherwise the call is refused as `not_run`.
- **Three writers end a dead instance's call**, `interrupted` when it
  was sent and `not_run` when not:
  - the leader, once it has led for a lease and its database has not
    failed within one;
  - any instance waiting on the row, once its own database has not
    failed within a lease and the lease ran out more than a lease ago,
    so that calls also end while an older leader runs during a rollout,
    or after the leader died with the instance;
  - a start under the same instance ID.
- **ferund never repeats an interrupted call**: the caller reads that
  the call was interrupted and decides. What the exec did in the sandbox
  is in the sandbox. A read-only call is not recorded, and its caller
  repeats it cheaply.
- **A call on an instance that lost its database stops** when the nodes
  drop the instance once its row is stale, within about 15–25 s, which
  ends its execs; or, on reconnect, when the instance cancels a call
  whose renewal misses it. Its final write is refused.

### A stopping instance drains its calls

- When the shutdown grace's deadline starts, a keyed call reaches the
  guest only if its timeout plus 30 s fits before the deadline;
  otherwise it ends `not_run`, and the client sends it elsewhere.
  Requests waiting on another instance's call are told to ask again
  (REST 503 `call_running`).
- After the HTTP shutdown, the instance waits for its remaining keyed
  calls until the deadline, then cuts and records them.
- `--shutdown-grace` defaults to 11 minutes. The termination grace is
  the grace plus 60 s: 720 s, and systemd's `TimeoutStopSec=720`.

### Clients can tell

- The capability `experimental["ferun.callId"]`, `{tools,
  retentionSeconds, maxTimeoutSeconds}`, on `initialize` and
  `server/discover`.
- Results carry `_meta` `ferun.callId`, `ferun.callState` and
  `ferun.callReplayed`.
- REST answers carry `Ferun-Call-State` and `Ferun-Call-Replayed`.

### What it applies to

Every tool call with a side effect: `bash`, the file tools that write
(`write`, `edit`), the process tools' start and stop. A read-only call
may carry a key and gains nothing from it. The port door, watches and
the console are streams and stay ADR 0015's: the client's to reopen.

## Alternatives considered

- **The caller retries** without a key: a `bash` runs twice; a file is
  written twice, which is harmless, or appended twice, which is not.
- **Every tool idempotent by construction**: possible for the file
  tools, impossible for `bash`.
- **Every exec a process tool**, started detached and polled: the same
  machinery with an asynchronous face for every call, and an agent's
  loop that polls; a key keeps the synchronous call and adds the record
  underneath.
- **Keeping the call on the request's context**, as today: the caller's
  availability becomes the sandbox's correctness.
- **Forwarding a repeated call to the instance that runs it**, instead
  of waiting on the row: the routing layer ADR 0015 set aside; waiting
  on a row and a NOTIFY needs none.
- **Results kept until acknowledged**: a caller that never acknowledges
  keeps every result; an hour is longer than any takeover.
- **Keys per token**: a token is revocable and goes with its sandbox,
  while every token of a sandbox sees all of its files; the argument
  hash already refuses a key reused for another call.
- **Recording the read-only calls**: a `read` would store pictures of up
  to 3.75 MB, for nothing.
- **Deleting the row of a call that did not run**: a caller whose answer
  was lost could not tell "did not run" from "no record".
- **Recovering a lost answer by sending the call again with its key**:
  in a mixed-version window or after a rollback, the repeat can reach an
  instance that ignores keys and runs the call twice; a read of the
  record runs nothing.
- **Clamping a longer timeout silently**, or no cap: the first changes
  the call, the second gives detached calls and drains without bound.
- **Ending a dead instance's calls at once, or by the leader alone**:
  after a failover every lease looks expired; during a rollout a leader
  of the older version ends none, and attached requests would wait
  without end.
- **A self-fence by time**, an instance cancelling its calls once its
  renewals failed for a while: in a failover of the whole database the
  nodes keep every connection and the leader's guard prevents false
  verdicts already, so the fence would only kill every keyed call on a
  failover longer than its threshold; a threshold long enough to avoid
  that is longer than the verdict and guarantees nothing.
- **Mapping sessionless MCP cancels** by token hash and request ID:
  request IDs repeat across FeCode processes that share a token.
- **Handing a call to another instance at a drain**: the exec stream
  lives on this instance's node connection.
- **Keeping a shutdown grace of 2 minutes**: a FeCode `bash` would be
  cut on every deploy. A grace that covers preparation and 600 s,
  18 minutes, is unnecessary with the send gate.

## Consequences

- `ferund` gains `tool_calls`, the detached context with its lease and
  `sent`, the attach on a repeated key, the `ferun_call` channel, the
  states `interrupted` and `not_run`, three writers that end a dead
  instance's calls, the record with its wait, the cancel by `DELETE`,
  the drain, and the key in MCP's `_meta` and REST's header; the tools'
  outputs are stored once per call for an hour, bounded as they are
  bounded in memory today. The e2e tests gain a caller that hangs up and
  comes back, a partitioned instance and drains.
- A call runs on while its caller is away, up to its timeout: a `bash`
  of 600 s started and abandoned holds the sandbox awake for 600 s, as
  it would have for a caller that waited; the hold is the call's, not
  the request's.
- Ending the call of an instance that lost its database relies on the
  nodes' handling of the instance list: fleet-client keeps its list
  only while telemetry fails everywhere.
- An old instance lingers for up to its grace and 60 s, 12 minutes by
  default, while new ones serve: a rolling deploy takes that long when a
  call runs.
- FeCode reads the call's record after a takeover or a lost answer, and
  sends a call again only after a `not_run` answer (FeCode ADR 0010,
  phase 2).
- An agent that repeats a call with the same ID by mistake gets the
  first call's result, which is the right answer to a repeated call.
- The table is additive: an older instance ignores the key, the header
  and `ferun_cancel` kind `call`, so both versions run side by side
  during a rollout.

## Open questions

- **The cap**: whether 600 s is right for a keyed `bash`. Models
  sometimes ask for 1800 s for big builds; a larger cap raises the
  shutdown grace one for one.
- **The retention** as a flag (`--call-retention`); it is fixed at an
  hour.
- **The `_meta` name**: `ferun.callId`, or a prefixed form such as
  `ferun.dev/callId` before clients depend on it.
- **An operator's view**: whether the admin UI or the admin API lists a
  sandbox's keyed calls, running and recent; today the logs and the
  sandbox token's `GET` of a call show them.
