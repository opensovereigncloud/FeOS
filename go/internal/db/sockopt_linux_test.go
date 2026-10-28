// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package db

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The dialer's connections carry the keepalive and TCP_USER_TIMEOUT.
func TestDialerSetsTCPOptions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The accepting goroutine ends with the test.
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		c, err := l.Accept()
		if err != nil {
			return
		}
		<-ctx.Done()
		c.Close()
	}()
	defer func() {
		cancel()
		l.Close()
		<-accepted
	}()

	c, err := dialer(0).DialContext(ctx, "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("dialed a %T, want a *net.TCPConn", c)
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct {
		name       string
		level, opt int
		want       int
	}{
		{"TCP_USER_TIMEOUT", syscall.IPPROTO_TCP, tcpUserTimeoutOpt, 15000},
		{"SO_KEEPALIVE", syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1},
		{"TCP_KEEPIDLE", syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 5},
		{"TCP_KEEPINTVL", syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, 5},
		{"TCP_KEEPCNT", syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 3},
	} {
		var got int
		var gerr error
		if err := raw.Control(func(fd uintptr) {
			got, gerr = syscall.GetsockoptInt(int(fd), o.level, o.opt)
		}); err != nil {
			t.Fatal(err)
		}
		if gerr != nil {
			t.Fatalf("%s: %v", o.name, gerr)
		}
		if got != o.want {
			t.Errorf("%s = %d, want %d", o.name, got, o.want)
		}
	}
}

func TestDialerTimeout(t *testing.T) {
	t.Parallel()
	if got := dialer(0).Timeout; got != 10*time.Second {
		t.Errorf("dialer(0).Timeout = %s, want 10s", got)
	}
	if got := dialer(3 * time.Second).Timeout; got != 3*time.Second {
		t.Errorf("dialer(3s).Timeout = %s, want 3s", got)
	}
}

// WARNING notices are logged, at most one per noticeInterval; other
// severities are not.
func TestOnNoticeRateLimit(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := &DB{log: slog.New(slog.NewTextHandler(&buf, nil))}
	for i := range 3 {
		d.onNotice(nil, &pgconn.Notice{Severity: "WARNING", SeverityUnlocalized: "WARNING", Message: fmt.Sprint("warning ", i)})
	}
	d.onNotice(nil, &pgconn.Notice{Severity: "NOTICE", SeverityUnlocalized: "NOTICE", Message: "notice"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "warning 0") {
		t.Fatalf("logged %q, want one line with the first warning", buf.String())
	}
	if d.noticeSuppressed != 2 {
		t.Fatalf("noticeSuppressed = %d, want 2", d.noticeSuppressed)
	}
}
