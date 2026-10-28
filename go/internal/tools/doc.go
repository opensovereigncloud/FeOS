// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package tools runs the tool baseline in sandboxes: the twelve tools an
// agent gets (bash, read, write, edit, ls, rg, fd, the four process tools
// and webfetch), their descriptions and schemas, and the rendering of their
// answers into the texts the clients were built against.
//
// A call goes onto the sandbox's node over this instance's node connection,
// with the pod's ID in the feos-pod-id metadata: read, write, edit, ls, rg,
// fd and webfetch are unary calls of feos.tool.v1.ToolService in the main
// container; bash runs as ContainerService.Exec ["bash", "-c", command] in
// "/"; the process tools run Python scripts the same way, which keep their
// records in the container. The Runner acquires the sandbox first (the
// engine ensures it runs and holds it awake for the call), retries once
// when the pod was not where or not as the lease said, caps the output, and
// writes the call's tool_start and tool_end rows. Call is the one entry
// point of both doors: REST (the REST handlers here) and MCP (mcpsrv).
//
// A call of a tool with a side effect (bash, write, edit, process_start,
// process_stop) that carries an id is a call that outlives its caller (ADR
// 0023, keyed.go, CallKeyed): it is a row of tool_calls, unique per
// sandbox and id, and runs on a context of its own in the instance that
// received it, so that a request that ends only stops waiting. A repeat of
// the id with the same tool and normalised arguments attaches to the call
// (in memory on this instance, through the row and ferun_call on another)
// or reads its stored answer; a repeat with another call is refused. The
// row commits sent before the call's request leaves for the guest, which
// makes every outcome exact: done, interrupted (it may have run, and
// stopped without a recorded result) or not_run (it cannot have run, and
// the same call may be sent again under the same id). The instance renews
// its rows' leases while Run runs and records no call with an id
// otherwise; a client cancels a call explicitly (CancelCall) and reads its
// record with a wait (WaitCall). A waiter ends the row of an instance that
// stopped renewing it. A stopping instance drains its calls
// (keyed_drain.go: BeginDrain, Drain): it sends only those that end
// before its shutdown grace, releases the requests that wait for other
// instances' calls, and cuts what still runs at the grace.
package tools
