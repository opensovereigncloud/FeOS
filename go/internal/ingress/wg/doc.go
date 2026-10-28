// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package wg sets up the WireGuard device of ferun-ingress in the kernel
// (ADR 0021): the link of kind wireguard, its MTU, addresses and routes
// over rtnetlink, and its key, port and peers over the generic netlink
// family wireguard, as ip and wg set them, without them. It uses the
// standard library's syscall package only, and defines every netlink
// constant it uses itself, so that its portable part (keys, types, the
// encoding and parsing of messages, the diff of peers) builds everywhere.
//
// Every call opens a netlink socket of its own, which belongs to the
// network namespace of the calling thread. A caller that works in another
// namespace runs its calls on a goroutine locked to a thread in it.
//
// A device is set up in this order: EnsureLink, SetMTU, SetUp,
// ReplaceAddress, ReplaceRoute, Configure, SetPeers. IPv6 drops the
// addresses of a link that goes down, and a route needs its link up and
// its preferred source configured; ReplaceAddress sets IFA_F_NODAD, so the
// address is usable at once.
//
// SetPeers changes only the peers that differ: a peer that stays as it is
// keeps its session.
package wg
