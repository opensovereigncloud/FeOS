// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"errors"
)

// ErrNotImplemented is the error of a function that a later lane
// implements.
var ErrNotImplemented = errors.New("spec: not implemented yet")

// Parse reads a spec strictly: raw must be one JSON object of at most
// MaxSpecBytes; an unknown field anywhere, a value of the wrong type, an
// env value of no known form, a duplicate key and trailing data are error
// findings with their paths, all of them, not only the first. The Spec
// holds what could be read; it is meaningful only without error findings.
func Parse(raw []byte) (Spec, []Finding) {
	return Spec{}, []Finding{notImplemented("parsing")}
}

// EqualNormalised reports whether two normalised specs are the same
// release: their canonical JSON (map keys sorted by the encoder) is
// equal. Specs that are not normalised may differ only in form.
func EqualNormalised(a, b Spec) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
