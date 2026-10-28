// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package wg

import (
	"errors"
	"fmt"
	"net/netip"
)

// errUnsupported is the answer of every call to the kernel off Linux.
var errUnsupported = fmt.Errorf("WireGuard in the kernel needs Linux: %w", errors.ErrUnsupported)

// LinkByName returns errors.ErrUnsupported.
func LinkByName(string) (Link, error) { return Link{}, errUnsupported }

// EnsureLink returns errors.ErrUnsupported.
func EnsureLink(string) (int, error) { return 0, errUnsupported }

// SetMTU returns errors.ErrUnsupported.
func SetMTU(int, int) error { return errUnsupported }

// SetUp returns errors.ErrUnsupported.
func SetUp(int) error { return errUnsupported }

// ReplaceAddress returns errors.ErrUnsupported.
func ReplaceAddress(int, netip.Prefix) error { return errUnsupported }

// Addresses returns errors.ErrUnsupported.
func Addresses(int) ([]netip.Prefix, error) { return nil, errUnsupported }

// DeleteAddress returns errors.ErrUnsupported.
func DeleteAddress(int, netip.Prefix) error { return errUnsupported }

// ReplaceRoute returns errors.ErrUnsupported.
func ReplaceRoute(Route) error { return errUnsupported }

// DeleteLink returns errors.ErrUnsupported.
func DeleteLink(int) error { return errUnsupported }

// MoveLinkToNetns returns errors.ErrUnsupported.
func MoveLinkToNetns(int, int) error { return errUnsupported }

// Configure returns errors.ErrUnsupported.
func Configure(string, Key, int) error { return errUnsupported }

// Get returns errors.ErrUnsupported.
func Get(string) (Device, error) { return Device{}, errUnsupported }

// SetPeers returns errors.ErrUnsupported.
func SetPeers(string, []Peer) error { return errUnsupported }
