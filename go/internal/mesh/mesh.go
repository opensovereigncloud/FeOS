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
	"crypto/ecdh"
	"crypto/rand"
	"errors"
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

// keyProbe is a fixed X25519 private key whose shared secret with a
// public key is all zeros exactly when that key is a point of small
// order: X25519 clamps every scalar to a multiple of the cofactor 8.
var keyProbe = [32]byte{0x6d, 0x65, 0x73, 0x68, 0x20, 0x6b, 0x65, 0x79, 0x20, 0x70, 0x72, 0x6f, 0x62, 0x65, 0x2e, 0x2e,
	0x9b, 0x3c, 0x41, 0x7e, 0x05, 0xd2, 0x88, 0x16, 0xaf, 0x63, 0x2d, 0xe4, 0x51, 0x90, 0x0c, 0x77}

// CheckKey checks a WireGuard public key that a node or an ingress
// instance offers: KeyLen bytes, and not a point of small order (all
// zeros among them), with which every private key computes the same
// shared secret, so that no handshake would authenticate its holder.
func CheckKey(key []byte) error {
	if len(key) != KeyLen {
		return fmt.Errorf("the key has %d bytes, not %d", len(key), KeyLen)
	}
	pub, err := ecdh.X25519().NewPublicKey(key)
	if err != nil {
		return fmt.Errorf("the key: %w", err)
	}
	probe, err := ecdh.X25519().NewPrivateKey(keyProbe[:])
	if err != nil {
		return fmt.Errorf("the key's check: %w", err)
	}
	if _, err := probe.ECDH(pub); err != nil {
		return errors.New("the key is a point of small order, which is nobody's key")
	}
	return nil
}

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

// HostAddress is a peer's own address on its /64: the prefix's network
// address with its last byte 1, <prefix>::1 (an IPv4 prefix's in its own
// family; an invalid prefix has the zero Addr). An ingress instance's
// address is the HostAddress of its prefix.
func HostAddress(p netip.Prefix) netip.Addr {
	if !p.IsValid() {
		return netip.Addr{}
	}
	a := p.Masked().Addr()
	if a.Is4() {
		b := a.As4()
		b[3] = 1
		return netip.AddrFrom4(b)
	}
	b := a.As16()
	b[15] = 1
	return netip.AddrFrom16(b)
}

// ingressPeerPrefix starts the key of an ingress instance among the
// mesh's peers. Node names are DNS labels, so the dot never collides with
// a node's key.
const ingressPeerPrefix = "ingress."

// IngressPeerName is the key of ingress instance name among the mesh's
// peers (Peer.Node and the keys of Peers.Nodes): "ingress.<name>". Nodes
// see an ingress instance as one more peer with a /64.
func IngressPeerName(name string) string { return ingressPeerPrefix + name }

// IsIngressPeer reports whether key, a key of the mesh's peers, is an
// ingress instance's (IngressPeerName) rather than a node's.
func IsIngressPeer(key string) bool { return strings.HasPrefix(key, ingressPeerPrefix) }

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
