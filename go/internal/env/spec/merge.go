// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

// MergePatch applies the RFC 7396 merge patch patch to the JSON document
// doc and returns the result: an object in patch merges into doc's
// object member by member, null removes a member, anything else replaces.
// Both must be JSON; doc may be empty (an empty object).
func MergePatch(doc, patch []byte) ([]byte, error) {
	return nil, ErrNotImplemented
}
