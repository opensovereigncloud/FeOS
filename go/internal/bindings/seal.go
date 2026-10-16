// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// The tables whose rows carry a sealed credential; each is part of the
// additional data of its blobs.
const (
	tableKube = "kube_bindings"
	tableSSH  = "ssh_bindings"
)

// sealVersion is the first byte of every blob, so that a later key
// rotation can tell generations apart.
const sealVersion = 1

// newAEAD is the one cipher both surfaces seal with: AES-256-GCM under the
// key of --secret-key-file.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("the bindings' secret key has %d bytes, want 32 (AES-256)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealAAD binds a blob to its row: a blob copied into another binding,
// another sandbox's row, another auth type or another table does not
// open.
func sealAAD(table string, sandboxID uuid.UUID, name, authType string) []byte {
	return []byte("ferun/" + table + "/v1\x00" + sandboxID.String() + "\x00" + name + "\x00" + authType)
}

// sealBlob encrypts pt: the version byte, a random nonce, then the
// AES-GCM ciphertext.
func sealBlob(aead cipher.AEAD, pt, aad []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{sealVersion}, nonce...)
	return aead.Seal(out, nonce, pt, aad), nil
}

// openBlob is sealBlob's inverse.
func openBlob(aead cipher.AEAD, blob, aad []byte) ([]byte, error) {
	ns := aead.NonceSize()
	if len(blob) < 1+ns || blob[0] != sealVersion {
		return nil, errors.New("sealed credential is malformed")
	}
	pt, err := aead.Open(nil, blob[1:1+ns], blob[1+ns:], aad)
	if err != nil {
		return nil, errors.New("sealed credential does not open (was the key of --secret-key-file changed?)")
	}
	return pt, nil
}

// podVersion is the version of a pod's bindings as IssuePodCredentials
// seals and issued_certs records it: "" when the pod has neither set,
// otherwise the lowercase hex SHA-256 of the Kubernetes set's version, the
// SSH set's version and, for each SSH binding by name, its name and
// server, each followed by a zero byte. The SSH set's version leaves the
// server out (it is ferund's business on the REST surface), but the pod
// receives it, so the pod version covers it: the version changes exactly
// when what PodBindings delivers does.
func podVersion(kubeVersion, sshVersion string, ssh []sshStored) string {
	if kubeVersion == "" && sshVersion == "" {
		return ""
	}
	sorted := append([]sshStored(nil), ssh...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	h.Write([]byte(kubeVersion + "\x00" + sshVersion + "\x00"))
	for _, b := range sorted {
		h.Write([]byte(b.Name + "\x00" + b.server + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
