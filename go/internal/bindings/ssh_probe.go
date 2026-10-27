// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// The SSH preflight and check make one SSH connection from this instance,
// as far as a login and not one step further: TCP through the destination
// rule's dialer, the key exchange, the host key checked against the pinned
// ones, then, with a key, publickey authentication as the binding's user,
// and the connection is closed the moment USERAUTH_SUCCESS arrives. No
// session, no channel, no request: nothing runs on the server. Without a
// key the connection ends inside the key exchange, right after the server
// has proven it holds the host key it presented and before any
// authentication request is sent.

// sshPreflightClientVersion is how the preflight introduces itself in the
// server's log.
const sshPreflightClientVersion = "SSH-2.0-ferund-preflight"

type sshPresentedKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Line        string `json:"line"`
}

type sshPreflightResult struct {
	ServerVersion string          `json:"serverVersion"`
	HostKey       sshPresentedKey `json:"hostKey"`
	// Authenticated is false when no key was given: the preflight then
	// only reports the host key.
	Authenticated bool `json:"authenticated"`
}

// sshProbe is one connection's worth of binding: where, as whom, which
// host keys to insist on (none: report what is presented) and the key to
// log in with (nil: stop after the key exchange).
type sshProbe struct {
	server   string // for messages: the binding's ssh:// URL
	host     string
	port     int
	user     string
	hostKeys []ssh.PublicKey
	signer   ssh.Signer
}

// The two ways the probe ends a handshake on purpose, from inside the host
// key callback.
var (
	errSSHHostKeyMismatch = errors.New("host key mismatch")
	errSSHKexOnly         = errors.New("key exchange done, no authentication asked for")
)

// sshDefaultHostKeyAlgos is the host key preference without pinned keys:
// OpenSSH's order over the supported types, so the key reported is the one
// the sandbox's own ssh would most likely be shown.
var sshDefaultHostKeyAlgos = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
}

// sshHostKeyAlgos offers exactly the pinned keys' algorithms, as OpenSSH
// does with keys in known_hosts: a server with several host keys is asked
// for one the binding names.
func sshHostKeyAlgos(pinned []ssh.PublicKey) []string {
	if len(pinned) == 0 {
		return sshDefaultHostKeyAlgos
	}
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, k := range pinned {
		if k.Type() == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA512) // never SHA-1
			add(ssh.KeyAlgoRSASHA256)
			continue
		}
		add(k.Type())
	}
	return out
}

// versionRecorder keeps the first bytes the server sends, which hold its
// identification line: the one thing a failed handshake does not hand
// back otherwise.
type versionRecorder struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
}

func (r *versionRecorder) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	r.mu.Lock()
	if room := 8192 - len(r.buf); room > 0 && n > 0 {
		r.buf = append(r.buf, b[:min(n, room)]...)
	}
	r.mu.Unlock()
	return n, err
}

// version is the server's "SSH-" line (RFC 4253 §4.2 allows other lines
// before it), or "".
func (r *versionRecorder) version() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for rest := r.buf; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			return ""
		}
		line := strings.TrimRight(string(rest[:i]), "\r")
		if strings.HasPrefix(line, "SSH-") {
			return line
		}
		rest = rest[i+1:]
	}
	return ""
}

// probe makes the connection within the budget, with the destination rule
// on the dialled address, and answers the codes for every way it can end.
func (s *Service) probe(ctx context.Context, p sshProbe) (*sshPreflightResult, *bindingErr) {
	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	addr := net.JoinHostPort(p.host, strconv.Itoa(p.port))
	raw, err := s.dest.dialer().DialContext(ctx, "tcp4", addr)
	if err != nil {
		var denied *errDialDenied
		if errors.As(err, &denied) {
			return nil, bindingErrf(codeDestinationDenied, "%s: %v", p.server, denied.err)
		}
		return nil, bindingErrf(codeUnreachable, "%s: %v", p.server, err)
	}
	deadline, _ := ctx.Deadline()
	_ = raw.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	conn := &versionRecorder{Conn: raw}

	var presented ssh.PublicKey
	var mismatch, kexOnly bool // what the callback decided
	cfg := &ssh.ClientConfig{
		User:              p.user,
		ClientVersion:     sshPreflightClientVersion,
		HostKeyAlgorithms: sshHostKeyAlgos(p.hostKeys),
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			// x/crypto has verified the server's signature with key by now:
			// the server holds it.
			presented = key
			if len(p.hostKeys) > 0 && !sshKeyIn(key, p.hostKeys) {
				mismatch = true
				return errSSHHostKeyMismatch
			}
			if p.signer == nil {
				kexOnly = true
				return errSSHKexOnly
			}
			return nil
		},
	}
	if p.signer != nil {
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(p.signer)}
	}
	c, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if err == nil {
		// USERAUTH_SUCCESS: done. Closing here is the point: no channel is
		// ever opened.
		_ = c.Close()
	} else {
		_ = raw.Close()
	}
	res := &sshPreflightResult{ServerVersion: conn.version(), Authenticated: err == nil}
	if presented != nil {
		res.HostKey = sshPresentedKey{Type: presented.Type(),
			Fingerprint: ssh.FingerprintSHA256(presented), Line: sshKeyLine(presented)}
	}
	timedOut := ctx.Err() != nil || isTimeout(err)
	switch {
	case err == nil, kexOnly:
		return res, nil
	case mismatch:
		return nil, bindingErrf(codeHostKeyMismatch,
			"%s presented the host key %s (%s), which is not among the binding's hostKeys",
			p.server, res.HostKey.Fingerprint, res.HostKey.Line)
	case presented == nil && timedOut:
		return nil, bindingErrf(codeUnreachable, "%s: no SSH key exchange within %s", p.server, s.budget)
	case presented == nil && strings.Contains(err.Error(), "no common algorithm for host key"):
		if len(p.hostKeys) > 0 {
			return nil, bindingErrf(codeHostKeyMismatch,
				"%s offers no host key of the types in the binding's hostKeys (%s)",
				p.server, strings.Join(sshHostKeyAlgos(p.hostKeys), ", "))
		}
		return nil, bindingErrf(codeUnreachable,
			"%s offers no host key of a supported type (ssh-ed25519, ecdsa-sha2-nistp256/384, ssh-rsa)", p.server)
	case presented == nil:
		return nil, bindingErrf(codeUnreachable, "%s: not an SSH server, or the key exchange failed: %v", p.server, err)
	case timedOut:
		return nil, bindingErrf(codeUnreachable, "%s: authentication did not finish within %s", p.server, s.budget)
	default:
		// The key exchange succeeded and the login did not: the server
		// refused the key for this user (or hung up on it).
		return nil, bindingErrf(codeUnauthorized, "%s refused the key %s for user %q: %v",
			p.server, ssh.FingerprintSHA256(p.signer.PublicKey()), p.user, err)
	}
}

func sshKeyIn(k ssh.PublicKey, set []ssh.PublicKey) bool {
	b := k.Marshal()
	for _, s := range set {
		if bytes.Equal(b, s.Marshal()) {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	var ne net.Error
	return err != nil && (errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded))
}
