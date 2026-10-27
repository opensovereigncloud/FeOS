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
package tools
