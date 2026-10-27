// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package testnode

import (
	"os"
	"os/exec"
	"syscall"
)

// ownProcessGroup runs cmd in a process group of its own and makes its
// cancellation kill the whole group, as the guest does when an Exec call
// ends: a command's children go with it.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// exitCode is the exit code the guest reports: 128 + the signal when the
// process was signalled.
func exitCode(st *os.ProcessState) int32 {
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int32(ws.Signal())
	}
	return int32(st.ExitCode())
}
