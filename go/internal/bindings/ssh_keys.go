// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSH keys: a person either uploads the key their servers already trust
// (OpenSSH or PEM, encrypted or not) or lets ferund generate one; either
// way the key is normalised to one format, unencrypted OpenSSH, before it
// is sealed, versioned or delivered, so the pod's agent parses exactly one
// thing.

// sshKeyTypes are the key types bound, for the bound key and for host keys
// alike: no DSA, no P-521, no security keys, no certificates.
var sshKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519:  true,
	ssh.KeyAlgoECDSA256: true,
	ssh.KeyAlgoECDSA384: true,
	ssh.KeyAlgoRSA:      true,
}

// sshMinRSABits is the floor for RSA keys.
const sshMinRSABits = 2048

// checkSSHKeyType says why a public key is not one ferund binds, or nil.
func checkSSHKeyType(pub ssh.PublicKey) error {
	t := pub.Type()
	if !sshKeyTypes[t] {
		switch {
		case t == ssh.KeyAlgoDSA:
			return errors.New("DSA keys are not supported")
		case strings.HasPrefix(t, "sk-"):
			return fmt.Errorf("%s is a security-key type, which is not supported", t)
		case strings.Contains(t, "-cert-"):
			return fmt.Errorf("%s is a certificate, which is not supported", t)
		}
		return fmt.Errorf("key type %s is not supported (ssh-ed25519, ecdsa-sha2-nistp256, "+
			"ecdsa-sha2-nistp384, ssh-rsa)", t)
	}
	if t == ssh.KeyAlgoRSA {
		cpk, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return errors.New("RSA key without a usable public part")
		}
		rk, ok := cpk.CryptoPublicKey().(*rsa.PublicKey)
		if !ok {
			return errors.New("RSA key without a usable public part")
		}
		if n := rk.N.BitLen(); n < sshMinRSABits {
			return fmt.Errorf("RSA key of %d bits; at least %d are required", n, sshMinRSABits)
		}
	}
	return nil
}

// parseSSHPublicKeyLine reads one public key in authorized_keys form,
// "<type> <base64> [comment]", and answers it with its normalised line
// "<type> <base64>" (the comment dropped). Options in front of the type
// and a known_hosts host field are not this form: the first field must be
// the key's own type.
func parseSSHPublicKeyLine(line string) (ssh.PublicKey, string, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, "", errors.New(`not a public key line ("<type> <base64> [comment]")`)
	}
	if !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") &&
		!strings.HasPrefix(fields[0], "sk-") {
		return nil, "", fmt.Errorf("%q is not a key type (a known_hosts line? leave the host name out)", fields[0])
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(fields[0] + " " + fields[1]))
	if err != nil {
		return nil, "", fmt.Errorf("does not parse: %v", err)
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return nil, "", errors.New("holds more than one key")
	}
	if pub.Type() != fields[0] {
		return nil, "", fmt.Errorf("names type %s but holds a %s key", fields[0], pub.Type())
	}
	if err := checkSSHKeyType(pub); err != nil {
		return nil, "", err
	}
	return pub, sshKeyLine(pub), nil
}

