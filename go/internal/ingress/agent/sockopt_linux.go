// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package agent

import (
	"syscall"
	"time"
)

// tcpUserTimeoutOpt is TCP_USER_TIMEOUT on Linux.
const tcpUserTimeoutOpt = 18

// setUserTimeout sets TCP_USER_TIMEOUT: unacknowledged data waits at most
// d before the kernel drops the connection, so that a dead control-plane
// instance is noticed within seconds.
func setUserTimeout(raw syscall.RawConn, d time.Duration) {
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeoutOpt, int(d.Milliseconds()))
	})
}
