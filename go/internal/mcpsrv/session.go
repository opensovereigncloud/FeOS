// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gardenlinux/feos/go/internal/db"
)

// The JSON-RPC methods the door looks at, and the session header.
const (
	methodInitialize = "initialize"
	methodDiscover   = "server/discover"
	methodToolsCall  = "tools/call"
	methodCancelled  = "notifications/cancelled"
	sessionIDHeader  = "Mcp-Session-Id"
)

// Session ids: base64url(nonce || HMAC-SHA256(key, token hash || nonce)
// cut to macLen bytes). They bind a session to the token that opened it,
// and every instance verifies them with the shared key.
const (
	nonceLen = 16
	macLen   = 16
)

// errNoKey is a door without a session key: no database and none
// configured.
var errNoKey = errors.New("no session key")

// sessionKey is the key of the session ids: the configured one, or the
// cluster's mcp-session secret, read once.
func (s *Server) sessionKey(ctx context.Context) ([]byte, error) {
	s.keyMu.Lock()
	key := s.key
	s.keyMu.Unlock()
	if key != nil {
		return key, nil
	}
	if s.st == nil {
		return nil, errNoKey
	}
	key, err := s.st.ClusterSecret(ctx, db.SecretMCPSession)
	if err != nil {
		return nil, fmt.Errorf("read the session key: %w", err)
	}
	s.keyMu.Lock()
	s.key = key
	s.keyMu.Unlock()
	return key, nil
}

func sessionMAC(key []byte, tokenHash string, nonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(tokenHash))
	m.Write(nonce)
	return m.Sum(nil)[:macLen]
}

// newSession makes a session id for the token whose hash is tokenHash.
func (s *Server) newSession(ctx context.Context, tokenHash string) (string, error) {
	key, err := s.sessionKey(ctx)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(nonce, sessionMAC(key, tokenHash, nonce)...)), nil
}

// verifySession reports whether sid is a session id of the token whose
// hash is tokenHash. A request whose id does not verify is served as one
// without a session.
func (s *Server) verifySession(ctx context.Context, sid, tokenHash string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(sid)
	if err != nil || len(raw) != nonceLen+macLen {
		return false
	}
	key, err := s.sessionKey(ctx)
	if err != nil {
		return false
	}
	return hmac.Equal(raw[nonceLen:], sessionMAC(key, tokenHash, raw[:nonceLen]))
}

// rpcMessage is what the door reads of a JSON-RPC message.
type rpcMessage struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params json.RawMessage `json:"params"`
}

// hasID reports whether the message is a call: it has an id.
func (m rpcMessage) hasID() bool {
	return len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null"))
}

// maxBody is how much of a request body the door reads: what the SDK
// accepts.
const maxBody = mcp.DefaultMaxRequestBodyBytes

// peekBody reads the request's JSON-RPC message, or batch of them, and puts
// the body back for the SDK. An error is a body that cannot be read or is
// not JSON-RPC.
func peekBody(r *http.Request) ([]rpcMessage, error) {
	if r.Body == nil {
		return nil, errors.New("no body")
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil {
		return nil, err
	}
	if int64(len(head)) > maxBody {
		return nil, errors.New("body too large")
	}
	trimmed := bytes.TrimSpace(head)
	var msgs []rpcMessage
	if bytes.HasPrefix(trimmed, []byte("[")) {
		err = json.Unmarshal(trimmed, &msgs)
	} else {
		var m rpcMessage
		err = json.Unmarshal(trimmed, &m)
		msgs = []rpcMessage{m}
	}
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.Method == "" && !m.hasID() {
			return nil, errors.New("not a JSON-RPC message")
		}
	}
	return msgs, nil
}

// compactJSON is a JSON value without insignificant space.
func compactJSON(v json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, v); err != nil {
		return strings.TrimSpace(string(v))
	}
	return b.String()
}

// callKey names a tool call across instances: its session and its
// JSON-RPC request id.
func callKey(session string, id json.RawMessage) string {
	return session + ":" + compactJSON(id)
}
