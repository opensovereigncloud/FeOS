// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

// ValidateDomain checks a lowercase DNS name: labels
// [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])? joined by dots, at most 255 bytes,
// without a scheme or a port, and not an IP address. It is the check of
// the trust domain and of the certificates' DNS names, for the other
// domains of the configuration (--apps-zone).
func ValidateDomain(s string) error { return validateDomain(s) }
