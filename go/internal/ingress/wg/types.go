// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"net/netip"
	"time"
)

// Peer is a peer as it is set: its key, where it is, the prefixes it may
// send from and that go to it, and how often a keepalive goes to it.
type Peer struct {
	PublicKey Key
	// Endpoint is the peer's UDP address. The zero value sets none and
	// leaves the one the device has learnt.
	Endpoint netip.AddrPort
	// AllowedIPs replace those the peer had.
	AllowedIPs []netip.Prefix
	// Keepalive is the persistent keepalive interval, in whole seconds up
	// to 65535; 0 is off.
	Keepalive time.Duration
}

// PeerState is a peer as the device has it.
type PeerState struct {
	Peer
	// LastHandshake is the time of the last completed handshake; the zero
	// time if there was none.
	LastHandshake time.Time
	RxBytes       uint64
	TxBytes       uint64
}

// Device is a WireGuard device as the kernel has it.
type Device struct {
	Name  string
	Index int
	// PublicKey is zero while the device has no private key.
	PublicKey  Key
	ListenPort int
	Peers      []PeerState
}

// Link is a network link as rtnetlink reports it.
type Link struct {
	Index int
	Name  string
	// Kind is the link's kind, "wireguard" for a WireGuard device; empty
	// for a link without one, such as lo or a physical device.
	Kind string
	MTU  int
	Up   bool
}

// Route is a route out of a link, without a gateway.
type Route struct {
	// Dst is the destination.
	Dst netip.Prefix
	// Index is the index of the link the route goes out of.
	Index int
	// Src is the preferred source address; the zero value sets none.
	Src netip.Addr
	// MTU is the route's MTU; 0 uses the link's.
	MTU int
}
