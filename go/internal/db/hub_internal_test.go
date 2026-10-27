// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A subscriber that falls behind keeps what it has buffered, loses only
// the payloads that do not fit, and learns that it did: one "" follows the
// buffered payloads.
func TestSubscriberOverflow(t *testing.T) {
	d := &DB{}
	d.hub.subs = map[string]map[*subscriber]struct{}{}
	ch, unsubscribe := d.Subscribe(ChanSandbox)
	defer unsubscribe()
	for i := range subscriberBuffer + 10 {
		d.hub.deliver(ChanSandbox, fmt.Sprint(i))
	}
	var got []string
	for len(ch) > 0 {
		got = append(got, <-ch)
	}
	if len(got) != subscriberBuffer+1 {
		t.Fatalf("got %d payloads, want the %d buffered ones and \"\"", len(got), subscriberBuffer)
	}
	for i, p := range got[:subscriberBuffer] {
		if p != fmt.Sprint(i) {
			t.Fatalf("payload %d is %q; a buffered payload was lost", i, p)
		}
	}
	if got[subscriberBuffer] != "" {
		t.Fatalf("the last payload is %q, want \"\"", got[subscriberBuffer])
	}

	// Once the subscriber reads again, payloads are queued behind the ""
	// still in its buffer as soon as a buffer slot is free; the next
	// overflow gets the reserved slot again.
	for i := range subscriberBuffer {
		d.hub.deliver(ChanSandbox, fmt.Sprint("a", i))
	}
	d.hub.deliver(ChanSandbox, "lost")
	for _, want := range []string{"a0", "a1"} {
		if p := <-ch; p != want {
			t.Fatalf("got %q, want %s", p, want)
		}
	}
	d.hub.deliver(ChanSandbox, "kept")
	d.hub.deliver(ChanSandbox, "lost too")
	got = got[:0]
	for len(ch) > 0 {
		got = append(got, <-ch)
	}
	n := len(got)
	if n != subscriberBuffer+1 || got[0] != "a2" || got[n-4] != "a255" || got[n-3] != "" || got[n-2] != "kept" || got[n-1] != "" {
		t.Fatalf("after reading on: %d payloads, %q ... %q", n, got[:min(1, n)], got[max(0, n-4):])
	}

	// A subscriber that keeps up sees every payload; another channel's
	// payloads do not reach it.
	d.hub.deliver(ChanNode, "n1")
	d.hub.deliver(ChanSandbox, "a")
	if p := <-ch; p != "a" {
		t.Fatalf("got %q, want a", p)
	}

	// After unsubscribing nothing more arrives.
	unsubscribe()
	d.hub.deliver(ChanSandbox, "b")
	if len(ch) != 0 {
		t.Fatalf("payload after unsubscribe")
	}
}

func TestResetReachesEveryChannel(t *testing.T) {
	d := &DB{}
	d.hub.subs = map[string]map[*subscriber]struct{}{}
	a, ua := d.Subscribe(ChanSandbox)
	defer ua()
	b, ub := d.Subscribe(ChanWatch)
	defer ub()
	d.hub.reset()
	if p := <-a; p != "" {
		t.Fatalf("sandbox subscriber got %q", p)
	}
	if p := <-b; p != "" {
		t.Fatalf("watch subscriber got %q", p)
	}
}

func TestConnectionFailure(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil", context.Background(), nil, false},
		{"syntax error", context.Background(), &pgconn.PgError{Code: "42601"}, false},
		{"unique violation", context.Background(), &pgconn.PgError{Code: "23505"}, false},
		{"admin shutdown", context.Background(), &pgconn.PgError{Code: "57P01"}, true},
		{"connection exception", context.Background(), &pgconn.PgError{Code: "08006"}, true},
		{"eof", context.Background(), fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"net", context.Background(), &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"caller cancelled", cancelled, &net.OpError{Op: "read", Err: errors.New("x")}, false},
		{"deadline", context.Background(), context.DeadlineExceeded, false},
		{"scan", context.Background(), errors.New("can't scan into dest[0]"), false},
	} {
		if got := connectionFailure(c.ctx, c.err); got != c.want {
			t.Errorf("%s: connectionFailure = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetryable(t *testing.T) {
	for code, want := range map[string]bool{"40001": true, "40P01": true, "55P03": true, "23505": false, "57014": false} {
		if got := Retryable(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})); got != want {
			t.Errorf("Retryable(%s) = %v, want %v", code, got, want)
		}
	}
	if Retryable(errors.New("plain")) {
		t.Error("a plain error is retryable")
	}
}
