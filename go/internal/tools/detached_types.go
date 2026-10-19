// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import "errors"

// The types of the Runner's detached calls (detached.go): what the
// environment controller (internal/env/ctl) runs in a sandbox or an
// instance's pod on its own behalf. A detached call takes the lease the
// caller read from the row (it does not acquire the sandbox), takes no
// hold, writes no activity row and does not count as activity of the
// sandbox, so that probing and supervising never keep a sandbox awake.

// ProcessStart is a background process the controller starts: an
// attached service's process (ADR 0018). It is started like process_start
// with timeout 0, but the argv is shell-quoted into the start script and
// Env reaches the process through ExecRequest.Env only: no value of it is
// written to the container's disk, the process records or the activity
// log. HOME is /root unless Env sets it.
type ProcessStart struct {
	// Argv is the command; Argv[0] is looked up in PATH.
	Argv []string
	// Cwd is the working directory; "": /. A directory that does not
	// exist fails the start with ErrNoCwd.
	Cwd string
	// Env is added to the container's environment.
	Env map[string]string
	// Name is the process's name in process_list, e.g.
	// "ferun-env-<env name>-<service>".
	Name string
}

// ProcessStatus is a background process as process_list shows it.
type ProcessStatus struct {
	// ID is the process's id (process_start's "HHMMSS-<6 hex>").
	ID   string
	Name string
	// Running is false once it exited, was stopped, or did not survive a
	// cold restore of its sandbox.
	Running bool
	// ExitCode is its exit status once it exited by itself; nil while it
	// runs, and for a process that was killed or lost without one.
	ExitCode *int
}

// The definite answers of the detached calls, which callers tell apart
// with errors.Is.
var (
	// ErrNoCwd: StartProcessDetached's working directory does not exist.
	ErrNoCwd = errors.New("the working directory does not exist")
	// ErrNoProcess: the process id names no process of the sandbox.
	ErrNoProcess = errors.New("no such process")
)
