// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"hash/fnv"
	"net"
	"slices"
	"sync"
	"time"
)

// The sink's addresses are looked up again after sinkLookupEvery, each
// lookup waiting at most sinkLookupWait.
const (
	sinkLookupEvery = 10 * time.Second
	sinkLookupWait  = 2 * time.Second
)

// sinkAddresses turns --sink-endpoint into the address a node ships to
// (ADR 0026). A node does not resolve the cluster's names, and on a
// Kubernetes host it does not reach a Service's ClusterIP either, so a
// name (a headless Service's) is resolved here, into the addresses of the
// sink's ready instances, and each node is given one of them, the same
// one while it is there. An address that is not a name goes out as it is,
// and so does the name while it resolves to nothing.
type sinkAddresses struct {
	endpoint   string
	host, port string
	lookup     func(ctx context.Context, host string) ([]string, error)
	now        func() time.Time

	mu      sync.Mutex
	addrs   []string
	fetched time.Time
}

func newSinkAddresses(endpoint string) *sinkAddresses {
	s := &sinkAddresses{endpoint: endpoint, lookup: net.DefaultResolver.LookupHost, now: time.Now}
	if host, port, err := net.SplitHostPort(endpoint); err == nil && net.ParseIP(host) == nil {
		s.host, s.port = host, port
	}
	return s
}

// forNode is the address node ships to; "" when there is no sink.
func (s *sinkAddresses) forNode(ctx context.Context, node string) string {
	if s == nil || s.endpoint == "" || s.host == "" {
		return s.fixed()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now().Sub(s.fetched) >= sinkLookupEvery {
		ctx, cancel := context.WithTimeout(ctx, sinkLookupWait)
		addrs, err := s.lookup(ctx, s.host)
		cancel()
		if err == nil && len(addrs) > 0 {
			slices.Sort(addrs)
			s.addrs = addrs
		}
		// A failed lookup keeps the addresses there were until the next.
		s.fetched = s.now()
	}
	if len(s.addrs) == 0 {
		return s.endpoint
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(node))
	return net.JoinHostPort(s.addrs[int(h.Sum32()%uint32(len(s.addrs)))], s.port)
}

func (s *sinkAddresses) fixed() string {
	if s == nil {
		return ""
	}
	return s.endpoint
}
