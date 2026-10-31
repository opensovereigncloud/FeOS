// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A name becomes one of its addresses per node, the same one while the
// addresses stay; an address goes out as it is; a failed lookup keeps
// the addresses before; a name that never resolved goes out as it is.
func TestSinkAddresses(t *testing.T) {
	ctx := context.Background()
	if got := newSinkAddresses("").forNode(ctx, "n1"); got != "" {
		t.Fatalf("no sink: %q", got)
	}
	if got := newSinkAddresses("100.152.70.71:4317").forNode(ctx, "n1"); got != "100.152.70.71:4317" {
		t.Fatalf("an address: %q", got)
	}
	if got := newSinkAddresses("[fd00::1]:4317").forNode(ctx, "n1"); got != "[fd00::1]:4317" {
		t.Fatalf("an IPv6 address: %q", got)
	}

	now := time.Unix(1000, 0)
	answer, lookups := []string{"10.0.0.2", "10.0.0.1"}, 0
	var fail error
	s := newSinkAddresses("ferun-sink-ingest.ferun.svc:4317")
	s.now = func() time.Time { return now }
	s.lookup = func(context.Context, string) ([]string, error) {
		lookups++
		if fail != nil {
			return nil, fail
		}
		return append([]string(nil), answer...), nil
	}
	seen := map[string]bool{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		got := s.forNode(ctx, n)
		if got != "10.0.0.1:4317" && got != "10.0.0.2:4317" {
			t.Fatalf("node %s: %q", n, got)
		}
		if again := s.forNode(ctx, n); again != got {
			t.Fatalf("node %s: %q, then %q", n, got, again)
		}
		seen[got] = true
	}
	if len(seen) != 2 || lookups != 1 {
		t.Fatalf("addresses given %v after %d lookups; want both after one", seen, lookups)
	}
	first := s.forNode(ctx, "a")

	now = now.Add(sinkLookupEvery)
	fail = errors.New("no answer")
	if got := s.forNode(ctx, "a"); got != first || lookups != 2 {
		t.Fatalf("after a failed lookup: %q (%d lookups); want %q", got, lookups, first)
	}

	now = now.Add(sinkLookupEvery)
	fail, answer = nil, []string{"10.0.0.3"}
	if got := s.forNode(ctx, "a"); got != "10.0.0.3:4317" {
		t.Fatalf("after the addresses changed: %q", got)
	}

	never := newSinkAddresses("ferun-sink-ingest.ferun.svc:4317")
	never.lookup = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	if got := never.forNode(ctx, "a"); got != "ferun-sink-ingest.ferun.svc:4317" {
		t.Fatalf("a name that never resolved: %q", got)
	}
}