// sshKeyLine is a public key as "<type> <base64>".
func sshKeyLine(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

// parseSSHPrivateKey reads an uploaded key (OpenSSH, PKCS#1, PKCS#8 or SEC1
// PEM, optionally encrypted) and answers the raw private key. The
// passphrase is used here, once, and nowhere else: a key that needs one and
// gets none, gets a wrong one, or gets one it does not need is bad_key.
func parseSSHPrivateKey(keyPEM, passphrase string) (crypto.PrivateKey, *bindingErr) {
	raw, err := ssh.ParseRawPrivateKey([]byte(keyPEM))
	var missing *ssh.PassphraseMissingError
	switch {
	case err == nil && passphrase != "":
		return nil, bindingErrf(codeBadKey, "auth.passphrase is given but the key is not encrypted")
	case err == nil:
	case errors.As(err, &missing) && passphrase == "":
		return nil, bindingErrf(codeBadKey, "the key is encrypted; auth.passphrase is required")
	case errors.As(err, &missing):
		raw, err = ssh.ParseRawPrivateKeyWithPassphrase([]byte(keyPEM), []byte(passphrase))
		if errors.Is(err, x509.IncorrectPasswordError) {
			return nil, bindingErrf(codeBadKey, "auth.passphrase does not decrypt the key")
		}
		if err != nil {
			return nil, bindingErrf(codeBadKey, "the key does not parse: %v", err)
		}
	default:
		return nil, bindingErrf(codeBadKey, "auth.keyPem does not parse as a private key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, bindingErrf(codeBadKey, "the key is not usable for SSH: %v", err)
	}
	if err := checkSSHKeyType(signer.PublicKey()); err != nil {
		return nil, bindingErrf(codeBadKey, "%v", err)
	}
	switch k := raw.(type) {
	case *rsa.PrivateKey:
		if len(k.Primes) != 2 {
			return nil, bindingErrf(codeBadKey, "multi-prime RSA keys are not supported")
		}
		if err := k.Validate(); err != nil {
			return nil, bindingErrf(codeBadKey, "the RSA key is inconsistent: %v", err)
		}
		k.Precompute()
	case *ecdsa.PrivateKey, ed25519.PrivateKey, *ed25519.PrivateKey:
	default:
		return nil, bindingErrf(codeBadKey, "unsupported key %T", raw)
	}
	return raw, nil
}

// normalizeSSHPrivateKey writes a key as unencrypted OpenSSH with comment.
// The format carries two "check" integers that x/crypto fills with random
// bytes; they matter only to decryption, which an unencrypted key has none
// of, so they are set from the public key instead. That makes the output a
// function of key and comment: the same upload twice is the same string,
// so the same set has the same version and a repeated PUT stays a no-op.
func normalizeSSHPrivateKey(raw crypto.PrivateKey, comment string) (string, error) {
	blk, err := ssh.MarshalPrivateKey(raw, comment)
	if err != nil {
		return "", err
	}
	if err := fixOpenSSHCheck(blk.Bytes); err != nil {
		return "", err
	}
	return opensshArmor(blk.Bytes), nil
}

// opensshArmor writes an openssh-key-v1 blob the way OpenSSH does: base64
// in lines of 70. encoding/pem wraps at 64, which OpenSSH reads but strict
// parsers (RustCrypto's ssh-key, the agent's) refuse.
func opensshArmor(blob []byte) string {
	const width = 70
	b64 := base64.StdEncoding.EncodeToString(blob)
	var sb strings.Builder
	sb.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for len(b64) > width {
		sb.WriteString(b64[:width])
		sb.WriteByte('\n')
		b64 = b64[width:]
	}
	sb.WriteString(b64)
	sb.WriteString("\n-----END OPENSSH PRIVATE KEY-----\n")
	return sb.String()
}

// fixOpenSSHCheck overwrites the check integers of an unencrypted
// openssh-key-v1 blob in place with the first four bytes of the SHA-256 of
// its public key blob.
func fixOpenSSHCheck(b []byte) error {
	const magic = "openssh-key-v1\x00"
	if !strings.HasPrefix(string(b), magic) {
		return errors.New("openssh key: bad magic")
	}
	off := len(magic)
	str := func() ([]byte, error) {
		if len(b) < off+4 {
			return nil, errors.New("openssh key: truncated")
		}
		n := int(binary.BigEndian.Uint32(b[off:]))
		if n < 0 || len(b) < off+4+n {
			return nil, errors.New("openssh key: truncated")
		}
		s := b[off+4 : off+4+n]
		off += 4 + n
		return s, nil
	}
	cipherName, err := str()
	if err != nil {
		return err
	}
	if string(cipherName) != "none" {
		return errors.New("openssh key: encrypted")
	}
	for range 2 { // kdfname, kdfoptions
		if _, err := str(); err != nil {
			return err
		}
	}
	if len(b) < off+4 || binary.BigEndian.Uint32(b[off:]) != 1 {
		return errors.New("openssh key: not exactly one key")
	}
	off += 4
	pubBlob, err := str()
	if err != nil {
		return err
	}
	priv, err := str()
	if err != nil {
		return err
	}
	if len(priv) < 8 {
		return errors.New("openssh key: truncated")
	}
	sum := sha256.Sum256(pubBlob)
	copy(priv[0:4], sum[:4])
	copy(priv[4:8], sum[:4])
	return nil
}

// generateSSHKey makes the key of a generate binding: Ed25519, already
// normalised.
func generateSSHKey(comment string) (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return normalizeSSHPrivateKey(priv, comment)
}

// sshKeyComment is the comment a binding's key carries, in its public line
// and in the normalised private key: "<name>@sandbox-<first 8 of id>",
// what a person sees in authorized_keys or a deploy-key list and can trace
// back to the sandbox.
func sshKeyComment(name, sandboxID string) string {
	short := sandboxID
	if len(short) > 8 {
		short = short[:8]
	}
	return name + "@sandbox-" + short
}

// sshKeyFacts are the public facts of a normalised private key: its type,
// its SHA256 fingerprint (what ssh-keygen -l prints) and its public line
// with comment.
type sshKeyFacts struct {
	keyType, fingerprint, publicKey string
}

func sshFactsOf(privateKey, comment string) (sshKeyFacts, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return sshKeyFacts{}, err
	}
	pub := signer.PublicKey()
	line := sshKeyLine(pub)
	if comment != "" {
		line += " " + comment
	}
	return sshKeyFacts{keyType: pub.Type(), fingerprint: ssh.FingerprintSHA256(pub), publicKey: line}, nil
}
