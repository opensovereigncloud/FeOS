// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package wg

// sysSetns is the number of setns(2), which syscall lacks.
const sysSetns = 268
