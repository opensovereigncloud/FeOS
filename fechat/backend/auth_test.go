// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeIdP is an OpenID provider that signs whoever the test names in.
type fakeIdP struct {
	srv  *httptest.Server
	key  *rsa.PrivateKey
	mu   sync.Mutex
	auth map[string]url.Values // code -> the authorization request
	// What the next ID token says.
	nonceOverride string
	groups        []string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, auth: map[string]url.Values{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL + "/",
			"authorization_endpoint":                f.srv.URL + "/oauth2/auth",
			"token_endpoint":                        f.srv.URL + "/oauth2/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"end_session_endpoint":                  f.srv.URL + "/oauth2/sessions/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		req, ok := f.auth[r.PostForm.Get("code")]
		f.mu.Unlock()
		id, secret, _ := r.BasicAuth()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || id != "fechat" || secret != "s3cret" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != req.Get("code_challenge") {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		nonce := req.Get("nonce")
		w.Header().Set("Content-Type", "application/json")
		if f.nonceOverride != "" {
			nonce = f.nonceOverride
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": f.idToken(t, map[string]any{
				"iss": f.srv.URL + "/", "aud": "fechat", "sub": "u-1", "nonce": nonce,
				"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
				"preferred_username": "octo", "name": "Octo Cat", "email": "octo@example.com",
				"groups": f.groups,
			}),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) idToken(t *testing.T, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := obj.CompactSerialize()
	return s
}

// authorize stands in for the browser at the IdP: it takes the request
// and answers with a code.
func (f *fakeIdP) authorize(t *testing.T, location string) (code, state string) {
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, f.srv.URL+"/oauth2/auth?") {
		t.Fatalf("not sent to the IdP: %s", location)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("client_id") != "fechat" {
		t.Fatalf("authorization request %v", q)
	}
	code = fmt.Sprintf("code-%d", time.Now().UnixNano())
	f.mu.Lock()
	f.auth[code] = q
	f.mu.Unlock()
	return code, q.Get("state")
}

type authHarness struct {
	t   *testing.T
	idp *fakeIdP
	srv *httptest.Server
	a   *auth
}

func newAuthHarness(t *testing.T) *authHarness {
	idp := newFakeIdP(t)
	h := &authHarness{t: t, idp: idp}
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) {
		u, _ := userFrom(r.Context())
		fmt.Fprint(w, "hello "+u.Login)
	}
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	a, err := newAuth(idp.srv.URL+"/", "fechat", "s3cret", h.srv.URL, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	h.a = a
	a.routes(mux)
	mux.HandleFunc("/api/v1/chat", a.require(ok))
	return h
}

type jar map[string]string

func (h *authHarness) do(j jar, method, path, origin string) *http.Response {
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range j {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(j, c.Name)
		} else {
			j[c.Name] = c.Value
		}
	}
	return resp
}

// signIn runs the whole sign-in and returns the callback's answer.
func (h *authHarness) signIn(j jar, ret string) *http.Response {
	r := h.do(j, http.MethodGet, "/api/v1/auth/login?return="+url.QueryEscape(ret), "")
	if r.StatusCode != http.StatusFound {
		h.t.Fatalf("login: %d", r.StatusCode)
	}
	code, state := h.idp.authorize(h.t, r.Header.Get("Location"))
	return h.do(j, http.MethodGet, "/api/v1/auth/callback?code="+code+"&state="+url.QueryEscape(state), "")
}

func readBody(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	return string(b)
}

func TestSignIn(t *testing.T) {
	h := newAuthHarness(t)
	j := jar{}
	if r := h.do(j, http.MethodPost, "/api/v1/chat", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("chat without a session: %d", r.StatusCode)
	}
	if r := h.do(j, http.MethodGet, "/api/v1/auth/me", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me without a session: %d", r.StatusCode)
	}

	h.idp.groups = []string{"admins"}
	r := h.signIn(j, "/c/42")
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/c/42" {
		t.Fatalf("callback: %d %s %s", r.StatusCode, r.Header.Get("Location"), readBody(r))
	}
	if _, ok := j[loginCookie]; ok {
		t.Fatal("the login cookie stayed")
	}
	r = h.do(j, http.MethodGet, "/api/v1/auth/me", "")
	var u User
	json.NewDecoder(r.Body).Decode(&u)
	if r.StatusCode != http.StatusOK || u.Login != "octo" || u.Subject != "u-1" || !u.Admin {
		t.Fatalf("me: %d %+v", r.StatusCode, u)
	}
	if r := h.do(j, http.MethodPost, "/api/v1/chat", h.srv.URL); readBody(r) != "hello octo" {
		t.Fatalf("chat with a session: %d", r.StatusCode)
	}
	if r := h.do(j, http.MethodPost, "/api/v1/chat", "https://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin chat: %d", r.StatusCode)
	}

	// Signing out ends the session and points at the IdP's end.
	r = h.do(j, http.MethodPost, "/api/v1/auth/logout", h.srv.URL)
	var out map[string]string
	json.NewDecoder(r.Body).Decode(&out)
	if out["redirect"] != h.idp.srv.URL+"/oauth2/sessions/logout" {
		t.Fatalf("logout: %v", out)
	}
	if r := h.do(j, http.MethodPost, "/api/v1/chat", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("chat after logout: %d", r.StatusCode)
	}
}

func TestSignInRefusals(t *testing.T) {
	h := newAuthHarness(t)

	// A state that is not the login cookie's.
	j := jar{}
	r := h.do(j, http.MethodGet, "/api/v1/auth/login", "")
	code, _ := h.idp.authorize(t, r.Header.Get("Location"))
	if r := h.do(j, http.MethodGet, "/api/v1/auth/callback?code="+code+"&state=forged", ""); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: %d", r.StatusCode)
	}

	// No login cookie at all: a callback started elsewhere.
	r = h.do(jar{}, http.MethodGet, "/api/v1/auth/login", "")
	code, state := h.idp.authorize(t, r.Header.Get("Location"))
	if r := h.do(jar{}, http.MethodGet, "/api/v1/auth/callback?code="+code+"&state="+state, ""); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without the login cookie: %d", r.StatusCode)
	}

	// An ID token with another nonce.
	h.idp.nonceOverride = "replayed"
	j = jar{}
	if r := h.signIn(j, "/"); r.StatusCode != http.StatusBadGateway {
		t.Fatalf("wrong nonce: %d", r.StatusCode)
	}
	if _, ok := j[sessionCookie]; ok {
		t.Fatal("a session for a wrong nonce")
	}

	// A forged session cookie.
	forged := jar{sessionCookie: "eyJ1Ijp7ImxvZ2luIjoiZXZlIn0sImUiOjk5OTk5OTk5OTl9.AAAA"}
	if r := h.do(forged, http.MethodGet, "/api/v1/auth/me", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged session: %d", r.StatusCode)
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/c/1": "/c/1", "": "/", "//evil.example": "/", "https://evil.example": "/",
		"/\\evil.example": "/", "/api/v1/auth/logout": "/", "c/1": "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}
