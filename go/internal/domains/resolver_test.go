// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// Only a denial is an answer: NXDOMAIN or no such records; a timeout, a
// temporary failure (SERVFAIL) and any other error are failed lookups.
func TestNotFound(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&net.DNSError{Err: "no such host", Name: "a.example", IsNotFound: true}, true},
		{fmt.Errorf("lookup: %w", &net.DNSError{Err: "no such host", IsNotFound: true}), true},
		{&net.DNSError{Err: "i/o timeout", IsTimeout: true}, false},
		{&net.DNSError{Err: "server misbehaving", IsTemporary: true}, false},
		{&net.DNSError{Err: "no such host", IsNotFound: true, IsTemporary: true}, false},
		{&net.DNSError{Err: "server misbehaving"}, false},
		{errors.New("dial udp: connection refused"), false},
	} {
		if got := notFound(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}

// The resolver asks --dns-resolver; one that does not answer fails both
// lookups, and a failed lookup is no answer.
func TestResolverAsksItsServer(t *testing.T) {
	t.Parallel()
	dead, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	r := NewResolver(dead.LocalAddr().String(), time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a := r.Lookup(ctx, Query{Name: "app.example.com", TXTName: "_ferun-challenge.app.example.com"})
	if a.TXTErr == nil || a.RouteErr == nil || len(a.TXT) != 0 || len(a.Addresses) != 0 {
		t.Fatalf("%+v", a)
	}
	buf := make([]byte, 512)
	_ = dead.SetReadDeadline(time.Now().Add(time.Second))
	if n, _, err := dead.ReadFromUDP(buf); err != nil || n == 0 {
		t.Fatalf("the server got no query: %v", err)
	}
}
