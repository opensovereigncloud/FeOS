// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package db

import (
	"syscall"
	"time"
)

// setUserTimeout does nothing where TCP_USER_TIMEOUT does not exist; the
// keepalive alone notices a dead server there.
func setUserTimeout(syscall.RawConn, time.Duration) {}
