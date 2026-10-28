// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package db_test

import (
	"context"
	"crypto/tls"
	"net"
	"syscall"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// A connection of the pool carries TCP_USER_TIMEOUT (18 on Linux) of
// 15 s: Open dials with its own dialer.
func TestOpenSetsTCPUserTimeout(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	c, err := d.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	nc := c.Conn().PgConn().Conn()
	if tc, ok := nc.(*tls.Conn); ok {
		nc = tc.NetConn()
	}
	if _, ok := nc.(*net.UnixConn); ok {
		t.Skip("the test database is reached through a Unix socket")
	}
	tc, ok := nc.(*net.TCPConn)
	if !ok {
		t.Fatalf("the pool's connection is a %T, want a *net.TCPConn", nc)
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, 18)
	}); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got != 15000 {
		t.Fatalf("TCP_USER_TIMEOUT = %d, want 15000", got)
	}
}
