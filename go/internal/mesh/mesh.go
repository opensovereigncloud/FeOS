// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package mesh is the arithmetic of the pods' mesh (ADR 0017): the
// cluster's ULA, a /48 with a Global ID of 40 random bits (RFC 4193,
// section 3.2); a node's /64 of it, the subnet ID in bits 48 to 63; the
// endpoints of the nodes' WireGuard devices; and the deltas of the peers
// that the leader sends every node.
package mesh

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// The subnet IDs a node can get: 0 is never given to a node.
const (
	FirstSubnet = 1
	LastSubnet  = 65535
)

// KeyLen is the length of a WireGuard public key.
const KeyLen = 32

// Draw returns a new ULA /48: fd and a Global ID of 40 random bits.
func Draw() (netip.Prefix, error) {
	var a [16]byte
	a[0] = 0xfd
	if _, err := rand.Read(a[1:6]); err != nil {
		return netip.Prefix{}, fmt.Errorf("draw the pod network's Global ID: %w", err)
	}
	return netip.PrefixFrom(netip.AddrFrom16(a), 48), nil
}

// NodePrefix is the /64 of subnet in ula.
func NodePrefix(ula netip.Prefix, subnet int) netip.Prefix {
	a := ula.Masked().Addr().As16()
	a[6], a[7] = byte(subnet>>8), byte(subnet)
	return netip.PrefixFrom(netip.AddrFrom16(a), 64)
}

// CheckEndpoint checks a node's endpoint, "host:port" with an IP address
// as the host, an IPv6 one in brackets, and a port from 1 to 65535, and
// returns it in its canonical form. An unspecified or multicast address,
// an IPv4-mapped one and an IPv6 zone are refused.
func CheckEndpoint(s string) (string, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return "", fmt.Errorf("endpoint %q is not address:port: %w", s, err)
	}
	a := ap.Addr()
	switch {
	case ap.Port() == 0:
		return "", fmt.Errorf("endpoint %q has no port", s)
	case a.Zone() != "":
		return "", fmt.Errorf("endpoint %q has a zone", s)
	case a.Is4In6():
		return "", fmt.Errorf("endpoint %q is an IPv4-mapped address", s)
	case a.IsUnspecified() || a.IsMulticast():
		return "", fmt.Errorf("endpoint %q is no host's address", s)
	}
	return ap.String(), nil
}

// Peer is another node, as a node's peer.
type Peer struct {
	Node      string
	PublicKey []byte
	Endpoint  string
	PodPrefix string
}

// Equal reports whether p and q are the same peer.
func (p Peer) Equal(q Peer) bool {
	return p.Node == q.Node && bytes.Equal(p.PublicKey, q.PublicKey) && p.Endpoint == q.Endpoint &&
		p.PodPrefix == q.PodPrefix
}

// Peers is the mesh's nodes at a version, by name.
type Peers struct {
	Version uint64
	Nodes   map[string]Peer
}

// For returns the peers of node: every node of p but itself, by name.
func (p Peers) For(node string) []Peer {
	out := make([]Peer, 0, len(p.Nodes))
	for name, peer := range p.Nodes {
		if name != node {
			out = append(out, peer)
		}
	}
	slices.SortFunc(out, func(a, b Peer) int { return strings.Compare(a.Node, b.Node) })
	return out
}

// Delta is what turns node's peers of from into its peers of to: the peers
// new or changed, by name, and the names of those gone.
func Delta(from, to Peers, node string) (put []Peer, remove []string) {
	for name, peer := range to.Nodes {
		if name == node {
			continue
		}
		if old, ok := from.Nodes[name]; !ok || !old.Equal(peer) {
			put = append(put, peer)
		}
	}
	for name := range from.Nodes {
		if _, ok := to.Nodes[name]; !ok && name != node {
			remove = append(remove, name)
		}
	}
	slices.SortFunc(put, func(a, b Peer) int { return strings.Compare(a.Node, b.Node) })
	slices.Sort(remove)
	return put, remove
}
