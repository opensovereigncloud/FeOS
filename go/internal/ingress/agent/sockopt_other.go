// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package agent

import (
	"syscall"
	"time"
)

// setUserTimeout does nothing where TCP_USER_TIMEOUT does not exist; TCP
// keepalive still notices a dead instance, later.
func setUserTimeout(syscall.RawConn, time.Duration) {}
