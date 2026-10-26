// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

// Sandbox references. ferund mints every sandbox's ID (ADR 0019), and
// this backend keeps no database, so the browser keeps a conversation's
// sandbox ID: the backend hands it out as a reference, bound to the
// signed-in user and the conversation, and takes it back with every chat
// request. A reference is
//
//	v1.<sandbox ID>.<MAC>
//
// MAC is the unpadded base64url HMAC-SHA256, keyed with a key derived
// from SESSION_SECRET, over the version, the user's subject, the
// conversation ID and the sandbox ID, each with its length in front.
// The sandbox ID is readable: it is no secret (a sandbox is reached with
// its token, which never leaves the backend), and the reference only
// makes sure the backend gives a user's conversation no sandbox it did
// not hand out for that user and that conversation.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"log"
	"regexp"
	"strings"
)

const sandboxRefVersion = "v1"

// sandboxIDPattern takes the IDs ferund mints: the short IDs of ADR 0019
// and the UUIDs from before them. Nothing a reference carries needs
// escaping in a URL path or in the reference itself.
var sandboxIDPattern = regexp.MustCompile(`^[0-9a-z][0-9a-z-]{0,63}$`)

func validSandboxID(id string) bool { return sandboxIDPattern.MatchString(id) }

// uuidPattern is a UUID as the SPA makes conversation IDs and as the
// sandboxes of conversations from before ADR 0019 are named.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newRefKey derives the references' key from SESSION_SECRET. Without
// one (development, no sign-in) the key is random: references then hold
// until the backend restarts.
func newRefKey(secret string) []byte {
	if secret == "" {
		var b [32]byte
		_, _ = rand.Read(b[:]) // crypto/rand does not fail on Linux
		log.Printf("SESSION_SECRET is not set: sandbox references hold until the backend restarts")
		secret = base64.RawURLEncoding.EncodeToString(b[:])
	}
	k := sha256.Sum256([]byte("fechat sandbox references\x00" + secret))
	return k[:]
}

func sandboxRefMAC(key []byte, subject, conv, sandboxID string) []byte {
	var msg []byte
	for _, f := range []string{sandboxRefVersion, subject, conv, sandboxID} {
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(f)))
		msg = append(msg, f...)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// signSandboxRef hands out sandboxID for subject's conversation conv.
func signSandboxRef(key []byte, subject, conv, sandboxID string) string {
	return sandboxRefVersion + "." + sandboxID + "." +
		base64.RawURLEncoding.EncodeToString(sandboxRefMAC(key, subject, conv, sandboxID))
}

// verifySandboxRef returns the sandbox ID of ref when this backend
// handed it out for subject's conversation conv.
func verifySandboxRef(key []byte, subject, conv, ref string) (string, bool) {
	parts := strings.Split(ref, ".")
	if len(parts) != 3 || parts[0] != sandboxRefVersion || !validSandboxID(parts[1]) {
		return "", false
	}
	sum, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sum, sandboxRefMAC(key, subject, conv, parts[1])) {
		return "", false
	}
	return parts[1], true
}
