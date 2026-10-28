// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package db

import (
	"syscall"
	"time"
)

// tcpUserTimeoutOpt is TCP_USER_TIMEOUT on Linux; syscall does not name
// it, and x/sys is not a dependency.
const tcpUserTimeoutOpt = 18

// setUserTimeout sets TCP_USER_TIMEOUT: data the server has not
// acknowledged within d fails the connection.
func setUserTimeout(raw syscall.RawConn, d time.Duration) {
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeoutOpt, int(d.Milliseconds()))
	})
}
