// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package podid is a pod's ID, which is its sandbox's ID (ADR 0019): 11
// characters of lowercase Crockford base32, the first a letter, which
// ferund mints; or, for a pod made before, a UUID in its lowercase
// hyphenated form, which it keeps until it is deleted.
//
// A short ID names its pod's TAP device whole (pod-<ID>), so no two pods
// with short IDs share one. A UUID's TAP device is pod- and its first 8 hex
// digits, which other UUIDs may share: TAPConflict says which.
package podid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

// ID is a pod's (and its sandbox's) ID, short or a legacy UUID, always in
// its canonical form; the zero value "" is none.
type ID string

// Len is the length of a short ID; LegacyLen of a UUID's.
const (
	Len       = 11
	LegacyLen = 36
)

// Alphabet is Crockford's base32 in lowercase: no i, l, o or u.
const Alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// letters are the alphabet's letters, the first character of a short ID.
const letters = "abcdefghjkmnpqrstvwxyz"

// ErrInvalid is the error of Parse, wrapped with the value.
var ErrInvalid = errors.New("not a pod ID")

// Mint draws a new short ID from the cryptographic random source: about
// 2^54 of them, so a caller that stores it unique and draws again on a
// conflict rarely draws twice.
func Mint() ID {
	var b [Len]byte
	for {
		// crypto/rand.Read never fails: it crashes the program when the
		// system's source does.
		_, _ = rand.Read(b[:])
		// 242 = 11 × 22: below it, b[0] % 22 picks a letter uniformly.
		if b[0] < 242 {
			break
		}
	}
	out := make([]byte, Len)
	out[0] = letters[int(b[0])%len(letters)]
	for i := 1; i < Len; i++ {
		out[i] = Alphabet[b[i]&31]
	}
	return ID(out)
}

// Parse reads an ID: a short ID, exactly as minted (lowercase), or a UUID
// in its hyphenated form in either case, which it returns in lowercase.
func Parse(s string) (ID, error) {
	switch len(s) {
	case Len:
		if validShort(s) {
			return ID(s), nil
		}
	case LegacyLen:
		if l := strings.ToLower(s); validUUID(l) {
			return ID(l), nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrInvalid, s)
}

// Valid reports whether s is an ID in its canonical form: a short ID, or
// a UUID in lowercase.
func Valid(s string) bool {
	switch len(s) {
	case Len:
		return validShort(s)
	case LegacyLen:
		return validUUID(s)
	}
	return false
}

// validShort: ^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$.
func validShort(s string) bool {
	if len(s) != Len || strings.IndexByte(letters, s[0]) < 0 {
		return false
	}
	for i := 1; i < Len; i++ {
		if strings.IndexByte(Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// validUUID: ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$.
func validUUID(s string) bool {
	if len(s) != LegacyLen {
		return false
	}
	for i := 0; i < LegacyLen; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// String is the ID as it is written everywhere.
func (id ID) String() string { return string(id) }

// IsZero reports whether the ID is none.
func (id ID) IsZero() bool { return id == "" }

// IsLegacy reports whether the ID is a UUID: a pod made before ADR 0019.
func (id ID) IsLegacy() bool { return len(id) == LegacyLen }

// TAPKey is the part of the ID that names the pod's TAP device on its
// node: a short ID whole, a UUID's first 8 hex digits.
func (id ID) TAPKey() string {
	if id.IsLegacy() {
		return string(id[:8])
	}
	return string(id)
}

// TAP is the name of the pod's TAP device: pod- and TAPKey, at most 15
// characters (IFNAMSIZ).
func (id ID) TAP() string { return "pod-" + id.TAPKey() }

// TAPConflict reports whether two different pods' TAP devices have the
// same name, so that they cannot share a node: only two UUIDs with the
// same first 8 hex digits do.
func TAPConflict(a, b ID) bool {
	return a != b && a.IsLegacy() && b.IsLegacy() && a[:8] == b[:8]
}

// MarshalText renders the ID (JSON: a string).
func (id ID) MarshalText() ([]byte, error) { return []byte(id), nil }

// UnmarshalText parses the ID with Parse; the empty text is none.
func (id *ID) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*id = ""
		return nil
	}
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}
