// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package testnode

import (
	"os"
	"os/exec"
)

// ownProcessGroup leaves cmd as it is where process groups do not exist:
// cancellation kills the process alone.
func ownProcessGroup(*exec.Cmd) {}

// exitCode is the process's exit code.
func exitCode(st *os.ProcessState) int32 { return int32(st.ExitCode()) }
