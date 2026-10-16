// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package nodelink is the nodes' port of an instance (ADR 0015): one TLS
// 1.3 listener that nodes dial, routed strictly by ALPN.
//
//   - "feos-node/1" with the node's certificate is a node connection: the
//     node serves its feos.* APIs over it and the instance is the gRPC
//     client. The registry here keeps every node connection of this
//     instance, picks the current one per node and lets the others drain;
//     Conns is how the rest of ferund reaches a node.
//   - "h2" with the node's certificate is an uplink: the node calls
//     feos.fleet.v1 on the uplink server, which fleet implements.
//   - "h2" without a certificate goes to the join server, which serves only
//     Join; "h2" whose certificate is definitively refused goes to the
//     refusal server, which tells the node to join again.
//
// Every connection with a certificate is checked against the database
// through Config.CheckNode at accept and on every re-check, never from a
// cache, and is closed when its certificate expires or is revoked.
package nodelink
