// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"
)

// LinkByName returns the link name. A missing link is an error that
// matches syscall.ENODEV.
func LinkByName(name string) (Link, error) {
	if err := checkName(name); err != nil {
		return Link{}, err
	}
	msgs, err := request(netlinkRoute, getLinkMessage(name))
	if err != nil {
		return Link{}, fmt.Errorf("look up link %s: %w", name, err)
	}
	for _, m := range msgs {
		if m.typ == rtmNewLink {
			return parseLink(m)
		}
	}
	return Link{}, fmt.Errorf("look up link %s: the kernel did not answer with the link", name)
}

// EnsureLink makes the WireGuard device name unless it is there, and
// returns its index. A link of that name of another kind is an error.
func EnsureLink(name string) (int, error) {
	l, err := LinkByName(name)
	if errors.Is(err, syscall.ENODEV) {
		if _, err := request(netlinkRoute, newLinkMessage(name)); err != nil && !errors.Is(err, syscall.EEXIST) {
			return 0, fmt.Errorf("add link %s of kind %s: %w", name, wgLinkKind, err)
		}
		l, err = LinkByName(name)
	}
	if err != nil {
		return 0, err
	}
	if l.Kind != wgLinkKind {
		return 0, fmt.Errorf("link %s is of kind %q, not %s", name, l.Kind, wgLinkKind)
	}
	return l.Index, nil
}

// SetMTU sets the MTU of the link index.
func SetMTU(index, mtu int) error {
	if err := checkIndex(index); err != nil {
		return err
	}
	if mtu <= 0 || mtu > 65535 {
		return fmt.Errorf("MTU %d out of range", mtu)
	}
	if _, err := request(netlinkRoute, setMTUMessage(index, mtu)); err != nil {
		return fmt.Errorf("set the MTU of link %d to %d: %w", index, mtu, err)
	}
	return nil
}

// SetUp brings the link index up.
func SetUp(index int) error {
	if err := checkIndex(index); err != nil {
		return err
	}
	if _, err := request(netlinkRoute, setUpMessage(index)); err != nil {
		return fmt.Errorf("bring link %d up: %w", index, err)
	}
	return nil
}

// ReplaceAddress puts the address p (p.Addr() with its prefix length) on
// the link index without duplicate address detection, or updates it there.
func ReplaceAddress(index int, p netip.Prefix) error {
	if err := checkIndex(index); err != nil {
		return err
	}
	if !p.IsValid() {
		return fmt.Errorf("address %v is not address/length", p)
	}
	if _, err := request(netlinkRoute, replaceAddrMessage(index, p)); err != nil {
		return fmt.Errorf("put address %s on link %d: %w", p, index, err)
	}
	return nil
}

// Addresses returns the addresses on the link index, of every family,
// with their prefix lengths, link-local ones included.
func Addresses(index int) ([]netip.Prefix, error) {
	if err := checkIndex(index); err != nil {
		return nil, err
	}
	msgs, err := request(netlinkRoute, getAddrMessage(index))
	if err != nil {
		return nil, fmt.Errorf("list the addresses of link %d: %w", index, err)
	}
	return parseAddrs(msgs, index)
}

// DeleteAddress deletes the address p from the link index.
func DeleteAddress(index int, p netip.Prefix) error {
	if err := checkIndex(index); err != nil {
		return err
	}
	if !p.IsValid() {
		return fmt.Errorf("address %v is not address/length", p)
	}
	if _, err := request(netlinkRoute, delAddrMessage(index, p)); err != nil {
		return fmt.Errorf("delete address %s from link %d: %w", p, index, err)
	}
	return nil
}

// ReplaceRoute puts r in the main table, or replaces the route there with
// the same destination.
func ReplaceRoute(r Route) error {
	if err := checkIndex(r.Index); err != nil {
		return err
	}
	switch {
	case !r.Dst.IsValid():
		return fmt.Errorf("route destination %v is not a prefix", r.Dst)
	case r.Src.IsValid() && r.Src.Is4() != r.Dst.Addr().Is4():
		return fmt.Errorf("route to %s with a source %s of another family", r.Dst, r.Src)
	case r.MTU < 0:
		return fmt.Errorf("route MTU %d out of range", r.MTU)
	}
	if _, err := request(netlinkRoute, replaceRouteMessage(r)); err != nil {
		return fmt.Errorf("route %s via link %d: %w", r.Dst, r.Index, err)
	}
	return nil
}

// DeleteLink deletes the link index.
func DeleteLink(index int) error {
	if err := checkIndex(index); err != nil {
		return err
	}
	if _, err := request(netlinkRoute, delLinkMessage(index)); err != nil {
		return fmt.Errorf("delete link %d: %w", index, err)
	}
	return nil
}

// MoveLinkToNetns moves the link index to the network namespace that the
// open file nsFD refers to, such as /proc/<tid>/ns/net. A WireGuard
// device keeps its UDP socket in the namespace it was made in.
func MoveLinkToNetns(index, nsFD int) error {
	if err := checkIndex(index); err != nil {
		return err
	}
	if nsFD < 0 {
		return fmt.Errorf("namespace file descriptor %d", nsFD)
	}
	if _, err := request(netlinkRoute, moveLinkMessage(index, nsFD)); err != nil {
		return fmt.Errorf("move link %d to another network namespace: %w", index, err)
	}
	return nil
}

func checkIndex(index int) error {
	if index <= 0 || index > 1<<31-1 {
		return fmt.Errorf("link index %d out of range", index)
	}
	return nil
}
