// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// DefaultDenyPrefixes is the destination deny set when --deny-prefix names
// none: the private, shared, link-local, loopback, "this network",
// multicast and reserved IPv4 ranges. A binding's server is external by
// definition; the platform's own networks are infrastructure no binding
// may name.
func DefaultDenyPrefixes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
}

// denyPolicy judges one address: IPv4 outside the deny set, or an error
// saying why not. The platform reaches servers over IPv4 only, so an IPv6
// address is never a destination either.
func denyPolicy(deny []netip.Prefix) func(netip.Addr) error {
	return func(a netip.Addr) error {
		a = a.Unmap()
		if !a.Is4() {
			return fmt.Errorf("%s is not an IPv4 address (the platform reaches servers over IPv4 only)", a)
		}
		for _, p := range deny {
			if p.Contains(a) {
				return fmt.Errorf("%s is an internal address (%s)", a, p)
			}
		}
		return nil
	}
}

// destRule is the destination rule as a value: the resolver and two
// policies. allowDest judges the addresses a server resolves to at
// validation; allowDial judges the address a preflight actually dials, so
// that a name resolving differently a moment later is judged again. Both
// are the deny policy in production; tests replace them to reach a
// loopback test server.
type destRule struct {
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error)
	allowDest func(netip.Addr) error
	allowDial func(netip.Addr) error
}

func newDestRule(deny []netip.Prefix) destRule {
	p := denyPolicy(deny)
	return destRule{lookup: lookupIPv4, allowDest: p, allowDial: p}
}

// lookupIPv4 resolves a host's A records.
func lookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

// checkDestination applies the rule to a server host at validation: an
// address as it is, a name by every address it resolves to now. One
// internal address is enough to refuse: a name that answers with both can
// be made to answer with the internal one only.
func (d *destRule) checkDestination(ctx context.Context, host string) *bindingErr {
	if a, err := netip.ParseAddr(host); err == nil {
		if err := d.allowDest(a); err != nil {
			return bindingErrf(codeDestinationDenied, "server %s: %v", host, err)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := d.lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		why := "no IPv4 address"
		if err != nil {
			why = err.Error()
		}
		return bindingErrf(codeDestinationDenied,
			"server host %s does not resolve (%s); the destination rule needs its addresses", host, why)
	}
	for _, a := range addrs {
		if err := d.allowDest(a); err != nil {
			return bindingErrf(codeDestinationDenied, "server host %s resolves to %v", host, err)
		}
	}
	return nil
}

// errDialDenied marks a dial the rule refused.
type errDialDenied struct{ err error }

func (e *errDialDenied) Error() string { return "destination denied: " + e.err.Error() }

// dialer applies the rule to every address it is about to connect to.
func (d *destRule) dialer() *net.Dialer {
	allow := d.allowDial
	return &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return &errDialDenied{fmt.Errorf("unparseable address %q", address)}
			}
			if err := allow(ap.Addr()); err != nil {
				return &errDialDenied{err}
			}
			return nil
		},
	}
}
