// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package pki is the control plane's certificate authority: the offline
// root and the one intermediate, the issuer, that every instance holds
// (ADR 0014, ADR 0015); the issuance of node, ingress, pod and server
// certificates; the checks of signing requests; the SPIFFE IDs; join
// tokens with their pins and the verification a node runs against them;
// and the HPKE sealing of a pod's bindings.
//
// The issuer's name constraints keep every certificate inside the platform
// DNS domain and the trust domain and away from IP addresses. They do not
// separate pods from the control plane, whose domain lies beneath the same
// platform domain: that separation is the issuance code here (only
// IssueServer writes the control plane's SPIFFE ID, pods never get a name
// in the control-plane domain) and the node's check of the control plane's
// URI (VerifyJoinServer, VerifyControlPlane).
package pki
