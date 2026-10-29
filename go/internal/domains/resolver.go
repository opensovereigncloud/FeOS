// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// DNS is what a check asks of DNS: *Resolver, and fakes in tests.
type DNS interface {
	// Lookup asks for a domain's TXT record and its addresses.
	Lookup(ctx context.Context, q Query) Answer
	// Resolve asks for name's addresses (the edge's). A name that does
	// not exist is no error: no addresses.
	Resolve(ctx context.Context, name string) ([]netip.Addr, error)
}

// Query is one check's question.
type Query struct {
	// Name is the domain's name; TXTName the name of its TXT record,
	// <label>.<name>.
	Name, TXTName string
}

// Answer is what one check's lookups found. A part whose lookup failed
// (a timeout, SERVFAIL, no server reached) has its error set; an answer
// that denies the name or the record (NXDOMAIN, no such records) is no
// error, but an empty part.
type Answer struct {
	// TXT are the values of the TXT records at TXTName.
	TXT    []string
	TXTErr error
	// Addresses are what Name resolves to, through any CNAME or ALIAS;
	// CNAME is its canonical name when that differs from Name, shown for
	// information only.
	Addresses []netip.Addr
	CNAME     []string
	RouteErr  error
}

// Resolver is the DNS of the checks: Go's own resolver (net.Resolver
// with PreferGo) over --dns-resolver, or over the system's resolvers
// (/etc/resolv.conf) when that is not set. It asks a recursive resolver,
// which may answer from its cache for a record's TTL: a record just set
// or removed, and a negative answer cached before, may take minutes to
// show, also to POST …/check. Names are asked absolute (with a trailing
// dot), so that no search domain is tried.
type Resolver struct {
	r *net.Resolver
}

// NewResolver returns the checks' resolver: over server (host:port) when
// it is not "", else over the system's resolvers; timeout bounds one dial.
func NewResolver(server string, timeout time.Duration) *Resolver {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	r := &net.Resolver{PreferGo: true}
	if server != "" {
		d := net.Dialer{Timeout: timeout}
		r.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, server)
		}
	}
	return &Resolver{r: r}
}

// absolute is name with a trailing dot.
func absolute(name string) string { return strings.TrimSuffix(name, ".") + "." }

// notFound reports whether err is an answer that denies the name or the
// record: NXDOMAIN, or no records of the type. Every other error, a
// timeout, SERVFAIL, a refusal or no server reached, is a failed lookup.
func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound && !de.IsTimeout && !de.IsTemporary
}

// Lookup answers q: the TXT record, the addresses, and the canonical
// name.
func (r *Resolver) Lookup(ctx context.Context, q Query) Answer {
	var a Answer
	txt, err := r.r.LookupTXT(ctx, absolute(q.TXTName))
	switch {
	case err == nil:
		a.TXT = txt
	case !notFound(err):
		a.TXTErr = err
	}
	a.Addresses, a.RouteErr = r.Resolve(ctx, q.Name)
	if a.RouteErr == nil {
		if cn, err := r.r.LookupCNAME(ctx, absolute(q.Name)); err == nil {
			if cn = strings.ToLower(strings.TrimSuffix(cn, ".")); cn != "" && cn != q.Name {
				a.CNAME = []string{cn}
			}
		}
	}
	return a
}

// Resolve returns name's A and AAAA records, IPv4 ones unmapped, sorted.
func (r *Resolver) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	ips, err := r.r.LookupIPAddr(ctx, absolute(name))
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip.IP); ok {
			out = append(out, a)
		}
	}
	return sortAddrs(out), nil
}

// sortAddrs sorts and dedups addresses, IPv4 ones unmapped.
func sortAddrs(as []netip.Addr) []netip.Addr {
	for i, a := range as {
		as[i] = a.Unmap()
	}
	slices.SortFunc(as, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(as)
}
